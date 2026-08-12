package claudeadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

func TestConfigAndBuildArgsRejectCredentialBearingOrNonLoopbackValues(t *testing.T) {
	workspace := t.TempDir()
	values := map[string]string{
		"AGW_HARNESS":                   "claude-code",
		"AGW_BROKER":                    "http://127.0.0.1:8081",
		"AGW_CLAUDE_ANTHROPIC_BASE_URL": "http://127.0.0.1:8081/api",
		"AGW_CLAUDE_WORKSPACE":          workspace,
		"AGW_CLAUDE_MODEL":              "claude-test",
		"AGW_TASK":                      "run the test",
		"AGW_INSTRUCTIONS":              "be deterministic",
		"AGW_CLAUDE_REQUIRE_ARTIFACT":   "false",
		"AGW_CLAUDE_ENABLE_TOOLS":       "false",
		"ANTHROPIC_API_KEY":             "must-not-be-read",
	}
	cfg, err := ConfigFromEnv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequireArtifact || cfg.EnableTools || cfg.AnthropicBaseURL != "http://127.0.0.1:8081/api" {
		t.Fatalf("config=%#v", cfg)
	}
	if _, err := BuildArgs(cfg, "hello", ""); err != nil {
		t.Fatalf("tools-disabled args: %v", err)
	}
	values["AGW_CLAUDE_ANTHROPIC_BASE_URL"] = "https://169.254.169.254/api"
	if _, err := ConfigFromEnv(func(name string) string { return values[name] }); err == nil {
		t.Fatal("metadata endpoint was accepted")
	}
}

func TestRunWithFakeClaudeEmitsBoundedEventsAndUploadsArtifacts(t *testing.T) {
	workspace := t.TempDir()
	fake := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
test "$ANTHROPIC_API_KEY" = "agw-loopback-dummy"
test "$ANTHROPIC_AUTH_TOKEN" = "agw-loopback-dummy"
test -n "$HOME"
test -n "$CLAUDE_CONFIG_DIR"
mkdir -p .agw/artifacts
printf '%s' '{"schema":"agents-gateway.artifact.v1","title":"Result","description":"fake","content_kind":"document","media_type":"text/plain","source":"result.txt","capabilities":[]}' > .agw/artifacts/result.json
printf '%s' 'artifact body' > result.txt
	printf '%s\n' '{"type":"system","subtype":"init"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":3,"output_tokens":2}}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/artifacts/output" || r.URL.Path == "/v1/artifacts/create" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"artifact-1","uri":"artifact://agw/object","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":0,"media_type":"application/json"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	cfg := Config{
		ClaudeBinary: fake, AnthropicBaseURL: server.URL, Workspace: workspace, Model: "claude-test",
		MCPURL: server.URL + "/mcp", ArtifactURL: server.URL + "/v1/artifacts/output", ArtifactCreateURL: server.URL + "/v1/artifacts/create",
		Task: "test task", Instructions: "test instructions", MaxRuntime: time.Minute, TerminationGrace: time.Second,
		MaxTurns: 2, RequireArtifact: true, EnableTools: true,
	}
	start, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: "run-claude", Seq: 1, Data: json.RawMessage(`{"organization_id":"agw","project_id":"default","run_id":"run-claude","workflow_name":"agentrun","step_id":"work","agent_ref":"agent","execution":{"agent":{"kind":"Agent","name":"agent","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"sandbox_profile":{"kind":"Sandbox","name":"work","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"instructions_ref":"agw://run-claude/instructions","instructions":"test"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), bytes.NewReader(start), &output, io.Discard, cfg); err != nil {
		t.Fatalf("Run: %v; output=%s", err, output.String())
	}
	decoder := runtimeproto.NewDecoder(bytes.NewReader(output.Bytes()))
	var types []string
	for {
		frame, err := decoder.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, frame.Type)
	}
	want := []string{proto.EventRunStarted, proto.EventModelRequested, proto.EventAssistantMessage, proto.EventModelCompleted, proto.EventArtifactCreated, proto.EventArtifactCreated, proto.EventRunCompleted}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types=%v want=%v", types, want)
	}
	if strings.Contains(output.String(), "must-not-be-read") || strings.Contains(output.String(), "ANTHROPIC_API_KEY") {
		t.Fatal("runtime output exposed provider credential material")
	}
}

func TestRunRejectsMalformedSuccessfulResult(t *testing.T) {
	workspace := t.TempDir()
	fake := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
printf '%s\n' '{"type":"result","result":"done"}'
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ClaudeBinary: fake, AnthropicBaseURL: "http://127.0.0.1:8787", Workspace: workspace, Model: "claude-test",
		Task: "test task", Instructions: "test instructions", MaxRuntime: time.Minute, TerminationGrace: time.Second,
		MaxTurns: 2, RequireArtifact: false, EnableTools: false,
	}
	start, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: "run-claude-incomplete", Seq: 1, Data: json.RawMessage(`{"organization_id":"agw","project_id":"default","run_id":"run-claude-incomplete","workflow_name":"agentrun","step_id":"work","agent_ref":"agent","execution":{"agent":{"kind":"Agent","name":"agent","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"sandbox_profile":{"kind":"Sandbox","name":"work","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"instructions_ref":"agw://run-claude-incomplete/instructions","instructions":"test"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), bytes.NewReader(start), &output, io.Discard, cfg); err == nil {
		t.Fatal("result without subtype=success was accepted")
	}
	decoder := runtimeproto.NewDecoder(bytes.NewReader(output.Bytes()))
	var last proto.Envelope
	for {
		frame, decodeErr := decoder.Next()
		if decodeErr == io.EOF {
			break
		}
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		last = frame
	}
	if last.Type != proto.EventRunFailed || !last.Terminal {
		t.Fatalf("expected terminal run.failed, got %#v", last)
	}
}

func TestRunDoesNotCloseClaudeOutputBeforeConsumerFinishes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the deterministic child-exit check uses Linux /proc state")
	}
	workspace := t.TempDir()
	fake := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
