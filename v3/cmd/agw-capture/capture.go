// The agw-capture executable is the trusted, networkless boundary between a
// work Sandbox and independent verification. It intentionally uses Git's
// plumbing with a temporary index and object store instead of `git add`: a
// repository can configure clean filters, hooks, or external helpers, and
// none of those are allowed to run while evidence is being produced.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	contractVersion = 2

	maxSpecJSONBytes = 128 << 10
	maxEnvValueBytes = 128 << 10
	maxPathBytes     = 4096
	maxTrackedPaths  = 1_000_000
	maxGitStderr     = 64 << 10
	maxGitStdout     = 4 << 20
	maxFileBytes     = int64(1 << 30)

	gitConfigFile    = "/dev/null"
	noNetworkCommand = "/bin/false"
)

var (
	errInvalidContract = errors.New("invalid capture contract")
	errUnsafeWorktree  = errors.New("unsafe worktree")
	errGitOperation    = errors.New("git operation failed")
	errBoundExceeded   = errors.New("capture bound exceeded")
	errUnsafePath      = errors.New("unsafe repository path")
	errSpecialFile     = errors.New("special file is not capturable")
)

// executionSpec mirrors the private captureSpec emitted by internal/capture.
// Keep this structure deliberately closed: unknown fields at this boundary
// are configuration drift, not harmless forward compatibility.
type executionSpec struct {
	Version            int                       `json:"version"`
	Protocol           string                    `json:"protocol"`
	ResolvedSpecDigest string                    `json:"resolvedSpecDigest"`
	BaseSHA            string                    `json:"baseSHA"`
	RepoPath           string                    `json:"repoPath"`
	OutputDir          string                    `json:"outputDir"`
	PatchPath          string                    `json:"patchPath"`
	ManifestPath       string                    `json:"manifestPath"`
	ResultPath         string                    `json:"resultPath"`
	Scope              v1alpha1.ScopeSpec        `json:"scope"`
	Requirements       v1alpha1.GateRequirements `json:"requirements"`
	MaxPatchBytes      int64                     `json:"maxPatchBytes"`
	MaxResultBytes     int64                     `json:"maxResultBytes"`
	MaxManifestBytes   int64                     `json:"maxManifestBytes"`
}

type contract struct {
	Spec         executionSpec
	RunUID       string
	GitPath      string
	MaxPatch     int64
	MaxResult    int64
	MaxManifest  int64
	RepoPath     string
	OutputDir    string
	PatchPath    string
	ManifestPath string
	ResultPath   string
}

type patchEvidence struct {
	paths  []string
	lines  int64
	binary bool
}

type treeEntry struct {
	mode uint32
	oid  string
	path string
}

// run is kept injectable for unit and temp-repository integration tests. The
// production caller passes os.LookupEnv and os.Stdout.
func run(ctx context.Context, lookup func(string) (string, bool), stdout io.Writer) error {
	if ctx == nil || lookup == nil || stdout == nil {
		return errInvalidContract
	}
	c, err := parseContract(lookup)
	if err != nil {
		return err
	}
	patch, manifest, manifestDigest, evidence, err := capturePatch(ctx, c)
	if err != nil {
		return err
	}
	if err := enforceEvidence(c, evidence, int64(len(patch))); err != nil {
		return err
	}

	patchDigest := sha256Digest(patch)
	envelope := capture.ResultEnvelope{
		SchemaVersion:  capture.ResultSchemaVersion,
		RunUID:         c.RunUID,
		SpecDigest:     c.Spec.ResolvedSpecDigest,
		BaseSHA:        c.Spec.BaseSHA,
		PatchDigest:    patchDigest,
		PatchBytes:     int64(len(patch)),
		ManifestDigest: manifestDigest,
		ManifestBytes:  int64(len(manifest)),
		FilesChanged:   int64(len(evidence.paths)),
		LinesChanged:   evidence.lines,
		HasBinaryFiles: evidence.binary,
		ChangedPaths:   evidence.paths,
	}
	resultJSON, err := capture.MarshalResult(envelope)
	if err != nil {
		return fmt.Errorf("marshal capture result: %w", err)
	}
	if int64(len(resultJSON)) > c.MaxResult {
		return fmt.Errorf("%w: result JSON is larger than configured bound", errBoundExceeded)
	}
	if err := writeRegularFile(c.PatchPath, patch); err != nil {
		return fmt.Errorf("write patch staging file: %w", err)
	}
	if err := writeRegularFile(c.ManifestPath, manifest); err != nil {
		return fmt.Errorf("write manifest staging file: %w", err)
	}
	if err := writeRegularFile(c.ResultPath, resultJSON); err != nil {
		return fmt.Errorf("write result staging file: %w", err)
	}
	frame, err := capture.EncodeOutputFrame(resultJSON, patch, manifest)
	if err != nil {
		return fmt.Errorf("encode capture frame: %w", err)
	}
	if int64(len(frame)) > capture.MaxEncodedFrameBytes(c.MaxResult, c.MaxPatch, c.MaxManifest) {
		return fmt.Errorf("%w: encoded frame exceeds configured bound", errBoundExceeded)
	}
	if err := writeAll(stdout, frame); err != nil {
		return fmt.Errorf("write capture frame: %w", err)
	}
	return nil
}

