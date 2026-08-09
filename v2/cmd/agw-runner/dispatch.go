package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v2/pkg/sandbox"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"github.com/Astatide1337/agents-gateway/v2/proto"
	"golang.org/x/sys/unix"
)

const maxDispatchBodyBytes int64 = 1 << 20

const (
	// Runtime history is deliberately small enough to fit in one bounded status
	// response. The source cursor remains monotonic when older entries are
	// evicted, and the truncation marker lets consumers detect a missed window.
	maxRuntimeEventHistory      = 256
	maxRuntimeEventHistoryBytes = 896 << 10
)

// ScheduleTaskInput is the HTTP wire contract. The workflow package will add
// the same execution field; this local wire type lets the runner evolve
// without editing workflow or runnerdispatch.
type ScheduleTaskInput struct {
	OrganizationID   string                          `json:"organization_id"`
	ProjectID        string                          `json:"project_id"`
	RunID            string                          `json:"run_id"`
	WorkflowName     string                          `json:"workflow_name"`
	StepID           string                          `json:"step_id"`
	AgentRef         string                          `json:"agent_ref"`
	InputRef         string                          `json:"input_ref,omitempty"`
	DependencyOutput []agentworkflow.ArtifactRef     `json:"dependency_output,omitempty"`
	IdempotencyKey   string                          `json:"idempotency_key"`
	Execution        runner.SandboxSpec              `json:"execution"`
	Contract         agentworkflow.ExecutionContract `json:"contract"`
}

type StatusTaskInput = agentworkflow.StatusRunnerTaskInput

type TaskStatusResult struct {
	Status                string                             `json:"status"`
	Error                 string                             `json:"error,omitempty"`
	ApprovalID            string                             `json:"approval_id,omitempty"`
	Output                agentworkflow.ArtifactRef          `json:"output,omitempty"`
	Events                []agentworkflow.RunnerRuntimeEvent `json:"events,omitempty"`
	EventCursor           uint64                             `json:"event_cursor,omitempty"`
	EventHistoryStart     uint64                             `json:"event_history_start,omitempty"`
	EventHistoryTruncated bool                               `json:"event_history_truncated,omitempty"`
}

type DispatchService interface {
	Schedule(context.Context, ScheduleTaskInput, string) (agentworkflow.ScheduleRunnerTaskResult, error)
	Cancel(context.Context, agentworkflow.CancelRunnerTaskInput, string) error
	Resume(context.Context, agentworkflow.ResumeRunnerTaskInput, string) error
	Status(context.Context, StatusTaskInput, string) (TaskStatusResult, error)
}

type persistedTask struct {
	Input                    ScheduleTaskInput                      `json:"input"`
	Result                   agentworkflow.ScheduleRunnerTaskResult `json:"result"`
	Owner                    string                                 `json:"owner"`
	Lease                    runner.Lease                           `json:"lease"`
	State                    string                                 `json:"state"`
	Error                    string                                 `json:"error,omitempty"`
	ApprovalID               string                                 `json:"approval_id,omitempty"`
	Commands                 map[string]persistedCommand            `json:"commands,omitempty"`
	RuntimeEvents            []agentworkflow.RunnerRuntimeEvent     `json:"runtime_events,omitempty"`
	RuntimeEventCursor       uint64                                 `json:"runtime_event_cursor,omitempty"`
	RuntimeEventHistoryStart uint64                                 `json:"runtime_event_history_start,omitempty"`
	RuntimeHistoryTruncated  bool                                   `json:"runtime_history_truncated,omitempty"`
	UpdatedAt                time.Time                              `json:"updated_at"`
}

type persistedCommand struct {
	Sequence uint64 `json:"sequence"`
	Digest   string `json:"digest"`
	Sent     bool   `json:"sent"`
}

type activeTask struct {
	cancel          context.CancelFunc
	handle          sandbox.ControlHandle
	requestSeq      uint64
	cancelRequested bool
}

// InMemoryDispatch is the durable-capable task manager. With a non-empty root
// and backend it persists task metadata and runs real sandboxes. With empty
// root/backend it remains a deterministic fake for unit tests only.
type InMemoryDispatch struct {
	mu           sync.Mutex
	tasks        map[string]*persistedTask
	active       map[string]*activeTask
	root         string
	backend      runner.RuntimeBackend
	Materializer EnvironmentMaterializer
	nextFence    uint64
	Now          func() time.Time
}

func NewInMemoryDispatch() *InMemoryDispatch {
	return &InMemoryDispatch{tasks: make(map[string]*persistedTask), active: make(map[string]*activeTask), Now: time.Now}
}

