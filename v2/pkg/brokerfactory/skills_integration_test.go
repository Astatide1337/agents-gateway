package brokerfactory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/sandbox"
	"github.com/Astatide1337/agents-gateway/v2/pkg/skills"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

const (
	integrationSkillID    = "preview-skill"
	integrationGatewayKey = "gateway-secret-must-not-be-staged"
)

type strictSkillsGateway struct {
	testing         *testing.T
	server          *httptest.Server
	contents        map[string]string
	digest          string
	requests        atomic.Int64
	credentialCalls atomic.Int64
	methodsMu       sync.Mutex
	methods         []string
}

func newStrictSkillsGateway(t *testing.T) *strictSkillsGateway {
	t.Helper()
	contents := map[string]string{
		"SKILL.md":      "# Preview skill\n\nUse the staged files.\n",
		"docs/guide.md": "# Guide\n\nThis content came from the Skills Gateway.\n",
	}
	digest, err := skills.CanonicalDigest(stringBytes(contents))
	if err != nil {
		t.Fatal(err)
	}
	fake := &strictSkillsGateway{testing: t, contents: contents, digest: digest}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func stringBytes(contents map[string]string) map[string][]byte {
	result := make(map[string][]byte, len(contents))
	for name, content := range contents {
		result[name] = []byte(content)
	}
	return result
}

func (g *strictSkillsGateway) endpoint() string { return g.server.URL + "/mcp" }

func (g *strictSkillsGateway) serveHTTP(response http.ResponseWriter, request *http.Request) {
	g.requests.Add(1)
	if request.Method != http.MethodPost || request.URL.Path != "/mcp" || request.URL.RawQuery != "" {
		http.Error(response, "invalid MCP endpoint", http.StatusNotFound)
		return
	}
	if got := request.Header.Get("Authorization"); got != "Bearer "+integrationGatewayKey {
		http.Error(response, "invalid gateway credential", http.StatusUnauthorized)
		return
	}
	if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
		http.Error(response, "invalid MCP headers", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10))
	if err != nil {
		http.Error(response, "request read failed", http.StatusBadRequest)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &message); err != nil || message.JSONRPC != "2.0" || message.Method == "" {
		http.Error(response, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	g.methodsMu.Lock()
	g.methods = append(g.methods, message.Method)
	g.methodsMu.Unlock()

	if message.Method == "notifications/initialized" {
		if len(message.ID) != 0 || request.Header.Get("Mcp-Session-Id") != "skills-session-1" {
			http.Error(response, "invalid initialization notification", http.StatusBadRequest)
			return
		}
		response.WriteHeader(http.StatusAccepted)
		return
	}
	if request.Header.Get("Mcp-Session-Id") != "" {
		if request.Header.Get("Mcp-Session-Id") != "skills-session-1" {
			http.Error(response, "invalid MCP session", http.StatusBadRequest)
			return
		}
	} else if message.Method != "initialize" {
		http.Error(response, "MCP session is required", http.StatusBadRequest)
		return
	}
	var id string
	if err := json.Unmarshal(message.ID, &id); err != nil || id == "" {
		http.Error(response, "MCP request ID must be a string", http.StatusBadRequest)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.Header().Set("Mcp-Session-Id", "skills-session-1")
	switch message.Method {
	case "initialize":
		writeSkillsJSON(response, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "strict-skills-fake", "version": "1"},
			},
		})
	case "tools/call":
		var params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			http.Error(response, "invalid tools/call params", http.StatusBadRequest)
			return
		}
		switch params.Name {
		case "skills_inspect":
			if params.Arguments["name"] != integrationSkillID {
				http.Error(response, "unexpected skill inspect", http.StatusBadRequest)
				return
			}
			writeSkillsJSON(response, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"structuredContent": map[string]any{
					"catalog": map[string]any{
						"id":     integrationSkillID,
						"source": map[string]any{"revision": "commit-preview-1", "repository": "example/skills"},
					},
					"files": []string{"SKILL.md", "docs/guide.md"},
				}},
			})
		case "skill_read":
			path := params.Arguments["path"]
			if !strings.HasPrefix(path, integrationSkillID+"/") {
				http.Error(response, "unexpected skill path", http.StatusBadRequest)
				return
			}
			content, ok := g.contents[strings.TrimPrefix(path, integrationSkillID+"/")]
			if !ok {
				http.Error(response, "missing skill file", http.StatusNotFound)
				return
			}
			writeSkillsJSON(response, map[string]any{
				"jsonrpc": "2.0", "id": id,
				"result": map[string]any{"content": []map[string]string{{"type": "text", "text": content}}},
			})
		default:
			http.Error(response, "unexpected MCP tool", http.StatusBadRequest)
		}
	default:
		http.Error(response, "unexpected MCP method", http.StatusBadRequest)
	}
}

func writeSkillsJSON(response http.ResponseWriter, value any) {
	_ = json.NewEncoder(response).Encode(value)
}

func (g *strictSkillsGateway) methodNames() []string {
	g.methodsMu.Lock()
	defer g.methodsMu.Unlock()
	return append([]string(nil), g.methods...)
}