func parseContract(lookup func(string) (string, bool)) (contract, error) {
	allowed := map[string]bool{
		"AGW_CAPTURE_PROTOCOL":           true,
		"AGW_CAPTURE_SPEC_JSON":          true,
		"AGW_CAPTURE_SPEC_DIGEST":        true,
		"AGW_CAPTURE_BASE_SHA":           true,
		"AGW_CAPTURE_RUN_UID":            true,
		"AGW_CAPTURE_REPO_PATH":          true,
		"AGW_CAPTURE_OUTPUT_DIR":         true,
		"AGW_CAPTURE_PATCH_PATH":         true,
		"AGW_CAPTURE_MANIFEST_PATH":      true,
		"AGW_CAPTURE_RESULT_PATH":        true,
		"AGW_CAPTURE_MAX_PATCH_BYTES":    true,
		"AGW_CAPTURE_MAX_RESULT_BYTES":   true,
		"AGW_CAPTURE_MAX_MANIFEST_BYTES": true,
	}
	// The production image has no reason to receive another AGW_CAPTURE_*
	// variable. Rejecting one catches a controller/image contract mismatch and
	// prevents an ignored setting from becoming a security assumption.
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "AGW_CAPTURE_") && !allowed[key] {
			return contract{}, fmt.Errorf("%w: unknown environment variable %q", errInvalidContract, key)
		}
	}

	get := func(name string, max int) (string, error) {
		value, ok := lookup(name)
		if !ok || value == "" || len(value) > max || strings.IndexByte(value, 0) >= 0 {
			return "", fmt.Errorf("%w: missing or oversized %s", errInvalidContract, name)
		}
		return value, nil
	}
	protocol, err := get("AGW_CAPTURE_PROTOCOL", 64)
	if err != nil {
		return contract{}, err
	}
	if protocol != capture.OutputProtocol {
		return contract{}, fmt.Errorf("%w: unsupported output protocol", errInvalidContract)
	}
	specJSON, err := get("AGW_CAPTURE_SPEC_JSON", maxSpecJSONBytes)
	if err != nil {
		return contract{}, err
	}
	if strictjson.ValidateObject([]byte(specJSON)) != nil {
		return contract{}, fmt.Errorf("%w: spec JSON is not strict JSON", errInvalidContract)
	}
	var spec executionSpec
	decoder := json.NewDecoder(strings.NewReader(specJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return contract{}, fmt.Errorf("%w: decode spec JSON", errInvalidContract)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return contract{}, fmt.Errorf("%w: spec JSON has trailing data", errInvalidContract)
	}

	digest, err := get("AGW_CAPTURE_SPEC_DIGEST", 80)
	if err != nil {
		return contract{}, err
	}
	baseSHA, err := get("AGW_CAPTURE_BASE_SHA", 64)
	if err != nil {
		return contract{}, err
	}
	runUID, err := get("AGW_CAPTURE_RUN_UID", 63)
	if err != nil {
		return contract{}, err
	}
	repoPath, err := get("AGW_CAPTURE_REPO_PATH", maxPathBytes)
	if err != nil {
		return contract{}, err
	}
	outputDir, err := get("AGW_CAPTURE_OUTPUT_DIR", maxPathBytes)
	if err != nil {
		return contract{}, err
	}
	patchPath, err := get("AGW_CAPTURE_PATCH_PATH", maxPathBytes)
	if err != nil {
		return contract{}, err
	}
	manifestPath, err := get("AGW_CAPTURE_MANIFEST_PATH", maxPathBytes)
	if err != nil {
		return contract{}, err
	}
	resultPath, err := get("AGW_CAPTURE_RESULT_PATH", maxPathBytes)
	if err != nil {
		return contract{}, err
	}
	maxPatchText, err := get("AGW_CAPTURE_MAX_PATCH_BYTES", 32)
	if err != nil {
		return contract{}, err
	}
	maxResultText, err := get("AGW_CAPTURE_MAX_RESULT_BYTES", 32)
	if err != nil {
		return contract{}, err
	}
	maxManifestText, err := get("AGW_CAPTURE_MAX_MANIFEST_BYTES", 32)
	if err != nil {
		return contract{}, err
	}
	maxPatch, err := parseDecimalBound(maxPatchText)
	if err != nil {
		return contract{}, fmt.Errorf("%w: patch byte bound", errInvalidContract)
	}
	maxResult, err := parseDecimalBound(maxResultText)
	if err != nil {
		return contract{}, fmt.Errorf("%w: result byte bound", errInvalidContract)
	}
	maxManifest, err := parseDecimalBound(maxManifestText)
	if err != nil {
		return contract{}, fmt.Errorf("%w: manifest byte bound", errInvalidContract)
	}

	if spec.Version != contractVersion || spec.Protocol != protocol || spec.ResolvedSpecDigest != digest || spec.BaseSHA != baseSHA || spec.RepoPath != repoPath || spec.OutputDir != outputDir || spec.PatchPath != patchPath || spec.ManifestPath != manifestPath || spec.ResultPath != resultPath || spec.MaxPatchBytes != maxPatch || spec.MaxResultBytes != maxResult || spec.MaxManifestBytes != maxManifest {
		return contract{}, fmt.Errorf("%w: environment and JSON contract disagree", errInvalidContract)
	}
	if !canonical.ValidDigest(digest) || !validBaseSHA(baseSHA) || len(baseSHA) > 64 || len(runUID) > 63 || len(validation.IsValidLabelValue(runUID)) != 0 {
		return contract{}, fmt.Errorf("%w: identity is malformed", errInvalidContract)
	}
	if err := validatePathContract(spec); err != nil {
		return contract{}, err
	}
	if err := validateScopeAndRequirements(spec.Scope, spec.Requirements); err != nil {
		return contract{}, err
	}
	if maxPatch <= 0 || maxPatch > capture.HardMaxPatchBytes || maxResult <= 0 || maxResult > capture.HardMaxResultBytes || maxManifest <= 0 || maxManifest > capture.HardMaxManifestBytes || capture.MaxEncodedFrameBytes(maxResult, maxPatch, maxManifest) == 0 {
		return contract{}, fmt.Errorf("%w: byte bounds are outside the hard ceiling", errInvalidContract)
	}
	if !filepath.IsAbs(repoPath) || !filepath.IsAbs(outputDir) || !filepath.IsAbs(patchPath) || !filepath.IsAbs(manifestPath) || !filepath.IsAbs(resultPath) {
		return contract{}, fmt.Errorf("%w: capture paths must be absolute", errInvalidContract)
	}
	return contract{Spec: spec, RunUID: runUID, MaxPatch: maxPatch, MaxResult: maxResult, MaxManifest: maxManifest, RepoPath: repoPath, OutputDir: outputDir, PatchPath: patchPath, ManifestPath: manifestPath, ResultPath: resultPath}, nil
}