func NewPersistentDispatch(root string, backend runner.RuntimeBackend) (*InMemoryDispatch, error) {
	if backend == nil {
		return nil, errors.New("persistent dispatch requires a sandbox backend")
	}
	if strings.TrimSpace(root) == "" {
		root = sandbox.DefaultWorkspaceRoot
	}
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return nil, errors.New("task metadata root must be a non-root absolute path")
	}
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0700); err != nil {
		return nil, fmt.Errorf("create task metadata directory: %w", err)
	}
	service := &InMemoryDispatch{tasks: make(map[string]*persistedTask), active: make(map[string]*activeTask), root: root, backend: backend, Now: time.Now}
	if err := service.load(); err != nil {
		return nil, err
	}
	return service, nil
}

func (d *InMemoryDispatch) Schedule(ctx context.Context, input ScheduleTaskInput, owner string) (agentworkflow.ScheduleRunnerTaskResult, error) {
	if err := validateScheduleInput(input); err != nil {
		return agentworkflow.ScheduleRunnerTaskResult{}, err
	}
	if strings.TrimSpace(owner) == "" {
		return agentworkflow.ScheduleRunnerTaskResult{}, errors.New("authenticated runner client identity is required")
	}
	if err := ctx.Err(); err != nil {
		return agentworkflow.ScheduleRunnerTaskResult{}, err
	}
	d.mu.Lock()
	taskKey := taskStorageKey(input)
	if existing := d.tasks[taskKey]; existing != nil {
		if existing.Owner != owner || !sameScheduleIdentity(existing.Input, input) {
			d.mu.Unlock()
			return agentworkflow.ScheduleRunnerTaskResult{}, errors.New("idempotency key is bound to another task")
		}
		if err := d.validateFenceLocked(existing, owner); err != nil {
			d.mu.Unlock()
			return agentworkflow.ScheduleRunnerTaskResult{}, err
		}
		result := existing.Result
		d.mu.Unlock()
		return result, nil
	}
	d.nextFence++
	taskID := "task-" + stableTaskID(taskKey)
	lease := runner.Lease{RunID: input.RunID, LeaseID: taskID, Owner: owner, FencingToken: d.nextFence, ExpiresAt: d.now().Add(input.Execution.Resources.Timeout + time.Hour)}
	task := &persistedTask{Input: input, Owner: owner, Lease: lease, State: "scheduled", UpdatedAt: d.now(), Result: agentworkflow.ScheduleRunnerTaskResult{Status: "scheduled", TaskID: taskID}}
	if err := d.persistLocked(task); err != nil {
		d.mu.Unlock()
		return agentworkflow.ScheduleRunnerTaskResult{}, err
	}
	d.tasks[taskKey] = task
	shouldStart := d.backend != nil
	d.mu.Unlock()
	if shouldStart {
		d.startTask(task)
	}
	return task.Result, nil
}

func (d *InMemoryDispatch) Cancel(ctx context.Context, input agentworkflow.CancelRunnerTaskInput, owner string) error {
	if err := validateCancelInput(input); err != nil {
		return err
	}
	d.mu.Lock()
	task := d.findTaskLocked(input.TaskID)
	if err := d.validateMutationLocked(task, input.RunID, input.OrganizationID, input.ProjectID, owner); err != nil {
		d.mu.Unlock()
		return err
	}
	if task.State == "succeeded" || task.State == "failed" || task.State == "cancelled" || task.State == "lost" {
		d.mu.Unlock()
		return nil
	}
	active := d.active[input.TaskID]
	if active != nil {
		active.cancelRequested = true
	}
	task.State, task.Error, task.UpdatedAt = "cancelled", "", d.now()
	if err := d.persistLocked(task); err != nil {
		d.mu.Unlock()
		return err
	}
	d.mu.Unlock()
	if active != nil {
		active.cancel()
	}
	return nil
}

