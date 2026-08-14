package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/pkg/codexadapter"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

// TestCodexOpenRouterMCPToolCallLive runs the installed Codex CLI through the
// real broker HTTP handlers. It is opt-in because it launches a real agent
// process, but it uses only a deterministic local provider/MCP fixture and no
// API credential or hosted CI runner.
func TestCodexOpenRouterMCPToolCallLive(t *testing.T) {
	if os.Getenv("AGW_CODEX_MCP_LIVE") != "1" {
		t.Skip("set AGW_CODEX_MCP_LIVE=1 to exercise Codex, broker translation, and MCP dispatch")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	transport := &testTransport{}
	transport.modelResponder = func(call int, body []byte) (string, string) {
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			return "application/json", `{"error":"invalid request fixture"}`
		}
		if call == 1 {
			flatName := flattenedProviderToolName(request["tools"], "read_issue")
			if flatName == "" {
				return "application/json", `{"error":"read_issue was not flattened for the provider"}`
			}
			return "text/event-stream", codexFunctionCallSSE(flatName, `{"issue":"427"}`)
		}
		return "text/event-stream", codexMessageSSE("The MCP tool call completed successfully.")
	}

	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.ModelRoute.Providers[0].Kind = "openrouter-responses"
	config.ModelRoute.Budget.MaxCostUSD = "100.00"
	config.ModelRoute.Budget.MaxTokens = 1_000_000
	config.MaxCostUSD = "100.00"
	config.MaxModelTokens = 1_000_000
	config.ProviderEndpoints["openai"] = "https://api.example.test/api/v1/responses"
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := b.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	status, listBody, listErr := performMCP(mcp, "tools/list", map[string]any{})
	if listErr != nil || status != http.StatusOK || !bytes.Contains(listBody, []byte(`"read_issue"`)) {
		t.Fatalf("local MCP tools/list is not exposing read_issue: status=%d error=%v body=%s", status, listErr, listBody)
	}
	var localToolCalls atomic.Int64
	var localMethodMu sync.Mutex
	var localMethods []string
	var localCallResponseMu sync.Mutex
	var localCallRequest, localCallResponse []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			if r.Method == http.MethodPost {
				body, readErr := io.ReadAll(r.Body)
				if readErr == nil {
					var request struct {
						Method string `json:"method"`
					}
					if json.Unmarshal(body, &request) == nil {
						localMethodMu.Lock()
						localMethods = append(localMethods, request.Method)
						localMethodMu.Unlock()
						if request.Method == "tools/call" {
							localToolCalls.Add(1)
							record := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(body))
							record.RemoteAddr = r.RemoteAddr
							record.Header = r.Header.Clone()
							response := httptest.NewRecorder()
							mcp.ServeHTTP(response, record)
							for key, values := range response.Header() {
								w.Header()[key] = append([]string(nil), values...)
							}
							w.WriteHeader(response.Code)
							_, _ = w.Write(response.Body.Bytes())
							localCallResponseMu.Lock()
							localCallRequest = append([]byte(nil), body...)
							localCallResponse = append([]byte(nil), response.Body.Bytes()...)
							localCallResponseMu.Unlock()
							return
						}
					}
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			mcp.ServeHTTP(w, r)
		case "/v1/responses":
			b.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	cfg := codexadapter.Config{
		CodexBinary:      binary,
		ResponsesURL:     server.URL + "/v1",
		MCPURL:           server.URL + "/mcp",
		Workspace:        workspace,
		Model:            "test-model",
		EnableTools:      true,
		MaxRuntime:       3 * time.Minute,
		TerminationGrace: 2 * time.Second,
	}
	var output bytes.Buffer
	if err := codexadapter.Run(context.Background(), strings.NewReader(codexLiveRunStart(t)), &output, io.Discard, cfg); err != nil {
		t.Fatalf("Codex through OpenRouter compatibility path: %v; output=%s", err, output.String())
	}
	transport.mu.Lock()
	modelCalls, mcpCalls := transport.modelCalls, transport.mcpCalls
	firstBody := append([]byte(nil), transport.bodies[0]...)
	transport.mu.Unlock()
	localMethodMu.Lock()
	methods := append([]string(nil), localMethods...)
	localMethodMu.Unlock()
	localCallResponseMu.Lock()
	callRequest := append([]byte(nil), localCallRequest...)
	callResponse := append([]byte(nil), localCallResponse...)
	localCallResponseMu.Unlock()
	var firstRequest map[string]any
	_ = json.Unmarshal(firstBody, &firstRequest)
	if modelCalls < 2 || mcpCalls == 0 || localToolCalls.Load() == 0 || !bytes.Contains(output.Bytes(), []byte("MCP tool call completed successfully")) {
		t.Fatalf("Codex did not complete the tool loop: modelCalls=%d upstreamMCPCalls=%d localToolCalls=%d localMethods=%v localCallRequest=%s localCallResponse=%s toolCatalog=%s output=%s", modelCalls, mcpCalls, localToolCalls.Load(), methods, callRequest, callResponse, summarizeResponseTools(firstRequest["tools"]), output.String())
	}
}

func summarizeResponseTools(value any) string {
	var result []string
	items, ok := value.([]any)
	if !ok {
		return "<missing>"
	}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] == "namespace" {
			namespace, _ := tool["name"].(string)
			nestedTools, _ := tool["tools"].([]any)
			for _, nestedValue := range nestedTools {
				nested, _ := nestedValue.(map[string]any)
				name, _ := nested["name"].(string)
				result = append(result, namespace+":"+name)
			}
			continue
		}
		name, _ := tool["name"].(string)
		result = append(result, name)
	}
	return strings.Join(result, ",")
}

