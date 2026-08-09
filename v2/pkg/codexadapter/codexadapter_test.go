package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerbridge"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v2/proto"
)

func TestConfigRejectsNonLoopbackAndAuthBearingEndpoints(t *testing.T) {
	base := map[string]string{
		"AGW_CODEX_RESPONSES_URL": "http://127.0.0.1:8787/v1",
		"AGW_CODEX_WORKSPACE":     t.TempDir(),
	}
	for name, endpoint := range map[string]string{
		"public":      "https://api.openai.com/v1",
		"credentials": "http://user:pass@127.0.0.1:8787/v1",
		"query":       "http://127.0.0.1:8787/v1?token=secret",
	} {
		t.Run(name, func(t *testing.T) {
			values := cloneEnv(base)
			values["AGW_CODEX_RESPONSES_URL"] = endpoint
			if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
				t.Fatal("expected endpoint validation error")
			}
		})
	}
}

func TestConfigRequiresExactModelAndResponsesBasePath(t *testing.T) {
	workspace := t.TempDir()
	values := map[string]string{"AGW_CODEX_RESPONSES_URL": "http://127.0.0.1:8787/v1", "AGW_CODEX_WORKSPACE": workspace}
	if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
		t.Fatal("missing model was accepted")
	}
	values["AGW_CODEX_MODEL"] = "gpt-test"
	values["AGW_CODEX_RESPONSES_URL"] = "http://127.0.0.1:8787/v1/responses"
	if _, err := ConfigFromEnv(func(key string) string { return values[key] }); err == nil {
		t.Fatal("Responses endpoint was accepted where a /v1 base URL is required")
	}
}

func TestBuildArgsUsesBoundedCodexInvocation(t *testing.T) {
	cfg := Config{ResponsesURL: "http://127.0.0.1:8787/v1", Workspace: t.TempDir(), Model: "gpt-5.6-codex"}
	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "workspace-write", "--config", `model_provider="agw_loopback"`}
	for _, value := range want {
		if !contains(args, value) {
			t.Fatalf("args missing %q: %#v", value, args)
		}
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "base_url=\"http://127.0.0.1:8787/v1\"") || !strings.Contains(joined, "wire_api=\"responses\"") {
		t.Fatalf("loopback provider config missing: %#v", args)
	}
	if contains(args, "--dangerously-bypass-approvals-and-sandbox") || contains(args, "--full-auto") {
		t.Fatalf("unsafe or deprecated flag present: %#v", args)
	}
}

func TestBuildArgsAddsOnlyScopedMCPServerWhenEnabled(t *testing.T) {
	cfg := Config{ResponsesURL: "http://127.0.0.1:8787/v1", MCPURL: "http://127.0.0.1:8787/mcp", EnableTools: true, Workspace: t.TempDir(), Model: "gpt-test"}
	args, err := BuildArgs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, `mcp_servers.agw.url="http://127.0.0.1:8787/mcp"`) || !strings.Contains(joined, "mcp_servers.agw.required=true") {
		t.Fatalf("scoped MCP configuration missing: %#v", args)
	}
}

