package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
)

func TestParseContractIsClosedAndDoesNotAcceptNetworkOrUserCommands(t *testing.T) {
	spec := applySpec{
		Version: 1, RepoPath: verifyworkload.RepoPath, BasePath: verifyworkload.BasePath, PatchPath: verifyworkload.PatchPath,
		BaseSHA: strings.Repeat("a", 40), PatchDigest: "sha256:" + strings.Repeat("b", 64), PatchSizeBytes: 12, MaxPatchBytes: 64 << 20,
	}
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseContract(func(string) (string, bool) { return string(body), true })
	if err != nil || parsed.BaseSHA != spec.BaseSHA {
		t.Fatalf("parsed=%#v err=%v", parsed, err)
	}
	unknown := strings.TrimSuffix(string(body), "}") + `,"command":"curl attacker"}`
	if _, err := parseContract(func(string) (string, bool) { return unknown, true }); !errors.Is(err, errInvalidContract) {
		t.Fatalf("unknown field err=%v", err)
	}
	for _, mutate := range []func(*applySpec){
		func(s *applySpec) { s.PatchDigest = "https://presigned.example/secret" },
		func(s *applySpec) { s.RepoPath = "/tmp/repo" },
		func(s *applySpec) { s.PatchSizeBytes = s.MaxPatchBytes + 1 },
	} {
		candidate := spec
		mutate(&candidate)
		candidateBody, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, err := parseContract(func(string) (string, bool) { return string(candidateBody), true }); err == nil {
			t.Fatalf("mutated contract %#v unexpectedly accepted", candidate)
		}
	}
}

func TestReadPatchEnforcesSizeDigestAndRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch.diff")
	body := []byte("diff --git a/a b/a\n")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	spec := applySpec{PatchPath: path, PatchSizeBytes: int64(len(body)), MaxPatchBytes: 64 << 20, PatchDigest: digestBytes(body)}
	got, err := readPatch(spec)
	if err != nil || string(got) != string(body) {
		t.Fatalf("patch=%q err=%v", got, err)
	}
	for _, mutate := range []func(*applySpec){
		func(s *applySpec) { s.PatchDigest = digestBytes([]byte("other")) },
		func(s *applySpec) { s.PatchSizeBytes++ },
		func(s *applySpec) { s.MaxPatchBytes = 2 },
	} {
		candidate := spec
		mutate(&candidate)
		if _, err := readPatch(candidate); !errors.Is(err, errPatch) {
			t.Fatalf("mutated patch spec=%#v err=%v", candidate, err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	spec.PatchPath = link
	if _, err := readPatch(spec); !errors.Is(err, errPatch) {
		t.Fatalf("patch symlink err=%v", err)
	}
}

func TestOfflineApplyChecksBaseSHAThenAppliesPatch(t *testing.T) {
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	env := safeGitEnvironment()
	if _, err := runGit(ctx, repo, env, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "config", "user.email", "agw@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "config", "user.name", "agw-test"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "file.txt")
	if err := os.WriteFile(file, []byte("old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "add", "--", "file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "commit", "--quiet", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	baseBytes, err := runGit(ctx, repo, env, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseBytes))
	patch := []byte("diff --git a/file.txt b/file.txt\nindex 3367afd..3e75765 100644\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n")
	patchPath := filepath.Join(t.TempDir(), "patch.diff")
	if err := os.WriteFile(patchPath, patch, 0600); err != nil {
		t.Fatal(err)
	}
	spec := applySpec{RepoPath: repo, PatchPath: patchPath, BaseSHA: baseSHA, PatchDigest: digestBytes(patch), PatchSizeBytes: int64(len(patch)), MaxPatchBytes: 64 << 20}
	if err := verifyCheckoutSHA(ctx, repo, env, baseSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "apply", "--check", "--", patchPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repo, env, "apply", "--whitespace=error", "--", patchPath); err != nil {
		t.Fatal(err)
	}
	if err := verifyCheckoutSHA(ctx, repo, env, baseSHA); err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(file)
	if err != nil || string(changed) != "new\n" {
		t.Fatalf("changed file=%q err=%v", changed, err)
	}
	if !canonical.ValidDigest(spec.PatchDigest) {
		t.Fatal("test patch digest is not canonical")
	}
	if strings.Contains(strings.Join(env, "\x00"), "API_KEY") || strings.Contains(strings.Join(env, "\x00"), "http") {
		t.Fatalf("offline Git environment exposes network/credential settings: %v", env)
	}
}

func TestSafeEnvironmentDoesNotUseHostGitConfiguration(t *testing.T) {
	env := strings.Join(safeGitEnvironment(), "\x00")
	for _, forbidden := range []string{"GIT_ASKPASS=", "GIT_CREDENTIAL", "HTTP_PROXY", "HTTPS_PROXY", "GIT_CONFIG_GLOBAL=/home"} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("offline environment contains %q: %s", forbidden, env)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is unavailable in this test environment")
	}
}
