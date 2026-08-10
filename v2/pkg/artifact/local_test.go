package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLocalObjectClientRejectsUnsafeRootsAndKeys(t *testing.T) {
	root := t.TempDir()
	if _, err := NewLocalObjectClient(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing root accepted")
	}
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalObjectClient(public); err == nil {
		t.Fatal("world-accessible root accepted")
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalObjectClient(link); err == nil {
		t.Fatal("symlink root accepted")
	}
	client, err := NewLocalObjectClient(private)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestFor("x")
	for _, key := range []string{"../outside", "a/../../outside", "/absolute", "a\\b", "a//b", "a/./b", "a/..", ""} {
		if err := client.Put(context.Background(), "bucket", key, strings.NewReader("x"), 1, "text/plain", digest); err == nil {
			t.Fatalf("unsafe key accepted: %q", key)
		}
	}
}

func TestLocalObjectClientIsImmutableAndVerifiesMetadata(t *testing.T) {
	root := t.TempDir()
	client := mustLocalClient(t, root)
	ctx := context.Background()
	key := generatedKey("output")
	if err := client.Put(ctx, "artifacts", key, strings.NewReader("payload"), 7, "text/plain", digestFor("payload")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(key))
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0400 || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("stored object mode=%v", info.Mode())
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "payload" {
		t.Fatalf("stored content=%q err=%v", content, err)
	}
	if err := client.Put(ctx, "artifacts", key, strings.NewReader("changed"), 7, "text/plain", digestFor("changed")); err == nil {
		t.Fatal("overwrite accepted")
	}
	if err := client.Put(ctx, "artifacts", generatedKey("short"), strings.NewReader("x"), 2, "text/plain", digestFor("x")); err == nil {
		t.Fatal("size mismatch accepted")
	}
	if err := client.Put(ctx, "artifacts", generatedKey("digest"), strings.NewReader("x"), 1, "text/plain", digestFor("different")); err == nil {
		t.Fatal("digest mismatch accepted")
	}
	uri, err := client.PresignGet(ctx, "artifacts", key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uri != "artifact://artifacts/"+key || strings.Contains(uri, root) {
		t.Fatalf("unexpected local artifact URI: %q", uri)
	}
}

func TestLocalObjectClientNeverFollowsSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "org")); err != nil {
		t.Fatal(err)
	}
	client := mustLocalClient(t, root)
	key := generatedKey("output")
	if err := client.Put(context.Background(), "bucket", key, strings.NewReader("x"), 1, "text/plain", digestFor("x")); err == nil {
		t.Fatal("symlinked directory accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "project", "run", "00000000000000000000000000000000-output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestLocalObjectClientConcurrentNoOverwrite(t *testing.T) {
	client := mustLocalClient(t, t.TempDir())
	const workers = 32
	var wait sync.WaitGroup
	var mu sync.Mutex
	var successes int
	var unexpected []error
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := client.Put(context.Background(), "bucket", generatedKey("object"), strings.NewReader("payload"), 7, "text/plain", digestFor("payload"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else if !strings.Contains(err.Error(), "already exists") {
				unexpected = append(unexpected, err)
			}
		}()
	}
	wait.Wait()
	if successes != 1 || len(unexpected) != 0 {
		t.Fatalf("successes=%d unexpected=%v", successes, unexpected)
	}
}

func mustLocalClient(t *testing.T, root string) *LocalObjectClient {
	t.Helper()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	client, err := NewLocalObjectClient(root)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func digestFor(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func generatedKey(name string) string {
	return "org/project/run/00000000000000000000000000000000-" + name
}
