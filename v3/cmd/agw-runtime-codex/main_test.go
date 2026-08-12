package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/pkg/codexadapter"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

func TestConfigRequiresPinnedBrokerWorkspaceAndBase(t *testing.T) {
	values := map[string]string{}
	if _, err := configFromEnv(mapGetter(values)); err == nil {
		t.Fatal("empty runtime environment was accepted")
	}

	workspace := t.TempDir()
	values = validEnvironment(workspace)
	delete(values, "AGW_BASE_SHA")
	if _, err := configFromEnv(mapGetter(values)); err == nil || !strings.Contains(err.Error(), "AGW_BASE_SHA") {
		t.Fatalf("missing base SHA error=%v", err)
	}
}

func TestConfigRejectsUnsafeRuntimeEnvironment(t *testing.T) {
	workspace := t.TempDir()
	values := validEnvironment(workspace)
	for name, value := range map[string]string{
		"AGW_HARNESS":  "claude-code",
		"AGW_BASE_SHA": strings.Repeat("A", 40),
		"AGW_BROKER":   "https://broker.example.test",
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneValues(values)
			candidate[name] = value
			if _, err := configFromEnv(mapGetter(candidate)); err == nil {
				t.Fatalf("unsafe value %q was accepted", value)
			}
		})
	}
}

func TestConfigAcceptsWorkloadRuntimeContract(t *testing.T) {
	workspace := t.TempDir()
	base := t.TempDir()
	values := validEnvironment(workspace)
	values["AGW_BASE_PATH"] = base
	values["AGW_HARNESS"] = "codex"
	config, err := configFromEnv(mapGetter(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.adapter.Workspace != workspace || config.basePath != base || config.baseSHA != values["AGW_BASE_SHA"] {
		t.Fatalf("unexpected runtime config: %#v", config)
	}
}

func TestVerifyBaseSHAChecksWorkspaceAndPristineBase(t *testing.T) {
	workspace := initGitRepository(t)
	base := filepath.Join(t.TempDir(), "base")
	if output, err := exec.Command("git", "clone", "--quiet", workspace, base).CombinedOutput(); err != nil {
		t.Fatalf("clone base: %v: %s", err, output)
	}
	sha := gitRevisionForTest(t, workspace)
	config := runtimeConfig{
		adapter:  codexadapter.Config{Workspace: workspace},
		baseSHA:  sha,
		basePath: base,
	}
	if err := verifyBaseSHA(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(base, "README.md"), []byte("different\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commitFile(t, base, "different"); err != nil {
		t.Fatal(err)
	}
	if err := verifyBaseSHA(context.Background(), config); err == nil || !strings.Contains(err.Error(), "base checkout revision") {
		t.Fatalf("base mismatch was not rejected: %v", err)
	}
}

func TestVerifyBaseSHARejectsUnpinnedOrNonRepositoryCheckout(t *testing.T) {
	config := runtimeConfig{adapter: codexadapter.Config{Workspace: t.TempDir()}, baseSHA: strings.Repeat("a", 40)}
	if err := verifyBaseSHA(context.Background(), config); err == nil {
		t.Fatal("non-repository workspace was accepted")
	}

	config.baseSHA = strings.Repeat("A", 40)
	if err := verifyBaseSHA(context.Background(), config); err == nil {
		t.Fatal("invalid base SHA was not rejected by the identity check")
	}
}

func TestStartFrameIsGeneratedFromImmutableWorkloadContract(t *testing.T) {
	config, err := configFromEnv(mapGetter(validEnvironment(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	line, err := startFrame(config)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := runtimeproto.ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Kind != proto.KindRequest || frame.Type != proto.RequestRunStart || frame.RunID != config.runUID || frame.Seq != 1 || frame.Terminal {
		t.Fatalf("unexpected start frame: %#v", frame)
	}
	var payload struct {
		RunID     string `json:"run_id"`
		AgentRef  string `json:"agent_ref"`
		Execution struct {
			Agent struct {
				Name string `json:"name"`
			} `json:"agent"`
			Instructions string `json:"instructions"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(frame.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RunID != config.runUID || payload.AgentRef != config.agentRef || payload.Execution.Agent.Name != config.agentRef || !strings.Contains(payload.Execution.Instructions, config.task) || !strings.Contains(payload.Execution.Instructions, config.instructions) {
		t.Fatalf("unexpected start payload: %#v", payload)
	}
}

func TestEventPosterSynchronouslyRequiresBrokerAcceptance(t *testing.T) {
	accepted := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/runtime/events" || request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("unexpected request: %s %s %q", request.Method, request.URL.Path, request.Header.Get("Content-Type"))
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		accepted = true
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	line, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventHeartbeat, RunID: "run-uid", Seq: 1, Data: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	poster := newEventPoster(context.Background(), server.URL+"/v1/runtime/events")
	if written, err := poster.Write(line); err != nil || written != len(line) || !accepted {
		t.Fatalf("written=%d accepted=%v err=%v", written, accepted, err)
	}

	rejecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusConflict) }))
	defer rejecting.Close()
	if _, err := newEventPoster(context.Background(), rejecting.URL).Write(line); err == nil {
		t.Fatal("broker rejection was acknowledged as a durable event")
	}
	if _, err := poster.Write([]byte(`{"not":"jsonl"}`)); err == nil {
		t.Fatal("malformed frame was posted")
	}
}

func validEnvironment(workspace string) map[string]string {
	return map[string]string{
		"AGW_HARNESS":                "codex",
		"AGW_BROKER":                 "http://127.0.0.1:8081",
		"AGW_CODEX_WORKSPACE":        workspace,
		"AGW_CODEX_MODEL":            "gpt-test",
		"AGW_BASE_SHA":               strings.Repeat("a", 40),
		"AGW_RUN_UID":                "run-uid-123",
		"AGW_AGENT_REF":              "issue-fixer",
		"AGW_TASK":                   "Fix the bug and add a regression test.",
		"AGW_INSTRUCTIONS":           "Work only in the declared scope.",
		"AGW_SPEC_DIGEST":            "sha256:" + strings.Repeat("b", 64),
		"AGW_CODEX_ENABLE_TOOLS":     "false",
		"AGW_CODEX_REQUIRE_ARTIFACT": "false",
	}
}

func mapGetter(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func cloneValues(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func initGitRepository(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", directory, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commitFile(t, directory, "initial"); err != nil {
		t.Fatal(err)
	}
	return directory
}

func commitFile(t *testing.T, directory, message string) error {
	t.Helper()
	if output, err := exec.Command("git", "-C", directory, "add", "--", ".").CombinedOutput(); err != nil {
		return &testCommandError{command: "git add", output: string(output), err: err}
	}
	command := exec.Command("git", "-C", directory, "-c", "user.name=AGW Test", "-c", "user.email=agw-test@example.invalid", "commit", "--quiet", "-m", message)
	if output, err := command.CombinedOutput(); err != nil {
		return &testCommandError{command: "git commit", output: string(output), err: err}
	}
	return nil
}

func gitRevisionForTest(t *testing.T, directory string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", directory, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

type testCommandError struct {
	command string
	output  string
	err     error
}

func (e *testCommandError) Error() string {
	return e.command + ": " + e.err.Error() + ": " + e.output
}
