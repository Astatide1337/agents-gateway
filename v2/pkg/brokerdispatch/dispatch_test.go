package brokerdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

type fakeActivities struct {
	mu             sync.Mutex
	scheduleInputs []workflow.ScheduleRunnerTaskInput
	cancelCalls    int
	status         string
	nextTaskID     string
}

func (f *fakeActivities) ScheduleRunnerTask(_ context.Context, input workflow.ScheduleRunnerTaskInput) (workflow.ScheduleRunnerTaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scheduleInputs = append(f.scheduleInputs, input)
	taskID := f.nextTaskID
	if taskID == "" {
		taskID = "task-1"
	}
	return workflow.ScheduleRunnerTaskResult{Status: "scheduled", TaskID: taskID}, nil
}

func (f *fakeActivities) CancelRunnerTask(context.Context, workflow.CancelRunnerTaskInput) error {
	f.mu.Lock()
	f.cancelCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeActivities) ResumeRunnerTask(context.Context, workflow.ResumeRunnerTaskInput) error {
	return nil
}

func (f *fakeActivities) StatusRunnerTask(context.Context, workflow.StatusRunnerTaskInput) (workflow.StatusRunnerTaskResult, error) {
	f.mu.Lock()
	status := f.status
	f.mu.Unlock()
	if status == "" {
		status = "running"
	}
	return workflow.StatusRunnerTaskResult{Status: status}, nil
}

func (f *fakeActivities) inputs() []workflow.ScheduleRunnerTaskInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workflow.ScheduleRunnerTaskInput(nil), f.scheduleInputs...)
}

type countingFactory struct {
	calls  atomic.Int32
	secret string
	result HandlerSpec
}

func (f *countingFactory) NewHandler(context.Context, HandlerRequest) (HandlerSpec, error) {
	f.calls.Add(1)
	if f.secret != "" {
		return HandlerSpec{}, errors.New(f.secret)
	}
	return f.result, nil
}

func newTestDispatcher(t *testing.T, inner *fakeActivities, factory HandlerFactory) (*Activities, *runbroker.Manager) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "agw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	manager, err := runbroker.NewManager(runbroker.ManagerConfig{MaxTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := New(inner, Config{
		BrokerRoot: root,
		Manager:    manager,
		Factory:    factory,
		UserID:     "user-1",
		RunnerUID:  uint32(os.Geteuid()),
		RunnerGID:  uint32(os.Getegid()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	return dispatcher, manager
}

func brokerFactory() *countingFactory {
	return &countingFactory{result: HandlerSpec{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		}),
		AllowedModel: "model-a",
		PolicyDigest: "sha256:" + strings.Repeat("a", 64),
	}}
}

func brokerInput() workflow.ScheduleRunnerTaskInput {
	return workflow.ScheduleRunnerTaskInput{
		OrganizationID: "org-1",
		ProjectID:      "project-1",
		RunID:          "run-1",
		WorkflowName:   "workflow-1",
		StepID:         "step-1",
		AgentRef:       "agent-1",
		IdempotencyKey: "idem-1",
		Execution:      runner.SandboxSpec{Network: runner.NetworkNone},
		Contract: workflow.ExecutionContract{
			ModelRoute: &workflow.RevisionRef{
				Kind: "ModelRoute", Name: "route-1", Digest: "sha256:" + strings.Repeat("b", 64),
			},
		},
	}
}

