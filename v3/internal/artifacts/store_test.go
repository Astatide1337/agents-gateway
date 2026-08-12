package artifacts

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
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
	value, ok := s.values[key]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), value...), nil
}

func TestSaveResolvedSpecIsContentAddressedAndIdempotent(t *testing.T) {
	body, err := canonical.CanonicalizeResolvedSpec(map[string]any{"run": "one"})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts"}
	writer, _ := NewWriter(store)
	first, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, body)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Digest != digest || first.SizeBytes != int64(len(body)) {
		t.Fatalf("refs differ: %#v %#v", first, second)
	}
	loaded, err := writer.LoadResolvedSpec(context.Background(), "run-uid", digest)
	if err != nil || string(loaded) != string(body) {
		t.Fatalf("loaded body=%q err=%v", loaded, err)
	}
}

func TestSaveResolvedSpecFailsClosedOnCollisionOrAmbiguity(t *testing.T) {
	body, _ := canonical.CanonicalizeResolvedSpec(map[string]any{"run": "one"})
	digest, _ := canonical.ResolvedSpecDigest(body)
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts"}
	writer, _ := NewWriter(store)
	key := "runs/run-uid/resolved/" + digest[len(canonical.DigestPrefix):] + ".json"
	store.values[key] = []byte("different")
	if _, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, body); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("collision error=%v", err)
	}
	store = &memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts", fail: errors.New("timeout")}
	writer, _ = NewWriter(store)
	if _, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, body); err == nil {
		t.Fatal("ambiguous put was accepted")
	}
}

func TestSaveResolvedSpecRejectsDigestMismatchAndUnsafeURI(t *testing.T) {
	body, _ := canonical.CanonicalizeResolvedSpec(map[string]any{"run": "one"})
	digest, _ := canonical.ResolvedSpecDigest(body)
	store := &memoryStore{values: map[string][]byte{}, uri: "https://user@example.test?token=x"}
	writer, _ := NewWriter(store)
	if _, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, []byte(`{"run":"two"}`)); err == nil {
		t.Fatal("digest mismatch was accepted")
	}
	if _, err := writer.SaveResolvedSpec(context.Background(), "run-uid", digest, body); err == nil {
		t.Fatal("unsafe URI was accepted")
	}
}
