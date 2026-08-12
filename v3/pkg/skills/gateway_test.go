package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeGatewayOptions struct {
	Files       []string
	Contents    map[string]string
	InspectSSE  bool
	ReadSSE     bool
	RawResponse func(method string, request []byte) ([]byte, string, int, bool)
}

func newFakeGateway(t *testing.T, options fakeGatewayOptions) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	if options.Files == nil {
		options.Files = []string{"SKILL.md", "docs/guide.md"}
	}
	if options.Contents == nil {
		options.Contents = map[string]string{"SKILL.md": "# Demo\n", "docs/guide.md": "guide\n"}
	}
	var calls atomic.Int64
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			http.Error(response, "missing auth", http.StatusUnauthorized)
			return
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(request.Body)
		var message map[string]json.RawMessage
		if err := json.Unmarshal(body.Bytes(), &message); err != nil {
			http.Error(response, "bad request", http.StatusBadRequest)
			return
		}
		var method string
		_ = json.Unmarshal(message["method"], &method)
		if method == "notifications/initialized" {
			response.WriteHeader(http.StatusAccepted)
			return
		}
		var id string
		_ = json.Unmarshal(message["id"], &id)
		if options.RawResponse != nil {
			if payload, contentType, status, handled := options.RawResponse(method, body.Bytes()); handled {
				response.Header().Set("Content-Type", contentType)
				response.WriteHeader(status)
				_, _ = response.Write(payload)
				return
			}
		}
		response.Header().Set("Mcp-Session-Id", "fake-session")
		if method == "initialize" {
			writeFakeJSON(response, map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"result": map[string]any{
					"protocolVersion": mcpProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]string{"name": "fake", "version": "1"},
				},
			})
			return
		}
		if method != "tools/call" {
			writeFakeJSON(response, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		var params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		_ = json.Unmarshal(message["params"], &params)
		var result any
		switch params.Name {
		case "skills_inspect":
			result = map[string]any{"structuredContent": map[string]any{
				"metadata": map[string]any{"name": "Demo"},
				"catalog": map[string]any{
					"id":     "demo",
					"source": map[string]any{"revision": "commit-123", "repository": "example"},
				},
				"files": options.Files,
			}}
		case "skill_read":
			path := params.Arguments["path"]
			const prefix = "demo/"
			if !strings.HasPrefix(path, prefix) {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ERROR: skill file not found"}}, "isError": false}
				break
			}
			value, ok := options.Contents[strings.TrimPrefix(path, prefix)]
			if !ok {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ERROR: skill file not found"}}, "isError": false}
				break
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": value}}, "isError": false}
		default:
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "unknown tool"}}, "isError": true}
		}
		messageResponse := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
		if (method == "tools/call" && params.Name == "skills_inspect" && options.InspectSSE) || (method == "tools/call" && params.Name == "skill_read" && options.ReadSSE) {
			response.Header().Set("Content-Type", "text/event-stream")
			encoded, _ := json.Marshal(messageResponse)
			_, _ = fmt.Fprintf(response, "event: message\ndata: %s\n\n", encoded)
			return
		}
		writeFakeJSON(response, messageResponse)
	})
	return httptest.NewServer(handler), &calls
}

func writeFakeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(value)
}

func demoContents() map[string][]byte {
	return map[string][]byte{"SKILL.md": []byte("# Demo\n"), "docs/guide.md": []byte("guide\n")}
}

func newTestClient(t *testing.T, endpoint string, calls *atomic.Int64) *GatewayClient {
	t.Helper()
	return &GatewayClient{
		Endpoint: endpoint,
		Credential: func(context.Context) (string, error) {
			return "test-token", nil
		},
		Limits: GatewayLimits{MaxFileBytes: 1024, MaxTotalBytes: 4096, MaxFiles: 10, MaxResponseBytes: 8192, MaxRequestBytes: 4096},
	}
}

