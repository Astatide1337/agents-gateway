package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"github.com/Astatide1337/agents-gateway/v2/proto"
)

func validRunLease() runner.Lease {
	return runner.Lease{RunID: "run-1", LeaseID: "lease-1", Owner: "runner-a", FencingToken: 4, ExpiresAt: time.Now().Add(time.Hour)}
}

type fakeBackend struct{ handle *fakeHandle }

func (f *fakeBackend) Name() runner.BackendKind { return runner.BackendPodman }
func (f *fakeBackend) Capabilities() runner.BackendCapabilities {
	return runner.RootlessPodmanCapabilities()
}
func (f *fakeBackend) Start(context.Context, runner.SandboxSpec) (runner.SandboxHandle, error) {
	return f.handle, nil
}

type fakeHandle struct {
	stdout   string
	commands bytes.Buffer
	status   runner.ExitStatus
	waitErr  error
	stops    atomic.Int32
}

func (f *fakeHandle) ID() string        { return "sandbox-1" }
func (f *fakeHandle) Stdout() io.Reader { return strings.NewReader(f.stdout) }
func (f *fakeHandle) Wait(context.Context) (runner.ExitStatus, error) {
	if f.status.FinishedAt.IsZero() {
		return runner.ExitStatus{Code: 0, StartedAt: time.Now(), FinishedAt: time.Now()}, nil
	}
	return f.status, f.waitErr
}
func (f *fakeHandle) Send(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := f.commands.Write(payload)
	return err
}
func (f *fakeHandle) Stop(context.Context) error { f.stops.Add(1); return nil }

func validRunContract() RunContract {
	return RunContract{OrganizationID: "org", ProjectID: "project", RunID: "run-1", WorkflowName: "wf", StepID: "step", AgentRef: "agent/a", InputRef: "ref/input", Execution: validExecutionContract()}
}

func validExecutionContract() agentworkflow.ExecutionContract {
	return agentworkflow.ExecutionContract{
		Agent:           agentworkflow.RevisionRef{Kind: "Agent", Name: "a", Digest: "sha256:" + strings.Repeat("c", 64)},
		SandboxProfile:  agentworkflow.RevisionRef{Kind: "SandboxProfile", Name: "profile", Digest: "sha256:" + strings.Repeat("d", 64)},
		InstructionsRef: "definition://sha256:" + strings.Repeat("c", 64) + "/spec/instructions",
		Instructions:    "Do the bounded test task.",
	}
}

func validRuntimeSpec() runner.SandboxSpec {
	return runner.SandboxSpec{
		Backend: runner.BackendPodman, IsolationGrade: runner.IsolationStandard,
		Image: "example/agent", ImageDigest: "sha256:" + strings.Repeat("a", 64), RunAsUser: "65532",
		ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, Network: runner.NetworkNone,
		Resources: runner.ResourceLimits{CPUs: 1, MemoryBytes: 256 << 20, DiskBytes: 1 << 30, PIDs: 128, Timeout: time.Minute},
		Mounts:    []runner.Mount{{Kind: "workspace", Destination: "/workspace"}},
	}
}

func validArtifact() agentworkflow.ArtifactRef {
	return agentworkflow.ArtifactRef{ID: "artifact-1", URI: "s3://artifacts/artifact-1", Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 2, MediaType: "application/json"}
}

func TestDaemonSupervisesEventsAndFencesCleanup(t *testing.T) {
	lease := validRunLease()
	handle := &fakeHandle{stdout: strings.Join([]string{
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.started","run_id":"run-1","seq":1,"terminal":false,"data":{"agent_id":"a","sandbox_id":"s"}}`,
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.completed","run_id":"run-1","seq":2,"terminal":true,"data":{"result":"ok","output":{"id":"artifact-1","uri":"s3://artifacts/artifact-1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":2,"media_type":"application/json"}}}`,
		"",
	}, "\n")}
	var seen int
	daemon := Daemon{
		Backend: &fakeBackend{handle: handle}, Leases: StaticLeaseSource{Lease: lease},
		Sink: func(context.Context, proto.Envelope) error { seen++; return nil },
	}
	result, err := daemon.Run(context.Background(), RunRequest{RunID: "run-1", Lease: lease, Spec: validRuntimeSpec(), Contract: validRunContract()})
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 || seen != 2 || handle.stops.Load() != 1 {
		t.Fatalf("unexpected result/events/cleanup: %#v seen=%d stops=%d", result, seen, handle.stops.Load())
	}
	command, err := runtimeproto.ParseLine(handle.commands.Bytes())
	if err != nil {
		t.Fatalf("parse run.start command: %v", err)
	}
	if command.Kind != proto.KindRequest || command.Type != proto.RequestRunStart || command.Seq != 1 {
		t.Fatalf("unexpected run.start command: %#v", command)
	}
	var contract RunContract
	if err := json.Unmarshal(command.Data, &contract); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(contract, validRunContract()) {
		t.Fatalf("adapter received wrong run contract: %#v", contract)
	}
}