func (d *InMemoryDispatch) Resume(ctx context.Context, input agentworkflow.ResumeRunnerTaskInput, owner string) error {
	if err := validateResumeInput(input); err != nil {
		return err
	}
	d.mu.Lock()
	task := d.findTaskLocked(input.TaskID)
	if err := d.validateMutationLocked(task, input.RunID, input.OrganizationID, input.ProjectID, owner); err != nil {
		d.mu.Unlock()
		return err
	}
	active := d.active[input.TaskID]
	if active == nil || active.handle == nil {
		d.mu.Unlock()
		return errors.New("task is not active")
	}
	if task.State != "waiting_approval" && task.State != "running" {
		d.mu.Unlock()
		return errors.New("task is not waiting for input")
	}
	handle := active.handle
	commandDigest, err := digestJSON(input)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if task.Commands == nil {
		task.Commands = make(map[string]persistedCommand)
	}
	command, exists := task.Commands[input.IdempotencyKey]
	if exists {
		if command.Digest != commandDigest {
			d.mu.Unlock()
			return errors.New("resume idempotency key is bound to another command")
		}
		if command.Sent {
			d.mu.Unlock()
			return nil
		}
	} else {
		active.requestSeq++
		command = persistedCommand{Sequence: active.requestSeq, Digest: commandDigest}
		task.Commands[input.IdempotencyKey] = command
	}
	requestSeq := command.Sequence
	task.State, task.UpdatedAt = "running", d.now()
	if err := d.persistLocked(task); err != nil {
		if !exists {
			delete(task.Commands, input.IdempotencyKey)
			active.requestSeq--
		}
		d.mu.Unlock()
		return err
	}
	d.mu.Unlock()
	frame := proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindRequest, RunID: input.RunID, Seq: requestSeq, Terminal: false}
	if input.Action == "approval" {
		frame.Type = proto.RequestApproval
		frame.Data = json.RawMessage(fmt.Sprintf(`{"approval_id":%q,"decision":%q}`, input.ApprovalID, input.Decision))
	} else {
		frame.Type = proto.RequestInput
		frame.Data = json.RawMessage(fmt.Sprintf(`{"reference":%q}`, input.Reference))
	}
	payload, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		return err
	}
	if err := handle.Send(ctx, payload); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	task = d.findTaskLocked(input.TaskID)
	if task == nil {
		return errors.New("task disappeared after sending command")
	}
	command = task.Commands[input.IdempotencyKey]
	command.Sent = true
	task.Commands[input.IdempotencyKey] = command
	task.UpdatedAt = d.now()
	return d.persistLocked(task)
}

func (d *InMemoryDispatch) Status(ctx context.Context, input StatusTaskInput, owner string) (TaskStatusResult, error) {
	if err := validateStatusInput(input); err != nil {
		return TaskStatusResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	task := d.findTaskLocked(input.TaskID)
	if err := d.validateMutationLocked(task, input.RunID, input.OrganizationID, input.ProjectID, owner); err != nil {
		return TaskStatusResult{}, err
	}
	events := make([]agentworkflow.RunnerRuntimeEvent, 0, len(task.RuntimeEvents))
	for _, event := range task.RuntimeEvents {
		if event.Sequence <= input.AfterSequence {
			continue
		}
		events = append(events, cloneRuntimeEvent(event))
	}
	return TaskStatusResult{
		Status: task.State, Error: task.Error, ApprovalID: task.ApprovalID, Output: task.Result.Output,
		Events: events, EventCursor: task.RuntimeEventCursor,
		EventHistoryStart: task.RuntimeEventHistoryStart, EventHistoryTruncated: task.RuntimeHistoryTruncated,
	}, nil
}

func (d *InMemoryDispatch) startTask(task *persistedTask) {
	d.mu.Lock()
	if d.active[task.Result.TaskID] != nil || task.State != "scheduled" {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), task.Input.Execution.Resources.Timeout)
	d.active[task.Result.TaskID] = &activeTask{cancel: cancel}
	task.State, task.UpdatedAt = "running", d.now()
	if err := d.persistLocked(task); err != nil {
		// The scheduled record remains the durable source of truth. Do not
		// launch a sandbox whose running state cannot be persisted.
		delete(d.active, task.Result.TaskID)
		cancel()
		task.State = "scheduled"
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	go func() {
		active := d.activeTask(task.Result.TaskID)
		daemon := Daemon{Backend: d.backend, Leases: StaticLeaseSource{Lease: task.Lease}, Materializer: d.Materializer, Sink: func(ctx context.Context, frame proto.Envelope) error {
			return d.observe(task.Result.TaskID, frame)
		}, OnStart: func(handle sandbox.OutputHandle) {
			d.mu.Lock()
			if current := d.active[task.Result.TaskID]; current != nil {
				if control, ok := handle.(sandbox.ControlHandle); ok {
					current.handle = control
					current.requestSeq = 1 // run.start is the first runner-to-adapter frame.
				}
			}
			d.mu.Unlock()
		}}
		_, err := daemon.Run(ctx, RunRequest{
			RunID: task.Input.RunID, Lease: task.Lease, CurrentLease: task.Lease,
			Spec: task.Input.Execution,
			Contract: RunContract{
				OrganizationID: task.Input.OrganizationID, ProjectID: task.Input.ProjectID,
				RunID: task.Input.RunID, WorkflowName: task.Input.WorkflowName,
				StepID: task.Input.StepID, AgentRef: task.Input.AgentRef,
				InputRef: task.Input.InputRef, DependencyOutput: task.Input.DependencyOutput,
				Execution: task.Input.Contract,
			},
		})
		d.finish(task.Result.TaskID, err, active)
	}()
}

func (d *InMemoryDispatch) activeTask(taskID string) *activeTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active[taskID]
}

