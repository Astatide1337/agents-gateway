package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type faultInjectionObjects struct {
	puts    int
	keys    []string
	putErr  error
	deletes []string
}

func (f *faultInjectionObjects) Delete(_ context.Context, _ string, key string) error {
	f.deletes = append(f.deletes, key)
	for index, candidate := range f.keys {
		if candidate == key {
			f.keys = append(f.keys[:index], f.keys[index+1:]...)
			break
		}
	}
	return nil
}

func (f *faultInjectionObjects) Put(_ context.Context, _ string, key string, body io.Reader, _ int64, _ string, _ string) error {
	f.puts++
	if _, err := io.ReadAll(body); err != nil {
		return err
	}
	if f.putErr != nil {
		return f.putErr
	}
	f.keys = append(f.keys, key)
	return nil
}

func (f *faultInjectionObjects) PresignGet(context.Context, string, string, time.Duration) (string, error) {
	return "", errors.New("not used by fault injection")
}

// TestFaultInjectionArtifactObjectFailurePreventsCatalogCallback verifies the
// safe half of the split: a failed object commit never invokes catalog code.
func TestFaultInjectionArtifactObjectFailurePreventsCatalogCallback(t *testing.T) {
	objects := &faultInjectionObjects{putErr: errors.New("object store unavailable")}
	store, err := New(objects, Config{Bucket: "fault", Prefix: "agw", MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	handler, err := NewPublishHandler(store, PublishHandlerConfig{
		OrganizationID: "org", ProjectID: "project", RunID: "run", Path: "/publish",
		OnStored: func(context.Context, Metadata, string, PublishDescriptor) error {
			callbackCalled = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := publishFaultRequest(t, handler)
	if recorder.Code != http.StatusBadRequest || callbackCalled || objects.puts != 1 {
		t.Fatalf("object failure status=%d callback=%t puts=%d body=%s", recorder.Code, callbackCalled, objects.puts, recorder.Body.String())
	}
}

// TestFaultInjectionArtifactCatalogFailureCompensatesObject verifies that a
// failed catalog commit cannot leave a downloadable orphan behind. A retry
// may use a fresh immutable ID, but each failed attempt is compensated.
func TestFaultInjectionArtifactCatalogFailureCompensatesObject(t *testing.T) {
	objects := &faultInjectionObjects{}
	store, err := New(objects, Config{Bucket: "fault", Prefix: "agw", MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	callbackCalls := 0
	handler, err := NewPublishHandler(store, PublishHandlerConfig{
		OrganizationID: "org", ProjectID: "project", RunID: "run", Path: "/publish",
		OnStored: func(context.Context, Metadata, string, PublishDescriptor) error {
			callbackCalls++
			return errors.New("catalog unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := publishFaultRequest(t, handler)
	second := publishFaultRequest(t, handler)
	if first.Code != http.StatusServiceUnavailable || second.Code != http.StatusServiceUnavailable {
		t.Fatalf("catalog failures returned statuses %d/%d", first.Code, second.Code)
	}
	if callbackCalls != 2 || objects.puts != 2 || len(objects.keys) != 0 || len(objects.deletes) != 2 || objects.deletes[0] == objects.deletes[1] {
		t.Fatalf("compensation evidence callback=%d puts=%d live_keys=%v deletes=%v", callbackCalls, objects.puts, objects.keys, objects.deletes)
	}
}

func publishFaultRequest(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	descriptor, err := EncodePublishDescriptor(PublishDescriptor{
		Schema: "agents-gateway.artifact.v1", Title: "fault artifact", ContentKind: "document",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("fault body"))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set(PublishMetadataHeader, descriptor)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
