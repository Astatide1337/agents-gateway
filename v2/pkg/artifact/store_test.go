package artifact

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeObjects struct {
	bucket, key, body, mediaType, digest string
	size                                 int64
	deleted                              string
}

func (f *fakeObjects) Delete(_ context.Context, _ string, key string) error {
	f.deleted = key
	return nil
}

func (f *fakeObjects) Put(_ context.Context, bucket, key string, body io.Reader, size int64, mediaType, digest string) error {
	payload, _ := io.ReadAll(body)
	f.bucket, f.key, f.body, f.size, f.mediaType, f.digest = bucket, key, string(payload), size, mediaType, digest
	return nil
}
func (f *fakeObjects) PresignGet(_ context.Context, bucket, key string, ttl time.Duration) (string, error) {
	return "https://objects.example/" + bucket + "/" + key + "?ttl=" + ttl.String(), nil
}

func TestImmutableTenantScopedArtifact(t *testing.T) {
	objects := &fakeObjects{}
	store, err := New(objects, Config{Bucket: "artifacts", Prefix: "agw", MaxBytes: 1024, DownloadTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := store.Put(context.Background(), PutRequest{OrganizationID: "org", ProjectID: "project", RunID: "run", Name: "report.json", MediaType: "application/json", Body: strings.NewReader(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if objects.body != `{"ok":true}` || !strings.HasPrefix(metadata.ObjectKey, "agw/org/project/run/") || metadata.Digest == "" {
		t.Fatalf("metadata=%#v objects=%#v", metadata, objects)
	}
	if _, err := store.DownloadURL(context.Background(), "other", "project", "run", metadata.ObjectKey); err == nil {
		t.Fatal("cross-tenant download was presigned")
	}
	if _, err := store.DownloadURL(context.Background(), "org", "project", "run", metadata.ObjectKey); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactLimitsAndIdentifiers(t *testing.T) {
	store, _ := New(&fakeObjects{}, Config{Bucket: "bucket", MaxBytes: 4})
	if _, err := store.Put(context.Background(), PutRequest{OrganizationID: "org", ProjectID: "project", RunID: "run", Name: "file", MediaType: "text/plain", Body: bytes.NewReader([]byte("12345"))}); err == nil {
		t.Fatal("oversized artifact accepted")
	}
	if _, err := store.Put(context.Background(), PutRequest{OrganizationID: "../org", ProjectID: "project", RunID: "run", Name: "file", MediaType: "text/plain", Body: strings.NewReader("x")}); err == nil {
		t.Fatal("unsafe identifier accepted")
	}
}