func (d *InMemoryDispatch) observe(taskID string, frame proto.Envelope) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	task := d.findTaskLocked(taskID)
	if task == nil {
		return errors.New("task disappeared while supervising")
	}
	if frame.RunID != task.Input.RunID || frame.Kind != proto.KindEvent {
		return errors.New("runtime event does not belong to task")
	}
	if err := runtimeproto.ValidateEnvelope(frame); err != nil {
		return fmt.Errorf("runtime event validation: %w", err)
	}
	if err := appendRuntimeEventLocked(task, frame); err != nil {
		return err
	}
	switch frame.Type {
	case proto.EventApprovalRequested:
		var data struct {
			ApprovalID string `json:"approval_id"`
		}
		if err := json.Unmarshal(frame.Data, &data); err != nil || strings.TrimSpace(data.ApprovalID) == "" {
			return errors.New("approval.requested has no valid approval_id")
		}
		task.State, task.ApprovalID, task.UpdatedAt = "waiting_approval", data.ApprovalID, d.now()
	case proto.EventRunCompleted:
		output, err := terminalArtifact(frame.Data)
		if err != nil {
			return err
		}
		task.Result.Output = output
		task.UpdatedAt = d.now()
	}
	return d.persistLocked(task)
}

func (d *InMemoryDispatch) finish(taskID string, runErr error, active *activeTask) {
	d.mu.Lock()
	defer d.mu.Unlock()
	task := d.findTaskLocked(taskID)
	delete(d.active, taskID)
	if task == nil {
		return
	}
	var terminalErr *TerminalOutcomeError
	_ = errors.As(runErr, &terminalErr)
	if active != nil && active.cancelRequested || (terminalErr != nil && terminalErr.Type == proto.EventRunCancelled) {
		task.State, task.Error = "cancelled", ""
	} else if errors.Is(runErr, context.DeadlineExceeded) {
		task.State, task.Error = "failed", "execution_timeout"
	} else if runErr != nil {
		task.State, task.Error = "failed", executionFailureCode(runErr)
	} else {
		task.State, task.Error = "succeeded", ""
	}
	task.Result.Status = task.State
	task.UpdatedAt = d.now()
	var persistErr error
	for attempt := 0; attempt < 3; attempt++ {
		if persistErr = d.persistLocked(task); persistErr == nil {
			return
		}
	}
	task.State, task.Result.Status, task.Error = "lost", "lost", "terminal_persistence_failed"
	task.UpdatedAt = d.now()
	_ = d.persistLocked(task)
}

func executionFailureCode(err error) string {
	switch {
	case errors.Is(err, sandbox.ErrPodmanWorkspace):
		return "podman_workspace_failed"
	case errors.Is(err, sandbox.ErrPodmanPlan):
		return "podman_plan_failed"
	case errors.Is(err, sandbox.ErrPodmanMounts):
		return "podman_mounts_failed"
	case errors.Is(err, sandbox.ErrPodmanArguments):
		return "podman_arguments_failed"
	case errors.Is(err, sandbox.ErrPodmanStart):
		return "podman_start_failed"
	case errors.Is(err, sandbox.ErrRuntimeBroker):
		return "runtime_broker_start_failed"
	case errors.Is(err, sandbox.ErrRuntimeConfig):
		return "runtime_adapter_config_failed"
	case errors.Is(err, sandbox.ErrRuntimeContract):
		return "runtime_contract_rejected"
	case errors.Is(err, sandbox.ErrPodmanPermission):
		return "podman_runtime_permission_failed"
	case errors.Is(err, sandbox.ErrPodmanMount):
		return "podman_runtime_mount_failed"
	case errors.Is(err, sandbox.ErrPodmanRuntime):
		return "podman_runtime_setup_failed"
	case errors.Is(err, sandbox.ErrRuntimePreamble):
		return "runtime_adapter_preamble_failed"
	case errors.Is(err, sandbox.ErrRuntimeEnvironment):
		return "runtime_adapter_environment_failed"
	case errors.Is(err, sandbox.ErrRuntimeEntrypoint):
		return "runtime_adapter_entrypoint_failed"
	case errors.Is(err, sandbox.ErrRuntimeMissing):
		return "runtime_adapter_entrypoint_missing"
	case errors.Is(err, sandbox.ErrRuntimeKilled):
		return "runtime_adapter_killed"
	case errors.Is(err, ErrRuntimeContractSend):
		return "runtime_contract_send_failed"
	case errors.Is(err, ErrRuntimeStreamIncomplete):
		return "runtime_stream_incomplete"
	case errors.Is(err, ErrRuntimeStream):
		return "runtime_stream_failed"
	case errors.Is(err, ErrRuntimeWait):
		return "runtime_wait_failed"
	case errors.Is(err, ErrRuntimeCleanup):
		return "runtime_cleanup_failed"
	default:
		return "execution_failed"
	}
}

func (d *InMemoryDispatch) findTaskLocked(taskID string) *persistedTask {
	for _, task := range d.tasks {
		if task.Result.TaskID == taskID {
			return task
		}
	}
	return nil
}