printf '%s' "$$" > .fake-claude.pid
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"gated"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"gated"}'
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/artifacts/output" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"artifact-1","uri":"artifact://agw/object","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size_bytes":0,"media_type":"application/json"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	cfg := Config{
		ClaudeBinary: fake, AnthropicBaseURL: server.URL, Workspace: workspace, Model: "claude-test",
		MCPURL: server.URL + "/mcp", ArtifactURL: server.URL + "/v1/artifacts/output", ArtifactCreateURL: server.URL + "/v1/artifacts/create",
		Task: "test task", Instructions: "test instructions", MaxRuntime: time.Minute, TerminationGrace: time.Second,
		MaxTurns: 2, RequireArtifact: true, EnableTools: false,
	}
	start, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, Type: proto.RequestRunStart, RunID: "run-claude-gated", Seq: 1, Data: json.RawMessage(`{"organization_id":"agw","project_id":"default","run_id":"run-claude-gated","workflow_name":"agentrun","step_id":"work","agent_ref":"agent","execution":{"agent":{"kind":"Agent","name":"agent","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"sandbox_profile":{"kind":"Sandbox","name":"work","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"instructions_ref":"agw://run-claude-gated/instructions","instructions":"test"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	output := &gatedWriter{blockWrite: 3, blocked: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), bytes.NewReader(start), output, io.Discard, cfg) }()
	select {
	case <-output.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("Claude output consumer did not reach the gate")
	}
	waitForFakeClaudeExit(t, filepath.Join(workspace, ".fake-claude.pid"))
	close(output.release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v; output=%s", err, output.String())
	}
	if !strings.Contains(output.String(), `"type":"run.completed"`) {
		t.Fatalf("run did not complete: %s", output.String())
	}
}

type gatedWriter struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	writes     int
	blockWrite int
	blocked    chan struct{}
	release    chan struct{}
}

func (w *gatedWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	shouldBlock := w.writes == w.blockWrite
	if shouldBlock {
		close(w.blocked)
	}
	w.mu.Unlock()
	if shouldBlock {
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(value)
}

func (w *gatedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func waitForFakeClaudeExit(t *testing.T, pidPath string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := os.ReadFile(pidPath)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(value)))
			if parseErr == nil && pid > 0 {
				stat, statErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
				fields := strings.Fields(string(stat))
				if os.IsNotExist(statErr) || (statErr == nil && len(fields) > 2 && fields[2] == "Z") {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fake Claude process did not exit: %s", pidPath)
}
