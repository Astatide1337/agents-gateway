package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

func TestCaptureProducesBinarySafeBoundedFrameFromTempGitRepo(t *testing.T) {
	repo, baseSHA := newGitRepo(t, "src/main.go", []byte("package main\n\nfunc main() {}\n"))
	if err := os.WriteFile(filepath.Join(repo, "src", "removed.go"), []byte("package main\n\nvar Removed = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "src/removed.go")
	runGit(t, repo, "commit", "-m", "second base file")
	baseSHA = gitText(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "src", "main.go"), []byte("package main\n\nfunc main() { println(\"changed\") }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "src", "removed.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "new.go"), []byte("package main\n\nvar Added = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "src", "new.go"), 0700); err != nil {
		t.Fatal(err)
	}

	values, spec := validContract(t, repo, baseSHA)
	var stdout bytes.Buffer
	if err := run(context.Background(), mapLookup(values), &stdout); err != nil {
		t.Fatal(err)
	}
	decoded, err := capture.DecodeOutputFrame(stdout.Bytes(), spec.MaxResultBytes, spec.MaxPatchBytes, spec.MaxManifestBytes)
	if err != nil {
		t.Fatalf("DecodeOutputFrame: %v", err)
	}
	var result capture.ResultEnvelope
	decodeStrictResult(t, decoded.ResultJSON, &result)
	if result.RunUID != values["AGW_CAPTURE_RUN_UID"] || result.BaseSHA != baseSHA || result.PatchBytes != int64(len(decoded.Patch)) || result.PatchDigest == "" {
		t.Fatalf("unexpected result identity: %#v", result)
	}
	wantPaths := []string{"src/main.go", "src/new.go", "src/removed.go"}
	if fmt.Sprint(result.ChangedPaths) != fmt.Sprint(wantPaths) {
		t.Fatalf("changed paths=%v, want %v", result.ChangedPaths, wantPaths)
	}
	files, err := publish.DecodePatchManifest(decoded.Manifest, result.ManifestDigest)
	if err != nil {
		t.Fatalf("DecodePatchManifest: %v", err)
	}
	if result.ManifestBytes != int64(len(decoded.Manifest)) || len(files) != len(wantPaths) {
		t.Fatalf("manifest evidence=%#v result=%#v", files, result)
	}
	changes := make(map[string]publish.FileChange, len(files))
	for _, file := range files {
		changes[file.Path] = file
	}
	if !changes["src/removed.go"].Delete || changes["src/removed.go"].Content != nil || changes["src/main.go"].Delete || !bytes.Contains(changes["src/main.go"].Content, []byte("changed")) || changes["src/new.go"].Mode != "100755" {
		t.Fatalf("manifest changes=%#v", changes)
	}
	assertRegularFile(t, spec.PatchPath, decoded.Patch)
	assertRegularFile(t, spec.ManifestPath, decoded.Manifest)
	assertRegularFile(t, spec.ResultPath, decoded.ResultJSON)
	entries, err := os.ReadDir(filepath.Dir(spec.OutputDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".capture-") {
			t.Fatalf("temporary Git state was not cleaned: %s", entry.Name())
		}
	}
	if got := stdout.String(); strings.Count(got, capture.OutputProtocol+"\n") != 1 {
		t.Fatalf("stdout contains %d protocol frames, want one", strings.Count(got, capture.OutputProtocol+"\n"))
	}
}

func TestCaptureRejectsGitlinksBeforeTrustingSubmoduleState(t *testing.T) {
	repo, _ := newGitRepo(t, "src/main.go", []byte("base\n"))
	commit := gitText(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+commit+",vendor/submodule")
	runGit(t, repo, "commit", "-m", "gitlink")
	baseSHA := gitText(t, repo, "rev-parse", "HEAD")
	values, _ := validContract(t, repo, baseSHA)
	var stdout bytes.Buffer
	if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
		t.Fatal("gitlink/submodule state was accepted")
	}
}

func TestCaptureDoesNotExecuteCleanFiltersAndDoesNotMutateTheRealIndex(t *testing.T) {
	repo, baseSHA := newGitRepo(t, "src/main.go", []byte("original\n"))
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("src/main.go filter=block\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitattributes")
	runGit(t, repo, "commit", "-m", "attributes")
	baseSHA = gitText(t, repo, "rev-parse", "HEAD")
	marker := filepath.Join(t.TempDir(), "filter-ran")
	runGit(t, repo, "config", "filter.block.clean", "/bin/sh -c 'touch "+marker+"; cat'")
	runGit(t, repo, "config", "core.splitIndex", "true")
	runGit(t, repo, "config", "core.untrackedCache", "true")
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "main.go"), []byte("changed raw bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	beforeIndex := gitText(t, repo, "rev-parse", "HEAD")
	values, spec := validContract(t, repo, baseSHA)
	var stdout bytes.Buffer
	if err := run(context.Background(), mapLookup(values), &stdout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean filter executed or marker check failed: %v", err)
	}
	if got := gitText(t, repo, "rev-parse", "HEAD"); got != beforeIndex {
		t.Fatalf("capture changed repository HEAD: got %s, want %s", got, beforeIndex)
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal("capture changed the real Git index")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "index.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture left a real index lock: %v", err)
	}
	decoded, err := capture.DecodeOutputFrame(stdout.Bytes(), spec.MaxResultBytes, spec.MaxPatchBytes, spec.MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(decoded.Patch, []byte("changed raw bytes")) {
		t.Fatal("patch does not contain raw worktree content")
	}
}

func TestCaptureRejectsHeadMismatchScopeBinaryAndSpecialFiles(t *testing.T) {
	t.Run("head mismatch", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
		values, spec := validContract(t, repo, baseSHA)
		values["AGW_CAPTURE_BASE_SHA"] = strings.Repeat("f", 40)
		spec.BaseSHA = values["AGW_CAPTURE_BASE_SHA"]
		values["AGW_CAPTURE_SPEC_JSON"] = marshalSpec(t, spec)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("HEAD mismatch was accepted")
		}
		if stdout.Len() != 0 {
			t.Fatal("HEAD mismatch emitted stdout")
		}
	})

	t.Run("scope", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("outside\n"), 0600); err != nil {
			t.Fatal(err)
		}
		values, _ := validContract(t, repo, baseSHA)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("scope violation was accepted")
		}
		if stdout.Len() != 0 {
			t.Fatal("scope violation emitted stdout")
		}
	})

	t.Run("binary forbidden", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/data.bin", []byte{0, 1, 2, 3})
		if err := os.WriteFile(filepath.Join(repo, "src", "data.bin"), []byte{0, 9, 8, 7, 6}, 0600); err != nil {
			t.Fatal(err)
		}
		values, spec := validContract(t, repo, baseSHA)
		spec.Requirements.NoBinaryFiles = true
		values["AGW_CAPTURE_SPEC_JSON"] = marshalSpec(t, spec)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("binary patch was accepted by a no-binary Gate")
		}
	})

	t.Run("special file", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
		pipe := filepath.Join(repo, "src", "pipe")
		if err := syscall.Mkfifo(pipe, 0600); err != nil {
			t.Skipf("mkfifo unavailable: %v", err)
		}
		values, _ := validContract(t, repo, baseSHA)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("special file was accepted")
		}
	})

	t.Run("changed symlink", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
		outside := filepath.Join(filepath.Dir(repo), "outside-secret")
		if err := os.WriteFile(outside, []byte("must-not-be-read"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(repo, "src", "link")); err != nil {
			t.Fatal(err)
		}
		values, _ := validContract(t, repo, baseSHA)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("changed symlink was accepted")
		}
		if stdout.Len() != 0 {
			t.Fatal("changed symlink emitted stdout")
		}
	})

	t.Run("manifest content bound", func(t *testing.T) {
		repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
		content := []byte(strings.Repeat("x", publish.MaxPatchBytes+1))
		if err := os.WriteFile(filepath.Join(repo, "src", "main.go"), content, 0600); err != nil {
			t.Fatal(err)
		}
		values, _ := validContract(t, repo, baseSHA)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
			t.Fatal("oversized manifest content was accepted")
		}
	})
}

