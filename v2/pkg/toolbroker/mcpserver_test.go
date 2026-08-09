package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

type countingMCPPolicy struct {
	mu     sync.Mutex
	calls  int
	grants []toolpolicy.Grant
}

func (p *countingMCPPolicy) Grants(context.Context, string, string, string) ([]toolpolicy.Grant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.grants, nil
}

func (p *countingMCPPolicy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type countingMCPServerSource struct {
	mu     sync.Mutex
	calls  int
	server Server
}

func (s *countingMCPServerSource) Server(context.Context, string, string, string) (Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.server, nil
}

func (s *countingMCPServerSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func mcpTestHandler(t *testing.T, policy PolicySource, servers ServerSource, effects EffectLedger, tools []ExposedTool) *MCPHandler {
	t.Helper()
	broker, err := New(policy, credentialSource("test-only-secret"), servers, effects, &memoryAudit{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewMCPHandler(broker, MCPHandlerConfig{
		OrganizationID: "org",
		ProjectID:      "project",
		UserID:         "user",
		RunID:          "run",
		Tools:          tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func testExposedTool(name string, effect toolpolicy.Effect) ExposedTool {
	return ExposedTool{
		Name:        name,
		Description: "bounded test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Server:      "github",
		Resource:    "github:repo:owner/repo",
		Effect:      effect,
	}
}

func mcpRequest(method string, id any, params string) *http.Request {
	payload := `{"jsonrpc":"2.0","method":` + jsonString(method)
	if id != nil {
		encoded, _ := json.Marshal(id)
		payload += `,"id":` + string(encoded)
	}
	if params != "" {
		payload += `,"params":` + params
	}
	payload += `}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	return request
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func decodeMCPResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, recorder.Body.String())
	}
	return response
}

func TestMCPHandlerFiltersCatalogAndRejectsUnknownBeforeBroker(t *testing.T) {
	policy := &countingMCPPolicy{grants: []toolpolicy.Grant{{Server: "github", Tool: "safe", Resources: []string{"github:repo:owner/repo"}, Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow}}}
	servers := &countingMCPServerSource{}
	handler := mcpTestHandler(t, policy, servers, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, mcpRequest("tools/list", 1, `{}`))
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", list.Code, list.Body.String())
	}
	response := decodeMCPResponse(t, list)
	result := response["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "safe" {
		t.Fatalf("unexpected filtered catalog: %#v", tools)
	}
	if _, exposedServer := tools[0].(map[string]any)["server"]; exposedServer {
		t.Fatal("internal server binding leaked through tools/list")
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, mcpRequest("tools/call", 2, `{"name":"not-exposed","arguments":{}}`))
	unknownResponse := decodeMCPResponse(t, unknown)
	unknownError := unknownResponse["error"].(map[string]any)
	if unknownError["code"] != float64(-32602) || policy.count() != 0 || servers.count() != 0 {
		t.Fatalf("unknown call crossed broker boundary: response=%#v policy=%d servers=%d", unknownResponse, policy.count(), servers.count())
	}
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, mcpRequest("tools/call", 3, `{"name":"denied","arguments":{}}`))
	if policy.count() != 0 || servers.count() != 0 || !strings.Contains(denied.Body.String(), "tool is not exposed") {
		t.Fatalf("denied catalog entry crossed broker boundary: body=%s policy=%d servers=%d", denied.Body.String(), policy.count(), servers.count())
	}
}

func TestMCPHandlerMapsPolicyDenialWithoutLeakingBrokerError(t *testing.T) {
	policy := &countingMCPPolicy{}
	servers := &countingMCPServerSource{}
	handler := mcpTestHandler(t, policy, servers, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, mcpRequest("tools/call", "deny", `{"name":"safe","arguments":{}}`))
	response := decodeMCPResponse(t, recorder)
	errObject := response["error"].(map[string]any)
	if errObject["code"] != float64(-32001) || errObject["message"] != "tool call denied by policy" {
		t.Fatalf("unexpected denial response: %#v", response)
	}
	if strings.Contains(recorder.Body.String(), "test-only-secret") || servers.count() != 0 {
		t.Fatalf("denial leaked a secret or reached upstream: %s", recorder.Body.String())
	}
}

func TestMCPHandlerBindsCatalogAndDerivesStableWriteEffectKey(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		id, _ := r.Context().Value(mcpTestUpstreamRequestID{}).(string)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, id)
	})))
	defer upstream.Close()

	effects := &memoryEffects{}
	policy := policySource{{Server: "github", Tool: "write", Resources: []string{"github:repo:owner/repo"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow}}
	servers := &countingMCPServerSource{server: Server{Name: "github", Endpoint: upstream.URL, CredentialRef: "cred"}}
	handler := mcpTestHandler(t, policy, servers, effects, []ExposedTool{testExposedTool("write", toolpolicy.EffectWrite)})

	call := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		// A standard MCP client supplies only the JSON-RPC request identity. The
		// handler must still derive a stable idempotency key across retries.
		handler.ServeHTTP(recorder, mcpRequest("tools/call", 10, `{"name":"write","arguments":{"title":"change"}}`))
		return recorder
	}
	first := call()
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"content"`) {
		t.Fatalf("first write failed: %s", first.Body.String())
	}
	second := call()
	secondResponse := decodeMCPResponse(t, second)
	if secondResponse["error"].(map[string]any)["code"] != float64(-32003) || upstreamCalls != 1 {
		t.Fatalf("write effect was not stable/idempotent: response=%#v upstream=%d effects=%v", secondResponse, upstreamCalls, effects.states)
	}
	if len(effects.claims) != 1 {
		t.Fatalf("expected one stable effect claim, got %v", effects.claims)
	}
}

func TestMCPHandlerRejectsStrictProtocolViolations(t *testing.T) {
	policy := &countingMCPPolicy{}
	handler := mcpTestHandler(t, policy, &countingMCPServerSource{}, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})
	tests := []struct {
		name    string
		request *http.Request
		code    int
	}{
		{"get", httptest.NewRequest(http.MethodGet, "/mcp", nil), http.StatusMethodNotAllowed},
		{"wrong-path", httptest.NewRequest(http.MethodPost, "/not-mcp", nil), http.StatusNotFound},
		{"content-type", func() *http.Request {
			r := mcpRequest("ping", 1, `{}`)
			r.Header.Set("Content-Type", "text/plain")
			return r
		}(), http.StatusUnsupportedMediaType},
		{"accept", func() *http.Request {
			r := mcpRequest("ping", 1, `{}`)
			r.Header.Set("Accept", "text/plain")
			return r
		}(), http.StatusNotAcceptable},
		{"unknown-field", mcpRequestWithBody(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{},"extra":true}`), http.StatusOK},
		{"duplicate-field", mcpRequestWithBody(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{},"params":{}}`), http.StatusOK},
		{"batch", mcpRequestWithBody(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`), http.StatusOK},
		{"unsupported-method", mcpRequest("resources/list", 1, `{}`), http.StatusOK},
		{"sandbox-binding-field", mcpRequest("tools/call", 1, `{"name":"safe","arguments":{},"server":"attacker-controlled"}`), http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, test.request)
			if recorder.Code != test.code {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if test.name == "unsupported-method" && !strings.Contains(recorder.Body.String(), `"code":-32601`) {
				t.Fatalf("unsupported method was not failed closed: %s", recorder.Body.String())
			}
		})
	}
}

func TestMCPHandlerInitializeIsTypedAndReturnsMCPMetadata(t *testing.T) {
	handler := mcpTestHandler(t, policySource{}, &countingMCPServerSource{}, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, mcpRequest("initialize", "init", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex","version":"1"}}`))
	if recorder.Code != http.StatusOK || recorder.Header().Get("MCP-Protocol-Version") != MCPProtocolVersion {
		t.Fatalf("initialize failed: status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	response := decodeMCPResponse(t, recorder)
	result := response["result"].(map[string]any)
	if result["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("unexpected negotiated version: %#v", result)
	}
	if _, ok := result["serverInfo"].(map[string]any); !ok {
		t.Fatalf("serverInfo missing: %#v", result)
	}
}

func mcpRequestWithBody(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	return request
}

func TestMCPHandlerValidatesInitializedNotificationAndAcceptsJSONSSEHeader(t *testing.T) {
	handler := mcpTestHandler(t, policySource{}, &countingMCPServerSource{}, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})
	request := mcpRequest("notifications/initialized", nil, `{"unexpected":true}`)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"code":-32602`) {
		t.Fatalf("invalid notification was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = mcpRequest("ping", 1, `{}`)
	request.Header.Set("Accept", "text/event-stream")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("SSE-compatible request did not receive MCP JSON: status=%d content-type=%q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}

func TestMCPHandlerContextCancellationIsNonSecret(t *testing.T) {
	policy := blockingMCPPolicy{}
	handler := mcpTestHandler(t, policy, &countingMCPServerSource{}, &memoryEffects{}, []ExposedTool{testExposedTool("safe", toolpolicy.EffectRead)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := mcpRequest("tools/call", 1, `{"name":"safe","arguments":{}}`).WithContext(ctx)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), `"message":"tool call failed"`) && !strings.Contains(recorder.Body.String(), `"message":"request canceled"`) {
		t.Fatalf("cancellation exposed an unexpected error: %s", recorder.Body.String())
	}
}

type blockingMCPPolicy struct{}

func (blockingMCPPolicy) Grants(ctx context.Context, _, _, _ string) ([]toolpolicy.Grant, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