func (d *InMemoryDispatch) validateMutationLocked(task *persistedTask, runID, org, project, owner string) error {
	if task == nil {
		return errors.New("task not found")
	}
	if task.Input.RunID != runID || task.Input.OrganizationID != org || task.Input.ProjectID != project {
		return errors.New("task identity does not match lease")
	}
	if err := d.validateFenceLocked(task, owner); err != nil {
		return err
	}
	return nil
}

func (d *InMemoryDispatch) validateFenceLocked(task *persistedTask, owner string) error {
	if task.Owner != owner {
		return errors.New("task owner does not match authenticated client")
	}
	if err := runner.ValidateLease(task.Lease, task.Lease, d.now()); err != nil {
		return fmt.Errorf("lease fencing rejected: %w", err)
	}
	return nil
}

func (d *InMemoryDispatch) load() error {
	entries, err := os.ReadDir(filepath.Join(d.root, "tasks"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d.root, "tasks", entry.Name()))
		if err != nil {
			return err
		}
		var task persistedTask
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&task); err != nil {
			return fmt.Errorf("read task metadata: %w", err)
		}
		if task.Input.IdempotencyKey == "" {
			return errors.New("task metadata has no idempotency key")
		}
		if err := normalizeRuntimeHistory(&task); err != nil {
			return fmt.Errorf("read task runtime history: %w", err)
		}
		if task.State == "running" || task.State == "waiting_approval" || task.State == "approval" {
			task.State, task.Error = "lost", "runner_restart"
		}
		if task.State == "approval" {
			task.State = "lost"
		}
		d.tasks[taskStorageKey(task.Input)] = &task
		if task.Lease.FencingToken > d.nextFence {
			d.nextFence = task.Lease.FencingToken
		}
		if task.State == "lost" {
			if err := d.persistLocked(&task); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *InMemoryDispatch) Recover(ctx context.Context) error {
	d.mu.Lock()
	pending := make([]*persistedTask, 0)
	for _, task := range d.tasks {
		if task.State == "scheduled" {
			pending = append(pending, task)
		}
	}
	d.mu.Unlock()
	for _, task := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		d.startTask(task)
	}
	return nil
}