func parseDecimalBound(value string) (int64, error) {
	if value == "" || len(value) > 19 || value != strings.TrimSpace(value) {
		return 0, errInvalidContract
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, errInvalidContract
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
		return 0, errInvalidContract
	}
	return parsed, nil
}

func validatePathContract(spec executionSpec) error {
	for name, value := range map[string]string{"repoPath": spec.RepoPath, "outputDir": spec.OutputDir, "patchPath": spec.PatchPath, "manifestPath": spec.ManifestPath, "resultPath": spec.ResultPath} {
		if value == "" || len(value) > maxPathBytes || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.IndexAny(value, "\x00\r\n") >= 0 {
			return fmt.Errorf("%w: invalid %s", errInvalidContract, name)
		}
	}
	if spec.PatchPath != filepath.Join(spec.OutputDir, "patch.diff") || spec.ManifestPath != filepath.Join(spec.OutputDir, "patch-manifest.json") || spec.ResultPath != filepath.Join(spec.OutputDir, "result.json") {
		return fmt.Errorf("%w: output paths do not match the fixed staging layout", errInvalidContract)
	}
	if filepath.Base(spec.OutputDir) != "capture" || filepath.Base(filepath.Dir(spec.OutputDir)) != ".agw" {
		return fmt.Errorf("%w: output directory must be .agw/capture", errInvalidContract)
	}
	if sameOrWithin(spec.RepoPath, spec.OutputDir) || sameOrWithin(spec.OutputDir, spec.RepoPath) {
		return fmt.Errorf("%w: output staging overlaps the repository", errInvalidContract)
	}
	return nil
}

func validateScopeAndRequirements(scope v1alpha1.ScopeSpec, requirements v1alpha1.GateRequirements) error {
	if len(scope.Paths) > 128 || len(scope.Forbidden) > 128 || len(scope.Paths)+len(scope.Forbidden) > gate.MaxScopePatterns {
		return fmt.Errorf("%w: scope pattern bound exceeded", errInvalidContract)
	}
	for _, pattern := range append(append([]string(nil), scope.Paths...), scope.Forbidden...) {
		if !validGlobPattern(pattern) {
			return fmt.Errorf("%w: invalid scope pattern", errInvalidContract)
		}
	}
	if requirements.MaxFilesChanged < 0 || int64(requirements.MaxFilesChanged) > gate.MaxObservedFiles || requirements.MaxDiffLines < 0 || int64(requirements.MaxDiffLines) > gate.MaxObservedLines {
		return fmt.Errorf("%w: Gate bounds are invalid", errInvalidContract)
	}
	if requirements.TestStrength != v1alpha1.TestStrengthNone && requirements.TestStrength != v1alpha1.TestStrengthNewTestsFailOnBase {
		return fmt.Errorf("%w: unsupported test-strength policy", errInvalidContract)
	}
	if len(requirements.CoverageDelta) > 64 || !utf8.ValidString(requirements.CoverageDelta) {
		return fmt.Errorf("%w: coverage requirement is oversized", errInvalidContract)
	}
	return nil
}

