// The verify-fetch executable is the only verify init container that receives
// credentials. It performs setup work only: a fresh exact-SHA GitHub clone,
// an exact S3-compatible object read, and creation of a pristine read-only
// base tree. It never runs repository-authored code.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	contractVersion = 1

	maxSpecJSONBytes   = 32 << 10
	maxRepositoryBytes = 253
	maxDepth           = 100
	maxTokenBytes      = 64 << 10
	maxGitOutputBytes  = 128 << 10
	maxArchiveBytes    = 1 << 30
	maxArchiveFiles    = 1_000_000
	maxArchiveFileSize = 1 << 30
	maxPathBytes       = 4096

	defaultGitDepth = 1
)

var (
	errInvalidContract = errors.New("invalid verify-fetch contract")
	errGitOperation    = errors.New("verify-fetch git operation failed")
	errWorkspace       = errors.New("verify-fetch workspace is unsafe")
	errPatch           = errors.New("verify-fetch patch is invalid")
	errArchive         = errors.New("verify-fetch base archive is invalid")

	githubRepositoryPattern = regexp.MustCompile(`^github[.]com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	shaPattern              = regexp.MustCompile(`^[0-9a-f]+$`)
)

type fetchSpec struct {
	Version                     int                     `json:"version"`
	Repository                  string                  `json:"repository"`
	BaseSHA                     string                  `json:"baseSHA"`
	Depth                       int                     `json:"depth"`
	RepoPath                    string                  `json:"repoPath"`
	BasePath                    string                  `json:"basePath"`
	PatchPath                   string                  `json:"patchPath"`
	PatchBucket                 string                  `json:"patchBucket"`
	PatchKey                    string                  `json:"patchKey"`
	PatchDigest                 string                  `json:"patchDigest"`
	PatchSizeBytes              int64                   `json:"patchSizeBytes"`
	MaxPatchBytes               int64                   `json:"maxPatchBytes"`
	CloneTokenFile              string                  `json:"cloneTokenFile"`
	ArtifactAccessKeyIDFile     string                  `json:"artifactAccessKeyIDFile"`
	ArtifactSecretAccessKeyFile string                  `json:"artifactSecretAccessKeyFile"`
	ArtifactSessionTokenFile    string                  `json:"artifactSessionTokenFile,omitempty"`
	ArtifactStore               verifyfetch.StoreConfig `json:"artifactStore"`
	DisableHooks                bool                    `json:"disableHooks"`
	RequireExactBase            bool                    `json:"requireExactBase"`
}

// run is kept dependency-light so the production entrypoint cannot be
// tricked into printing a command's stderr or a credential. All diagnostics
// are intentionally converted to stable, non-secret errors by main.
func run(ctx context.Context, lookup func(string) (string, bool)) error {
	if ctx == nil || lookup == nil {
		return errInvalidContract
	}
	spec, err := parseContract(lookup)
	if err != nil {
		return err
	}
	if err := ensureWorkspace(spec); err != nil {
		return err
	}
	token, err := readSecretFile(spec.CloneTokenFile, maxTokenBytes, false)
	if err != nil {
		return errInvalidContract
	}
	if err := cloneExact(ctx, spec, token); err != nil {
		return err
	}
	accessKeyID, err := readSecretFile(spec.ArtifactAccessKeyIDFile, verifyfetch.MaxAccessKeyBytes, false)
	if err != nil {
		return errInvalidContract
	}
	secretAccessKey, err := readSecretFile(spec.ArtifactSecretAccessKeyFile, verifyfetch.MaxSecretKeyBytes, false)
	if err != nil {
		return errInvalidContract
	}
	sessionToken := ""
	if spec.ArtifactSessionTokenFile != "" {
		sessionToken, err = readSecretFile(spec.ArtifactSessionTokenFile, verifyfetch.MaxSessionTokenBytes, true)
		if err != nil {
			return errInvalidContract
		}
	}
	getter, err := verifyfetch.NewS3Getter(ctx, spec.ArtifactStore, verifyfetch.Credentials{
		AccessKeyID: accessKeyID, SecretAccessKey: secretAccessKey, SessionToken: sessionToken,
	})
	if err != nil {
		return err
	}
	patch, err := getter.Get(ctx, spec.PatchKey, spec.PatchSizeBytes)
	if err != nil {
		return err
	}
	if err := validatePatchBytes(spec, patch); err != nil {
		return err
	}
	// The patch is non-secret evidence. It must be readable by the UID 1000
	// apply initContainer, while credentials remain only in the Secret mount.
	if err := writeNewRegularFile(spec.PatchPath, patch, 0644); err != nil {
		return errPatch
	}
	return nil
}

func validatePatchBytes(spec fetchSpec, patch []byte) error {
	if int64(len(patch)) != spec.PatchSizeBytes || int64(len(patch)) > spec.MaxPatchBytes || digest(patch) != spec.PatchDigest {
		return errPatch
	}
	return nil
}

func parseContract(lookup func(string) (string, bool)) (fetchSpec, error) {
	for _, env := range os.Environ() {
		key, _, ok := strings.Cut(env, "=")
		if ok && strings.HasPrefix(key, "AGW_FETCH_") && key != "AGW_FETCH_SPEC_JSON" {
			return fetchSpec{}, errInvalidContract
		}
	}
	raw, ok := lookup("AGW_FETCH_SPEC_JSON")
	if !ok || raw == "" || len(raw) > maxSpecJSONBytes || strings.ContainsRune(raw, '\x00') || strictjson.ValidateObject([]byte(raw)) != nil {
		return fetchSpec{}, errInvalidContract
	}
	var spec fetchSpec
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return fetchSpec{}, errInvalidContract
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fetchSpec{}, errInvalidContract
	}
	if err := validateSpec(spec); err != nil {
		return fetchSpec{}, err
	}
	return spec, nil
}

func validateSpec(spec fetchSpec) error {
	if spec.Version != contractVersion || !spec.DisableHooks || !spec.RequireExactBase || !githubRepositoryPattern.MatchString(spec.Repository) || !validSHA(spec.BaseSHA) {
		return errInvalidContract
	}
	if spec.Depth < 0 || spec.Depth > maxDepth {
		return errInvalidContract
	}
	if spec.Depth == 0 {
		spec.Depth = defaultGitDepth
	}
	if spec.RepoPath != verifyworkload.RepoPath || spec.BasePath != verifyworkload.BasePath || spec.PatchPath != verifyworkload.PatchPath || spec.CloneTokenFile != verifyworkload.CloneTokenFile || spec.ArtifactAccessKeyIDFile != verifyworkload.ArtifactAccessKeyIDFile || spec.ArtifactSecretAccessKeyFile != verifyworkload.ArtifactSecretAccessKeyFile || spec.ArtifactSessionTokenFile != verifyworkload.ArtifactSessionTokenFile {
		return errInvalidContract
	}
	if !filepath.IsAbs(spec.RepoPath) || !filepath.IsAbs(spec.BasePath) || !filepath.IsAbs(spec.PatchPath) || len(spec.RepoPath) > maxPathBytes || len(spec.BasePath) > maxPathBytes || len(spec.PatchPath) > maxPathBytes {
		return errInvalidContract
	}
	if spec.PatchBucket == "" || spec.PatchBucket != spec.ArtifactStore.Bucket || verifyfetch.ValidateObjectKey(spec.PatchKey) != nil || !canonical.ValidDigest(spec.PatchDigest) || spec.PatchSizeBytes < 0 || spec.MaxPatchBytes <= 0 || spec.MaxPatchBytes > verifyfetch.PatchFetchMaxObjectBytes || spec.PatchSizeBytes > spec.MaxPatchBytes || spec.ArtifactStore.MaxObjectBytes != spec.MaxPatchBytes {
		return errInvalidContract
	}
	if err := verifyfetch.ValidateStoreConfig(spec.ArtifactStore); err != nil {
		return errInvalidContract
	}
	return nil
}

func ensureWorkspace(spec fetchSpec) error {
	workspace := filepath.Dir(spec.RepoPath)
	if workspace != filepath.Dir(spec.BasePath) || workspace != filepath.Dir(spec.PatchPath) || filepath.Clean(workspace) != verifyworkload.WorkspaceMountPath {
		return errWorkspace
	}
	if err := ensureNoSymlinkComponents(workspace); err != nil {
		return errWorkspace
	}
	if err := ensureExistingDirectory(workspace); err != nil {
		return errWorkspace
	}
	for _, target := range []string{spec.RepoPath, spec.BasePath, spec.PatchPath} {
		if _, err := os.Lstat(target); err == nil {
			return errWorkspace
		} else if !errors.Is(err, os.ErrNotExist) {
			return errWorkspace
		}
	}
	return nil
}

func cloneExact(ctx context.Context, spec fetchSpec, token string) error {
	return cloneExactFromRemote(ctx, spec, token, githubRemote(spec.Repository))
}

// cloneExactFromRemote is split out solely to make the exact-SHA and archive
// logic testable with a local bare repository. Production always calls
// cloneExact, which creates the fixed HTTPS GitHub origin.
func cloneExactFromRemote(ctx context.Context, spec fetchSpec, token, remote string) error {
	if ctx == nil || token == "" || strings.ContainsAny(token, "\x00\r\n") || remote == "" {
		return errInvalidContract
	}
	if err := os.Mkdir(spec.RepoPath, 0755); err != nil {
		return errWorkspace
	}
	tmp, err := os.MkdirTemp(filepath.Dir(spec.RepoPath), ".agw-fetch-")
	if err != nil {
		return errWorkspace
	}
	defer os.RemoveAll(tmp)
	askpass := filepath.Join(tmp, "askpass")
	if err := writeNewRegularFile(askpass, []byte("#!/bin/sh\nset -eu\ncase \"${1:-}\" in\n  *[Uu]sername*) printf '%s\\n' x-access-token ;;\n  *) exec cat \"$AGW_GIT_TOKEN_FILE\" ;;\nesac\n"), 0700); err != nil {
		return errWorkspace
	}
	gitEnv := safeGitEnvironment(askpass, spec.CloneTokenFile, tmp)
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "init", "--quiet"); err != nil {
		return errGitOperation
	}
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "config", "--local", "core.hooksPath", "/dev/null"); err != nil {
		return errGitOperation
	}
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "remote", "add", "origin", remote); err != nil {
		return errGitOperation
	}
	depth := spec.Depth
	if depth == 0 {
		depth = defaultGitDepth
	}
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "fetch", "--quiet", "--no-tags", "--depth", strconv.Itoa(depth), "origin", spec.BaseSHA); err != nil {
		return errGitOperation
	}
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "checkout", "--quiet", "--detach", "--force", spec.BaseSHA); err != nil {
		return errGitOperation
	}
	got, err := runGit(ctx, spec.RepoPath, gitEnv, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(got)) != spec.BaseSHA {
		return errGitOperation
	}
	if _, err := runGit(ctx, spec.RepoPath, gitEnv, "remote", "remove", "origin"); err != nil {
		return errGitOperation
	}
	archive, err := runGit(ctx, spec.RepoPath, gitEnv, "archive", "--format=tar", spec.BaseSHA)
	if err != nil {
		return errGitOperation
	}
	if err := os.Mkdir(spec.BasePath, 0755); err != nil {
		return errWorkspace
	}
	if err := extractArchive(spec.BasePath, archive); err != nil {
		return err
	}
	if err := makeReadOnlyTree(spec.BasePath); err != nil {
		return errWorkspace
	}
	if err := makeWritableTree(spec.RepoPath); err != nil {
		return errWorkspace
	}
	return nil
}

func githubRemote(repository string) string {
	return "https://" + repository + ".git"
}

func safeGitEnvironment(askpass, tokenFile, home string) []string {
	return []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=" + askpass,
		"AGW_GIT_TOKEN_FILE=" + tokenFile,
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_OPTIONAL_LOCKS=0",
	}
}

func runGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if ctx == nil || dir == "" || len(args) == 0 {
		return nil, errGitOperation
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append([]string(nil), env...)
	var stdout, stderr boundedBuffer
	stdout.max, stderr.max = maxGitOutputBytes, maxGitOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil || stdout.tooLarge || stderr.tooLarge {
		return nil, errGitOperation
	}
	return stdout.Bytes(), nil
}

func extractArchive(root string, archive []byte) error {
	if root == "" || !filepath.IsAbs(root) || len(archive) > maxArchiveBytes {
		return errArchive
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	seen := make(map[string]struct{})
	files := 0
	total := int64(0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errArchive
		}
		if header.Typeflag == tar.TypeXHeader || header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeGNULongName || header.Typeflag == tar.TypeGNULongLink {
			continue
		}
		name, err := safeArchiveName(header.Name, header.Typeflag == tar.TypeDir)
		if err != nil {
			return errArchive
		}
		if _, exists := seen[name]; exists {
			return errArchive
		}
		seen[name] = struct{}{}
		destination := filepath.Join(root, filepath.FromSlash(name))
		if !sameOrWithin(root, destination) || ensureNoSymlinkComponents(filepath.Dir(destination)) != nil {
			return errArchive
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 || os.Mkdir(destination, 0755) != nil {
				return errArchive
			}
		case tar.TypeReg, tar.TypeRegA:
			files++
			if files > maxArchiveFiles || header.Size < 0 || header.Size > maxArchiveFileSize || total > maxArchiveBytes-header.Size {
				return errArchive
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
				return errArchive
			}
			if err := writeTarFile(destination, reader, header.Size); err != nil {
				return errArchive
			}
			total += header.Size
		default:
			// Symlinks, hard links, devices, and FIFOs are never materialized
			// into the pristine base tree.
			return errArchive
		}
	}
	return nil
}

func safeArchiveName(name string, directory bool) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", errArchive
	}
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	clean := path.Clean(name)
	if clean == "." || clean != name || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", errArchive
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errArchive
		}
	}
	return clean, nil
}

func writeTarFile(path string, reader io.Reader, size int64) error {
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errArchive
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(file, reader, size)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return errArchive
	}
	return nil
}

func makeReadOnlyTree(root string) error {
	return filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return errWorkspace
		}
		// Symlinks are valid Git worktree entries. Leave them untouched so
		// the checkout remains faithful; chmod is never applied through one.
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.Type()&os.ModeNamedPipe != 0 || entry.Type()&os.ModeDevice != 0 || entry.Type()&os.ModeSocket != 0 {
			return errWorkspace
		}
		if entry.IsDir() {
			return os.Chmod(current, 0555)
		}
		return os.Chmod(current, 0444)
	})
}

// makeWritableTree keeps the working clone usable by the UID 1000 apply and
// verify containers without granting fetch CAP_CHOWN. The base tree is a
// separate root-owned, read-only tree and is never passed here.
func makeWritableTree(root string) error {
	return filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return errWorkspace
		}
		// Symlinks are valid Git worktree entries. Leave them untouched so
		// the checkout remains faithful; chmod is never applied through one.
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.Type()&os.ModeNamedPipe != 0 || entry.Type()&os.ModeDevice != 0 || entry.Type()&os.ModeSocket != 0 {
			return errWorkspace
		}
		info, err := entry.Info()
		if err != nil {
			return errWorkspace
		}
		if info.IsDir() {
			return os.Chmod(current, 0777)
		}
		if info.Mode().Perm()&0111 != 0 {
			return os.Chmod(current, 0777)
		}
		return os.Chmod(current, 0666)
	})
}

func readSecretFile(name string, max int, optional bool) (string, error) {
	if name == "" {
		if optional {
			return "", nil
		}
		return "", errInvalidContract
	}
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && optional {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return "", errInvalidContract
	}
	file, err := os.Open(name)
	if err != nil {
		return "", errInvalidContract
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, int64(max)+1))
	if err != nil || len(value) == 0 || len(value) > max || !utf8.Valid(value) || strings.TrimSpace(string(value)) != string(value) || strings.ContainsAny(string(value), "\x00\r\n") {
		return "", errInvalidContract
	}
	return string(value), nil
}

func writeNewRegularFile(name string, body []byte, mode os.FileMode) error {
	if name == "" || !filepath.IsAbs(name) {
		return errWorkspace
	}
	if err := ensureNoSymlinkComponents(filepath.Dir(name)); err != nil {
		return errWorkspace
	}
	if _, err := os.Lstat(name); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errWorkspace
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errWorkspace
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return errWorkspace
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return errWorkspace
	}
	return nil
}

func ensureExistingDirectory(name string) error {
	info, err := os.Stat(name)
	if err != nil || !info.IsDir() {
		return errWorkspace
	}
	return nil
}

func ensureNoSymlinkComponents(name string) error {
	clean := filepath.Clean(name)
	if !filepath.IsAbs(clean) {
		return errWorkspace
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errWorkspace
		}
	}
	return nil
}

func sameOrWithin(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if root == candidate {
		return true
	}
	return strings.HasPrefix(candidate, root+string(filepath.Separator))
}

func validSHA(value string) bool {
	return (len(value) == 40 || len(value) == 64) && shaPattern.MatchString(value)
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

type boundedBuffer struct {
	bytes.Buffer
	max      int
	tooLarge bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		b.tooLarge = true
		return 0, io.ErrShortWrite
	}
	return b.Buffer.Write(p)
}

var _ = time.Second
