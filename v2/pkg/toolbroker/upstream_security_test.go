package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

func TestBrokerRejectsRedirectsWithCallerClientWithoutLeakingCredential(t *testing.T) {
	var leaked atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectRead, &memoryEffects{}, &http.Client{})
	_, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, ""))
	if err == nil || leaked.Load() {
		t.Fatalf("redirect was followed or credential leaked: error=%v leaked=%v", err, leaked.Load())
	}
	if strings.Contains(err.Error(), "redirect-secret") {
		t.Fatalf("credential appeared in redirect error: %v", err)
	}
}

func TestBrokerRejectsEndpointQueryBeforeCredentialResolution(t *testing.T) {
	var credentialCalls atomic.Int32
	credentials := countingCredentialSource{value: "redirect-secret", calls: &credentialCalls}
	policy := policySource{{Server: "upstream", Tool: "read", Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow}}
	broker, err := New(policy, credentials, serverSource(Server{Name: "upstream", Endpoint: "https://example.test/mcp?token=secret", CredentialRef: "credential"}), &memoryEffects{}, &memoryAudit{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, ""))
	if err == nil || credentialCalls.Load() != 0 {
		t.Fatalf("endpoint query was not rejected before credential resolution: error=%v credentialCalls=%d", err, credentialCalls.Load())
	}
}

func TestBrokerAcceptsBoundedMultilineSSEAndIgnoresNotification(t *testing.T) {
	var toolCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		request := upstreamJSONRPCRequest(t, r)
		switch request.method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sse-session")
			writeUpstreamJSON(w, request.id, map[string]any{"protocolVersion": MCPProtocolVersion, "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "sse", "version": "1"}}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			toolCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\ndata: \"id\": %s,\ndata: \"result\": {\"content\": [{\"type\": \"text\", \"text\": \"sse-ok\"}]}}\n\n", request.id)
		case "":
			w.WriteHeader(http.StatusBadRequest)
		default:
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectRead, &memoryEffects{}, nil)
	result, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, ""))
	if err != nil || !strings.Contains(string(result.Content), "sse-ok") || toolCalls.Load() != 1 {
		t.Fatalf("multiline SSE read failed: result=%s error=%v calls=%d", result.Content, err, toolCalls.Load())
	}
}

func TestBrokerRejectsMismatchedResponseIDAsUnknownWrite(t *testing.T) {
	effects := &memoryEffects{}
	upstream := newUpstreamResponseServer(t, func(w http.ResponseWriter, request upstreamRPCRequest) {
		if request.method == "tools/call" {
			w.Header().Set("Content-Type", "application/json")
			writeUpstreamJSON(w, json.RawMessage(`"wrong-id"`), map[string]any{"content": []any{}}, nil)
		}
	})
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectWrite, effects, nil)
	_, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectWrite, "write-id"))
	if !errors.Is(err, ErrOutcomeUnknown) || effects.states["run/write-id"] != "unknown" {
		t.Fatalf("mismatched response was not classified as unknown: error=%v states=%v", err, effects.states)
	}
}

func TestBrokerPreservesReadIsErrorAndMarksWriteIsErrorUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		effect toolpolicy.Effect
	}{
		{name: "read", effect: toolpolicy.EffectRead},
		{name: "write", effect: toolpolicy.EffectWrite},
	} {
		t.Run(test.name, func(t *testing.T) {
			effects := &memoryEffects{}
			upstream := newUpstreamResponseServer(t, func(w http.ResponseWriter, request upstreamRPCRequest) {
				if request.method == "tools/call" {
					w.Header().Set("Content-Type", "application/json")
					writeUpstreamJSON(w, request.id, map[string]any{"content": []any{map[string]any{"type": "text", "text": "tool failed"}}, "isError": true}, nil)
				}
			})
			defer upstream.Close()

			broker := newUpstreamTestBroker(t, upstream.URL, test.effect, effects, nil)
			result, err := broker.Call(context.Background(), upstreamTestRequest(test.effect, "write-id"))
			if test.effect == toolpolicy.EffectRead {
				if err != nil || !strings.Contains(string(result.Content), `"isError":true`) {
					t.Fatalf("read isError was not preserved: result=%s error=%v", result.Content, err)
				}
				return
			}
			if !errors.Is(err, ErrOutcomeUnknown) || effects.states["run/write-id"] != "unknown" {
				t.Fatalf("write isError was not classified as unknown: error=%v states=%v", err, effects.states)
			}
		})
	}
}

