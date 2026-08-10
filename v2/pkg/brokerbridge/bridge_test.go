package brokerbridge

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBridge(t *testing.T, handler http.Handler) (*Bridge, Endpoints, *requestCapture) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	capture := &requestCapture{handler: handler}
	socketPath := filepath.Join(directory, "broker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: capture}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		_ = listener.Close()
	})
	clientPath := filepath.Join(directory, "client.json")
	client := clientConfig{
		SessionID:       "ags_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO123",
		BearerToken:     "agt_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO123",
		PolicyDigest:    "sha256:" + strings.Repeat("a", 64),
		AllowedModel:    "gpt-test",
		ModelURL:        "http://127.0.0.1:8787/v1/responses",
		ToolsURL:        "http://127.0.0.1:8787/mcp",
		ArtifactURL:     "http://127.0.0.1:8787/v1/artifacts/output",
		ArtifactEnabled: true,
	}
	raw, err := json.Marshal(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientPath, raw, 0400); err != nil {
		t.Fatal(err)
	}
	bridge, endpoints, err := Start(Config{ClientConfigPath: clientPath, SocketPath: socketPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = bridge.Close(ctx)
	})
	return bridge, endpoints, capture
}

type requestCapture struct {
	handler http.Handler
	last    *http.Request
}

func (c *requestCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.last = r.Clone(r.Context())
	c.handler.ServeHTTP(w, r)
}

func TestBridgeForwardsOnlyApprovedRoutesAndInjectsCapability(t *testing.T) {
	_, endpoints, capture := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ResponsesPath {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "must-not-cross")
		_, _ = io.WriteString(w, "data: ok\n\n")
	}))
	request, err := http.NewRequest(http.MethodPost, endpoints.ResponsesBaseURL+"/responses", strings.NewReader(`{"model":"gpt-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer attacker")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "data: ok\n\n" || response.Header.Get("Set-Cookie") != "" {
		t.Fatalf("response = %d headers=%v body=%q", response.StatusCode, response.Header, body)
	}
	if capture.last.Header.Get("Authorization") == "Bearer attacker" || !strings.HasPrefix(capture.last.Header.Get("Authorization"), "Bearer agt_") {
		t.Fatalf("authorization was not replaced: %q", capture.last.Header.Get("Authorization"))
	}
	for _, name := range []string{"X-AGW-Session-ID", "X-AGW-Model", "X-AGW-Policy-Digest"} {
		if capture.last.Header.Get(name) == "" {
			t.Fatalf("missing injected header %s", name)
		}
	}

	for _, target := range []string{endpoints.ResponsesBaseURL, endpoints.ResponsesBaseURL + "/responses?x=1", strings.TrimSuffix(endpoints.ResponsesBaseURL, "/v1") + "/admin"} {
		response, err := http.Post(target, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("disallowed route %q status = %d", target, response.StatusCode)
		}
	}
}

func TestBridgeExposesMCPAndArtifactEndpoints(t *testing.T) {
	_, endpoints, _ := testBridge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	for _, test := range []struct{ method, target string }{{http.MethodPost, endpoints.MCPURL}, {http.MethodPut, endpoints.ArtifactURL}} {
		request, _ := http.NewRequest(test.method, test.target, strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("%s %s status = %d", test.method, test.target, response.StatusCode)
		}
	}
}

func TestBridgeRejectsUnsafeClientConfigAndSocket(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "client.json")
	if err := os.WriteFile(configPath, []byte(`{"session_id":"secret"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Start(Config{ClientConfigPath: configPath, SocketPath: filepath.Join(directory, "missing.sock")}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe config error = %v", err)
	}
}