func (d *InMemoryDispatch) persistLocked(task *persistedTask) error {
	if d.root == "" {
		return nil
	}
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	dir := filepath.Join(d.root, "tasks")
	temp, err := os.CreateTemp(dir, ".task-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, stableTaskID(taskStorageKey(task.Input))+".json")); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (d *InMemoryDispatch) now() time.Time {
	if d.Now == nil {
		return time.Now().UTC()
	}
	return d.Now().UTC()
}

func validateScheduleInput(input ScheduleTaskInput) error {
	for name, value := range map[string]string{"organization_id": input.OrganizationID, "project_id": input.ProjectID, "run_id": input.RunID, "workflow_name": input.WorkflowName, "step_id": input.StepID, "agent_ref": input.AgentRef, "idempotency_key": input.IdempotencyKey} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if err := runner.ValidateExecutableSandboxSpec(input.Execution); err != nil {
		return fmt.Errorf("execution: %w", err)
	}
	if err := input.Contract.Validate(); err != nil {
		return fmt.Errorf("contract: %w", err)
	}
	return nil
}
func validateCancelInput(input agentworkflow.CancelRunnerTaskInput) error {
	for name, value := range map[string]string{"organization_id": input.OrganizationID, "project_id": input.ProjectID, "run_id": input.RunID, "task_id": input.TaskID, "idempotency_key": input.IdempotencyKey} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
func validateResumeInput(input agentworkflow.ResumeRunnerTaskInput) error {
	if err := validateCancelInput(agentworkflow.CancelRunnerTaskInput{OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, TaskID: input.TaskID, IdempotencyKey: input.IdempotencyKey}); err != nil {
		return err
	}
	if input.Action != "approval" && input.Action != "user-reply" {
		return errors.New("action must be approval or user-reply")
	}
	if input.Action == "approval" && input.ApprovalID == "" {
		return errors.New("approval_id is required for approval resume")
	}
	return nil
}
func validateStatusInput(input StatusTaskInput) error {
	for name, value := range map[string]string{"organization_id": input.OrganizationID, "project_id": input.ProjectID, "run_id": input.RunID, "task_id": input.TaskID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}

func appendRuntimeEventLocked(task *persistedTask, frame proto.Envelope) error {
	if frame.Seq <= task.RuntimeEventCursor {
		// A replay after a persisted task snapshot is idempotent. The runtime
		// sequence validator protects the live stream; the durable cursor protects
		// a crash/retry from appending the same source frame twice.
		return nil
	}
	if frame.Type == proto.EventHeartbeat {
		// Heartbeats are transport liveness, not useful run history. The source
		// cursor still advances so a skipped heartbeat cannot look like a missing
		// retained event when the bounded history is truncated.
		task.RuntimeEventCursor = frame.Seq
		return nil
	}
	safeFrame, err := privacySafeRuntimeFrame(frame)
	if err != nil {
		return fmt.Errorf("persist runtime event %q: %w", frame.Type, err)
	}
	event := agentworkflow.RunnerRuntimeEvent{
		Sequence: safeFrame.Seq, Type: safeFrame.Type,
		Payload: append(json.RawMessage(nil), safeFrame.Data...), Terminal: safeFrame.Terminal,
	}
	if err := validateStoredRuntimeEvent(event); err != nil {
		return err
	}
	task.RuntimeEvents = append(task.RuntimeEvents, event)
	task.RuntimeEventCursor = event.Sequence
	for len(task.RuntimeEvents) > maxRuntimeEventHistory || runtimeHistoryBytes(task.RuntimeEvents) > maxRuntimeEventHistoryBytes {
		task.RuntimeHistoryTruncated = true
		task.RuntimeEvents = task.RuntimeEvents[1:]
	}
	if len(task.RuntimeEvents) == 0 {
		task.RuntimeEventHistoryStart = 0
	} else {
		task.RuntimeEventHistoryStart = task.RuntimeEvents[0].Sequence
	}
	return nil
}

func normalizeRuntimeHistory(task *persistedTask) error {
	if len(task.RuntimeEvents) > maxRuntimeEventHistory {
		return errors.New("runtime event history exceeds event limit")
	}
	var previous uint64
	for index := range task.RuntimeEvents {
		event := task.RuntimeEvents[index]
		if err := validateStoredRuntimeEvent(event); err != nil {
			return fmt.Errorf("runtime event %d: %w", index, err)
		}
		if event.Sequence <= previous {
			return errors.New("runtime event history is not ordered")
		}
		previous = event.Sequence
	}
	if bytes := runtimeHistoryBytes(task.RuntimeEvents); bytes > maxRuntimeEventHistoryBytes {
		return fmt.Errorf("runtime event history exceeds %d bytes", maxRuntimeEventHistoryBytes)
	}
	if task.RuntimeEventCursor == 0 {
		task.RuntimeEventCursor = previous
	}
	if task.RuntimeEventCursor < previous {
		return errors.New("runtime event cursor precedes retained history")
	}
	if len(task.RuntimeEvents) == 0 {
		task.RuntimeEventHistoryStart = 0
	} else {
		task.RuntimeEventHistoryStart = task.RuntimeEvents[0].Sequence
	}
	return nil
}

func validateStoredRuntimeEvent(event agentworkflow.RunnerRuntimeEvent) error {
	if event.Sequence == 0 || strings.TrimSpace(event.Type) == "" {
		return errors.New("runtime event requires a positive sequence and type")
	}
	data := bytes.TrimSpace(event.Payload)
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return errors.New("runtime event payload must be a JSON object")
	}
	if len(data) > runtimeproto.MaxDataBytes {
		return errors.New("runtime event payload exceeds protocol limit")
	}
	return nil
}

func cloneRuntimeEvent(event agentworkflow.RunnerRuntimeEvent) agentworkflow.RunnerRuntimeEvent {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	return event
}

func runtimeHistoryBytes(events []agentworkflow.RunnerRuntimeEvent) int {
	total := 0
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return maxRuntimeEventHistoryBytes + 1
		}
		total += len(encoded)
	}
	return total
}