func TestBrokerRenewsExpiredReadSessionButNeverReplaysWrite(t *testing.T) {
	var initializes atomic.Int32
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		request := upstreamJSONRPCRequest(t, r)
		switch request.method {
		case "initialize":
			n := initializes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", n))
			writeUpstreamJSON(w, request.id, map[string]any{"protocolVersion": MCPProtocolVersion}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			writeUpstreamJSON(w, request.id, map[string]any{"content": []any{}}, nil)
		}
	}))
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectRead, &memoryEffects{}, nil)
	if _, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, "")); err != nil {
		t.Fatal(err)
	}
	if initializes.Load() != 2 || calls.Load() != 2 {
		t.Fatalf("expired read session was not renewed exactly once: initializes=%d calls=%d", initializes.Load(), calls.Load())
	}
}

func TestBrokerDoesNotFollowTeardownRedirect(t *testing.T) {
	var leaked atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
			return
		}
		request := upstreamJSONRPCRequest(t, r)
		switch request.method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "teardown-session")
			writeUpstreamJSON(w, request.id, map[string]any{"protocolVersion": MCPProtocolVersion}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			writeUpstreamJSON(w, request.id, map[string]any{"content": []any{}}, nil)
		}
	}))
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectRead, &memoryEffects{}, &http.Client{})
	if _, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, "")); err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Fatal("teardown redirect received the upstream credential")
	}
}

func TestBrokerSupportsConcurrentIndependentSessions(t *testing.T) {
	var initializes atomic.Int32
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		request := upstreamJSONRPCRequest(t, r)
		switch request.method {
		case "initialize":
			n := initializes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("concurrent-%d", n))
			writeUpstreamJSON(w, request.id, map[string]any{"protocolVersion": MCPProtocolVersion}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			writeUpstreamJSON(w, request.id, map[string]any{"content": []any{}}, nil)
		}
	}))
	defer upstream.Close()

	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectRead, &memoryEffects{}, nil)
	const concurrentCalls = 24
	var wait sync.WaitGroup
	errorsSeen := make(chan error, concurrentCalls)
	for range concurrentCalls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectRead, ""))
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent read failed: %v", err)
		}
	}
	if initializes.Load() != concurrentCalls || calls.Load() != concurrentCalls {
		t.Fatalf("concurrent sessions crossed or dropped requests: initializes=%d calls=%d", initializes.Load(), calls.Load())
	}
}

func TestUpstreamParsingAndEndpointBounds(t *testing.T) {
	if _, err := readMCPBody(strings.NewReader(strings.Repeat("x", maxMCPResponse+1))); err == nil {
		t.Fatal("oversized upstream body was accepted")
	}
	for _, value := range []string{"https://example.test/mcp?secret=value", "https://user:pass@example.test/mcp", "https://example.test/mcp#fragment", "http://192.0.2.1/mcp"} {
		if _, err := validateEndpoint(value); err == nil {
			t.Fatalf("unsafe endpoint was accepted: %s", value)
		}
	}
	for _, value := range []string{"session\n", " session", strings.Repeat("a", maxMCPSessionHeader+1)} {
		if err := validateSessionHeader(value); err == nil {
			t.Fatalf("unsafe session header was accepted: %q", value)
		}
	}
}

type countingCredentialSource struct {
	value string
	calls *atomic.Int32
}

