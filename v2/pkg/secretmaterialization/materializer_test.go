package secretmaterialization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testSecret = "host-only-secret-value"

func privateTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFileResolverResolvesOnlyPrivateOpaqueReferences(t *testing.T) {
	root := privateTempDir(t)
	secretPath := filepath.Join(root, "provider-key")
	if err := os.WriteFile(secretPath, []byte(testSecret), 0600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewFileResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	value, err := resolver.Resolve(context.Background(), "secret://provider-key")
	if err != nil || string(value) != testSecret {
		t.Fatalf("resolve value=%q err=%v", value, err)
	}

	for _, ref := range []string{
		"env://provider-key", "secret://../provider-key", "secret:///provider-key",
		"secret://provider-key?x=1", "secret://provider-key#fragment", "secret://provider%2Fkey",
		"secret://provider/key", "secret://provider:key", "secret://",
	} {
		t.Run(ref, func(t *testing.T) {
			if _, err := resolver.Resolve(context.Background(), ref); err == nil {
				t.Fatal("unsafe secret reference was accepted")
			}
		})
	}
	if strings.Contains(string(value), "provider-key") {
		t.Log("reference is not secret material; value was still resolved from host storage")
	}
}

func TestFileResolverRejectsUnsafeSecretFiles(t *testing.T) {
	root := privateTempDir(t)
	resolver, err := NewFileResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		id   string
		mode os.FileMode
		data []byte
	}{
		{name: "group-readable", id: "group-readable", mode: 0640, data: []byte(testSecret)},
		{name: "empty", id: "empty", mode: 0600, data: nil},
		{name: "newline", id: "newline", mode: 0600, data: []byte("bad\nvalue")},
		{name: "nul", id: "nul", mode: 0600, data: []byte{'b', 0, 'a', 'd'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.WriteFile(path, tc.data, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Resolve(context.Background(), "secret://"+tc.id); err == nil {
				t.Fatal("unsafe secret file was accepted")
			}
		})
	}
	if err := os.Symlink(filepath.Join(root, "group-readable"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "secret://link"); err == nil {
		t.Fatal("symlink secret file was followed")
	}
}

type fakeResolver struct {
	value []byte
	err   error
}

func (r fakeResolver) Resolve(context.Context, string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	return append([]byte(nil), r.value...), nil
}

func TestMaterializeWritesStrictPerRunFileAndCleansIt(t *testing.T) {
	root := privateTempDir(t)
	materializer, err := New(root, fakeResolver{value: []byte(testSecret)})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "keep")
	if err := os.WriteFile(sentinel, []byte("not a run secret"), 0600); err != nil {
		t.Fatal(err)
	}
	materialized, err := materializer.Materialize(context.Background(), "run-1", []Reference{{Name: "API_KEY", Ref: "secret://opaque-provider"}})
	if err != nil {
		t.Fatal(err)
	}
	if materialized == nil || materialized.EnvironmentFile() == "" {
		t.Fatal("materialization returned no file")
	}
	info, err := os.Stat(materialized.EnvironmentFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("environment mode=%o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(materialized.EnvironmentFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "API_KEY="+testSecret+"\n" {
		t.Fatalf("unexpected environment file %q", data)
	}
	if strings.Contains(materialized.EnvironmentFile(), testSecret) {
		t.Fatal("secret appeared in materialized path")
	}
	if err := materialized.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materialized.EnvironmentFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized file remains after cleanup: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("cleanup removed unrelated host file: %v", err)
	}
	if err := materialized.Cleanup(); err != nil {
		t.Fatalf("cleanup was not idempotent: %v", err)
	}
}

func TestMaterializeFailsClosedAndDoesNotLeakSecret(t *testing.T) {
	secret := []byte(testSecret)
	materializer, err := New(privateTempDir(t), fakeResolver{value: secret, err: errors.New("resolver failed: " + testSecret)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = materializer.Materialize(context.Background(), "run-1", []Reference{{Name: "API_KEY", Ref: "secret://provider"}})
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("materialization error leaked secret or was absent: %v", err)
	}
	if _, err := materializer.Materialize(context.Background(), "run-1", []Reference{{Name: "bad-name", Ref: "secret://provider"}}); err == nil {
		t.Fatal("invalid environment name was accepted")
	}
	for _, refs := range [][]Reference{
		{{Name: "API_KEY", Ref: "secret://provider"}, {Name: "API_KEY", Ref: "secret://provider"}},
		{{Name: "API_KEY", Ref: "env://provider"}},
	} {
		if _, err := materializer.Materialize(context.Background(), "run-1", refs); err == nil {
			t.Fatal("invalid environment reference set was accepted")
		}
	}
}

func TestMaterializeHonorsCancellationAndConcurrentRuns(t *testing.T) {
	materializer, err := New(privateTempDir(t), fakeResolver{value: []byte("value")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := materializer.Materialize(ctx, "run-cancelled", []Reference{{Name: "VALUE", Ref: "secret://value"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled materialization error=%v", err)
	}
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := materializer.Materialize(context.Background(), "run-concurrent", []Reference{{Name: "VALUE", Ref: "secret://value"}})
			if err != nil {
				t.Errorf("concurrent materialization: %v", err)
				return
			}
			if err := value.Cleanup(); err != nil {
				t.Errorf("concurrent cleanup: %v", err)
			}
		}()
	}
	wg.Wait()
}