type skillsIntegrationActivities struct {
	mu    sync.Mutex
	input workflow.ScheduleRunnerTaskInput
}

func (a *skillsIntegrationActivities) ScheduleRunnerTask(_ context.Context, input workflow.ScheduleRunnerTaskInput) (workflow.ScheduleRunnerTaskResult, error) {
	a.mu.Lock()
	a.input = input
	a.mu.Unlock()
	return workflow.ScheduleRunnerTaskResult{Status: "scheduled", TaskID: "skills-task-1"}, nil
}

func (a *skillsIntegrationActivities) CancelRunnerTask(context.Context, workflow.CancelRunnerTaskInput) error {
	return nil
}

func (a *skillsIntegrationActivities) ResumeRunnerTask(context.Context, workflow.ResumeRunnerTaskInput) error {
	return nil
}

func (a *skillsIntegrationActivities) StatusRunnerTask(context.Context, workflow.StatusRunnerTaskInput) (workflow.StatusRunnerTaskResult, error) {
	return workflow.StatusRunnerTaskResult{Status: "running"}, nil
}

func (a *skillsIntegrationActivities) scheduledInput() workflow.ScheduleRunnerTaskInput {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.input
}

type integrationPodmanProcess struct{}

func (integrationPodmanProcess) Wait() (int, error)     { return 0, nil }
func (integrationPodmanProcess) Signal(os.Signal) error { return nil }

type integrationPodmanFactory struct {
	mu     sync.Mutex
	starts [][]string
	runs   [][]string
}

func (f *integrationPodmanFactory) Start(_ context.Context, _ string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) (sandbox.PodmanProcess, error) {
	f.mu.Lock()
	f.starts = append(f.starts, append([]string(nil), args...))
	f.mu.Unlock()
	return integrationPodmanProcess{}, nil
}

func (f *integrationPodmanFactory) Run(_ context.Context, _ string, args []string, _ io.Writer, _ io.Writer) error {
	f.mu.Lock()
	f.runs = append(f.runs, append([]string(nil), args...))
	f.mu.Unlock()
	return nil
}

func (f *integrationPodmanFactory) startArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		return nil
	}
	return append([]string(nil), f.starts[0]...)
}