func TestDaemonRejectsStaleLeaseBeforeStart(t *testing.T) {
	current := validRunLease()
	presented := current
	presented.FencingToken--
	handle := &fakeHandle{}
	_, err := (Daemon{Backend: &fakeBackend{handle: handle}, Leases: StaticLeaseSource{Lease: current}}).Run(context.Background(), RunRequest{RunID: current.RunID, Lease: presented, Spec: validRuntimeSpec(), Contract: validRunContract()})
	if !errors.Is(err, runner.ErrLeaseTokenStale) {
		t.Fatalf("expected stale lease, got %v", err)
	}
	if handle.stops.Load() != 0 {
		t.Fatal("sandbox was changed despite stale lease")
	}
}

func TestDaemonRejectsInvalidRuntimeStreamAndCleansUp(t *testing.T) {
	lease := validRunLease()
	handle := &fakeHandle{stdout: `{"protocol":"agw.runtime.v1","kind":"event","type":"run.completed","run_id":"run-1","seq":1,"terminal":true,"data":{"result":"bad"}}` + "\n"}
	_, err := (Daemon{Backend: &fakeBackend{handle: handle}, Leases: StaticLeaseSource{Lease: lease}}).Run(context.Background(), RunRequest{RunID: lease.RunID, Lease: lease, Spec: validRuntimeSpec(), Contract: validRunContract()})
	if err == nil || handle.stops.Load() != 1 {
		t.Fatalf("expected event validation failure and cleanup, err=%v stops=%d", err, handle.stops.Load())
	}
}

func TestDaemonEnforcesExitCodeAndTerminalEventState(t *testing.T) {
	tests := []struct {
		name     string
		terminal string
		data     string
		status   runner.ExitStatus
		wantType string
	}{
		{name: "completed requires zero exit", terminal: proto.EventRunCompleted, data: `{"result":"ok","output":{"id":"artifact-1","uri":"s3://artifacts/artifact-1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":2,"media_type":"application/json"}}`, status: runner.ExitStatus{Code: 7, StartedAt: time.Now(), FinishedAt: time.Now()}},
		{name: "failed terminal", terminal: proto.EventRunFailed, data: `{"error":"adapter_failed"}`, status: runner.ExitStatus{Code: 0, StartedAt: time.Now(), FinishedAt: time.Now()}, wantType: proto.EventRunFailed},
		{name: "cancelled terminal", terminal: proto.EventRunCancelled, data: `{"reason":"requested"}`, status: runner.ExitStatus{Code: 0, StartedAt: time.Now(), FinishedAt: time.Now()}, wantType: proto.EventRunCancelled},
		{name: "unknown effect terminal", terminal: proto.EventUnknownEffect, data: `{"effect_id":"effect-1"}`, status: runner.ExitStatus{Code: 0, StartedAt: time.Now(), FinishedAt: time.Now()}, wantType: proto.EventUnknownEffect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := validRunLease()
			handle := &fakeHandle{stdout: strings.Join([]string{
				`{"protocol":"agw.runtime.v1","kind":"event","type":"run.started","run_id":"run-1","seq":1,"terminal":false,"data":{"agent_id":"a","sandbox_id":"s"}}`,
				fmt.Sprintf(`{"protocol":"agw.runtime.v1","kind":"event","type":%q,"run_id":"run-1","seq":2,"terminal":true,"data":%s}`, test.terminal, test.data),
				"",
			}, "\n"), status: test.status}
			_, err := (Daemon{Backend: &fakeBackend{handle: handle}, Leases: StaticLeaseSource{Lease: lease}}).Run(context.Background(), RunRequest{RunID: lease.RunID, Lease: lease, Spec: validRuntimeSpec(), Contract: validRunContract()})
			if err == nil {
				t.Fatal("expected terminal outcome failure")
			}
			if test.wantType != "" {
				var terminalErr *TerminalOutcomeError
				if !errors.As(err, &terminalErr) || terminalErr.Type != test.wantType {
					t.Fatalf("expected terminal type %q, got %v", test.wantType, err)
				}
			}
		})
	}
}

