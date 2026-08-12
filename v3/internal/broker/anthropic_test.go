package broker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicMessagesProxyValidatesWireAuthUsageAndStreaming(t *testing.T) {
	transport := &testTransport{modelContent: "text/event-stream", modelBody: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\ndata: [DONE]\n\n"}
	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.ModelRoute.Providers[0].Kind = "openrouter-anthropic-messages"
	config.ProviderEndpoints["openai"] = "https://api.example.test/api/v1/messages"
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"test-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	result, err := b.InvokeAnthropicModel(context.Background(), body)
	if err != nil {
		t.Fatalf("InvokeAnthropicModel: %v", err)
	}
	if result.ContentType != "text/event-stream" || result.InputTokens != 4 || result.OutputTokens != 6 {
		t.Fatalf("result=%#v", result)
	}
	transport.mu.Lock()
	request := transport.requests[len(transport.requests)-1]
	transport.mu.Unlock()
	if request.Header.Get("Authorization") != "Bearer model-token" || request.Header.Get("x-api-key") != "" {
		t.Fatalf("upstream auth headers=%v", request.Header)
	}

	for _, invalid := range []string{
		`{"model":"test-model","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"unexpected":true}`,
		`{"model":"test-model","max_tokens":0,"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"test-model","max_tokens":32,"messages":[{"role":"system","content":"hello"}]}`,
	} {
		if _, err := b.InvokeAnthropicModel(context.Background(), []byte(invalid)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("invalid request %s returned %v", invalid, err)
		}
	}
}

func TestAnthropicHTTPRouteMatchesProviderKindAndNeverTrustsIncomingAuth(t *testing.T) {
	transport := &testTransport{}
	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.ModelRoute.Providers[0].Kind = "anthropic-messages"
	config.ProviderEndpoints["openai"] = "https://api.example.test/v1/messages"
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"test-model","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/messages", strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:4000"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "attacker-token")
	response := httptest.NewRecorder()
	b.ServeAnthropicHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	transport.mu.Lock()
	upstream := transport.requests[len(transport.requests)-1]
	transport.mu.Unlock()
	if upstream.Header.Get("x-api-key") != "model-token" || upstream.Header.Get("Authorization") != "" || upstream.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("upstream headers=%v", upstream.Header)
	}

	wrongPath := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/messages", strings.NewReader(body))
	wrongPath.RemoteAddr = "127.0.0.1:4000"
	wrongPath.Header.Set("Content-Type", "application/json")
	wrongResponse := httptest.NewRecorder()
	b.ServeAnthropicHTTP(wrongResponse, wrongPath)
	if wrongResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong path status=%d", wrongResponse.Code)
	}
}