func capturePatch(ctx context.Context, c contract) ([]byte, []byte, string, patchEvidence, error) {
	var zero patchEvidence
	if err := ensureExistingDirectory(c.RepoPath); err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: %v", errUnsafeWorktree, err)
	}
	if err := ensureNoSymlinkComponents(c.RepoPath); err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: repository path: %v", errUnsafeWorktree, err)
	}
	gitPath, err := trustedGitBinary()
	if err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: git is unavailable", errGitOperation)
	}
	baseEnv := safeGitEnvironment()
	baseRunner := &gitRunner{ctx: ctx, repo: c.RepoPath, gitPath: gitPath, env: baseEnv}
	if err := baseRunner.verifyRepository(c.Spec.BaseSHA); err != nil {
		return nil, nil, "", zero, err
	}
	objectDirText, err := baseRunner.text("rev-parse", "--git-path", "objects")
	if err != nil {
		return nil, nil, "", zero, err
	}
	objectDir := strings.TrimSpace(objectDirText)
	if !filepath.IsAbs(objectDir) {
		objectDir = filepath.Join(c.RepoPath, objectDir)
	}
	objectDir, err = filepath.Abs(objectDir)
	if err != nil || !filepath.IsAbs(objectDir) {
		return nil, nil, "", zero, fmt.Errorf("%w: object directory is not absolute", errUnsafeWorktree)
	}
	if err := ensureExistingDirectory(objectDir); err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: object directory: %v", errUnsafeWorktree, err)
	}
	if err := ensureNoSymlinkComponents(objectDir); err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: object directory: %v", errUnsafeWorktree, err)
	}

	// The capture Job has a read-only root filesystem. Keep all mutable Git
	// state beside the controller-selected staging directory on the writable
	// work PVC; never fall back to /tmp or another ambient host path.
	tempParent := filepath.Dir(c.OutputDir)
	if err := ensureDirectoryTree(tempParent); err != nil {
		return nil, nil, "", zero, fmt.Errorf("%w: temporary state parent: %v", errUnsafeWorktree, err)
	}
	tempDir, err := os.MkdirTemp(tempParent, ".capture-")
	if err != nil {
		return nil, nil, "", zero, fmt.Errorf("create temporary Git state: %w", err)
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0700); err != nil {
		return nil, nil, "", zero, fmt.Errorf("secure temporary Git state: %w", err)
	}
	tempObjects := filepath.Join(tempDir, "objects")
	if err := os.Mkdir(tempObjects, 0700); err != nil {
		return nil, nil, "", zero, fmt.Errorf("create temporary object store: %w", err)
	}
	runner := &gitRunner{ctx: ctx, repo: c.RepoPath, gitPath: gitPath, env: append(append([]string(nil), baseEnv...),
		"GIT_INDEX_FILE="+filepath.Join(tempDir, "index"),
		"GIT_OBJECT_DIRECTORY="+tempObjects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+objectDir,
	)}
	if _, err := runner.run("read-tree", "--reset", c.Spec.BaseSHA); err != nil {
		return nil, nil, "", zero, err
	}
	baseEntries, err := runner.listTree(c.Spec.BaseSHA)
	if err != nil {
		return nil, nil, "", zero, err
	}
	workEntries, err := runner.indexWorktree(c.RepoPath, c.MaxPatch)
	if err != nil {
		return nil, nil, "", zero, err
	}
	for path := range baseEntries {
		if _, exists := workEntries[path]; exists {
			continue
		}
		if _, err := runner.run("update-index", "--remove", "--", path); err != nil {
			return nil, nil, "", zero, err
		}
	}
	patch, err := runner.diff(c.MaxPatch, c.Spec.BaseSHA)
	if err != nil {
		return nil, nil, "", zero, err
	}
	evidence, err := analyzePatch(patch)
	if err != nil {
		return nil, nil, "", zero, err
	}
	if evidence.binary {
		return nil, nil, "", zero, fmt.Errorf("%w: binary changes are not publishable", errUnsafeWorktree)
	}
	files, err := runner.manifestFiles(evidence.paths, baseEntries, workEntries)
	if err != nil {
		return nil, nil, "", zero, err
	}
	manifest, manifestDigest, err := publish.MarshalPatchManifest(files)
	if err != nil || int64(len(manifest)) > c.MaxManifest {
		return nil, nil, "", zero, fmt.Errorf("%w: canonical manifest exceeds its contract", errBoundExceeded)
	}
	decoded, err := publish.DecodePatchManifest(manifest, manifestDigest)
	if err != nil || len(decoded) != len(files) {
		return nil, nil, "", zero, fmt.Errorf("%w: canonical manifest did not round trip", errInvalidContract)
	}
	return patch, manifest, manifestDigest, evidence, nil
}