func TestDaemonPreservesFailedTerminalEventWhenProcessExitsNonZero(t *testing.T) {
	lease := validRunLease()
	handle := &fakeHandle{
		stdout: strings.Join([]string{
			`{"protocol":"agw.runtime.v1","kind":"event","type":"run.started","run_id":"run-1","seq":1,"terminal":false,"data":{"agent_id":"a","sandbox_id":"s"}}`,
			`{"protocol":"agw.runtime.v1","kind":"event","type":"run.failed","run_id":"run-1","seq":2,"terminal":true,"data":{"error":{"code":"adapter_failed","message":"bounded failure"}}}`,
			"",
		}, "\n"),
		status:  runner.ExitStatus{Code: 1, StartedAt: time.Now(), FinishedAt: time.Now()},
		waitErr: errors.New("exit status 1"),
	}
	var seen int
	_, err := (Daemon{
		Backend: &fakeBackend{handle: handle}, Leases: StaticLeaseSource{Lease: lease},
		Sink: func(context.Context, proto.Envelope) error { seen++; return nil },
	}).Run(context.Background(), RunRequest{RunID: lease.RunID, Lease: lease, Spec: validRuntimeSpec(), Contract: validRunContract()})
	var terminalErr *TerminalOutcomeError
	if !errors.As(err, &terminalErr) || terminalErr.Type != proto.EventRunFailed {
		t.Fatalf("failed terminal event was hidden by process exit: %v", err)
	}
	if errors.Is(err, ErrRuntimeWait) {
		t.Fatalf("expected failed terminal exit was misclassified as runtime wait failure: %v", err)
	}
	if seen != 2 {
		t.Fatalf("sink observed %d events, want 2", seen)
	}
}

func mtlsRequest(method, path string, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Raw: []byte("client-cert")}}}
	return request
}

func scheduleJSON() string {
	data, _ := json.Marshal(ScheduleTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf", StepID: "step", AgentRef: "agent/a", IdempotencyKey: "run/wf/step", Execution: validRuntimeSpec(), Contract: validExecutionContract()})
	return string(data)
}

func TestHTTPDispatchContract(t *testing.T) {
	service := NewInMemoryDispatch()
	handler := &HTTPServer{Service: service, Report: runner.ReadinessReport{Ready: true}}

	record := httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/schedule", scheduleJSON()))
	if record.Code != http.StatusOK || !strings.Contains(record.Body.String(), `"status":"scheduled"`) || !strings.Contains(record.Body.String(), `"task_id"`) {
		t.Fatalf("schedule response: %d %s", record.Code, record.Body.String())
	}
	var scheduled agentworkflow.ScheduleRunnerTaskResult
	if err := json.Unmarshal(record.Body.Bytes(), &scheduled); err != nil {
		t.Fatal(err)
	}

	statusBody, _ := json.Marshal(agentworkflow.StatusRunnerTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID})
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/status", string(statusBody)))
	if record.Code != http.StatusOK || !strings.Contains(record.Body.String(), `"status":"scheduled"`) {
		t.Fatalf("status response without idempotency key: %d %s", record.Code, record.Body.String())
	}

	cancel := agentworkflow.CancelRunnerTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID, IdempotencyKey: "cancel/" + scheduled.TaskID}
	body, _ := json.Marshal(cancel)
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/cancel", string(body)))
	if record.Code != http.StatusNoContent || record.Body.Len() != 0 {
		t.Fatalf("cancel response: %d %q", record.Code, record.Body.String())
	}

	record = httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/schedule", strings.TrimSuffix(scheduleJSON(), "}")+`,"unknown":true}`))
	if record.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status: %d", record.Code)
	}

	record = httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/schedule", scheduleJSON()+"{}"))
	if record.Code != http.StatusBadRequest {
		t.Fatalf("trailing value status: %d", record.Code)
	}
}

