package skills

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type staticFetcher []byte

func (f staticFetcher) Fetch(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f)), nil
}

func archive(t *testing.T, entries map[string]string, link bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range entries {
		header := &tar.Header{Name: name, Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if link {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "/etc/passwd"
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !link {
			if _, err := writer.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func digestFor(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestMaterializeVerifiedReadOnlySkill(t *testing.T) {
	payload := archive(t, map[string]string{"skill/SKILL.md": "---\nname: test\n---\n", "skill/scripts/run.sh": "#!/bin/sh\n"}, false)
	root := t.TempDir()
	destination := filepath.Join(root, "skill")
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
	})
	err := (Materializer{Fetcher: staticFetcher(payload)}).Materialize(context.Background(), Reference{Source: "test", Digest: digestFor(payload)}, destination)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(destination, "skill", "SKILL.md"))
	if err != nil || len(manifest) == 0 {
		t.Fatalf("manifest=%q err=%v", manifest, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0555 {
		t.Fatalf("destination mode=%o", info.Mode().Perm())
	}
}

func TestRejectsTraversalAndCleansDestination(t *testing.T) {
	payload := archive(t, map[string]string{"../escape": "bad", "SKILL.md": "ok"}, false)
	destination := filepath.Join(t.TempDir(), "skill")
	err := (Materializer{Fetcher: staticFetcher(payload)}).Materialize(context.Background(), Reference{Source: "test", Digest: digestFor(payload)}, destination)
	if err == nil {
		t.Fatal("traversal accepted")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination was not cleaned: %v", statErr)
	}
}

func TestRejectsLinksAndDigestMismatch(t *testing.T) {
	payload := archive(t, map[string]string{"SKILL.md": ""}, true)
	destination := filepath.Join(t.TempDir(), "skill")
	if err := (Materializer{Fetcher: staticFetcher(payload)}).Materialize(context.Background(), Reference{Source: "test", Digest: digestFor(payload)}, destination); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := (Materializer{Fetcher: staticFetcher(payload)}).Materialize(context.Background(), Reference{Source: "test", Digest: "sha256:" + string(bytes.Repeat([]byte{'0'}, 64))}, filepath.Join(t.TempDir(), "other")); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}

func TestRejectsCumulativeExpandedSize(t *testing.T) {
	payload := archive(t, map[string]string{
		"SKILL.md":     strings.Repeat("a", 600),
		"reference.md": strings.Repeat("b", 600),
	}, false)
	destination := filepath.Join(t.TempDir(), "skill")
	materializer := Materializer{
		Fetcher: staticFetcher(payload),
		Limits:  Limits{PackageBytes: int64(len(payload)) + 1, ExpandedBytes: 1024, Files: 10},
	}
	err := materializer.Materialize(context.Background(), Reference{Source: "test", Digest: digestFor(payload)}, destination)
	if err == nil || !strings.Contains(err.Error(), "expanded-size") {
		t.Fatalf("cumulative expansion was not rejected: %v", err)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("failed materialization was not cleaned: %v", statErr)
	}
}

func TestMaterializationHonorsCancelledContext(t *testing.T) {
	payload := archive(t, map[string]string{"SKILL.md": "ok"}, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (Materializer{Fetcher: staticFetcher(payload)}).Materialize(ctx, Reference{Source: "test", Digest: digestFor(payload)}, filepath.Join(t.TempDir(), "skill"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization returned %v", err)
	}
}
