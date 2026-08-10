package workflow

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

const testDigest = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"

type fakeRunner struct {
	mu             sync.Mutex
	order          []string
	keys           []string
	dispatches     map[string]int
	firstError     bool
	failStep       string
	cooldown       bool
	waitForRunner  bool
	cancelledTasks []string
	resumes        []ResumeRunnerTaskInput
	states         []SetRunStateInput
}

func (f *fakeRunner) ScheduleRunnerTask(_ context.Context, input ScheduleRunnerTaskInput) (ScheduleRunnerTaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order = append(f.order, input.StepID)
	f.keys = append(f.keys, input.IdempotencyKey)
	if f.dispatches == nil {
		f.dispatches = make(map[string]int)
	}
	if f.cooldown {
		f.cooldown = false
		return ScheduleRunnerTaskResult{Status: "cooldown", CooldownUntil: time.Now().Add(time.Hour)}, nil
	}
	if f.firstError {
		f.firstError = false
		f.dispatches[input.IdempotencyKey]++
		return ScheduleRunnerTaskResult{}, errors.New("accepted by runner but response was lost")
	}
	if input.StepID == f.failStep {
		return ScheduleRunnerTaskResult{}, errors.New("runner rejected task")
	}
	if f.dispatches[input.IdempotencyKey] == 0 {
		f.dispatches[input.IdempotencyKey]++
	}
	if f.waitForRunner {
		return ScheduleRunnerTaskResult{Status: "scheduled", TaskID: "task-" + input.StepID}, nil
	}
	return ScheduleRunnerTaskResult{Status: "succeeded", TaskID: "task-" + input.StepID, Output: outputRef(input.StepID)}, nil
}

func (f *fakeRunner) CancelRunnerTask(_ context.Context, input CancelRunnerTaskInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelledTasks = append(f.cancelledTasks, input.TaskID)
	return nil
}

func (f *fakeRunner) ResumeRunnerTask(_ context.Context, input ResumeRunnerTaskInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, input)
	return nil
}

func (f *fakeRunner) StatusRunnerTask(_ context.Context, input StatusRunnerTaskInput) (StatusRunnerTaskResult, error) {
	return StatusRunnerTaskResult{Status: "running"}, nil
}

func (f *fakeRunner) SetRunState(_ context.Context, input SetRunStateInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, input)
	return nil
}

func outputRef(name string) ArtifactRef {
	return ArtifactRef{ID: "artifact-" + name, URI: "s3://test/" + name, Digest: testDigest, SizeBytes: 1, MediaType: "application/json"}
}

func newTestEnvironment(fake *fakeRunner) *testsuite.TestWorkflowEnvironment {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflowWithOptions(Workflow, workflow.RegisterOptions{Name: WorkflowName})
	env.RegisterWorkflowWithOptions(AgentRunWorkflow, workflow.RegisterOptions{Name: AgentRunName})
	handlers := ActivityHandlers{Impl: fake}
	env.RegisterActivityWithOptions(handlers.ScheduleRunnerTask, activity.RegisterOptions{Name: ActivitySchedule})
	env.RegisterActivityWithOptions(handlers.CancelRunnerTask, activity.RegisterOptions{Name: ActivityCancel})
	env.RegisterActivityWithOptions(handlers.ResumeRunnerTask, activity.RegisterOptions{Name: ActivityResume})
	env.RegisterActivityWithOptions(handlers.StatusRunnerTask, activity.RegisterOptions{Name: ActivityStatus})
	stateHandlers := RunStateHandlers{Impl: fake}
	env.RegisterActivityWithOptions(stateHandlers.SetRunState, activity.RegisterOptions{Name: ActivitySetRunState})
	return env
}

func manifest(steps ...Step) Manifest {
	return Manifest{Name: "test-workflow", Revision: "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222", Steps: steps}
}

func input(m Manifest) WorkflowInput {
	return WorkflowInput{OrganizationID: "org-1", ProjectID: "project-1", RunID: "run-1", Manifest: m}
}

func agent(id string, needs ...string) Step {
	return Step{ID: id, AgentRef: "agent/" + id, Needs: needs}
}

func TestWorkflowDependencyOrderIsIndependentOfManifestOrder(t *testing.T) {
	fake := &fakeRunner{}
	env := newTestEnvironment(fake)
	env.ExecuteWorkflow(Workflow, input(manifest(agent("c", "b"), agent("a"), agent("b", "a"))))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.order, []string{"a", "b", "c"}) {
		t.Fatalf("execution order = %#v", fake.order)
	}
}

func TestWorkflowApprovalResumesWithSignalAndReferenceOnlyOutput(t *testing.T) {
	fake := &fakeRunner{}
	env := newTestEnvironment(fake)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalUserReply, UserReplySignal{StepID: "approve", ReplyRef: "s3://test/reply"})
		env.SignalWorkflow(SignalApproval, ApprovalSignal{ApprovalID: "approval-1", StepID: "approve", Decision: "approved"})
	}, time.Second)
	env.ExecuteWorkflow(Workflow, input(manifest(Step{ID: "approve", Approval: &ApprovalSpec{ID: "approval-1", Reason: "release"}})))
	var result WorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Outputs["approve"].URI != "s3://test/reply" {
		t.Fatalf("approval reference = %#v", result.Outputs["approve"])
	}
}