func TestHTTPRejectsBrokeredNetworkBeforeTaskPersistence(t *testing.T) {
	service := NewInMemoryDispatch()
	handler := &HTTPServer{Service: service, Report: runner.ReadinessReport{Ready: true}}
	input := ScheduleTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf",
		StepID: "step", AgentRef: "agent/a", IdempotencyKey: "brokered-network",
		Execution: validRuntimeSpec(), Contract: validExecutionContract(),
	}
	input.Execution.Network = runner.NetworkBrokered
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/schedule", string(body)))
	if record.Code != http.StatusConflict || !strings.Contains(record.Body.String(), "task request rejected") {
		t.Fatalf("brokered schedule response: %d %s", record.Code, record.Body.String())
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.tasks) != 0 {
		t.Fatalf("rejected brokered task was persisted: %#v", service.tasks)
	}
}

func TestHTTPRequiresMTLSAndBoundsBody(t *testing.T) {
	handler := &HTTPServer{Service: NewInMemoryDispatch(), Report: runner.ReadinessReport{Ready: true}}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	if record.Code != http.StatusUnauthorized {
		t.Fatalf("expected mTLS rejection, got %d", record.Code)
	}
	large := bytes.Repeat([]byte("x"), int(maxDispatchBodyBytes)+1)
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, mtlsRequest(http.MethodPost, "/v1/tasks/schedule", string(large)))
	if record.Code != http.StatusBadRequest {
		t.Fatalf("expected bounded body rejection, got %d", record.Code)
	}
}

func TestUnixRunnerRejectsMissingPeerCredentials(t *testing.T) {
	handler := &HTTPServer{
		Config:  Config{Server: ServerTLSConfig{Transport: "unix", SocketPath: "/run/agw-runner/runner.sock"}},
		Service: NewInMemoryDispatch(),
		Report:  runner.ReadinessReport{Ready: true},
	}
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if record.Code != http.StatusUnauthorized {
		t.Fatalf("Unix request without peer credentials status=%d", record.Code)
	}
}

func TestUnixPeerIdentityComesFromKernelCredentials(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "peer.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serverConnection := <-accepted
	defer serverConnection.Close()
	identity, err := unixPeerIdentity(serverConnection)
	if err != nil {
		t.Fatalf("peer identity: %v", err)
	}
	want := fmt.Sprintf("unix:uid=%d:gid=%d", os.Geteuid(), os.Getegid())
	if identity != want {
		t.Fatalf("identity=%q want=%q", identity, want)
	}
}

func TestUnixRunnerAcceptsOnlyExplicitUnixTransport(t *testing.T) {
	handler := &HTTPServer{
		Config:  Config{Server: ServerTLSConfig{Transport: "unix", SocketPath: "/run/agw-runner/runner.sock"}},
		Service: NewInMemoryDispatch(),
		Report:  runner.ReadinessReport{Ready: true},
	}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request = request.WithContext(context.WithValue(request.Context(), unixPeerContextKey{}, "unix:uid=65532:gid=65532"))
	record := httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	if record.Code != http.StatusOK {
		t.Fatalf("explicit Unix transport status=%d body=%s", record.Code, record.Body.String())
	}

	handler.Config.Server.Transport = "mtls"
	record = httptest.NewRecorder()
	handler.ServeHTTP(record, request)
	if record.Code != http.StatusUnauthorized {
		t.Fatalf("mTLS transport accepted certificate-less request: %d", record.Code)
	}
}

func TestUnixServerConfigRequiresPrivateSocketDirectory(t *testing.T) {
	directory := t.TempDir()
	config := ServerTLSConfig{Transport: "unix", SocketPath: filepath.Join(directory, "runner.sock")}
	if err := config.Validate(); err == nil {
		t.Fatal("world-accessible socket directory must be rejected")
	}
	if err := os.Chmod(directory, 0750); err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("private Unix socket config: %v", err)
	}
}

func TestUnixSocketLockPreventsSecondRunnerAndCleansOwnInode(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0750); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "runner.sock")
	_, release, err := acquireUnixSocketLock(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acquireUnixSocketLock(socketPath); err == nil {
		t.Fatal("second runner acquired the same socket lock")
	}
	replacement := socketPath + ".lock.replacement"
	if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, socketPath+".lock"); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(socketPath + ".lock"); err != nil {
		t.Fatalf("release removed a replacement lock inode: %v", err)
	}
}