func (c countingCredentialSource) ResolveCredential(context.Context, string, string) ([]byte, error) {
	c.calls.Add(1)
	return []byte(c.value), nil
}

func newUpstreamTestBroker(t *testing.T, endpoint string, effect toolpolicy.Effect, effects EffectLedger, client *http.Client) *Broker {
	t.Helper()
	grant := toolpolicy.Grant{Server: "upstream", Tool: "read", Resources: []string{"resource"}, Effect: effect, Approval: toolpolicy.ApprovalAllow}
	if effect != toolpolicy.EffectRead {
		grant.Tool = "write"
	}
	broker, err := New(policySource{grant}, credentialSource("redirect-secret"), serverSource(Server{Name: "upstream", Endpoint: endpoint, CredentialRef: "credential"}), effects, &memoryAudit{}, client)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func upstreamTestRequest(effect toolpolicy.Effect, effectKey string) Request {
	tool := "read"
	if effect != toolpolicy.EffectRead {
		tool = "write"
	}
	return Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "upstream", Tool: tool, Resource: "resource", EffectKey: effectKey, Arguments: json.RawMessage(`{}`), Approved: effect == toolpolicy.EffectRead || effect == toolpolicy.EffectWrite}
}

type upstreamRPCRequest struct {
	method string
	id     json.RawMessage
}

func upstreamJSONRPCRequest(t *testing.T, request *http.Request) upstreamRPCRequest {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return upstreamRPCRequest{method: envelope.Method, id: envelope.ID}
}

func writeUpstreamJSON(w http.ResponseWriter, id json.RawMessage, result any, rpcError *mcpRPCError) {
	response := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcError != nil {
		response["error"] = rpcError
	} else {
		response["result"] = result
	}
	_ = json.NewEncoder(w).Encode(response)
}

func newUpstreamResponseServer(t *testing.T, toolResponse func(http.ResponseWriter, upstreamRPCRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		request := upstreamJSONRPCRequest(t, r)
		switch request.method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "test-session")
			writeUpstreamJSON(w, request.id, map[string]any{"protocolVersion": MCPProtocolVersion}, nil)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			toolResponse(w, request)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
}

type failingCompleteEffects struct{}

func (failingCompleteEffects) Claim(context.Context, string, string, string, string, string) (bool, error) {
	return true, nil
}

func (failingCompleteEffects) Complete(context.Context, string, string, string, string, string, []byte) error {
	return errors.New("effect store unavailable")
}

func TestBrokerMarksSuccessfulUpstreamWriteUnknownWhenLedgerPersistenceFails(t *testing.T) {
	upstream := newUpstreamResponseServer(t, func(w http.ResponseWriter, request upstreamRPCRequest) {
		w.Header().Set("Content-Type", "application/json")
		writeUpstreamJSON(w, request.id, map[string]any{"content": []any{}}, nil)
	})
	defer upstream.Close()
	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectWrite, failingCompleteEffects{}, nil)
	_, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectWrite, "write-id"))
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("ledger persistence failure was retryable/ambiguous incorrectly classified: %v", err)
	}
}

func TestBrokerRejectsInvalidIsErrorTypeAsUnknownWrite(t *testing.T) {
	upstream := newUpstreamResponseServer(t, func(w http.ResponseWriter, request upstreamRPCRequest) {
		w.Header().Set("Content-Type", "application/json")
		writeUpstreamJSON(w, request.id, map[string]any{"content": []any{}, "isError": nil}, nil)
	})
	defer upstream.Close()
	effects := &memoryEffects{}
	broker := newUpstreamTestBroker(t, upstream.URL, toolpolicy.EffectWrite, effects, nil)
	_, err := broker.Call(context.Background(), upstreamTestRequest(toolpolicy.EffectWrite, "write-id"))
	if !errors.Is(err, ErrOutcomeUnknown) || effects.states["run/write-id"] != "unknown" {
		t.Fatalf("invalid isError was not fail-closed: error=%v states=%v", err, effects.states)
	}
}
