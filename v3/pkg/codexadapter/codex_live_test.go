package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
)

// TestCodexCLILiveResponses exercises the installed, current Codex CLI over
// the exact Responses wire without any provider credential or public network.
// It is opt-in because normal CI runners need not have Codex installed.
func TestCodexCLILiveResponses(t *testing.T) {
	if os.Getenv("AGW_CODEX_LIVE") != "1" {
		t.Skip("set AGW_CODEX_LIVE=1 to exercise the installed Codex CLI")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("codex is not installed")
	}
	var receivedModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Codex request: %v", err)
		}
		receivedModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher := w.(http.Flusher)
		for _, event := range fakeResponseEvents("gpt-test", "live-codex-ok") {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data)
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	cfg := Config{CodexBinary: binary, ResponsesURL: upstream.URL + "/v1", Workspace: t.TempDir(), Model: "gpt-test", MaxRuntime: 2 * time.Minute, TerminationGrace: 2 * time.Second}
	var output bytes.Buffer
	var diagnostics bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(runStartLine(t, "run-live-codex", "Reply with exactly live-codex-ok and do not use tools.")), &output, &diagnostics, cfg); err != nil {
		t.Fatalf("live Codex adapter: %v; diagnostics=%s; output=%s", err, diagnostics.String(), output.String())
	}
	frames := decodeEvents(t, output.Bytes())
	if receivedModel != "gpt-test" || frames[len(frames)-1].Type != proto.EventRunCompleted || !bytes.Contains(output.Bytes(), []byte("live-codex-ok")) {
		t.Fatalf("live Responses flow incomplete: model=%q frames=%#v output=%s", receivedModel, frames, output.String())
	}
}

type fakeSSEEvent struct{ Type, Data string }

func fakeResponseEvents(model, text string) []fakeSSEEvent {
	responseID := "resp_agw_live"
	itemID := "msg_agw_live"
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": 1, "status": "completed",
		"model": model, "output": []any{map[string]any{
			"id": itemID, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}},
		}},
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"usage":               map[string]any{"input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 2},
	}
	marshal := func(value any) string { raw, _ := json.Marshal(value); return string(raw) }
	created := map[string]any{
		"type": "response.created", "sequence_number": 0,
		"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 1,
			"status": "in_progress", "model": model, "output": []any{},
			"parallel_tool_calls": true, "tool_choice": "auto", "tools": []any{},
		},
	}
	itemAdded := map[string]any{
		"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
		"item": map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	}
	return []fakeSSEEvent{
		{"response.created", marshal(created)},
		{"response.output_item.added", marshal(itemAdded)},
		{"response.content_part.added", marshal(map[string]any{"type": "response.content_part.added", "sequence_number": 2, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})},
		{"response.output_text.delta", marshal(map[string]any{"type": "response.output_text.delta", "sequence_number": 3, "item_id": itemID, "output_index": 0, "content_index": 0, "delta": text, "logprobs": []any{}})},
		{"response.output_text.done", marshal(map[string]any{"type": "response.output_text.done", "sequence_number": 4, "item_id": itemID, "output_index": 0, "content_index": 0, "text": text, "logprobs": []any{}})},
		{"response.content_part.done", marshal(map[string]any{"type": "response.content_part.done", "sequence_number": 5, "item_id": itemID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}})},
		{"response.output_item.done", marshal(map[string]any{"type": "response.output_item.done", "sequence_number": 6, "output_index": 0, "item": response["output"].([]any)[0]})},
		{"response.completed", marshal(map[string]any{"type": "response.completed", "sequence_number": 7, "response": response})},
	}
}
