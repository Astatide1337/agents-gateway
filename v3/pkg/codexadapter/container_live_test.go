package codexadapter

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
)

// TestCodexRuntimeContainerEndToEnd exercises the v3 runtime image's
// adapter boundary with a deterministic fake Codex executable and an in-
// container loopback broker fixture. The fake never contacts a provider, so
// the test runs with network=none while proving the production wrapper creates
// run.start itself and synchronously delivers events without leaking a host
// API credential.
func TestCodexRuntimeContainerEndToEnd(t *testing.T) {
	if os.Getenv("AGW_CODEX_CONTAINER_LIVE") != "1" {
		t.Skip("set AGW_CODEX_CONTAINER_LIVE=1 after building the v3 runtime image")
	}
	engine := strings.TrimSpace(os.Getenv("AGW_CODEX_CONTAINER_ENGINE"))
	if engine == "" {
		engine = "docker"
	}
	if engine != "docker" && engine != "podman" {
		t.Fatalf("unsupported container engine %q", engine)
	}
	if _, err := exec.LookPath(engine); err != nil {
		t.Fatalf("%s is unavailable", engine)
	}
	image := os.Getenv("AGW_CODEX_RUNTIME_IMAGE")
	if image == "" {
		image = "agents-gateway-v3-runtime-codex:verification"
	}

	fixtureDir := t.TempDir()
	fakePath := filepath.Join(fixtureDir, "fake-codex")
	fake := `#!/bin/sh
set -eu
if [ "${OPENAI_API_KEY+x}" = x ]; then
  printf '%s\n' '{"type":"error","error":{"message":"OPENAI_API_KEY leaked"}}'
  exit 42
fi
if [ "$HOME" != "$CODEX_HOME" ]; then
  printf '%s\n' '{"type":"error","error":{"message":"Codex home was not isolated"}}'
  exit 43
fi
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"container-v3-ok"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(fakePath, []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(fixtureDir, "test-wrapper")
	wrapper := `#!/bin/sh
set -eu
node -e '
const fs = require("fs");
const crypto = require("crypto");
const http = require("http");
http.createServer((request, response) => {
  const chunks = [];
  request.on("data", chunk => chunks.push(chunk));
  request.on("end", () => {
    const body = Buffer.concat(chunks);
    if (request.method === "POST" && request.url === "/v1/runtime/events") {
      fs.appendFileSync("/workspace/events.jsonl", body);
      response.writeHead(204); response.end(); return;
    }
    if (request.method === "PUT" && request.url === "/v1/artifacts/output") {
      const digest = "sha256:" + crypto.createHash("sha256").update(body).digest("hex");
      response.writeHead(201, {"content-type":"application/json"});
      response.end(JSON.stringify({id:"runtime-smoke-output",uri:"artifact://local/runs/output",digest,size_bytes:body.length,media_type:"application/vnd.agw.run-output+json"}));
      return;
    }
    response.writeHead(404); response.end();
  });
}).listen(8081, "127.0.0.1");
' >/workspace/broker.log 2>&1 &
sleep 1
exec /agw/agent
`
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(workspace, "repo")
	if err := os.MkdirAll(repository, 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repository, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("runtime image test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repository, "add", "README.md").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commit := exec.Command("git", "-C", repository, "-c", "user.name=AGW Test", "-c", "user.email=agw@example.invalid", "commit", "--quiet", "-m", "base")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	baseOutput, err := exec.Command("git", "-C", repository, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := strings.TrimSpace(string(baseOutput))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	uidGID := strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	args := []string{
		"run", "--rm", "--network=none", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", uidGID,
		"--pids-limit", "128", "--memory", "2147483648", "--cpus", "1.000",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
		"--mount", "type=bind,src=" + fixtureDir + ",dst=/run/agw,readonly",
		"--mount", "type=bind,src=" + workspace + ",dst=/workspace",
		"--entrypoint", "/run/agw/test-wrapper",
		"--env", "AGW_CODEX_BIN=/run/agw/fake-codex",
		"--env", "AGW_BROKER=http://127.0.0.1:8081",
		"--env", "AGW_HARNESS=codex",
		"--env", "AGW_CODEX_MODEL=gpt-test",
		"--env", "AGW_CODEX_WORKSPACE=/workspace/repo",
		"--env", "AGW_BASE_SHA=" + baseSHA,
		"--env", "AGW_RUN_UID=run-container-v3",
		"--env", "AGW_AGENT_REF=image-test",
		"--env", "AGW_TASK=Reply with exactly container-v3-ok.",
		"--env", "AGW_INSTRUCTIONS=Complete the deterministic image test.",
		"--env", "AGW_SPEC_DIGEST=sha256:" + strings.Repeat("a", 64),
		"--env", "AGW_CODEX_ENABLE_TOOLS=false",
		"--env", "AGW_CODEX_REQUIRE_ARTIFACT=true",
		"--env", "OPENAI_API_KEY=host-secret-must-not-be-inherited",
	}
	if engine == "podman" {
		args = append(args, "--userns=keep-id")
	}
	args = append(args, image)
	command := exec.CommandContext(ctx, engine, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("runtime container: %v; stderr=%s; stdout=%s", err, stderr.String(), stdout.String())
	}
	events, err := os.ReadFile(filepath.Join(workspace, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	frames := decodeEvents(t, events)
	wantTypes := []string{
		proto.EventRunStarted,
		proto.EventModelRequested,
		proto.EventAssistantMessage,
		proto.EventModelCompleted,
		proto.EventArtifactCreated,
		proto.EventRunCompleted,
	}
	if len(frames) != len(wantTypes) {
		t.Fatalf("container E2E event count=%d, want %d; stderr=%s stdout=%s events=%s", len(frames), len(wantTypes), stderr.String(), stdout.String(), events)
	}
	for index, want := range wantTypes {
		if frames[index].Type != want || frames[index].Seq != uint64(index+1) {
			t.Fatalf("container E2E event %d=%s seq=%d, want %s seq=%d; events=%s", index, frames[index].Type, frames[index].Seq, want, index+1, events)
		}
	}
	if !frames[len(frames)-1].Terminal || !bytes.Contains(events, []byte("container-v3-ok")) || stdout.Len() != 0 {
		t.Fatalf("container E2E incomplete: frames=%#v stderr=%s stdout=%s events=%s", frames, stderr.String(), stdout.String(), events)
	}
}
