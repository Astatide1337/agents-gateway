package modelbroker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAdapterUsesMessagesAPIWithoutLeakingCredential(t *testing.T) {
	secret := "anthropic-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" || request.Header.Get("x-api-key") != secret || request.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("unexpected request path=%s headers=%v", request.URL.Path, request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["system"] != "keep it short" || body["max_tokens"] != float64(128) {
			t.Fatalf("unexpected request body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-test","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	defer server.Close()
	provider, err := NewAnthropic(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Invoke(context.Background(), Request{Model: "claude-test", MaxTokens: 128, Messages: []Message{{Role: "system", Content: "keep it short"}, {Role: "user", Content: "work"}}}, []byte(secret))
	if err != nil || response.Content != "done" || response.InputTokens != 10 || response.OutputTokens != 2 {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if strings.Contains(response.Content, secret) {
		t.Fatal("credential leaked into model response")
	}
}