func TestWorkflowApprovalTimeoutIsNonRetryable(t *testing.T) {
	fake := &fakeRunner{}
	env := newTestEnvironment(fake)
	env.ExecuteWorkflow(Workflow, input(manifest(Step{
		ID: "approve", Timeout: time.Second,
		Approval: &ApprovalSpec{ID: "approval-1", Reason: "release"},
	})))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err == nil {
		t.Fatal("expected approval timeout")
	}
}

func TestAgentRunUserReplyResumesRunner(t *testing.T) {
	fake := &fakeRunner{waitForRunner: true}
	env := newTestEnvironment(fake)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalRunnerEvent, RunnerEventSignal{TaskID: "task-a", Status: "waiting_approval", ApprovalID: "approval-1"})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalUserReply, UserReplySignal{TaskID: "task-a", ReplyRef: "s3://test/reply"})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalRunnerEvent, RunnerEventSignal{TaskID: "task-a", Status: "succeeded", Output: outputRef("a")})
	}, 3*time.Second)
	env.ExecuteWorkflow(AgentRunWorkflow, AgentRunInput{OrganizationID: "org-1", ProjectID: "project-1", RunID: "run-1", WorkflowName: "test-workflow", StepID: "a", AgentRef: "agent/a"})
	var result AgentRunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if len(fake.resumes) != 1 || fake.resumes[0].Reference != "s3://test/reply" {
		t.Fatalf("resume calls = %#v", fake.resumes)
	}
}

func TestWorkflowCapacityCooldownUsesDeterministicTimer(t *testing.T) {
	fake := &fakeRunner{cooldown: true}
	env := newTestEnvironment(fake)
	env.ExecuteWorkflow(Workflow, input(manifest(agent("a"))))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.order) != 2 {
		t.Fatalf("expected schedule after cooldown, got %d calls", len(fake.order))
	}
}

func TestWorkflowFailurePropagatesAndDoesNotRunDependents(t *testing.T) {
	fake := &fakeRunner{failStep: "b"}
	env := newTestEnvironment(fake)
	env.ExecuteWorkflow(Workflow, input(manifest(agent("a"), agent("b", "a"), agent("c", "b"))))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err == nil {
		t.Fatal("expected workflow failure")
	}
	if !reflect.DeepEqual(fake.order, []string{"a", "b"}) {
		t.Fatalf("dependent step ran after failure: %#v", fake.order)
	}
}

func TestWorkflowCancellationCancelsRunnerTask(t *testing.T) {
	fake := &fakeRunner{waitForRunner: true}
	env := newTestEnvironment(fake)
	env.RegisterDelayedCallback(func() { env.CancelWorkflow() }, time.Second)
	env.ExecuteWorkflow(Workflow, input(manifest(agent("a"))))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err == nil {
		t.Fatal("expected cancellation")
	}
	if !reflect.DeepEqual(fake.cancelledTasks, []string{"task-a"}) {
		t.Fatalf("cancel calls = %#v", fake.cancelledTasks)
	}
}

func TestChildRetryUsesStableIdempotencyKeyWithoutDuplicateDispatch(t *testing.T) {
	fake := &fakeRunner{firstError: true}
	env := newTestEnvironment(fake)
	env.ExecuteWorkflow(Workflow, input(manifest(Step{ID: "a", AgentRef: "agent/a", Retry: RetryPolicy{MaxAttempts: 2}})))
	if err := env.GetWorkflowResult(&WorkflowResult{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.keys) != 2 {
		t.Fatalf("expected two activity attempts, got %#v", fake.keys)
	}
	if fake.keys[0] != fake.keys[1] || fake.dispatches[fake.keys[0]] != 1 {
		t.Fatalf("dispatch was not idempotent: keys=%#v dispatches=%#v", fake.keys, fake.dispatches)
	}
}

func TestManifestCycleIsRejected(t *testing.T) {
	_, err := CompileManifest(manifest(agent("a", "b"), agent("b", "a")))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestReplaySafePlanAndDispatchKeys(t *testing.T) {
	first, err := CompileManifest(manifest(agent("c", "b"), agent("b", "a"), agent("a")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileManifest(manifest(agent("a"), agent("b", "a"), agent("c", "b")))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("canonical plans differ across replay/input order: %#v != %#v", first, second)
	}
	if stepDispatchKey("run-1", first.Name, "a") != stepDispatchKey("run-1", second.Name, "a") {
		t.Fatal("dispatch key changed across replay")
	}

	// Run the same canonical history twice in separate test environments. The
	// activity is the only side-effect boundary and sees the same stable keys.
	for i := 0; i < 2; i++ {
		fake := &fakeRunner{}
		env := newTestEnvironment(fake)
		env.ExecuteWorkflow(Workflow, input(manifest(agent("b", "a"), agent("a"))))
		if err := env.GetWorkflowResult(&WorkflowResult{}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fake.keys, []string{"run-1/test-workflow/a", "run-1/test-workflow/b"}) {
			t.Fatalf("run %d keys = %#v", i, fake.keys)
		}
	}
}