func codexLiveRunStart(t *testing.T) string {
	t.Helper()
	runID := "run-codex-openrouter-live"
	data, err := json.Marshal(map[string]any{
		"organization_id": "org",
		"project_id":      "project",
		"run_id":          runID,
		"workflow_name":   "workflow",
		"step_id":         "step",
		"agent_ref":       "agent-ref",
		"execution": map[string]any{
			"agent":            map[string]any{"kind": "agent", "name": "codex-agent", "digest": "sha256:" + strings.Repeat("a", 64)},
			"sandbox_profile":  map[string]any{"kind": "sandbox", "name": "coding", "digest": "sha256:" + strings.Repeat("b", 64)},
			"instructions_ref": "instructions://" + runID,
			"instructions":     "Use the allowed MCP tool to inspect issue 427, then report the result.",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: runID, Seq: 1, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func codexFunctionCallSSE(name, arguments string) string {
	item := map[string]any{"id": "fc-1", "type": "function_call", "status": "completed", "name": name, "call_id": "call-1", "arguments": arguments}
	addedItem := map[string]any{"id": "fc-1", "type": "function_call", "status": "in_progress", "name": name, "call_id": "call-1", "arguments": ""}
	response := map[string]any{"id": "resp-1", "object": "response", "created_at": 1, "status": "completed", "model": "test-model", "output": []any{item}, "usage": codexLiveUsage()}
	return sseEvent("response.created", map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp-1", "object": "response", "created_at": 1, "status": "in_progress", "model": "test-model", "output": []any{}}}) +
		sseEvent("response.in_progress", map[string]any{"type": "response.in_progress", "sequence_number": 1, "response": map[string]any{"id": "resp-1", "status": "in_progress"}}) +
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "sequence_number": 2, "output_index": 0, "item": addedItem}) +
		sseEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": 3, "item_id": "fc-1", "output_index": 0, "delta": arguments}) +
		sseEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "sequence_number": 4, "item_id": "fc-1", "output_index": 0, "arguments": arguments}) +
		sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "sequence_number": 5, "output_index": 0, "item": item}) +
		sseEvent("response.completed", map[string]any{"type": "response.completed", "sequence_number": 6, "response": response}) +
		"data: [DONE]\n\n"
}

func flattenedProviderToolName(value any, target string) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok || tool["type"] != "function" {
			continue
		}
		name, _ := tool["name"].(string)
		if strings.HasSuffix(name, "__"+target) {
			return name
		}
	}
	return ""
}

func codexMessageSSE(text string) string {
	item := map[string]any{"id": "msg-1", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}}}
	response := map[string]any{"id": "resp-2", "object": "response", "created_at": 1, "status": "completed", "model": "test-model", "output": []any{item}, "usage": codexLiveUsage()}
	return sseEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-2", "object": "response", "created_at": 1, "status": "in_progress", "model": "test-model", "output": []any{}}}) +
		sseEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "msg-1", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}}) +
		sseEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "delta": text}) +
		sseEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item}) +
		sseEvent("response.completed", map[string]any{"type": "response.completed", "response": response}) +
		"data: [DONE]\n\n"
}

func codexLiveUsage() map[string]any {
	return map[string]any{"input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 2}
}

func sseEvent(event string, value any) string {
	raw, _ := json.Marshal(value)
	return "event: " + event + "\ndata: " + string(raw) + "\n\n"
}
