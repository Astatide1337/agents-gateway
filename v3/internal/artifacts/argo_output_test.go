package artifacts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
)

func TestArgoLifecycleOutputIsContentAddressedAndIdempotent(t *testing.T) {
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts"}
	writer, err := NewWriter(store)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	body := []byte(`{"kind":"argo-lifecycle-output","version":1}`)
	specDigest := canonical.DigestPrefix + strings.Repeat("a", 64)
	first, err := writer.SaveArgoLifecycleOutput(context.Background(), "run-uid", specDigest, body)
	if err != nil {
		t.Fatalf("SaveArgoLifecycleOutput(first): %v", err)
	}
	second, err := writer.SaveArgoLifecycleOutput(context.Background(), "run-uid", specDigest, body)
	if err != nil {
		t.Fatalf("SaveArgoLifecycleOutput(second): %v", err)
	}
	if first != second || first.Kind != "argo-lifecycle-output" || first.Name != "lifecycle-output.json" || first.MediaType != "application/json" || first.SizeBytes != int64(len(body)) {
		t.Fatalf("refs differ or are incomplete: first=%#v second=%#v", first, second)
	}
	loaded, err := writer.LoadArgoLifecycleOutput(context.Background(), "run-uid", specDigest, first.Digest)
	if err != nil {
		t.Fatalf("LoadArgoLifecycleOutput: %v", err)
	}
	if string(loaded) != string(body) {
		t.Fatalf("loaded=%q, want %q", loaded, body)
	}
}

func TestArgoLifecycleOutputFailsClosedOnCollisionAndDigestTampering(t *testing.T) {
	store := &memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts"}
	writer, err := NewWriter(store)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	body := []byte(`{"kind":"argo-lifecycle-output","version":1}`)
	specDigest := canonical.DigestPrefix + strings.Repeat("b", 64)
	first, err := writer.SaveArgoLifecycleOutput(context.Background(), "run-uid", specDigest, body)
	if err != nil {
		t.Fatalf("SaveArgoLifecycleOutput: %v", err)
	}
	key := "runs/run-uid/orchestration/" + strings.TrimPrefix(first.Digest, canonical.DigestPrefix) + ".json"
	store.values[key] = []byte(`{"different":true}`)
	if _, err := writer.SaveArgoLifecycleOutput(context.Background(), "run-uid", specDigest, body); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("collision error=%v, want ErrObjectConflict", err)
	}
	if _, err := writer.LoadArgoLifecycleOutput(context.Background(), "run-uid", specDigest, first.Digest); !errors.Is(err, ErrObjectConflict) {
		t.Fatalf("tampered load error=%v, want ErrObjectConflict", err)
	}
}

func TestArgoLifecycleOutputRejectsInvalidBoundsAndUnsafeStorage(t *testing.T) {
	specDigest := canonical.DigestPrefix + strings.Repeat("c", 64)
	body := []byte(`{"kind":"argo-lifecycle-output"}`)
	writer, _ := NewWriter(&memoryStore{values: map[string][]byte{}, uri: "s3://agw-artifacts"})
	for _, test := range []struct {
		name   string
		runUID string
		digest string
		body   []byte
	}{
		{name: "unsafe run uid", runUID: "../run", digest: specDigest, body: body},
		{name: "invalid spec digest", runUID: "run-uid", digest: "bad", body: body},
		{name: "empty body", runUID: "run-uid", digest: specDigest, body: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := writer.SaveArgoLifecycleOutput(context.Background(), test.runUID, test.digest, test.body); !errors.Is(err, ErrInvalidArtifact) {
				t.Fatalf("SaveArgoLifecycleOutput error=%v, want ErrInvalidArtifact", err)
			}
		})
	}
	unsafeWriter, _ := NewWriter(&memoryStore{values: map[string][]byte{}, uri: "https://user@example.test?token=x"})
	if _, err := unsafeWriter.SaveArgoLifecycleOutput(context.Background(), "run-uid", specDigest, body); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("unsafe URI error=%v, want ErrInvalidArtifact", err)
	}
}