func readClient(t *testing.T, root, sessionID string) (clientConfig, string) {
	t.Helper()
	path := filepath.Join(root, sessionID, "client.json")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != clientFileMode || !info.Mode().IsRegular() {
		t.Fatalf("client config mode/type = %v, want regular %o", info.Mode(), clientFileMode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config clientConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config, string(data)
}

func TestScheduleCreatesPrivateBrokerAndWritesMinimalClientConfig(t *testing.T) {
	inner := &fakeActivities{}
	factory := brokerFactory()
	dispatcher, manager := newTestDispatcher(t, inner, factory)

	input := brokerInput()
	result, err := dispatcher.ScheduleRunnerTask(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != "task-1" || factory.calls.Load() != 1 {
		t.Fatalf("schedule result/factory calls = %#v/%d", result, factory.calls.Load())
	}
	got := inner.inputs()
	if len(got) != 1 {
		t.Fatalf("underlying schedule calls = %d", len(got))
	}
	prepared := got[0]
	if prepared.Execution.Network != runner.NetworkBrokered || prepared.Execution.BrokerSessionID == "" {
		t.Fatalf("broker execution was not injected: %#v", prepared.Execution)
	}
	if input.Execution.BrokerSessionID != "" || input.Execution.Network != runner.NetworkNone {
		t.Fatal("caller-owned SandboxSpec was mutated")
	}

	root := dispatcher.root
	client, raw := readClient(t, root, prepared.Execution.BrokerSessionID)
	if client.SessionID != prepared.Execution.BrokerSessionID || client.BearerToken == "" || client.AllowedModel != "model-a" || client.PolicyDigest != factory.result.PolicyDigest {
		t.Fatalf("unexpected client config: %#v", client)
	}
	if client.ModelURL != ModelLoopbackURL || client.ToolsURL != ToolsLoopbackURL {
		t.Fatalf("loopback URLs = %q/%q", client.ModelURL, client.ToolsURL)
	}
	if client.ArtifactURL != ArtifactLoopbackURL || !client.ArtifactEnabled || client.ToolsEnabled {
		t.Fatalf("unexpected broker capabilities: %#v", client)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session_id", "bearer_token", "policy_digest", "allowed_model", "model_url", "tools_url", "artifact_url", "tools_enabled", "artifact_enabled"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("client config missing field %q", field)
		}
	}
	if len(fields) != 9 || strings.Contains(raw, "credential") || strings.Contains(raw, "secret") {
		t.Fatalf("client config contains unexpected data: %s", raw)
	}
	if manager.SessionCount() != 1 {
		t.Fatalf("manager sessions before cleanup = %d", manager.SessionCount())
	}

	response := authorizedUnixRequest(t, root, client, prepared.Execution.BrokerSessionID)
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("broker handler status = %d body=%q", response.StatusCode, body)
	}
	_ = response.Body.Close()

	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	if manager.SessionCount() != 0 {
		t.Fatalf("manager sessions after cleanup = %d", manager.SessionCount())
	}
	if _, err := os.Stat(filepath.Join(root, prepared.Execution.BrokerSessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned session directory after cleanup: %v", err)
	}
}

func TestRetriesReuseOneLiveSessionAndConcurrentIdentity(t *testing.T) {
	inner := &fakeActivities{}
	factory := brokerFactory()
	dispatcher, manager := newTestDispatcher(t, inner, factory)
	input := brokerInput()

	const calls = 32
	results := make(chan workflow.ScheduleRunnerTaskResult, calls)
	errorsCh := make(chan error, calls)
	var group sync.WaitGroup
	for i := 0; i < calls; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := dispatcher.ScheduleRunnerTask(context.Background(), input)
			results <- result
			errorsCh <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)
	var sessionID string
	for result := range results {
		if result.TaskID != "task-1" {
			t.Fatalf("unexpected task result: %#v", result)
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, scheduled := range inner.inputs() {
		if sessionID == "" {
			sessionID = scheduled.Execution.BrokerSessionID
		}
		if scheduled.Execution.BrokerSessionID != sessionID {
			t.Fatal("concurrent retry created multiple broker sessions")
		}
	}
	if factory.calls.Load() != 1 || len(inner.inputs()) != calls || manager.SessionCount() != 1 {
		t.Fatalf("factory/underlying/sessions = %d/%d/%d", factory.calls.Load(), len(inner.inputs()), manager.SessionCount())
	}
}

func TestIdentityMismatchDoesNotReuseSession(t *testing.T) {
	inner := &fakeActivities{}
	dispatcher, manager := newTestDispatcher(t, inner, brokerFactory())
	input := brokerInput()
	if _, err := dispatcher.ScheduleRunnerTask(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.AgentRef = "different-agent"
	if _, err := dispatcher.ScheduleRunnerTask(context.Background(), changed); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("identity mismatch error = %v", err)
	}
	if len(inner.inputs()) != 1 || manager.SessionCount() != 1 {
		t.Fatalf("mismatched request crossed boundary or changed session state: calls=%d sessions=%d", len(inner.inputs()), manager.SessionCount())
	}
}

func TestToolSetOnlyAlsoUsesBrokeredMode(t *testing.T) {
	inner := &fakeActivities{}
	dispatcher, manager := newTestDispatcher(t, inner, brokerFactory())
	input := brokerInput()
	input.Contract.ModelRoute = nil
	input.Contract.ToolSet = &workflow.RevisionRef{
		Kind: "ToolSet", Name: "tools-1", Digest: "sha256:" + strings.Repeat("c", 64),
	}
	if _, err := dispatcher.ScheduleRunnerTask(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	prepared := inner.inputs()
	if len(prepared) != 1 || prepared[0].Execution.Network != runner.NetworkBrokered || prepared[0].Execution.BrokerSessionID == "" {
		t.Fatalf("tool-only task was not brokered: %#v", prepared)
	}
	if manager.SessionCount() != 1 {
		t.Fatalf("tool-only task session count = %d", manager.SessionCount())
	}
}

func TestCancelAndTerminalStatusRevokeDeleteAndRemove(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		inner := &fakeActivities{}
		dispatcher, manager := newTestDispatcher(t, inner, brokerFactory())
		input := brokerInput()
		result, err := dispatcher.ScheduleRunnerTask(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatcher.CancelRunnerTask(context.Background(), workflow.CancelRunnerTaskInput{TaskID: result.TaskID, IdempotencyKey: input.IdempotencyKey}); err != nil {
			t.Fatal(err)
		}
		if manager.SessionCount() != 0 || len(rootEntries(t, dispatcher.root)) != 0 {
			t.Fatalf("cancel left broker state: sessions=%d entries=%v", manager.SessionCount(), rootEntries(t, dispatcher.root))
		}
	})

	t.Run("terminal status", func(t *testing.T) {
		inner := &fakeActivities{status: "succeeded"}
		dispatcher, manager := newTestDispatcher(t, inner, brokerFactory())
		result, err := dispatcher.ScheduleRunnerTask(context.Background(), brokerInput())
		if err != nil {
			t.Fatal(err)
		}
		status, err := dispatcher.StatusRunnerTask(context.Background(), workflow.StatusRunnerTaskInput{TaskID: result.TaskID})
		if err != nil || status.Status != "succeeded" {
			t.Fatalf("terminal status = %#v, error = %v", status, err)
		}
		if manager.SessionCount() != 0 || len(rootEntries(t, dispatcher.root)) != 0 {
			t.Fatalf("terminal status left broker state: sessions=%d entries=%v", manager.SessionCount(), rootEntries(t, dispatcher.root))
		}
	})
}

func TestReconcileAndPathSafety(t *testing.T) {
	inner := &fakeActivities{}
	dispatcher, _ := newTestDispatcher(t, inner, brokerFactory())
	stale := filepath.Join(dispatcher.root, "ags_stale")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory was not removed: %v", err)
	}

	outside := t.TempDir()
	marker := filepath.Join(outside, "must-survive")
	if err := os.WriteFile(marker, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dispatcher.root, "ags_symlink")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Reconcile(context.Background()); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink reconciliation error = %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reconcile followed symlink: %v", err)
	}

	rootTarget := t.TempDir()
	rootLink := filepath.Join(t.TempDir(), "broker-root")
	if err := os.Symlink(rootTarget, rootLink); err != nil {
		t.Fatal(err)
	}
	manager, err := runbroker.NewManager(runbroker.ManagerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(inner, Config{BrokerRoot: rootLink, Manager: manager, Factory: brokerFactory(), UserID: "user-1", RunnerUID: uint32(os.Geteuid()), RunnerGID: uint32(os.Getegid())}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink root error = %v", err)
	}
}

func TestFactoryErrorsAreNonSecret(t *testing.T) {
	secret := "provider-token-never-return-this"
	inner := &fakeActivities{}
	dispatcher, manager := newTestDispatcher(t, inner, &countingFactory{secret: secret})
	_, err := dispatcher.ScheduleRunnerTask(context.Background(), brokerInput())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("factory error leaked or was absent: %v", err)
	}
	if manager.SessionCount() != 0 || len(inner.inputs()) != 0 {
		t.Fatalf("failed factory changed state: sessions=%d calls=%d", manager.SessionCount(), len(inner.inputs()))
	}
}

func authorizedUnixRequest(t *testing.T, root string, client clientConfig, sessionID string) *http.Response {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", filepath.Join(root, sessionID, "broker.sock"))
	}}
	httpClient := &http.Client{Transport: transport}
	request, err := http.NewRequest(http.MethodPost, "http://broker.invalid/v1", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(runbroker.HeaderAuthorization, "Bearer "+client.BearerToken)
	request.Header.Set(runbroker.HeaderSessionID, client.SessionID)
	request.Header.Set(runbroker.HeaderModel, client.AllowedModel)
	request.Header.Set(runbroker.HeaderPolicyDigest, client.PolicyDigest)
	for attempt := 0; attempt < 20; attempt++ {
		response, err := httpClient.Do(request)
		if err == nil {
			return response
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("private broker socket did not become reachable")
	return nil
}

func rootEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}
