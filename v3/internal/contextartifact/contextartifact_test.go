package contextartifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
	uri    string
	fail   error
}

func (s *memoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, "", s.fail
	}
	if _, ok := s.values[key]; ok {
		return false, s.uri + "/" + key, nil
	}
	s.values[key] = append([]byte(nil), body...)
	return true, s.uri + "/" + key, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.values[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), body...), nil
}

func (s *memoryStore) URI(key string) (string, error) {
	return s.uri + "/" + key, nil
}

func TestPublishLoadAndDecodeAreDeterministicAndCredentialFree(t *testing.T) {
	root, contract := materializedFixture(t)
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://bucket/prefix"}
	first, err := Publish(context.Background(), store, root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Publish(context.Background(), store, root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Kind != Kind || first.Name != Name || first.MediaType != MediaType {
		t.Fatalf("non-idempotent context refs: %#v %#v", first, second)
	}
	body, uri, err := Load(context.Background(), store, contract.RunUID, contract.ResolvedSpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	bundle, ref, err := Decode(body, uri, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if ref != first || bundle.ContextPack.Digest == "" || len(bundle.Files) == 0 {
		t.Fatalf("decoded bundle/ref=%#v/%#v, want %#v and files", bundle, ref, first)
	}
	for _, forbidden := range []string{"Keep this task secret", "Do not copy instructions into evidence", "model", "credential", "transcript"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("context evidence copied forbidden content %q: %s", forbidden, body)
		}
	}
}

func TestPublishRejectsTamperedVolumeAndStoreConflict(t *testing.T) {
	root, contract := materializedFixture(t)
	refPath := filepath.Join(root, contextmaterializer.RefFileName)
	refBody, err := os.ReadFile(refPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(refPath, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath, append(refBody, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://bucket/prefix"}
	if _, err := Publish(context.Background(), store, root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA); !errors.Is(err, contextmaterializer.ErrTampered) {
		t.Fatalf("tampered volume error=%v, want contextmaterializer.ErrTampered", err)
	}

	root, contract = materializedFixture(t)
	_, body, err := Build(root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	store = &memoryStore{values: map[string][]byte{}, uri: "s3://bucket/prefix"}
	key, err := ObjectKey(contract.RunUID, contract.ResolvedSpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	store.values[key] = []byte("different")
	if _, err := Publish(context.Background(), store, root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA); !errors.Is(err, ErrConflict) {
		t.Fatalf("store conflict error=%v, want ErrConflict", err)
	}
	if len(body) > MaxBundleBytes {
		t.Fatalf("fixture bundle size=%d exceeds bound", len(body))
	}
}

func TestDecodeRejectsIdentityAndCanonicalTampering(t *testing.T) {
	root, contract := materializedFixture(t)
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://bucket/prefix"}
	ref, err := Publish(context.Background(), store, root, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	body, uri, err := Load(context.Background(), store, contract.RunUID, contract.ResolvedSpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decode(body, uri, "other-run", contract.ResolvedSpecDigest, contract.BaseSHA); err == nil {
		t.Fatal("run identity tampering was accepted")
	}
	if _, _, err := Decode(append(body, '\n'), uri, contract.RunUID, contract.ResolvedSpecDigest, contract.BaseSHA); err == nil {
		t.Fatal("non-canonical evidence was accepted")
	}
	if ref.Digest == "" {
		t.Fatal("publish returned no digest")
	}
}

func materializedFixture(t *testing.T) (string, contextmaterializer.Contract) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "base")
	skills := filepath.Join(root, "skills")
	output := filepath.Join(root, "context")
	for _, directory := range []string{base, skills, output} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	contract := contextmaterializer.Contract{
		SchemaVersion:      contextmaterializer.SchemaVersion,
		RunUID:             "run-context-1",
		BaseSHA:            strings.Repeat("a", 40),
		ResolvedSpecDigest: "sha256:" + strings.Repeat("b", 64),
		Task:               "Keep this task secret",
		Instructions:       "Do not copy instructions into evidence",
		Boundaries:         contextpack.Boundaries{AllowedPaths: []string{"src/**"}},
		Budgets: contextpack.Budgets{
			MaxInputBytes:  8 << 20,
			MaxOutputBytes: 8 << 20,
			MaxFileBytes:   1 << 20,
			MaxFiles:       1024,
			MaxTokens:      40000,
			MaxEntries:     4096,
		},
	}
	body, err := contextmaterializer.Encode(contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contextmaterializer.Run(context.Background(), contextmaterializer.Config{
		InputJSON: string(body), ExpectedRunUID: contract.RunUID, ExpectedSpecDigest: contract.ResolvedSpecDigest,
		ExpectedBaseSHA: contract.BaseSHA, BaseDir: base, SkillsDir: skills, OutputDir: output,
	}); err != nil {
		t.Fatal(err)
	}
	return output, contract
}