func privacySafeRuntimeFrame(frame proto.Envelope) (proto.Envelope, error) {
	if err := runtimeproto.ValidateEnvelope(frame); err != nil {
		return proto.Envelope{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(frame.Data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return proto.Envelope{}, err
	}
	redacted := redactRuntimeJSON(value, "")
	data, err := json.Marshal(redacted)
	if err != nil {
		return proto.Envelope{}, err
	}
	frame.Data = data
	if err := runtimeproto.ValidateEnvelope(frame); err != nil {
		return proto.Envelope{}, fmt.Errorf("redaction produced invalid frame: %w", err)
	}
	return frame, nil
}

func redactRuntimeJSON(value any, key string) any {
	if isSensitiveRuntimeKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for childKey, child := range typed {
			if childKey == "value" {
				if name, ok := typed["name"].(string); ok && isSensitiveRuntimeKey(name) {
					result[childKey] = "[REDACTED]"
					continue
				}
			}
			result[childKey] = redactRuntimeJSON(child, childKey)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = redactRuntimeJSON(child, "")
		}
		return result
	case string:
		return redactRuntimeText(typed)
	default:
		return value
	}
}

func isSensitiveRuntimeKey(key string) bool {
	key = strings.ToLower(key)
	// Token usage is numeric model telemetry, not authentication material. It
	// must retain its type so a privacy pass cannot invalidate a valid
	// model.completed protocol frame.
	if key == "input_tokens" || key == "output_tokens" {
		return false
	}
	for _, marker := range []string{"secret", "token", "password", "apikey", "api_key", "authorization", "privatekey", "private_key"} {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func redactRuntimeText(value string) string {
	for _, bearerPrefix := range []string{"Authorization: Bearer ", "authorization: bearer "} {
		searchFrom := 0
		for {
			relative := strings.Index(value[searchFrom:], bearerPrefix)
			if relative < 0 {
				break
			}
			index := searchFrom + relative
			end := index + len(bearerPrefix)
			for end < len(value) && !strings.ContainsRune(" \t\r\n\"'`,;)]}", rune(value[end])) {
				end++
			}
			value = value[:index+len(bearerPrefix)] + "[REDACTED]" + value[end:]
			searchFrom = index + len(bearerPrefix) + len("[REDACTED]")
		}
	}
	for _, prefix := range []string{"sk-", "ghp_", "github_pat_", "glpat-", "glsa_", "xoxb-", "xoxp-", "agt_", "ags_"} {
		searchFrom := 0
		for {
			relative := strings.Index(value[searchFrom:], prefix)
			if relative < 0 {
				break
			}
			index := searchFrom + relative
			end := index + len(prefix)
			for end < len(value) && !strings.ContainsRune(" \t\r\n\"'`,;)]}", rune(value[end])) {
				end++
			}
			value = value[:index+len(prefix)] + "[REDACTED]" + value[end:]
			searchFrom = index + len(prefix) + len("[REDACTED]")
		}
	}
	return value
}

func sameScheduleIdentity(a, b ScheduleTaskInput) bool {
	return reflect.DeepEqual(a, b)
}

// terminalArtifact is the runner/workflow handoff contract: a successful
// run.completed event must contain data.output as a validated immutable
// ArtifactRef. The reference, never the artifact bytes, is persisted and
// returned by /v1/tasks/status for Temporal to consume.
func terminalArtifact(data json.RawMessage) (agentworkflow.ArtifactRef, error) {
	var payload struct {
		Output agentworkflow.ArtifactRef `json:"output"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return agentworkflow.ArtifactRef{}, fmt.Errorf("run.completed output is invalid JSON: %w", err)
	}
	if err := payload.Output.Validate(); err != nil {
		return agentworkflow.ArtifactRef{}, fmt.Errorf("run.completed output artifact is invalid: %w", err)
	}
	return payload.Output, nil
}
func stableTaskID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func taskStorageKey(input ScheduleTaskInput) string {
	return strings.Join([]string{input.OrganizationID, input.ProjectID, input.IdempotencyKey}, "\x00")
}

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type HTTPServer struct {
	Config  Config
	Service DispatchService
	Report  runner.ReadinessReport
	Server  *http.Server
}

type unixPeerContextKey struct{}

func NewHTTPServer(cfg Config, backend runner.RuntimeBackend, report runner.ReadinessReport) (*HTTPServer, error) {
	if backend == nil {
		return nil, errors.New("runner backend is required")
	}
	if err := cfg.Server.Validate(); err != nil {
		return nil, err
	}
	service, err := NewPersistentDispatch(cfg.WorkspaceRoot, backend)
	if err != nil {
		return nil, err
	}
	materializer, err := cfg.BuildMaterializer()
	if err != nil {
		return nil, err
	}
	service.Materializer = materializer
	if err := service.Recover(context.Background()); err != nil {
		return nil, err
	}
	h := &HTTPServer{Config: cfg, Service: service, Report: report}
	h.Server = &http.Server{Addr: cfg.Server.ListenAddr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	h.Server.ConnContext = func(ctx context.Context, connection net.Conn) context.Context {
		if cfg.Server.TransportMode() != "unix" {
			return ctx
		}
		identity, err := unixPeerIdentity(connection)
		if err != nil {
			return ctx
		}
		return context.WithValue(ctx, unixPeerContextKey{}, identity)
	}
	return h, nil
}

func (s *HTTPServer) Serve(ctx context.Context) error {
	if s.Server == nil {
		return errors.New("HTTP server is nil")
	}
	if s.Config.Server.TransportMode() == "unix" {
		return s.serveUnix(ctx)
	}
	tlsConfig, err := s.Config.Server.TLSConfig()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", s.Server.Addr)
	if err != nil {
		return fmt.Errorf("listen for runner HTTP: %w", err)
	}
	tlsListener := tls.NewListener(listener, tlsConfig)
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Server.Serve(tlsListener) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return s.Server.Shutdown(shutdownCtx)
	}
}

func (s *HTTPServer) serveUnix(ctx context.Context) error {
	path := filepath.Clean(s.Config.Server.SocketPath)
	lock, releaseLock, err := acquireUnixSocketLock(path)
	if err != nil {
		return err
	}
	defer releaseLock()
	_ = lock
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("runner Unix socket path exists and is not a socket")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() {
			return errors.New("stale runner Unix socket is not owned by this runner")
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale runner Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect runner Unix socket: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on runner Unix socket: %w", err)
	}
	createdSocket, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("inspect created runner Unix socket: %w", err)
	}
	defer func() {
		_ = listener.Close()
		if current, statErr := os.Lstat(path); statErr == nil && os.SameFile(createdSocket, current) {
			_ = os.Remove(path)
		}
	}()
	if err := os.Chmod(path, 0660); err != nil {
		return fmt.Errorf("set runner Unix socket permissions: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Server.Serve(listener) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return s.Server.Shutdown(shutdownCtx)
	}
}

func acquireUnixSocketLock(socketPath string) (*os.File, func(), error) {
	lockPath := socketPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("open runner Unix socket lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil, errors.New("another runner owns the Unix socket")
		}
		return nil, nil, fmt.Errorf("lock runner Unix socket: %w", err)
	}
	lockInfo, err := lock.Stat()
	if err != nil {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		return nil, nil, fmt.Errorf("inspect runner Unix socket lock: %w", err)
	}
	release := func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
		if current, statErr := os.Lstat(lockPath); statErr == nil && os.SameFile(lockInfo, current) {
			_ = os.Remove(lockPath)
		}
	}
	return lock, release, nil
}

func (s *HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := ""
	if s.Config.Server.TransportMode() == "unix" {
		owner, _ = r.Context().Value(unixPeerContextKey{}).(string)
		if owner == "" {
			http.Error(w, "Unix peer credentials required", http.StatusUnauthorized)
			return
		}
	} else {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "mTLS required", http.StatusUnauthorized)
			return
		}
		owner = peerIdentity(r)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		if s.Report.Ready {
			writeJSON(w, http.StatusOK, s.Report)
		} else {
			writeJSON(w, http.StatusServiceUnavailable, s.Report)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/schedule":
		var input ScheduleTaskInput
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		result, err := s.Service.Schedule(r.Context(), input, owner)
		if err != nil {
			rejectDispatch(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/status":
		var input StatusTaskInput
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		result, err := s.Service.Status(r.Context(), input, owner)
		if err != nil {
			rejectDispatch(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/cancel":
		var input agentworkflow.CancelRunnerTaskInput
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		if err := s.Service.Cancel(r.Context(), input, owner); err != nil {
			rejectDispatch(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/resume":
		var input agentworkflow.ResumeRunnerTaskInput
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		if err := s.Service.Resume(r.Context(), input, owner); err != nil {
			rejectDispatch(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func unixPeerIdentity(connection net.Conn) (string, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return "", errors.New("runner local transport requires a Unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return "", fmt.Errorf("inspect Unix connection: %w", err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return "", fmt.Errorf("inspect Unix peer: %w", err)
	}
	if credentialErr != nil || credentials == nil {
		return "", fmt.Errorf("read Unix peer credentials: %w", credentialErr)
	}
	return fmt.Sprintf("unix:uid=%d:gid=%d", credentials.Uid, credentials.Gid), nil
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || strings.ToLower(mediaType) != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDispatchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "request must contain one JSON value", http.StatusBadRequest)
		return false
	}
	return true
}
func rejectDispatch(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, "request cancelled", http.StatusRequestTimeout)
		return
	}
	http.Error(w, "task request rejected", http.StatusConflict)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func peerIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

func (c ServerTLSConfig) Validate() error {
	switch c.TransportMode() {
	case "unix":
		path := filepath.Clean(strings.TrimSpace(c.SocketPath))
		if path == "." || path == string(filepath.Separator) || !filepath.IsAbs(path) {
			return errors.New("socketPath must be a non-root absolute path")
		}
		parent := filepath.Dir(path)
		info, err := os.Lstat(parent)
		if err != nil {
			return fmt.Errorf("socketPath parent: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("socketPath parent must be a real directory")
		}
		if info.Mode().Perm()&0027 != 0 {
			return errors.New("socketPath parent must not be group-writable or world-accessible")
		}
		return nil
	case "mtls":
	default:
		return errors.New("server transport must be unix or mtls")
	}
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listenAddr is required")
	}
	if c.CertFile == "" || c.KeyFile == "" || c.ClientCAFile == "" {
		return errors.New("certFile, keyFile, and clientCAFile are required")
	}
	for name, path := range map[string]string{"certFile": c.CertFile, "keyFile": c.KeyFile, "clientCAFile": c.ClientCAFile} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s must be a file", name)
		}
		if name == "keyFile" && info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("%s must not be group/world accessible", name)
		}
	}
	if _, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile); err != nil {
		return fmt.Errorf("load server certificate: %w", err)
	}
	caBytes, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return errors.New("client CA contains no certificates")
	}
	return nil
}
func (c ServerTLSConfig) TLSConfig() (*tls.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	caBytes, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("client CA contains no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}, nil
}