func trustedGitBinary() (string, error) {
	for _, candidate := range []string{"/usr/bin/git", "/bin/git"} {
		info, err := os.Lstat(candidate)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if err := ensureNoSymlinkComponents(candidate); err != nil {
			continue
		}
		return candidate, nil
	}
	return "", errGitOperation
}

func safeGitEnvironment() []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + gitConfigFile,
		"GIT_CONFIG_GLOBAL=" + gitConfigFile,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_ALLOW_PROTOCOL=none",
		"GIT_SSH_COMMAND=" + noNetworkCommand,
		"GIT_ASKPASS=" + noNetworkCommand,
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
	}
}

type gitRunner struct {
	ctx     context.Context
	repo    string
	gitPath string
	env     []string
}

func (g *gitRunner) command(args ...string) *exec.Cmd {
	base := []string{"-C", g.repo, "-c", "safe.directory=" + g.repo, "-c", "core.hooksPath=/dev/null", "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "core.fsmonitor=false", "-c", "core.splitIndex=false", "-c", "core.untrackedCache=false", "-c", "credential.helper=", "-c", "diff.external="}
	base = append(base, args...)
	cmd := exec.CommandContext(g.ctx, g.gitPath, base...)
	cmd.Env = append([]string(nil), g.env...)
	return cmd
}

func (g *gitRunner) run(args ...string) ([]byte, error) {
	var stdout boundedBuffer
	stdout.limit = maxGitStdout
	var stderr boundedBuffer
	stderr.limit = maxGitStderr
	cmd := g.command(args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stdout.exceeded || stderr.exceeded {
		operation := "unknown"
		if len(args) > 0 {
			operation = args[0]
		}
		return nil, fmt.Errorf("%w: %s", errGitOperation, operation)
	}
	return stdout.Bytes(), nil
}

func (g *gitRunner) text(args ...string) (string, error) {
	value, err := g.run(args...)
	if err != nil || !utf8.Valid(value) {
		return "", errGitOperation
	}
	return strings.TrimSuffix(string(value), "\n"), nil
}

func (g *gitRunner) verifyRepository(baseSHA string) error {
	inside, err := g.text("rev-parse", "--is-inside-work-tree")
	if err != nil || inside != "true" {
		return fmt.Errorf("%w: not a Git worktree", errUnsafeWorktree)
	}
	top, err := g.text("rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: cannot resolve worktree root", errUnsafeWorktree)
	}
	topAbs, err := filepath.Abs(top)
	if err != nil || topAbs != filepath.Clean(g.repo) {
		return fmt.Errorf("%w: Git root does not match the contract", errUnsafeWorktree)
	}
	gitDir, err := g.text("rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("%w: cannot resolve Git metadata", errUnsafeWorktree)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(g.repo, gitDir)
	}
	if err := ensureNoSymlinkComponents(filepath.Clean(gitDir)); err != nil {
		return fmt.Errorf("%w: Git metadata path: %v", errUnsafeWorktree, err)
	}
	head, err := g.text("rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || head != baseSHA {
		return fmt.Errorf("%w: HEAD is not the immutable base revision", errUnsafeWorktree)
	}
	resolved, err := g.text("rev-parse", "--verify", "--end-of-options", baseSHA+"^{commit}")
	if err != nil || resolved != baseSHA {
		return fmt.Errorf("%w: immutable base revision does not resolve to a commit", errUnsafeWorktree)
	}
	return nil
}

func (g *gitRunner) listTree(baseSHA string) (map[string]treeEntry, error) {
	data, err := g.run("ls-tree", "-r", "-z", "--full-tree", baseSHA, "--")
	if err != nil {
		return nil, err
	}
	entries := make(map[string]treeEntry)
	for _, record := range bytes.Split(data, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(entries) >= maxTrackedPaths {
			return nil, fmt.Errorf("%w: base tree has too many paths", errBoundExceeded)
		}
		tab := bytes.IndexByte(record, '\t')
		if tab <= 0 || tab+1 >= len(record) || !utf8.Valid(record[tab+1:]) {
			return nil, fmt.Errorf("%w: malformed base tree entry", errUnsafeWorktree)
		}
		fields := strings.Fields(string(record[:tab]))
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: malformed base tree metadata", errUnsafeWorktree)
		}
		mode, err := strconv.ParseUint(fields[0], 8, 32)
		if err != nil || (mode != 0100644 && mode != 0100755 && mode != 0120000 && mode != 0160000) {
			return nil, fmt.Errorf("%w: unsupported Git tree mode", errUnsafeWorktree)
		}
		path := string(record[tab+1:])
		if !gate.ValidateRepoPath(path) || hasGitComponent(path) {
			return nil, fmt.Errorf("%w: base tree contains unsafe path", errUnsafePath)
		}
		if mode == 0160000 {
			return nil, fmt.Errorf("%w: submodules are disabled", errUnsafeWorktree)
		}
		if _, exists := entries[path]; exists {
			return nil, fmt.Errorf("%w: duplicate base tree path", errUnsafeWorktree)
		}
		oid := fields[2]
		if (len(oid) != 40 && len(oid) != 64) || !isLowerHex(oid) {
			return nil, fmt.Errorf("%w: malformed base tree object ID", errUnsafeWorktree)
		}
		entries[path] = treeEntry{mode: uint32(mode), oid: oid, path: path}
	}
	return entries, nil
}