func TestRunThroughUnixBridgeUploadsImmutableOutputBeforeCompletion(t *testing.T) {
	directory := t.TempDir()
	socketPath := filepath.Join(directory, "broker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	var authorization string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.Method != http.MethodPut || r.URL.Path != "/v1/artifacts/output" || r.Header.Get("Content-Type") != "application/vnd.agw.run-output+json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"artifact-1","uri":"artifact://local/runs/output","digest":"sha256:`+strings.Repeat("a", 64)+`","size_bytes":42,"media_type":"application/vnd.agw.run-output+json"}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		_ = listener.Close()
	})
	clientPath := filepath.Join(directory, "client.json")
	client := map[string]any{
		"session_id":       "ags_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO123",
		"bearer_token":     "agt_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO123",
		"policy_digest":    "sha256:" + strings.Repeat("b", 64),
		"allowed_model":    "gpt-test",
		"model_url":        "http://127.0.0.1:8787/v1/responses",
		"tools_url":        "http://127.0.0.1:8787/mcp",
		"artifact_url":     "http://127.0.0.1:8787/v1/artifacts/output",
		"tools_enabled":    false,
		"artifact_enabled": true,
	}
	raw, _ := json.Marshal(client)
	if err := os.WriteFile(clientPath, raw, 0400); err != nil {
		t.Fatal(err)
	}
	fake := fakeCodex(t, `
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	cfg := Config{CodexBinary: fake, Workspace: t.TempDir(), BrokerClientConfigPath: clientPath, BrokerSocketPath: socketPath, MaxRuntime: time.Minute, TerminationGrace: time.Second}
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(runStartLine(t, "run-artifact", "work")), &output, io.Discard, cfg); err != nil {
		t.Fatal(err)
	}
	frames := decodeEvents(t, output.Bytes())
	if len(frames) != 6 || frames[4].Type != proto.EventArtifactCreated || frames[5].Type != proto.EventRunCompleted {
		t.Fatalf("artifact was not committed before completion: %#v", frames)
	}
	if !strings.HasPrefix(authorization, "Bearer agt_") {
		t.Fatalf("bridge capability was not injected: %q", authorization)
	}
}

func TestRunWithFakeCodexEmitsStrictEventsAndDoesNotInheritAuth(t *testing.T) {
	fake := fakeCodex(t, `
if [ "${OPENAI_API_KEY+x}" = x ]; then
  printf '%s\n' '{"type":"error","error":{"message":"OPENAI_API_KEY leaked"}}'
  exit 42
fi
if [ "$HOME" != "$CODEX_HOME" ]; then
  printf '%s\n' '{"type":"error","error":{"message":"Codex home was not isolated"}}'
  exit 43
fi
printf '%s\n' '{"type":"thread.started","thread_id":"fake"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done with sk-secret-token"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}'
`)
	workspace := t.TempDir()
	cfg := Config{CodexBinary: fake, ResponsesURL: "http://127.0.0.1:8787/v1", Workspace: workspace, Model: "gpt-5.6-codex", MaxRuntime: time.Minute, TerminationGrace: time.Second}
	input := runStartLine(t, "run-1", "complete the task")
	var output bytes.Buffer
	var diagnostics bytes.Buffer
	t.Setenv("OPENAI_API_KEY", "host-secret-must-not-be-inherited")
	if err := Run(context.Background(), strings.NewReader(input), &output, &diagnostics, cfg); err != nil {
		t.Fatal(err)
	}
	frames := decodeEvents(t, output.Bytes())
	if len(frames) != 5 || frames[0].Type != proto.EventRunStarted || frames[1].Type != proto.EventModelRequested || frames[2].Type != proto.EventAssistantMessage || frames[3].Type != proto.EventModelCompleted || frames[4].Type != proto.EventRunCompleted {
		t.Fatalf("unexpected event stream: %#v", frames)
	}
	if bytes.Contains(output.Bytes(), []byte("sk-secret-token")) || bytes.Contains(output.Bytes(), []byte("host-secret")) {
		t.Fatalf("secret leaked in protocol output: %s", output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("[REDACTED]")) {
		t.Fatalf("expected redaction in output: %s", output.String())
	}
	assertContiguousEvents(t, frames)

	invocation := filepath.Join(workspace, "invocation")
	_ = invocation // fake executable itself is the deterministic invocation fixture.
}

func TestRunPublishesAuthoredArtifactBeforeImmutableRunOutput(t *testing.T) {
	var authoredBody, authoredMetadata string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == brokerbridge.ArtifactCreatePath:
			authoredMetadata = r.Header.Get(artifact.PublishMetadataHeader)
			body, _ := io.ReadAll(r.Body)
			authoredBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"authored-1","uri":"artifact://local/runs/authored-1","digest":"sha256:`+strings.Repeat("a", 64)+`","size_bytes":17,"media_type":"text/html"}`)
		case r.Method == http.MethodPut && r.URL.Path == brokerbridge.ArtifactPath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"output-1","uri":"artifact://local/runs/output-1","digest":"sha256:`+strings.Repeat("b", 64)+`","size_bytes":42,"media_type":"application/vnd.agw.run-output+json"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	workspace := t.TempDir()
	fake := fakeCodex(t, `
mkdir -p .agw/artifacts
printf '%s' '<main>hello</main>' > report.html
printf '%s' '{"schema":"agents-gateway.artifact.v1","title":"Hello artifact","content_kind":"single_page_html","media_type":"text/html","source":"report.html"}' > .agw/artifacts/report.json
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	cfg := Config{
		CodexBinary: fake, ResponsesURL: "http://127.0.0.1:8787/v1", Workspace: workspace, Model: "gpt-test",
		ArtifactURL: server.URL + brokerbridge.ArtifactPath, MaxRuntime: time.Minute, TerminationGrace: time.Second,
	}
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(runStartLine(t, "run-authored", "build a page")), &output, io.Discard, cfg); err != nil {
		t.Fatal(err)
	}
	frames := decodeEvents(t, output.Bytes())
	if len(frames) != 7 || frames[4].Type != proto.EventArtifactCreated || frames[5].Type != proto.EventArtifactCreated || frames[6].Type != proto.EventRunCompleted {
		t.Fatalf("unexpected authored artifact event ordering: %#v", frames)
	}
	if authoredBody != "<main>hello</main>" || authoredMetadata == "" {
		t.Fatalf("authored upload body=%q metadata=%q", authoredBody, authoredMetadata)
	}
}

func TestRunCancellationTerminatesFakeCodexAndEmitsCancelled(t *testing.T) {
	fake := fakeCodex(t, `
trap 'exit 143' TERM INT
printf '%s\n' '{"type":"thread.started","thread_id":"fake"}'
while :; do sleep 1; done
`)
	cfg := Config{CodexBinary: fake, ResponsesURL: "http://localhost:8787/v1", Workspace: t.TempDir(), Model: "gpt-test", MaxRuntime: time.Minute, TerminationGrace: time.Second}
	input := runStartLine(t, "run-cancel", "wait")
	input += cancelLine(t, "run-cancel", "operator requested cancellation")
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &output, io.Discard, cfg); err != nil {
		t.Fatal(err)
	}
	frames := decodeEvents(t, output.Bytes())
	if frames[len(frames)-1].Type != proto.EventRunCancelled || !frames[len(frames)-1].Terminal {
		t.Fatalf("expected terminal cancellation: %#v", frames)
	}
	assertContiguousEvents(t, frames)
}

func TestRunRejectsProtocolMismatchBeforeLaunchingCodex(t *testing.T) {
	fake := fakeCodex(t, `exit 99`)
	cfg := Config{CodexBinary: fake, ResponsesURL: "http://127.0.0.1:8787/v1", Workspace: t.TempDir(), Model: "gpt-test"}
	bad := runStartLine(t, "envelope-id", "task")
	bad = strings.Replace(bad, `"run_id":"envelope-id"`, `"run_id":"payload-id"`, 1)
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(bad), &output, io.Discard, cfg); err == nil {
		t.Fatal("expected run id mismatch")
	}
	if output.Len() != 0 {
		t.Fatalf("must not emit events for invalid start: %s", output.String())
	}
}

func TestRunRejectsReplayedControlSequence(t *testing.T) {
	fake := fakeCodex(t, `
trap 'exit 143' TERM INT
printf '%s\n' '{"type":"thread.started","thread_id":"fake"}'
while :; do sleep 1; done
`)
	cfg := Config{CodexBinary: fake, ResponsesURL: "http://127.0.0.1:8787/v1", Workspace: t.TempDir(), Model: "gpt-test", MaxRuntime: time.Minute, TerminationGrace: 100 * time.Millisecond}
	input := runStartLine(t, "run-sequence", "wait") + strings.Replace(cancelLine(t, "run-sequence", "cancel"), `"seq":2`, `"seq":1`, 1)
	var output bytes.Buffer
	if err := Run(context.Background(), strings.NewReader(input), &output, io.Discard, cfg); err == nil || !strings.Contains(err.Error(), "adapter_invalid_input") {
		t.Fatalf("replayed control sequence was not rejected: %v", err)
	}
	frames := decodeEvents(t, output.Bytes())
	if frames[len(frames)-1].Type != proto.EventRunFailed {
		t.Fatalf("expected terminal failure: %#v", frames)
	}
}

func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\nset -eu\n[ \"${1:-}\" = exec ]\n[ \"${2:-}\" = --json ]\n[ \"${3:-}\" = --ephemeral ]\ncat >/dev/null\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runStartLine(t *testing.T, runID, instructions string) string {
	t.Helper()
	data := map[string]any{
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
			"instructions":     instructions,
		},
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	frame := proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: runID, Seq: 1, Data: raw}
	line, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func cancelLine(t *testing.T, runID, reason string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"reason": reason})
	frame := proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestCancel, RunID: runID, Seq: 2, Data: data}
	line, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func decodeEvents(t *testing.T, data []byte) []proto.Envelope {
	t.Helper()
	decoder := runtimeproto.NewDecoder(bytes.NewReader(data))
	var events []proto.Envelope
	validator := new(runtimeproto.SequenceValidator)
	for {
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := validator.Accept(frame); err != nil {
			t.Fatal(err)
		}
		events = append(events, frame)
	}
}

func assertContiguousEvents(t *testing.T, events []proto.Envelope) {
	t.Helper()
	for index, frame := range events {
		if frame.Seq != uint64(index+1) {
			t.Fatalf("non-contiguous sequence at %d: %#v", index, frame)
		}
	}
}

func cloneEnv(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
