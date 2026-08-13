// agw-verify-apply is the offline hand-off between the credentialed fetch
// initContainer and the networkless verifier. It does not receive a Secret,
// executes no repository command, and accepts only the fixed contract emitted
// by internal/verifyworkload.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	contractVersion  = 1
	maxSpecJSONBytes = 16 << 10
	maxPatchBytes    = 1 << 30
	maxOutputBytes   = 128 << 10
)

var (
	errInvalidContract = errors.New("invalid verify-apply contract")
	errWorkspace       = errors.New("verify-apply workspace is unsafe")
	errGitOperation    = errors.New("verify-apply git operation failed")
	errPatch           = errors.New("verify-apply patch is invalid")
)

type applySpec struct {
	Version        int    `json:"version"`
	RepoPath       string `json:"repoPath"`
	BasePath       string `json:"basePath"`
	PatchPath      string `json:"patchPath"`
	BaseSHA        string `json:"baseSHA"`
	PatchDigest    string `json:"patchDigest"`
	PatchSizeBytes int64  `json:"patchSizeBytes"`
	MaxPatchBytes  int64  `json:"maxPatchBytes"`
}

func run(ctx context.Context, lookup func(string) (string, bool)) error {
	if ctx == nil || lookup == nil {
		return errInvalidContract
	}
	spec, err := parseContract(lookup)
	if err != nil {
		return err
	}
	if err := validateWorkspace(spec); err != nil {
		return err
	}
	patch, err := readPatch(spec)
	if err != nil {
		return err
	}
	env := safeGitEnvironment()
	if err := verifyCheckoutSHA(ctx, spec.RepoPath, env, spec.BaseSHA); err != nil {
		return err
	}
	if _, err := runGit(ctx, spec.RepoPath, env, repoGitArgs(spec.RepoPath, "apply", "--check", "--", spec.PatchPath)...); err != nil {
		return errGitOperation
	}
	if _, err := runGit(ctx, spec.RepoPath, env, repoGitArgs(spec.RepoPath, "apply", "--whitespace=error", "--", spec.PatchPath)...); err != nil {
		return errGitOperation
	}
	if digestBytes(patch) != spec.PatchDigest {
		return errPatch
	}
	if err := verifyCheckoutSHA(ctx, spec.RepoPath, env, spec.BaseSHA); err != nil {
		return err
	}
	return nil
}

func parseContract(lookup func(string) (string, bool)) (applySpec, error) {
	for _, env := range os.Environ() {
		key, _, ok := strings.Cut(env, "=")
		if ok && strings.HasPrefix(key, "AGW_APPLY_") && key != "AGW_APPLY_SPEC_JSON" {
			return applySpec{}, errInvalidContract
		}
	}
	raw, ok := lookup("AGW_APPLY_SPEC_JSON")
	if !ok || raw == "" || len(raw) > maxSpecJSONBytes || strings.ContainsRune(raw, '\x00') || strictjson.ValidateObject([]byte(raw)) != nil {
		return applySpec{}, errInvalidContract
	}
	var spec applySpec
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return applySpec{}, errInvalidContract
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return applySpec{}, errInvalidContract
	}
	if spec.Version != contractVersion || spec.RepoPath != verifyworkload.RepoPath || spec.BasePath != verifyworkload.BasePath || spec.PatchPath != verifyworkload.PatchPath || !validSHA(spec.BaseSHA) || !canonical.ValidDigest(spec.PatchDigest) || spec.PatchSizeBytes < 0 || spec.MaxPatchBytes <= 0 || spec.MaxPatchBytes > maxPatchBytes || spec.PatchSizeBytes > spec.MaxPatchBytes {
		return applySpec{}, errInvalidContract
	}
	return spec, nil
}

func validateWorkspace(spec applySpec) error {
	for _, value := range []string{spec.RepoPath, spec.BasePath, spec.PatchPath} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || len(value) > 4096 || strings.ContainsRune(value, '\x00') || ensureNoSymlinkComponents(filepath.Dir(value)) != nil {
			return errWorkspace
		}
	}
	if filepath.Dir(spec.RepoPath) != verifyworkload.WorkspaceMountPath || filepath.Dir(spec.BasePath) != verifyworkload.WorkspaceMountPath || filepath.Dir(spec.PatchPath) != verifyworkload.WorkspaceMountPath {
		return errWorkspace
	}
	for _, value := range []string{spec.RepoPath, spec.BasePath} {
		info, err := os.Stat(value)
		if err != nil || !info.IsDir() {
			return errWorkspace
		}
	}
	if err := verifyReadOnlyTree(spec.BasePath); err != nil {
		return err
	}
	return nil
}

func readPatch(spec applySpec) ([]byte, error) {
	info, err := os.Lstat(spec.PatchPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errPatch
	}
	file, err := os.Open(spec.PatchPath)
	if err != nil {
		return nil, errPatch
	}
	defer file.Close()
	patch, err := io.ReadAll(io.LimitReader(file, spec.MaxPatchBytes+1))
	if err != nil || int64(len(patch)) != spec.PatchSizeBytes || int64(len(patch)) > spec.MaxPatchBytes || digestBytes(patch) != spec.PatchDigest || !utf8.Valid(patch) {
		return nil, errPatch
	}
	return patch, nil
}

func verifyCheckoutSHA(ctx context.Context, repo string, env []string, expected string) error {
	output, err := runGit(ctx, repo, env, repoGitArgs(repo, "rev-parse", "--verify", "HEAD^{commit}")...)
	if err != nil || strings.TrimSpace(string(output)) != expected {
		return errGitOperation
	}
	return nil
}

// repoGitArgs opts the verifier into exactly its fixed checkout path. The
// fetch init runs as namespace root and the apply/verifier steps run as UID
// 1000, so Git's ownership guard would otherwise reject this intentional
// hand-off. No repository-controlled or global Git configuration is enabled.
func repoGitArgs(repo string, args ...string) []string {
	result := make([]string, 0, len(args)+2)
	result = append(result, "-c", "safe.directory="+repo)
	return append(result, args...)
}

func safeGitEnvironment() []string {
	return []string{
		"HOME=/nonexistent",
		"PATH=/usr/bin:/bin",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=none",
		"GIT_OPTIONAL_LOCKS=0",
	}
}

func runGit(ctx context.Context, repo string, env []string, args ...string) ([]byte, error) {
	if ctx == nil || repo == "" || len(args) == 0 {
		return nil, errGitOperation
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = repo
	command.Env = append([]string(nil), env...)
	var stdout, stderr boundedBuffer
	stdout.max, stderr.max = maxOutputBytes, maxOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil || stdout.tooLarge || stderr.tooLarge {
		return nil, errGitOperation
	}
	return stdout.Bytes(), nil
}

func verifyReadOnlyTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink != 0 || entry.Type()&os.ModeNamedPipe != 0 || entry.Type()&os.ModeDevice != 0 || entry.Type()&os.ModeSocket != 0 {
			return errWorkspace
		}
		if entry.IsDir() {
			if entry.Type().Perm()&0222 != 0 {
				return errWorkspace
			}
			return nil
		}
		if entry.Type().Perm()&0222 != 0 {
			return errWorkspace
		}
		return nil
	})
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

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func digestBytes(body []byte) string {
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