func TestInMemoryDispatchBindsOwnerAndIdempotency(t *testing.T) {
	service := NewInMemoryDispatch()
	input := ScheduleTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf", StepID: "step", AgentRef: "agent/a", IdempotencyKey: "key", Execution: validRuntimeSpec(), Contract: validExecutionContract()}
	one, err := service.Schedule(context.Background(), input, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	two, err := service.Schedule(context.Background(), input, "owner-a")
	if err != nil || two.TaskID != one.TaskID {
		t.Fatalf("idempotency failed: %#v err=%v", two, err)
	}
	if _, err := service.Schedule(context.Background(), input, "owner-b"); err == nil {
		t.Fatal("different mTLS owner reused task")
	}
	otherTenant := input
	otherTenant.OrganizationID = "other-org"
	other, err := service.Schedule(context.Background(), otherTenant, "owner-a")
	if err != nil || other.TaskID == one.TaskID {
		t.Fatalf("tenant-scoped idempotency failed: %#v err=%v", other, err)
	}
}

func TestResumeCommandsContinueRunnerRequestSequence(t *testing.T) {
	handle := &fakeHandle{}
	service := NewInMemoryDispatch()
	input := ScheduleTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf", StepID: "step", AgentRef: "agent/a", IdempotencyKey: "sequence-key", Execution: validRuntimeSpec(), Contract: validExecutionContract()}
	scheduled, err := service.Schedule(context.Background(), input, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	service.active[scheduled.TaskID] = &activeTask{handle: handle, requestSeq: 1}
	service.tasks[taskStorageKey(input)].State = "running"
	service.mu.Unlock()
	for index, reference := range []string{"s3://reply/one", "s3://reply/two"} {
		err := service.Resume(context.Background(), agentworkflow.ResumeRunnerTaskInput{
			OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID,
			Action: "user-reply", Reference: reference, IdempotencyKey: fmt.Sprintf("reply-%d", index),
		}, "owner-a")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Resume(context.Background(), agentworkflow.ResumeRunnerTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID,
		Action: "user-reply", Reference: "s3://reply/one", IdempotencyKey: "reply-0",
	}, "owner-a"); err != nil {
		t.Fatal(err)
	}
	decoder := runtimeproto.NewDecoder(bytes.NewReader(handle.commands.Bytes()))
	for want := uint64(2); want <= 3; want++ {
		frame, err := decoder.Next()
		if err != nil {
			t.Fatal(err)
		}
		if frame.Seq != want || frame.Type != proto.RequestInput {
			t.Fatalf("request frame=%#v want seq=%d", frame, want)
		}
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("idempotent retry sent an extra frame: %v", err)
	}
}

func TestPersistentDispatchRunsAndRestoresTerminalState(t *testing.T) {
	root := t.TempDir()
	backend := &fakeBackend{handle: &fakeHandle{stdout: strings.Join([]string{
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.started","run_id":"run","seq":1,"terminal":false,"data":{"agent_id":"agent","sandbox_id":"sandbox"}}`,
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.completed","run_id":"run","seq":2,"terminal":true,"data":{"result":"ok","output":{"id":"artifact-1","uri":"s3://artifacts/artifact-1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":2,"media_type":"application/json"}}}`,
		"",
	}, "\n")}}
	service, err := NewPersistentDispatch(root, backend)
	if err != nil {
		t.Fatal(err)
	}
	input := ScheduleTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf", StepID: "step", AgentRef: "agent/a", IdempotencyKey: "persistent-key", Execution: validRuntimeSpec(), Contract: validExecutionContract()}
	scheduled, err := service.Schedule(context.Background(), input, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	var status TaskStatusResult
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, err = service.Status(context.Background(), StatusTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID}, "owner-a")
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == "succeeded" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if status.Status != "succeeded" {
		t.Fatalf("task did not reach terminal success: %#v", status)
	}
	if status.Output != validArtifact() {
		t.Fatalf("terminal artifact was not persisted into status: %#v", status.Output)
	}

	restarted, err := NewPersistentDispatch(root, &fakeBackend{handle: &fakeHandle{stdout: ""}})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.Status(context.Background(), StatusTaskInput{OrganizationID: "org", ProjectID: "project", RunID: "run", TaskID: scheduled.TaskID}, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != "succeeded" {
		t.Fatalf("terminal state was not restored: %#v", restored)
	}
}

func TestStatusReturnsCursorBoundedRedactedRuntimeHistory(t *testing.T) {
	service := NewInMemoryDispatch()
	input := ScheduleTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "run", WorkflowName: "wf", StepID: "step",
		AgentRef: "agent/a", IdempotencyKey: "history-key", Execution: validRuntimeSpec(), Contract: validExecutionContract(),
	}
	scheduled, err := service.Schedule(context.Background(), input, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	frames := []proto.Envelope{
		{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventRunStarted, RunID: input.RunID, Seq: 1, Data: json.RawMessage(`{"agent_id":"agent","sandbox_id":"sandbox"}`)},
		{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventHeartbeat, RunID: input.RunID, Seq: 2, Data: json.RawMessage(`{}`)},
		{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventAssistantMessage, RunID: input.RunID, Seq: 3, Data: json.RawMessage(`{"message":"Authorization: Bearer glsa_live-secret"}`)},
		{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventModelCompleted, RunID: input.RunID, Seq: 4, Data: json.RawMessage(`{"response":{"api_key":"hidden","ok":true}}`)},
	}
	for _, frame := range frames {
		if err := service.observe(scheduled.TaskID, frame); err != nil {
			t.Fatalf("observe seq=%d: %v", frame.Seq, err)
		}
	}
	status, err := service.Status(context.Background(), StatusTaskInput{
		OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID,
		TaskID: scheduled.TaskID, AfterSequence: 1,
	}, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if status.EventCursor != 4 || len(status.Events) != 2 {
		t.Fatalf("unexpected cursor/history: %#v", status)
	}
	if status.Events[0].Sequence != 3 || status.Events[1].Sequence != 4 {
		t.Fatalf("unexpected source sequences: %#v", status.Events)
	}
	encoded, _ := json.Marshal(status.Events)
	if strings.Contains(string(encoded), "glsa_live-secret") || strings.Contains(string(encoded), "hidden") || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("runtime history was not privacy-safe: %s", encoded)
	}

	for sequence := uint64(5); sequence < 5+maxRuntimeEventHistory+48; sequence++ {
		frame := proto.Envelope{
			Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: proto.EventAssistantMessage,
			RunID: input.RunID, Seq: sequence, Data: json.RawMessage(`{"message":"bounded"}`),
		}
		if err := service.observe(scheduled.TaskID, frame); err != nil {
			t.Fatalf("observe bounded seq=%d: %v", sequence, err)
		}
	}
	status, err = service.Status(context.Background(), StatusTaskInput{
		OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, TaskID: scheduled.TaskID,
	}, "owner-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Events) > maxRuntimeEventHistory || !status.EventHistoryTruncated || status.EventHistoryStart == 0 {
		t.Fatalf("history bound/truncation not reported: len=%d start=%d truncated=%t", len(status.Events), status.EventHistoryStart, status.EventHistoryTruncated)
	}
}

func TestPersistentDispatchLoadsLegacyTaskWithoutRuntimeHistory(t *testing.T) {
	root := t.TempDir()
	input := ScheduleTaskInput{
		OrganizationID: "org", ProjectID: "project", RunID: "legacy-run", WorkflowName: "wf", StepID: "step",
		AgentRef: "agent/a", IdempotencyKey: "legacy-key", Execution: validRuntimeSpec(), Contract: validExecutionContract(),
	}
	task := persistedTask{
		Input: input, Result: agentworkflow.ScheduleRunnerTaskResult{Status: "succeeded", TaskID: "task-legacy"},
		Owner: "owner-a", Lease: runner.Lease{RunID: input.RunID, LeaseID: "task-legacy", Owner: "owner-a", FencingToken: 1, ExpiresAt: time.Now().Add(time.Hour)},
		State: "succeeded", UpdatedAt: time.Now(),
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "runtime_events")
	delete(legacy, "runtime_event_cursor")
	delete(legacy, "runtime_event_history_start")
	delete(legacy, "runtime_history_truncated")
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tasks", stableTaskID(taskStorageKey(input))+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := NewPersistentDispatch(root, &fakeBackend{handle: &fakeHandle{}})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Status(context.Background(), StatusTaskInput{
		OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, TaskID: task.Result.TaskID,
	}, "owner-a")
	if err != nil || status.Status != "succeeded" || len(status.Events) != 0 {
		t.Fatalf("legacy task was not compatible: status=%#v err=%v", status, err)
	}
}