func (g *gitRunner) indexWorktree(root string, maxBytes int64) (map[string]treeEntry, error) {
	workEntries := make(map[string]treeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return errUnsafeWorktree
			}
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil // linked worktrees use a metadata file at the root.
		}
		if hasGitComponent(rel) {
			return fmt.Errorf("%w: nested Git metadata is not trusted", errUnsafeWorktree)
		}
		if !gate.ValidateRepoPath(rel) {
			return fmt.Errorf("%w: %s", errUnsafePath, rel)
		}
		if entry.IsDir() {
			return nil
		}
		if len(workEntries) >= maxTrackedPaths {
			return fmt.Errorf("%w: worktree has too many paths", errBoundExceeded)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode, data, err := readWorktreeEntry(path, info, maxBytes)
		if err != nil {
			return fmt.Errorf("capture %s: %w", rel, err)
		}
		oid, err := g.hashBlob(data)
		if err != nil {
			return err
		}
		if err := g.updateIndex(mode, oid, rel); err != nil {
			return err
		}
		workEntries[rel] = treeEntry{mode: mode, oid: oid, path: rel}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return workEntries, nil
}

func readWorktreeEntry(path string, info os.FileInfo, maxBytes int64) (uint32, io.Reader, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return 0, nil, err
		}
		if int64(len(target)) > maxFileBytes || strings.IndexByte(target, 0) >= 0 {
			return 0, nil, errBoundExceeded
		}
		return 0120000, bytes.NewReader([]byte(target)), nil
	}
	if !info.Mode().IsRegular() {
		return 0, nil, errSpecialFile
	}
	if info.Size() < 0 || info.Size() > maxFileBytes {
		return 0, nil, errBoundExceeded
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, nil, err
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != info.Size() {
		_ = file.Close()
		return 0, nil, errUnsafeWorktree
	}
	mode := uint32(0100644)
	if info.Mode().Perm()&0111 != 0 {
		mode = 0100755
	}
	return mode, &closeCheckingReader{Reader: file, file: file, expectedSize: info.Size()}, nil
}

type closeCheckingReader struct {
	io.Reader
	file         *os.File
	expectedSize int64
}

func (r *closeCheckingReader) Read(p []byte) (int, error) { return r.Reader.Read(p) }

func (r *closeCheckingReader) Close() error {
	stat, statErr := r.file.Stat()
	closeErr := r.file.Close()
	if statErr != nil || closeErr != nil || stat.Size() != r.expectedSize {
		return errUnsafeWorktree
	}
	return nil
}

func (g *gitRunner) hashBlob(data io.Reader) (string, error) {
	cmd := g.command("hash-object", "-w", "--no-filters", "--stdin")
	cmd.Stdin = data
	var stdout boundedBuffer
	stdout.limit = 128
	var stderr boundedBuffer
	stderr.limit = maxGitStderr
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if closer, ok := data.(io.Closer); ok {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil || stdout.exceeded || stderr.exceeded {
		return "", errGitOperation
	}
	oid := strings.TrimSpace(string(stdout.Bytes()))
	if (len(oid) != 40 && len(oid) != 64) || !isLowerHex(oid) {
		return "", errGitOperation
	}
	return oid, nil
}

func (g *gitRunner) updateIndex(mode uint32, oid, path string) error {
	_, err := g.run("update-index", "--add", "--cacheinfo", fmt.Sprintf("%o,%s,%s", mode, oid, path))
	return err
}

func (g *gitRunner) diff(maxBytes int64, baseSHA string) ([]byte, error) {
	var stdout boundedBuffer
	stdout.limit = maxBytes
	var stderr boundedBuffer
	stderr.limit = maxGitStderr
	cmd := g.command("diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames", baseSHA, "--", ".")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stdout.exceeded || stderr.exceeded {
		return nil, errGitOperation
	}
	return stdout.Bytes(), nil
}

func (g *gitRunner) manifestFiles(paths []string, base, work map[string]treeEntry) ([]publish.FileChange, error) {
	if len(paths) == 0 || len(paths) > publish.MaxPatchFiles {
		return nil, fmt.Errorf("%w: changed-file manifest count", errBoundExceeded)
	}
	files := make([]publish.FileChange, 0, len(paths))
	remaining := int64(publish.MaxPatchBytes)
	for _, path := range paths {
		if !gate.ValidateRepoPath(path) {
			return nil, fmt.Errorf("%w: manifest path", errUnsafePath)
		}
		entry, exists := work[path]
		if !exists {
			baseEntry, ok := base[path]
			if !ok {
				return nil, fmt.Errorf("%w: deletion is absent from the base tree", errUnsafeWorktree)
			}
			mode, err := publishableMode(baseEntry.mode)
			if err != nil {
				return nil, err
			}
			files = append(files, publish.FileChange{Path: path, Mode: mode, Delete: true, Content: nil})
			continue
		}
		mode, err := publishableMode(entry.mode)
		if err != nil {
			return nil, err
		}
		content, err := g.catBlob(entry.oid, remaining)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return nil, fmt.Errorf("%w: binary manifest content", errUnsafeWorktree)
		}
		remaining -= int64(len(content))
		files = append(files, publish.FileChange{Path: path, Mode: mode, Content: content})
	}
	return files, nil
}

