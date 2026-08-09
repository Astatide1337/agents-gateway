package codexadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/modelbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/proto"
)

func TestCodexRuntimeContainerEndToEnd(t *testing.T) {
	if os.Getenv("AGW_CODEX_CONTAINER_LIVE") != "1" {
		t.Skip("set AGW_CODEX_CONTAINER_LIVE=1 after building the runtime image")
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
		image = "agents-gateway-v2-runtime-codex:verification"
	}

	var providerAuthorization string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, event := range fakeResponseEvents("gpt-test", "container-e2e-ok") {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, event.Data)
			flusher.Flush()
		}
	}))
	defer provider.Close()
	responses, err := modelbroker.NewResponsesProxy(modelbroker.ResponsesProxyConfig{
		UpstreamURL: provider.URL + "/v1/responses", AllowedModels: []string{"gpt-test"},
		Credential: func(context.Context) ([]byte, error) { return []byte("provider-secret"), nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	artifactRoot := t.TempDir()
	if err := os.Chmod(artifactRoot, 0700); err != nil {
		t.Fatal(err)
	}
	objects, err := artifact.NewLocalObjectClient(artifactRoot)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(objects, artifact.Config{Bucket: "local", Prefix: "runs", MaxBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	upload, err := artifact.NewUploadHandler(artifacts, artifact.UploadHandlerConfig{
		OrganizationID: "org", ProjectID: "project", RunID: "run-container", Name: "output.json",
		Path: "/v1/artifacts/output", Method: http.MethodPut, MaxBytes: 8 << 20,
		AllowedMediaTypes: []string{"application/vnd.agw.run-output+json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/responses", responses)
	mux.Handle("/v1/artifacts/output", upload)

	manager, err := runbroker.NewManager(runbroker.ManagerConfig{MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	binding := runbroker.SessionBinding{OrgID: "org", ProjectID: "project", UserID: "owner", RunID: "run-container"}
	policy := "sha256:" + strings.Repeat("b", 64)
	created, err := manager.Create(context.Background(), runbroker.CreateRequest{Binding: binding, AllowedModels: []string{"gpt-test"}, PolicyDigest: policy, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sessionDirectory := t.TempDir()
	if err := os.Chmod(sessionDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	server, err := runbroker.NewUnixServer(runbroker.UnixServerConfig{
		SocketPath: filepath.Join(sessionDirectory, "broker.sock"), SessionID: created.Session.ID,
		Binding: binding, PolicyDigest: policy, Sessions: manager,
		Peer:    runbroker.PeerPolicy{UID: uint32TestPointer(uint32(os.Geteuid())), GID: uint32TestPointer(uint32(os.Getegid()))},
		Handler: mux,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveContext, cancelServe := context.WithCancel(context.Background())
	go func() { _ = server.Serve(serveContext) }()
	t.Cleanup(func() {
		cancelServe()
		_ = server.Close(context.Background())
	})
	clientConfig := map[string]any{
		"session_id": string(created.Session.ID), "bearer_token": created.Token.String(),
		"policy_digest": policy, "allowed_model": "gpt-test",
		"model_url": "http://127.0.0.1:8787/v1/responses", "tools_url": "http://127.0.0.1:8787/mcp",
		"artifact_url": "http://127.0.0.1:8787/v1/artifacts/output", "tools_enabled": false, "artifact_enabled": true,
	}
	rawClient, _ := json.Marshal(clientConfig)
	if err := os.WriteFile(filepath.Join(sessionDirectory, "client.json"), rawClient, 0400); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	uidGID := strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid())
	args := []string{"run", "--rm", "--interactive", "--network=none", "--read-only",
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", uidGID}
	if engine == "podman" {
		args = append(args, "--userns=keep-id")
	}
	args = append(args,
		"--workdir", "/workspace", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
		"--mount", "type=bind,src="+sessionDirectory+",dst=/run/agw,readonly")
	if os.Getenv("AGW_CODEX_WORKSPACE_TMPFS") == "1" {
		args = append(args, "--tmpfs", "/workspace:rw,nosuid,nodev,size=1g,mode=01777")
	} else {
		args = append(args, "--mount", "type=bind,src="+workspace+",dst=/workspace")
	}
	if os.Getenv("AGW_CODEX_EXTRA_MOUNTS") == "1" {
		skills := filepath.Join(sessionDirectory, "skills")
		if err := os.Mkdir(skills, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# Test skill\n"), 0444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(skills, 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(skills, 0700) })
		artifactMount := t.TempDir()
		args = append(args,
			"--mount", "type=bind,src="+skills+",dst=/skills,readonly",
			"--mount", "type=bind,src="+artifactMount+",dst=/artifacts,readonly")
	}
	args = append(args, image)
	command := exec.CommandContext(ctx, engine, args...)
	command.Stdin = strings.NewReader(runStartLine(t, "run-container", "Reply with exactly container-e2e-ok and do not use tools."))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("runtime container: %v; stderr=%s; stdout=%s", err, stderr.String(), stdout.String())
	}
	frames := decodeEvents(t, stdout.Bytes())
	if providerAuthorization != "Bearer provider-secret" || len(frames) < 2 || frames[len(frames)-2].Type != proto.EventArtifactCreated || frames[len(frames)-1].Type != proto.EventRunCompleted {
		t.Fatalf("container E2E incomplete: provider auth=%q frames=%#v stderr=%s", providerAuthorization, frames, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("container-e2e-ok")) || bytes.Contains(stdout.Bytes(), []byte("provider-secret")) {
		t.Fatalf("unexpected runtime output: %s", stdout.String())
	}
	entries, err := os.ReadDir(artifactRoot)
	if err != nil || len(entries) == 0 {
		t.Fatalf("immutable artifact was not stored: entries=%v err=%v", entries, err)
	}
	_, _ = io.Copy(io.Discard, &stderr)
}

func uint32TestPointer(value uint32) *uint32 { return &value }