func TestCaptureRejectsContractDriftAndUnsafeOutputPath(t *testing.T) {
	repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
	values, spec := validContract(t, repo, baseSHA)

	t.Run("missing explicit run identity", func(t *testing.T) {
		copyValues := cloneMap(values)
		delete(copyValues, "AGW_CAPTURE_RUN_UID")
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(copyValues), &stdout); err == nil {
			t.Fatal("missing run UID was accepted")
		}
	})

	t.Run("duplicate JSON key", func(t *testing.T) {
		copyValues := cloneMap(values)
		copyValues["AGW_CAPTURE_SPEC_JSON"] = strings.Replace(values["AGW_CAPTURE_SPEC_JSON"], `{"version":1`, `{"version":1,"version":1`, 1)
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(copyValues), &stdout); err == nil {
			t.Fatal("duplicate JSON key was accepted")
		}
	})

	t.Run("output symlink", func(t *testing.T) {
		copyValues := cloneMap(values)
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(spec.OutputDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, spec.ResultPath); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		if err := run(context.Background(), mapLookup(copyValues), &stdout); err == nil {
			t.Fatal("output symlink was followed")
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "keep" {
			t.Fatalf("symlink target changed: %q, %v", got, err)
		}
	})
}

func TestCaptureEnforcesPatchByteBound(t *testing.T) {
	repo, baseSHA := newGitRepo(t, "src/main.go", []byte("base\n"))
	if err := os.WriteFile(filepath.Join(repo, "src", "main.go"), []byte(strings.Repeat("x", 1024)), 0600); err != nil {
		t.Fatal(err)
	}
	values, spec := validContract(t, repo, baseSHA)
	spec.MaxPatchBytes = 32
	values["AGW_CAPTURE_MAX_PATCH_BYTES"] = "32"
	values["AGW_CAPTURE_SPEC_JSON"] = marshalSpec(t, spec)
	var stdout bytes.Buffer
	if err := run(context.Background(), mapLookup(values), &stdout); err == nil {
		t.Fatal("patch byte bound was not enforced")
	}
}