func publishableMode(mode uint32) (string, error) {
	switch mode {
	case 0100644:
		return "100644", nil
	case 0100755:
		return "100755", nil
	default:
		return "", fmt.Errorf("%w: changed symlink or unsupported mode", errUnsafeWorktree)
	}
}

func (g *gitRunner) catBlob(oid string, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 || (len(oid) != 40 && len(oid) != 64) || !isLowerHex(oid) {
		return nil, errBoundExceeded
	}
	var stdout boundedBuffer
	stdout.limit = maxBytes
	var stderr boundedBuffer
	stderr.limit = maxGitStderr
	cmd := g.command("cat-file", "blob", oid)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil || stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("%w: read indexed manifest blob", errBoundExceeded)
	}
	content := make([]byte, stdout.Len())
	copy(content, stdout.Bytes())
	return content, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit < 0 || int64(b.Len())+int64(len(p)) > b.limit {
		b.exceeded = true
		remaining := b.limit - int64(b.Len())
		if remaining > 0 {
			_, _ = b.Buffer.Write(p[:remaining])
		}
		return len(p), errBoundExceeded
	}
	return b.Buffer.Write(p)
}

func analyzePatch(patch []byte) (patchEvidence, error) {
	var evidence patchEvidence
	if len(patch) == 0 {
		return evidence, nil
	}
	if !utf8.Valid(patch) {
		return evidence, fmt.Errorf("%w: patch is not valid UTF-8", errUnsafeWorktree)
	}
	pathSet := make(map[string]struct{})
	headerCount := 0
	for _, line := range strings.Split(string(patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			headerCount++
			paths, err := parseDiffHeader(line)
			if err != nil {
				return evidence, fmt.Errorf("%w: %v", errUnsafePath, err)
			}
			for _, path := range paths {
				if _, exists := pathSet[path]; exists {
					return evidence, fmt.Errorf("%w: duplicate diff path", errUnsafePath)
				}
				pathSet[path] = struct{}{}
				if len(pathSet) > int(gate.MaxObservedFiles) {
					return evidence, errBoundExceeded
				}
			}
		}
		if strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") {
			evidence.binary = true
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ ") {
			evidence.lines++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- ") {
			evidence.lines++
		}
		if evidence.lines > gate.MaxObservedLines {
			return evidence, errBoundExceeded
		}
	}
	if headerCount == 0 {
		return evidence, fmt.Errorf("%w: non-empty patch has no Git diff header", errUnsafeWorktree)
	}
	evidence.paths = make([]string, 0, len(pathSet))
	for path := range pathSet {
		evidence.paths = append(evidence.paths, path)
	}
	for i := 1; i < len(evidence.paths); i++ {
		for j := i; j > 0 && evidence.paths[j] < evidence.paths[j-1]; j-- {
			evidence.paths[j], evidence.paths[j-1] = evidence.paths[j-1], evidence.paths[j]
		}
	}
	return evidence, nil
}

func parseDiffHeader(line string) ([]string, error) {
	rest := strings.TrimPrefix(line, "diff --git ")
	if rest == line || rest == "" {
		return nil, errors.New("missing diff header paths")
	}
	var left, right string
	if strings.HasPrefix(rest, "\"") {
		var consumed int
		var err error
		left, consumed, err = parseGitToken(rest)
		if err != nil {
			return nil, err
		}
		rest = rest[consumed:]
		if len(rest) == 0 || rest[0] != ' ' {
			return nil, errors.New("diff header has no second path")
		}
		right, consumed, err = parseGitToken(rest[1:])
		if err != nil || consumed != len(rest)-1 {
			return nil, errors.New("diff header has trailing path data")
		}
	} else {
		for index := 0; index < len(rest); index++ {
			if rest[index] != ' ' || index+1 >= len(rest) || !strings.HasPrefix(rest[index+1:], "b/") {
				continue
			}
			candidateLeft := rest[:index]
			candidateRight := rest[index+1:]
			if strings.HasPrefix(candidateLeft, "a/") && strings.HasPrefix(candidateRight, "b/") {
				left, right = candidateLeft, candidateRight
				break
			}
		}
		if left == "" || right == "" {
			return nil, errors.New("diff header paths are not parseable")
		}
	}
	left = strings.TrimPrefix(left, "a/")
	right = strings.TrimPrefix(right, "b/")
	if !gate.ValidateRepoPath(left) || !gate.ValidateRepoPath(right) {
		return nil, errors.New("diff header contains an unsafe repository path")
	}
	if left == right {
		return []string{left}, nil
	}
	return []string{left, right}, nil
}

