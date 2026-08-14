package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOpenRouterResponsesFlattensNamespaceToolsAndRestoresCalls(t *testing.T) {
	transport := &testTransport{modelBody: `{"id":"resp-1","object":"response","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"function_call","name":"mcp__github__read_issue","call_id":"call-1","arguments":"{\"issue\":\"427\"}"}]}`}
	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.ModelRoute.Providers[0].Kind = "openrouter-responses"
	config.ProviderEndpoints["openai"] = "https://api.example.test/api/v1/responses"
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"test-model","input":"use the issue tool","tools":[{"type":"namespace","name":"mcp__github","tools":[{"type":"function","name":"read_issue","description":"Read an issue","parameters":{"type":"object","properties":{"issue":{"type":"string"}},"required":["issue"]}}]}]}`)
	result, err := b.InvokeModel(context.Background(), body)
	if err != nil {
		t.Fatalf("InvokeModel: %v", err)
	}
	transport.mu.Lock()
	if len(transport.bodies) != 1 {
		transport.mu.Unlock()
		t.Fatalf("upstream bodies=%d", len(transport.bodies))
	}
	upstreamBody := append([]byte(nil), transport.bodies[0]...)
	transport.mu.Unlock()
	var upstream map[string]any
	if err := json.Unmarshal(upstreamBody, &upstream); err != nil {
		t.Fatal(err)
	}
	tools, ok := upstream["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("upstream tools=%#v", upstream["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "mcp__github__read_issue" || tool["namespace"] != nil {
		t.Fatalf("flattened tool=%#v", tool)
	}
	var restored map[string]any
	if err := json.Unmarshal(result.Body, &restored); err != nil {
		t.Fatal(err)
	}
	output := restored["output"].([]any)
	call := output[0].(map[string]any)
	if call["name"] != "read_issue" || call["namespace"] != "mcp__github" {
		t.Fatalf("restored call=%#v", call)
	}
}

func TestOpenRouterResponsesFlattensPriorNamespaceFunctionCallInput(t *testing.T) {
	transport := &testTransport{}
	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.ModelRoute.Providers[0].Kind = "openrouter-responses"
	config.ProviderEndpoints["openai"] = "https://api.example.test/api/v1/responses"
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"test-model","input":[{"type":"function_call","namespace":"mcp__github","name":"read_issue","call_id":"call-1","arguments":"{}"}],"tools":[{"type":"namespace","name":"mcp__github","tools":[{"type":"function","name":"read_issue","parameters":{"type":"object"}}]}]}`)
	if _, err := b.InvokeModel(context.Background(), body); err != nil {
		t.Fatalf("InvokeModel: %v", err)
	}
	transport.mu.Lock()
	upstreamBody := append([]byte(nil), transport.bodies[0]...)
	transport.mu.Unlock()
	if bytes.Contains(upstreamBody, []byte(`"namespace"`)) || !bytes.Contains(upstreamBody, []byte(`"name":"mcp__github__read_issue"`)) {
		t.Fatalf("upstream body did not flatten prior call: %s", upstreamBody)
	}
}

func TestOpenRouterResponsesRestoresStreamingFunctionCalls(t *testing.T) {
	tools := flattenedTools{byFlatName: map[string]flattenedTool{
		"mcp__github__read_issue": {namespace: "mcp__github", name: "read_issue"},
	}}
	body := []byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"mcp__github__read_issue\"}}\n\ndata: [DONE]\n\n")
	converted, err := restoreOpenRouterResponses(body, "text/event-stream", tools)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"namespace":"mcp__github"`) || !strings.Contains(string(converted), `"name":"read_issue"`) || !strings.Contains(string(converted), "data: [DONE]") {
		t.Fatalf("converted SSE=%s", converted)
	}
}

func TestOpenRouterResponsesRejectsAmbiguousOrUnknownNamespaceCalls(t *testing.T) {
	_, _, err := flattenOpenRouterResponsesRequest([]byte(`{"model":"test-model","tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"same"}]},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"same"}]}]}`))
	if err != nil {
		t.Fatalf("identical namespace tool should be idempotent: %v", err)
	}
	_, _, err = flattenOpenRouterResponsesRequest([]byte(`{"model":"test-model","input":[{"type":"function_call","namespace":"mcp","name":"not-allowed"}],"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"allowed"}]}]}`))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown namespace call error=%v", err)
	}
}

func TestCanonicalProviderFunctionNameBoundsLongNames(t *testing.T) {
	name := canonicalProviderFunctionName(strings.Repeat("server.", 20), strings.Repeat("tool-", 80))
	if len(name) > maxProviderFunctionNameBytes || !providerFunctionNamePattern.MatchString(name) {
		t.Fatalf("provider function name=%q length=%d", name, len(name))
	}
}