func TestGatewayMaterializeStreamableHTTPAndPreserveSourceRevision(t *testing.T) {
	server, calls := newFakeGateway(t, fakeGatewayOptions{InspectSSE: true, ReadSSE: true})
	defer server.Close()
	client := newTestClient(t, server.URL+"/mcp", calls)
	expected, err := CanonicalDigest(demoContents())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Cleanup(func() { makeGatewayTestTreeWritable(root) })
	destination := filepath.Join(root, "demo")
	result, err := client.Materialize(context.Background(), "demo", expected, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result.SkillID != "demo" || result.ContentDigest != expected {
		t.Fatalf("unexpected materialization result: %+v", result)
	}
	if result.SourceRevision["revision"] != "commit-123" {
		t.Fatalf("source revision was not preserved: %#v", result.SourceRevision)
	}
	if _, exists := result.SourceRevision["content_digest"]; exists {
		t.Fatal("source metadata was conflated with content digest")
	}
	if got, readErr := os.ReadFile(filepath.Join(destination, "docs", "guide.md")); readErr != nil || string(got) != "guide\n" {
		t.Fatalf("materialized file=%q err=%v", got, readErr)
	}
	for _, path := range []string{destination, filepath.Join(destination, "docs"), filepath.Join(destination, "SKILL.md"), filepath.Join(destination, "docs", "guide.md")} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != map[bool]os.FileMode{true: 0555, false: 0444}[info.IsDir()] {
			t.Fatalf("%s mode=%o", path, info.Mode().Perm())
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("expected initialize, notification, inspect, and two reads; calls=%d", calls.Load())
	}
}

func TestGatewayAnonymousModeOmitsAuthorizationHeader(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		http.Error(response, "expected test stop", http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := NewGatewayClient(server.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = client.Resolve(context.Background(), "demo")
	if authorization != "" {
		t.Fatalf("anonymous client sent Authorization=%q", authorization)
	}
}

func TestGatewayRejectsAdversarialListings(t *testing.T) {
	cases := []struct {
		name  string
		files []string
	}{
		{name: "traversal", files: []string{"SKILL.md", "../escape"}},
		{name: "duplicate", files: []string{"SKILL.md", "SKILL.md"}},
		{name: "case collision", files: []string{"SKILL.md", "skill.md"}},
		{name: "backslash", files: []string{"SKILL.md", "docs\\guide.md"}},
		{name: "special component", files: []string{"SKILL.md", "./guide.md"}},
		{name: "reserved name", files: []string{"SKILL.md", "CON.txt"}},
		{name: "missing manifest", files: []string{"docs/guide.md"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newFakeGateway(t, fakeGatewayOptions{Files: testCase.files})
			defer server.Close()
			client := newTestClient(t, server.URL+"/mcp", nil)
			destination := filepath.Join(t.TempDir(), "demo")
			expected, _ := CanonicalDigest(demoContents())
			if _, err := client.Materialize(context.Background(), "demo", expected, destination); err == nil {
				t.Fatal("adversarial file listing was accepted")
			}
			if _, err := os.Lstat(destination); !os.IsNotExist(err) {
				t.Fatalf("destination was created after rejected listing: %v", err)
			}
		})
	}
}

func TestGatewayRejectsInvalidUTF8OversizeAndDigestMismatch(t *testing.T) {
	server, _ := newFakeGateway(t, fakeGatewayOptions{
		RawResponse: func(method string, _ []byte) ([]byte, string, int, bool) {
			if method == "tools/call" {
				return []byte("{\xff"), "application/json", http.StatusOK, true
			}
			return nil, "", 0, false
		},
	})
	defer server.Close()
	client := newTestClient(t, server.URL+"/mcp", nil)
	destination := filepath.Join(t.TempDir(), "demo")
	expected, _ := CanonicalDigest(demoContents())
	if _, err := client.Materialize(context.Background(), "demo", expected, destination); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("invalid UTF-8 response returned %v", err)
	}

	server2, _ := newFakeGateway(t, fakeGatewayOptions{Contents: map[string]string{"SKILL.md": strings.Repeat("x", 100), "docs/guide.md": "ok"}})
	defer server2.Close()
	limited := newTestClient(t, server2.URL+"/mcp", nil)
	limited.Limits.MaxFileBytes = 10
	if _, err := limited.Materialize(context.Background(), "demo", expected, filepath.Join(t.TempDir(), "limited")); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize file returned %v", err)
	}
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	normal := newTestClient(t, server2.URL+"/mcp", nil)
	if _, err := normal.Materialize(context.Background(), "demo", wrongDigest, filepath.Join(t.TempDir(), "mismatch")); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("digest mismatch returned %v", err)
	}
}

