package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRuntimeCodexLive exercises the complete local runtime boundary around
// the installed Codex binary: broker readiness, pinned checkout validation,
// MCP initialization, Responses streaming, and durable runtime-event POSTs.
// It is opt-in because it launches a real agent process, but it never calls a
// hosted provider and never uses a credential.
func TestRuntimeCodexLive(t *testing.T) {
	if os.Getenv("AGW_RUNTIME_CODEX_LIVE") != "1" {
		t.Skip("set AGW_RUNTIME_CODEX_LIVE=1 to exercise the complete local Codex runtime")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("codex is not installed")
	}
	workspace := initRuntimeLiveRepository(t)
	baseSHA := runtimeLiveRevision(t, workspace)
	fixture := &runtimeLiveFixture{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(fixture.handle))
	brokerURL := "http://" + server.Listener.Addr().String()

	values := map[string]string{
		"AGW_HARNESS":                 "codex",
		"AGW_BROKER":                  brokerURL,
		"AGW_CODEX_BIN":               binary,
		"AGW_CODEX_WORKSPACE":         workspace,
		"AGW_CODEX_MODEL":             "gpt-test",
		"AGW_CODEX_ENABLE_TOOLS":      "true",
		"AGW_CODEX_REQUIRE_ARTIFACT":  "false",
		"AGW_CODEX_MAX_RUNTIME":       "2m",
		"AGW_CODEX_TERMINATION_GRACE": "2s",
		"AGW_BASE_SHA":                baseSHA,
		"AGW_RUN_UID":                 "run-runtime-codex-live",
		"AGW_AGENT_REF":               "runtime-live-agent",
		"AGW_TASK":                    "Reply with exactly runtime-live-ok.",
		"AGW_INSTRUCTIONS":            "Do not use tools; reply with exactly runtime-live-ok.",
		"AGW_SPEC_DIGEST":             "sha256:" + strings.Repeat("a", 64),
	}

	var diagnostics bytes.Buffer
	completed := make(chan int, 1)
	go func() { completed <- execute(nilContext{}, mapGetter(values), &diagnostics) }()
	// The runtime must wait for a concurrently starting sidecar rather than
	// racing Codex into MCP initialization before the broker is listening.
	time.Sleep(250 * time.Millisecond)
	server.Start()
	defer server.Close()
	if code := <-completed; code != 0 {
		t.Fatalf("runtime exit code=%d diagnostics=%s methods=%v events=%s", code, diagnostics.String(), fixture.methodsSnapshot(), fixture.eventsSnapshot())
	}
	methods := fixture.methodsSnapshot()
	events := fixture.eventsSnapshot()
	if !containsString(methods, "initialize") || !containsString(methods, "tools/list") {
		t.Fatalf("Codex did not complete MCP initialization: methods=%v", methods)
	}
	if !bytes.Contains(events, []byte(`"type":"run.started"`)) || !bytes.Contains(events, []byte(`"type":"run.completed"`)) {
		t.Fatalf("runtime event stream did not contain start and completion: %s", events)
	}
	if bytes.Contains(events, []byte(`"type":"run.failed"`)) {
		t.Fatalf("runtime emitted failure despite successful exit: %s", events)
	}
}

// nilContext is a small context implementation for execute's existing test
// seam. The real binary supplies signal.NotifyContext in main().
type nilContext struct{}

func (nilContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (nilContext) Done() <-chan struct{}       { return nil }
func (nilContext) Err() error                  { return nil }
func (nilContext) Value(any) any               { return nil }

type runtimeLiveFixture struct {
	mu      sync.Mutex
	methods []string
	events  bytes.Buffer
}

func (f *runtimeLiveFixture) handle(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/mcp":
		f.handleMCP(writer, request)
	case "/v1/responses":
		f.handleResponses(writer, request)
	case "/v1/artifacts/output":
		f.handleArtifact(writer, request)
	case "/v1/runtime/events":
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil || request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		_, _ = f.events.Write(body)
		f.mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(writer, request)
	}
}

func (f *runtimeLiveFixture) handleArtifact(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		http.NotFound(writer, request)
		return
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(request.Body, 64<<10)); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(writer, `{"id":"runtime-live-output","uri":"artifact://local/runs/runtime-live-output","digest":"sha256:`+strings.Repeat("b", 64)+`","size_bytes":1,"media_type":"application/vnd.agw.run-output+json"}`)
}

func (f *runtimeLiveFixture) handleMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.NotFound(writer, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10))
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if json.Unmarshal(body, &message) != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.methods = append(f.methods, message.Method)
	f.mu.Unlock()
	if message.Method == "notifications/initialized" {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	result := map[string]any{}
	switch message.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]string{"name": "agw-runtime-live", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name":        "read_issue",
			"description": "read-only fixture tool",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}
	default:
		result = map[string]any{}
	}
	writeJSONRPC(writer, message.ID, result)
}

func (f *runtimeLiveFixture) handleResponses(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	flusher, _ := writer.(http.Flusher)
	for _, event := range runtimeLiveResponseEvents("gpt-test", "runtime-live-ok") {
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event.Type, event.Data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

type runtimeLiveEvent struct{ Type, Data string }

func runtimeLiveResponseEvents(model, text string) []runtimeLiveEvent {
	marshal := func(value any) string { raw, _ := json.Marshal(value); return string(raw) }
	responseID, itemID := "resp_runtime_live", "msg_runtime_live"
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": 1, "status": "completed", "model": model,
		"output":              []any{map[string]any{"id": itemID, "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}}}},
		"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	}
	created := map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": responseID, "object": "response", "created_at": 1, "status": "in_progress", "model": model, "output": []any{}}}
	itemAdded := map[string]any{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}
	return []runtimeLiveEvent{
		{"response.created", marshal(created)},
		{"response.output_item.added", marshal(itemAdded)},
		{"response.output_text.delta", marshal(map[string]any{"type": "response.output_text.delta", "sequence_number": 2, "item_id": itemID, "output_index": 0, "content_index": 0, "delta": text})},
		{"response.output_text.done", marshal(map[string]any{"type": "response.output_text.done", "sequence_number": 3, "item_id": itemID, "output_index": 0, "content_index": 0, "text": text})},
		{"response.output_item.done", marshal(map[string]any{"type": "response.output_item.done", "sequence_number": 4, "output_index": 0, "item": response["output"].([]any)[0]})},
		{"response.completed", marshal(map[string]any{"type": "response.completed", "sequence_number": 5, "response": response})},
	}
}

func (f *runtimeLiveFixture) methodsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func (f *runtimeLiveFixture) eventsSnapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.events.Bytes()...)
}

func writeJSONRPC(writer http.ResponseWriter, id json.RawMessage, result any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"result":`, id)
	encoded, _ := json.Marshal(result)
	_, _ = writer.Write(encoded)
	_, _ = writer.Write([]byte("}"))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func initRuntimeLiveRepository(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", directory, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("runtime live\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", directory, "add", "--", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	command := exec.Command("git", "-C", directory, "-c", "user.name=AGW Test", "-c", "user.email=agw-test@example.invalid", "commit", "--quiet", "-m", "runtime live")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	return directory
}

func runtimeLiveRevision(t *testing.T, directory string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", directory, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
