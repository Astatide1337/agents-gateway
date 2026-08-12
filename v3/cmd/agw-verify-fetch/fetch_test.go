package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
)

func TestParseContractIsClosedAndDigestBound(t *testing.T) {
	spec := validFetchSpec()
	body, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) {
		if name == "AGW_FETCH_SPEC_JSON" {
			return string(body), true
		}
		return "", false
	}
	parsed, err := parseContract(lookup)
	if err != nil || parsed.PatchKey != spec.PatchKey {
		t.Fatalf("parsed=%#v err=%v", parsed, err)
	}
	unknown := strings.TrimSuffix(string(body), "}") + `,"unexpected":"ignored"}`
	if _, err := parseContract(func(string) (string, bool) { return unknown, true }); !errors.Is(err, errInvalidContract) {
		t.Fatalf("unknown field err=%v, want closed contract", err)
	}
	for _, mutate := range []func(*fetchSpec){
		func(s *fetchSpec) { s.PatchKey = "runs/../patch.diff" },
		func(s *fetchSpec) { s.PatchDigest = "https://presigned.example/secret" },
		func(s *fetchSpec) { s.ArtifactStore.Endpoint = "http://objects.example" },
		func(s *fetchSpec) { s.ArtifactAccessKeyIDFile = "/tmp/credential" },
		func(s *fetchSpec) { s.PatchSizeBytes = s.MaxPatchBytes + 1 },
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

func TestReadSecretFileDoesNotFollowSymlinksAndSupportsOptionalSession(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(secret, 64, false)
	if err != nil || got != "value" {
		t.Fatalf("secret=%q err=%v", got, err)
	}
	missing, err := readSecretFile(filepath.Join(dir, "missing"), 64, true)
	if err != nil || missing != "" {
		t.Fatalf("optional missing=%q err=%v", missing, err)
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(secret, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(symlink, 64, false); err == nil {
		t.Fatal("secret symlink was followed")
	}
	if _, err := readSecretFile(secret, 3, false); err == nil {
		t.Fatal("oversized secret was accepted")
	}
}

func TestExtractArchiveRejectsLinksAndKeepsBaseReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "base")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(path, 0755)
			}
			return nil
		})
	})
	archive := tarBytes(t, []tarEntry{{name: "src/", kind: tar.TypeDir}, {name: "src/main.go", body: []byte("package main\n")}})
	if err := extractArchive(root, archive); err != nil {
		t.Fatal(err)
	}
	if err := makeReadOnlyTree(root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "src", "main.go"))
	if err != nil || info.Mode().Perm()&0222 != 0 {
		t.Fatalf("base file mode=%v err=%v, expected read-only", info.Mode(), err)
	}
	for _, entry := range []tarEntry{{name: "../escape", body: []byte("bad")}, {name: "link", kind: tar.TypeSymlink, link: "../../outside"}} {
		badRoot := filepath.Join(t.TempDir(), "base")
		if err := os.Mkdir(badRoot, 0755); err != nil {
			t.Fatal(err)
		}
		if err := extractArchive(badRoot, tarBytes(t, []tarEntry{entry})); !errors.Is(err, errArchive) {
			t.Fatalf("archive entry %#v err=%v, want rejection", entry, err)
		}
	}
}

func TestMakeWritableTreeUsesModesWithoutOwnershipCapability(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "file.txt")
	executable := filepath.Join(root, "run.sh")
	if err := os.WriteFile(regular, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("file.txt", link); err != nil {
		t.Fatal(err)
	}
	if err := makeWritableTree(root); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{
		root:                        0777,
		filepath.Join(root, ".git"): 0777,
		regular:                     0666,
		executable:                  0777,
	} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode=%#o, want %#o", name, got, want)
		}
	}
	linkInfo, err := os.Lstat(link)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("working-tree symlink was not preserved: info=%v err=%v", linkInfo, err)
	}
}

func TestPatchDigestValidationAndGitEnvironmentDoNotCarrySecrets(t *testing.T) {
	spec := validFetchSpec()
	patch := []byte("diff --git a/a b/a\n")
	spec.PatchSizeBytes = int64(len(patch))
	spec.PatchDigest = digest(patch)
	if err := validatePatchBytes(spec, patch); err != nil {
		t.Fatal(err)
	}
	patch[0] = 'X'
	if err := validatePatchBytes(spec, patch); !errors.Is(err, errPatch) {
		t.Fatalf("tampered patch err=%v", err)
	}
	env := strings.Join(safeGitEnvironment("/tmp/askpass", verifyworkload.CloneTokenFile, "/tmp/home"), "\x00")
	if strings.Contains(env, "secret") || strings.Contains(env, "token-value") {
		t.Fatalf("Git environment contains credential material: %q", env)
	}
	if strings.Contains(string(mustJSON(t, spec)), "presign") {
		t.Fatal("fetch contract contains a presigned URL")
	}
}

func validFetchSpec() fetchSpec {
	return fetchSpec{
		Version: 1, Repository: "github.com/Astatide1337/jobmark", BaseSHA: strings.Repeat("a", 40), Depth: 1,
		RepoPath: verifyworkload.RepoPath, BasePath: verifyworkload.BasePath, PatchPath: verifyworkload.PatchPath,
		PatchBucket: "agw-artifacts", PatchKey: "runs/run-1/patch.diff", PatchDigest: "sha256:" + strings.Repeat("b", 64), PatchSizeBytes: 12, MaxPatchBytes: objectstore.GeneralMaxObjectBytes,
		CloneTokenFile: verifyworkload.CloneTokenFile, ArtifactAccessKeyIDFile: verifyworkload.ArtifactAccessKeyIDFile,
		ArtifactSecretAccessKeyFile: verifyworkload.ArtifactSecretAccessKeyFile, ArtifactSessionTokenFile: verifyworkload.ArtifactSessionTokenFile,
		ArtifactStore: verifyfetch.StoreConfig{Bucket: "agw-artifacts", Region: "us-east-1", MaxObjectBytes: objectstore.GeneralMaxObjectBytes},
		DisableHooks:  true, RequireExactBase: true,
	}
}

type tarEntry struct {
	name string
	kind byte
	body []byte
	link string
}

func tarBytes(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0644, Typeflag: entry.kind, Size: int64(len(entry.body)), Linkname: entry.link}
		if entry.kind == 0 {
			header.Typeflag = tar.TypeReg
		}
		if entry.kind == tar.TypeDir {
			header.Mode = 0755
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
