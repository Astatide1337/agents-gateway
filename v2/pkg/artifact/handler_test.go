package artifact

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadHandlerReturnsWorkflowCompatibleArtifact(t *testing.T) {
	root := t.TempDir()
	client := mustLocalClient(t, root)
	store, err := New(client, Config{Bucket: "artifacts", Prefix: "agw", MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewUploadHandler(store, UploadHandlerConfig{
		OrganizationID: "org", ProjectID: "project", RunID: "run", Name: "output.json",
		Path: "/v1/runs/run/output", Method: http.MethodPut, MaxBytes: 32,
		AllowedMediaTypes: []string{"application/json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/v1/runs/run/output", strings.NewReader(`{"ok":true}`))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 5 || response["id"] == nil || response["uri"] != "artifact://catalog/"+response["id"].(string)+"/"+response["id"].(string) {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response["size_bytes"] != float64(len(`{"ok":true}`)) || response["media_type"] != "application/json" {
		t.Fatalf("unexpected response fields: %#v", response)
	}
	for _, forbidden := range []string{root, "password", "credential", "secret"} {
		if strings.Contains(strings.ToLower(recorder.Body.String()), strings.ToLower(forbidden)) {
			t.Fatalf("response contains forbidden value %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestUploadHandlerRejectsMethodPathQueryMediaAndBodyViolations(t *testing.T) {
	client := mustLocalClient(t, t.TempDir())
	store, _ := New(client, Config{Bucket: "artifacts", Prefix: "agw", MaxBytes: 8})
	handler, err := NewUploadHandler(store, UploadHandlerConfig{
		OrganizationID: "org", ProjectID: "project", RunID: "run", Name: "output",
		Path: "/upload", Method: http.MethodPost, MaxBytes: 4, AllowedMediaTypes: []string{"text/plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		method string
		url    string
		media  string
		body   string
		status int
	}{
		{"wrong method", http.MethodPut, "/upload", "text/plain", "x", http.StatusNotFound},
		{"wrong path", http.MethodPost, "/other", "text/plain", "x", http.StatusNotFound},
		{"query", http.MethodPost, "/upload?run=other", "text/plain", "x", http.StatusNotFound},
		{"wrong media", http.MethodPost, "/upload", "application/json", "x", http.StatusUnsupportedMediaType},
		{"oversized", http.MethodPost, "/upload", "text/plain", "12345", http.StatusRequestEntityTooLarge},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.url, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.media)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.status {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestUploadHandlerScopeAndConfigAreImmutable(t *testing.T) {
	root := t.TempDir()
	client := mustLocalClient(t, root)
	store, _ := New(client, Config{Bucket: "artifacts", Prefix: "agw", MaxBytes: 8})
	if _, err := NewUploadHandler(store, UploadHandlerConfig{OrganizationID: "../org", ProjectID: "project", RunID: "run", Name: "output", Path: "/upload", AllowedMediaTypes: []string{"text/plain"}}); err == nil {
		t.Fatal("unsafe scope accepted")
	}
	handler, err := NewUploadHandler(store, UploadHandlerConfig{OrganizationID: "org", ProjectID: "project", RunID: "run", Name: "output", Path: "/upload", AllowedMediaTypes: []string{"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/upload", bytes.NewBufferString("x"))
	request.Header.Set("Content-Type", "text/plain")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "agw", "org", "project", "run")); err != nil {
		t.Fatal(err)
	}
}