func validContract(t *testing.T, repo, baseSHA string) (map[string]string, executionSpec) {
	t.Helper()
	outputDir := filepath.Join(filepath.Dir(repo), ".agw", "capture")
	spec := executionSpec{
		Version:            contractVersion,
		Protocol:           capture.OutputProtocol,
		ResolvedSpecDigest: "sha256:" + strings.Repeat("a", 64),
		BaseSHA:            baseSHA,
		RepoPath:           repo,
		OutputDir:          outputDir,
		PatchPath:          filepath.Join(outputDir, "patch.diff"),
		ManifestPath:       filepath.Join(outputDir, "patch-manifest.json"),
		ResultPath:         filepath.Join(outputDir, "result.json"),
		Scope:              v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
		Requirements:       v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 100, MaxDiffLines: 10000},
		MaxPatchBytes:      64 << 20,
		MaxResultBytes:     128 << 10,
		MaxManifestBytes:   capture.DefaultMaxManifestBytes,
	}
	values := map[string]string{
		"AGW_CAPTURE_PROTOCOL":           capture.OutputProtocol,
		"AGW_CAPTURE_SPEC_JSON":          marshalSpec(t, spec),
		"AGW_CAPTURE_SPEC_DIGEST":        spec.ResolvedSpecDigest,
		"AGW_CAPTURE_BASE_SHA":           baseSHA,
		"AGW_CAPTURE_RUN_UID":            "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15",
		"AGW_CAPTURE_REPO_PATH":          repo,
		"AGW_CAPTURE_OUTPUT_DIR":         spec.OutputDir,
		"AGW_CAPTURE_PATCH_PATH":         spec.PatchPath,
		"AGW_CAPTURE_MANIFEST_PATH":      spec.ManifestPath,
		"AGW_CAPTURE_RESULT_PATH":        spec.ResultPath,
		"AGW_CAPTURE_MAX_PATCH_BYTES":    strconvI64(spec.MaxPatchBytes),
		"AGW_CAPTURE_MAX_RESULT_BYTES":   strconvI64(spec.MaxResultBytes),
		"AGW_CAPTURE_MAX_MANIFEST_BYTES": strconvI64(spec.MaxManifestBytes),
	}
	return values, spec
}

func marshalSpec(t *testing.T, spec executionSpec) string {
	t.Helper()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func decodeStrictResult(t *testing.T, body []byte, result *capture.ResultEnvelope) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("result has trailing JSON: %v", err)
	}
}

func assertRegularFile(t *testing.T, path string, want []byte) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("%s is not a 0600 regular file: %v", path, info.Mode())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s contents changed", path)
	}
}

func newGitRepo(t *testing.T, relative string, content []byte) (string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, relative)), 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "--initial-branch=main")
	runGit(t, repo, "config", "user.email", "test@example.invalid")
	runGit(t, repo, "config", "user.name", "capture-test")
	path := filepath.Join(repo, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", relative)
	runGit(t, repo, "commit", "-m", "base")
	return repo, gitText(t, repo, "rev-parse", "HEAD")
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", commandArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func gitText(t *testing.T, repo string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repo}, args...)
	output, err := exec.Command("git", commandArgs...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}

func mapLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func cloneMap(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func strconvI64(value int64) string {
	return fmt.Sprintf("%d", value)
}