func TestCanonicalDigestIsOrderIndependentAndRejectsUnsafePaths(t *testing.T) {
	first, err := CanonicalDigest(map[string][]byte{"b.txt": []byte("b"), "a.txt": []byte("a")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalDigest(map[string][]byte{"a.txt": []byte("a"), "b.txt": []byte("b")})
	if err != nil || first != second {
		t.Fatalf("digest changed with map order: %s %s %v", first, second, err)
	}
	if first == "sha256:"+strings.Repeat("0", 64) {
		t.Fatal("digest was not computed")
	}
	for _, path := range []string{"../escape", "a//b", "a\\b", "CON.txt", "a/.", "a/ ", "/absolute"} {
		if _, err := CanonicalDigest(map[string][]byte{path: []byte("x")}); err == nil {
			t.Fatalf("unsafe path %q was accepted", path)
		}
	}
}

func TestGatewayRejectsRedirectAndExistingSymlinkDestination(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	defer redirectTarget.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := newTestClient(t, redirect.URL+"/mcp", nil)
	expected, _ := CanonicalDigest(demoContents())
	if _, err := client.Materialize(context.Background(), "demo", expected, filepath.Join(t.TempDir(), "redirect")); err == nil || !strings.Contains(err.Error(), "status 307") {
		t.Fatalf("redirect was followed or hidden: %v", err)
	}

	server, _ := newFakeGateway(t, fakeGatewayOptions{})
	defer server.Close()
	client = newTestClient(t, server.URL+"/mcp", nil)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Materialize(context.Background(), "demo", expected, destination); err == nil {
		t.Fatal("existing symlink destination was accepted")
	}
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

func TestGatewayRejectsDuplicateJSONRPCFields(t *testing.T) {
	server, _ := newFakeGateway(t, fakeGatewayOptions{
		RawResponse: func(method string, request []byte) ([]byte, string, int, bool) {
			if method == "initialize" {
				var message map[string]json.RawMessage
				_ = json.Unmarshal(request, &message)
				var id string
				_ = json.Unmarshal(message["id"], &id)
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","jsonrpc":"2.0","id":%q,"result":{}}`, id)), "application/json", http.StatusOK, true
			}
			return nil, "", 0, false
		},
	})
	defer server.Close()
	client := newTestClient(t, server.URL+"/mcp", nil)
	expected, _ := CanonicalDigest(demoContents())
	if _, err := client.Materialize(context.Background(), "demo", expected, filepath.Join(t.TempDir(), "duplicate")); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate JSON-RPC field was accepted: %v", err)
	}
}

func TestGatewayMaterializeConcurrent(t *testing.T) {
	server, _ := newFakeGateway(t, fakeGatewayOptions{ReadSSE: true})
	defer server.Close()
	var credentialCalls atomic.Int64
	client := &GatewayClient{
		Endpoint: server.URL + "/mcp",
		Credential: func(context.Context) (string, error) {
			credentialCalls.Add(1)
			return "test-token", nil
		},
		Limits: GatewayLimits{MaxFileBytes: 1024, MaxTotalBytes: 4096, MaxFiles: 10, MaxResponseBytes: 8192, MaxRequestBytes: 4096},
	}
	expected, _ := CanonicalDigest(demoContents())
	root := t.TempDir()
	t.Cleanup(func() { makeGatewayTestTreeWritable(root) })
	const workers = 12
	errorsCh := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, err := client.Materialize(context.Background(), "demo", expected, filepath.Join(root, fmt.Sprintf("skill-%d", index)))
			errorsCh <- err
		}(i)
	}
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if credentialCalls.Load() != workers*5 {
		t.Fatalf("unexpected credential callback count: %d", credentialCalls.Load())
	}
}

func makeGatewayTestTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0700)
		} else {
			_ = os.Chmod(path, 0600)
		}
		return nil
	})
}