func TestFactorySkillsGatewayRunPathStagesImmutableSkillsAndMapsVerifiedMount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root cannot exercise the rootless Podman integration path")
	}
	gateway := newStrictSkillsGateway(t)
	fixture := newFactoryFixture(t, "")
	fixture.factory.Skills, _ = skills.NewGatewayClient(gateway.endpoint(), func(context.Context) (string, error) {
		gateway.credentialCalls.Add(1)
		return integrationGatewayKey, nil
	})
	skillSet := &spec.SkillSet{
		ResourceMeta: resourceMeta(spec.KindSkillSet, "preview-skills"),
		Spec:         spec.SkillSetSpec{Skills: []spec.SkillRef{{Ref: integrationSkillID, Digest: gateway.digest}}},
	}
	skillSetRef := applyResource(t, fixture.store, fixture.binding, skillSet)
	request := newHandlerRequest(fixture)
	request.Input.Contract.SkillSet = &skillSetRef

	inner := &skillsIntegrationActivities{}
	brokerRoot, err := os.MkdirTemp("/tmp", "agw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(brokerRoot) })
	t.Cleanup(func() { makeIntegrationTreeWritable(brokerRoot) })
	if err := os.Chmod(brokerRoot, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := runbroker.NewManager(runbroker.ManagerConfig{MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := brokerdispatch.New(inner, brokerdispatch.Config{
		BrokerRoot: brokerRoot,
		Manager:    manager,
		Factory:    fixture.factory,
		UserID:     fixture.binding.UserID,
		RunnerUID:  uint32(os.Geteuid()),
		RunnerGID:  uint32(os.Getegid()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dispatcher.Close() }()

	if _, err := dispatcher.ScheduleRunnerTask(context.Background(), request.Input); err != nil {
		t.Fatal(err)
	}
	prepared := inner.scheduledInput()
	if prepared.Execution.Network != runner.NetworkBrokered || prepared.Execution.BrokerSessionID == "" {
		t.Fatalf("broker session was not injected: %#v", prepared.Execution)
	}
	sessionDirectory := filepath.Join(brokerRoot, prepared.Execution.BrokerSessionID)
	skillsDirectory := filepath.Join(sessionDirectory, "skills")
	assertImmutableSkillTree(t, skillsDirectory, integrationGatewayKey, stringBytes(gateway.contents))
	assertNoSecretInRegularFiles(t, sessionDirectory, integrationGatewayKey)
	if gateway.credentialCalls.Load() != int64(1+len(gateway.contents)*2) {
		t.Fatalf("unexpected gateway credential calls: %d", gateway.credentialCalls.Load())
	}
	if got := gateway.methodNames(); !sameStrings(got, []string{"initialize", "notifications/initialized", "tools/call", "tools/call", "tools/call"}) {
		t.Fatalf("unexpected strict MCP method sequence: %#v", got)
	}

	podmanFactory := &integrationPodmanFactory{}
	backend, err := sandbox.NewRootlessPodmanBackend(sandbox.PodmanConfig{
		WorkspaceRoot: t.TempDir(), BrokerRoot: brokerRoot,
		Factory: podmanFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := backend.Start(context.Background(), integrationSandboxSpec(prepared.Execution.BrokerSessionID))
	if err != nil {
		t.Fatalf("verified broker session was not accepted by sandbox: %v", err)
	}
	if err := handle.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := podmanFactory.startArgs()
	joined := strings.Join(args, "\x00")
	wantSkillMount := "type=bind,src=" + skillsDirectory + ",dst=/skills,ro"
	if !strings.Contains(joined, wantSkillMount) {
		t.Fatalf("sandbox did not map verified skills directory to /skills: %q", joined)
	}
	if !strings.Contains(joined, "type=bind,src="+sessionDirectory+",dst=/run/agw,ro") || !strings.Contains(joined, "--network=none") {
		t.Fatalf("sandbox broker mount/network policy was not preserved: %q", joined)
	}

	skillFile := findIntegrationSkillFile(t, skillsDirectory)
	if err := os.Chmod(skillFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Start(context.Background(), integrationSandboxSpec(prepared.Execution.BrokerSessionID)); err == nil {
		t.Fatal("sandbox accepted a writable staged skill file")
	}
	if err := os.Chmod(skillFile, 0444); err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	skillDestination := filepath.Dir(skillFile)
	if err := os.Chmod(skillDestination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(skillFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", skillFile); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Start(context.Background(), integrationSandboxSpec(prepared.Execution.BrokerSessionID)); err == nil {
		t.Fatal("sandbox accepted a symlink in the staged skill tree")
	}
	if err := os.Remove(skillFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, original, 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skillDestination, 0555); err != nil {
		t.Fatal(err)
	}
	wrongOwnerSpec := integrationSandboxSpec(prepared.Execution.BrokerSessionID)
	wrongOwnerSpec.RunAsUser = strconv.Itoa(os.Geteuid()+1) + ":" + strconv.Itoa(os.Getegid())
	if _, err := backend.Start(context.Background(), wrongOwnerSpec); err == nil {
		t.Fatal("sandbox accepted a broker session for the wrong owner")
	}

	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.SessionCount() != 0 {
		t.Fatalf("broker session survived cleanup: %d", manager.SessionCount())
	}
	if entries, err := os.ReadDir(brokerRoot); err != nil || len(entries) != 0 {
		t.Fatalf("broker session directory survived cleanup: entries=%v err=%v", entries, err)
	}
}

func integrationSandboxSpec(sessionID string) runner.SandboxSpec {
	uid := os.Geteuid()
	gid := os.Getegid()
	return runner.SandboxSpec{
		Backend: runner.BackendPodman, IsolationGrade: runner.IsolationStandard,
		Image: "ghcr.io/example/agent:stable", ImageDigest: "sha256:" + strings.Repeat("a", 64),
		RunAsUser:      strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
		Network: runner.NetworkBrokered, BrokerSessionID: sessionID,
		Mounts: []runner.Mount{
			{Kind: "workspace", Destination: "/workspace"},
			{Kind: "skills", Destination: "/skills", ReadOnly: true},
			{Kind: "artifact", Destination: "/artifacts", ReadOnly: true},
		},
		Resources: runner.ResourceLimits{CPUs: 1, MemoryBytes: 256 << 20, DiskBytes: 1 << 30, PIDs: 64, Timeout: time.Minute},
	}
}

func assertImmutableSkillTree(t *testing.T, root, secret string, expected map[string][]byte) {
	t.Helper()
	if info, err := os.Stat(root); err != nil || !info.IsDir() || info.Mode().Perm() != 0555 {
		t.Fatalf("skills root mode/type = %v err=%v", info, err)
	}
	seen := make(map[string]struct{}, len(expected))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill tree contains symlink %s", path)
		}
		if entry.IsDir() {
			if info.Mode().Perm() != 0555 {
				return fmt.Errorf("skill directory %s mode=%o", path, info.Mode().Perm())
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0444 {
			return fmt.Errorf("skill file %s is not immutable: mode=%o", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			return errors.New("Skills Gateway credential was staged")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("skill file is not below an immutable materialization directory: %q", rel)
		}
		relativeFile := parts[1]
		want, ok := expected[relativeFile]
		if !ok || !bytes.Equal(data, want) {
			return fmt.Errorf("unexpected staged skill file %q", relativeFile)
		}
		seen[relativeFile] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(expected) {
		t.Fatalf("staged files=%v want=%v", seen, expected)
	}
}

func findIntegrationSkillFile(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			found = path
			return nil
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatal("staged SKILL.md was not found")
	}
	return found
}

func assertNoSecretInRegularFiles(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(secret)) {
			return fmt.Errorf("gateway credential found in staged file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func makeIntegrationTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0700)
		} else {
			_ = os.Chmod(path, 0600)
		}
		return nil
	})
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