func parseGitToken(value string) (string, int, error) {
	if value == "" || value[0] != '"' {
		return "", 0, errors.New("quoted Git path is missing")
	}
	for index := 1; index < len(value); index++ {
		if value[index] != '"' || value[index-1] == '\\' {
			continue
		}
		decoded, err := strconv.Unquote(value[:index+1])
		if err != nil {
			return "", 0, err
		}
		return decoded, index + 1, nil
	}
	return "", 0, errors.New("unterminated quoted Git path")
}

func enforceEvidence(c contract, evidence patchEvidence, patchBytes int64) error {
	if patchBytes < 0 || patchBytes > c.MaxPatch {
		return fmt.Errorf("%w: patch bytes", errBoundExceeded)
	}
	if len(evidence.paths) == 0 || len(evidence.paths) > publish.MaxPatchFiles || int64(len(evidence.paths)) > gate.MaxObservedFiles || int64(len(evidence.paths)) > int64(c.Spec.Requirements.MaxFilesChanged) && c.Spec.Requirements.MaxFilesChanged > 0 {
		return fmt.Errorf("%w: changed file count", errBoundExceeded)
	}
	if c.Spec.Requirements.MaxDiffLines > 0 && evidence.lines > int64(c.Spec.Requirements.MaxDiffLines) {
		return fmt.Errorf("%w: changed line count", errBoundExceeded)
	}
	if evidence.binary {
		return fmt.Errorf("%w: binary files are not publishable", errBoundExceeded)
	}
	if c.Spec.Requirements.ScopeRespected {
		for _, path := range evidence.paths {
			allowed := false
			for _, pattern := range c.Spec.Scope.Paths {
				if gate.MatchGlob(pattern, path) {
					allowed = true
					break
				}
			}
			if !allowed {
				return fmt.Errorf("%w: path is outside the allowlist", errUnsafePath)
			}
			for _, pattern := range c.Spec.Scope.Forbidden {
				if gate.MatchGlob(pattern, path) {
					return fmt.Errorf("%w: path is forbidden by the Gate", errUnsafePath)
				}
			}
		}
	}
	return nil
}

func writeRegularFile(path string, data []byte) error {
	if len(data) > int(maxFileBytes) {
		return errBoundExceeded
	}
	if err := ensureDirectoryTree(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	good := false
	defer func() {
		if !good {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return errSpecialFile
	}
	if err := writeAll(file, data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	good = true
	return nil
}

func ensureExistingDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errUnsafeWorktree
	}
	return nil
}

func ensureNoSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return errUnsafeWorktree
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errUnsafeWorktree
		}
	}
	return nil
}

func ensureDirectoryTree(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return errUnsafeWorktree
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errUnsafeWorktree
		}
	}
	return nil
}

func sameOrWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func validBaseSHA(value string) bool {
	return (len(value) == 40 || len(value) == 64) && isLowerHex(value)
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func hasGitComponent(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

func validGlobPattern(pattern string) bool {
	if pattern == "" || len(pattern) > gate.MaxPatternBytes || !utf8.ValidString(pattern) || strings.IndexByte(pattern, 0) >= 0 || strings.ContainsRune(pattern, '\\') || strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") || strings.Contains(pattern, "//") || strings.HasPrefix(pattern, "!") {
		return false
	}
	if len(pattern) >= 2 && ((pattern[0] >= 'a' && pattern[0] <= 'z') || (pattern[0] >= 'A' && pattern[0] <= 'Z')) && pattern[1] == ':' {
		return false
	}
	for _, component := range strings.Split(pattern, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	runes := []rune(pattern)
	for index := 0; index < len(runes); index++ {
		if runes[index] == '[' {
			end, ok := globClassEnd(runes, index)
			if !ok {
				return false
			}
			index = end
		}
	}
	return true
}

func globClassEnd(pattern []rune, start int) (int, bool) {
	index := start + 1
	negated := false
	if index < len(pattern) && (pattern[index] == '!' || pattern[index] == '^') {
		negated = true
		index++
	}
	if index < len(pattern) && pattern[index] == ']' {
		closing := index + 1
		for closing < len(pattern) && pattern[closing] != ']' {
			closing++
		}
		if closing == len(pattern) {
			return 0, false
		}
		index++
	}
	for ; index < len(pattern); index++ {
		if pattern[index] == ']' {
			contentStart := start + 1
			if negated {
				contentStart++
			}
			return index, index > contentStart
		}
	}
	return 0, false
}
