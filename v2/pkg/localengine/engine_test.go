package localengine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

func testManifest() workflow.Manifest {
	return workflow.Manifest{
		Name: "demo", Revision: "sha256:" + strings.Repeat("a", 64),
		Steps: []workflow.Step{
			{ID: "second", AgentRef: "agent/second", Needs: []string{"first"}},
			{ID: "first", AgentRef: "agent/first"},
		},
	}
}

func testArtifact(id string) workflow.ArtifactRef {
	return workflow.ArtifactRef{
		ID: id, URI: "file:///artifacts/" + id,
		Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 1,
		MediaType: "text/plain",
	}
}

func claimedForTest(manifest workflow.Manifest) claimedWorkflow {
	state := newMachineState(manifest, "input://demo")
	return claimedWorkflow{
		Scope: store.Scope{OrganizationID: "org", ProjectID: "project"},
		RunID: "run-1", Manifest: manifest, InputRef: "input://demo", State: state,
		Status: "Running", LeaseOwner: "worker", LeaseToken: "token",
	}
}

func TestStateInitializesAndSchedulesDeterministically(t *testing.T) {
	plan, err := workflow.CompileManifest(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifest.Steps = plan.Steps
	claimed := claimedForTest(manifest)
	ready := readyStep(manifest, claimed.State)
	if len(ready) != 1 || ready[0].ID != "first" {
		t.Fatalf("initial ready steps=%v", ready)
	}
	claimed.State.Steps["first"] = StepState{Phase: StepSucceeded, Output: testArtifact("first")}
	claimed.State.Outputs["first"] = testArtifact("first")
	ready = readyStep(manifest, claimed.State)
	if len(ready) != 1 || ready[0].ID != "second" {
		t.Fatalf("dependent ready steps=%v", ready)
	}
}

func TestStateValidationRejectsMissingOrUnknownStepState(t *testing.T) {
	manifest := testManifest()
	state := newMachineState(manifest, "")
	delete(state.Steps, "first")
	if err := validateMachineState(state); err == nil {
		t.Fatal("missing step state was accepted")
	}
	state = newMachineState(manifest, "")
	state.Steps["unknown"] = StepState{Phase: StepPending}
	state.Steps["first"] = StepState{Phase: "invented"}
	if err := validateMachineState(state); err == nil {
		t.Fatal("unknown phase was accepted")
	}
}

func TestCommandsAreScopedToCurrentStepAndTarget(t *testing.T) {
	manifest := testManifest()
	manifest.Steps[1].AgentRef = ""
	manifest.Steps[1].Approval = &workflow.ApprovalSpec{ID: "approval-1", Reason: "review"}
	claimed := claimedForTest(manifest)
	claimed.State.CurrentStep = "first"
	claimed.State.Steps["first"] = StepState{Phase: StepWaitingApproval, ApprovalID: "approval-1"}

	if !commandApplies(Command{Kind: "approval", Target: "workflow", ApprovalID: "approval-1", StepID: "first"}, claimed) {
		t.Fatal("matching workflow approval did not apply")
	}
	if commandApplies(Command{Kind: "approval", Target: "agent", ApprovalID: "approval-1", StepID: "first"}, claimed) {
		t.Fatal("agent approval applied to workflow approval step")
	}
	if commandApplies(Command{Kind: "approval", Target: "workflow", ApprovalID: "other", StepID: "first"}, claimed) {
		t.Fatal("unmatched approval applied")
	}

	manifest.Steps[1].AgentRef = "agent/second"
	manifest.Steps[1].Approval = nil
	claimed.Manifest = manifest
	claimed.State.Manifest = manifest
	claimed.State.CurrentStep = "second"
	claimed.State.Steps["second"] = StepState{Phase: StepWaitingApproval, ApprovalID: "runner-approval", TaskID: "task-1"}
	if !commandApplies(Command{Kind: "approval", Target: "agent", ApprovalID: "runner-approval", StepID: "second"}, claimed) {
		t.Fatal("matching agent approval did not apply")
	}
	if commandApplies(Command{Kind: "approval", Target: "workflow", ApprovalID: "runner-approval", StepID: "second"}, claimed) {
		t.Fatal("workflow approval applied to agent step")
	}
}

func TestCommandValidationAndKeyNormalization(t *testing.T) {
	if err := validateCommand("run-1", Command{Kind: "reply", Target: "agent", IdempotencyKey: "key", ReplyRef: "reply://1"}); err != nil {
		t.Fatal(err)
	}
	if err := validateCommand("run-1", Command{Kind: "reply", Target: "agent", IdempotencyKey: "key", ReplyRef: "contains\nnewline"}); err == nil {
		t.Fatal("unbounded reply reference was accepted")
	}
	if err := validateCommand("run-1", Command{Kind: "cancel", Target: "agent", IdempotencyKey: "key"}); err == nil {
		t.Fatal("agent-targeted cancellation was accepted")
	}
	if got := normalizeKey("same-key"); len(got) != 64 {
		t.Fatalf("normalized key length=%d", len(got))
	}
	digest := strings.Repeat("a", 64)
	if got := normalizeKey(digest); got != digest {
		t.Fatalf("digest key changed: %s", got)
	}
}

func TestRunnerRuntimeEventValidationIsBoundedAndObjectOnly(t *testing.T) {
	valid := workflow.RunnerRuntimeEvent{Sequence: 1, Type: "assistant.message", Payload: []byte(`{"message":"ok"}`)}
	if err := validateRunnerRuntimeEvent(valid); err != nil {
		t.Fatalf("valid runtime event rejected: %v", err)
	}
	for _, event := range []workflow.RunnerRuntimeEvent{
		{Type: valid.Type, Payload: valid.Payload},
		{Sequence: 1, Payload: valid.Payload},
		{Sequence: 1, Type: valid.Type, Payload: []byte(`[]`)},
		{Sequence: 1, Type: valid.Type, Payload: []byte(`{"message":`)},
		{Sequence: 1, Type: valid.Type, Payload: []byte(`{"value":1,"value":2}`)},
		{Sequence: 1, Type: valid.Type, Payload: []byte(`{"value":1}{"value":1}`)},
		{Sequence: 1, Type: valid.Type, Payload: []byte(`{"message":"` + strings.Repeat("x", 768<<10) + `"}`)},
	} {
		if err := validateRunnerRuntimeEvent(event); err == nil {
			t.Fatalf("invalid runtime event was accepted: %#v", event)
		}
	}
}

func TestMachineStateDecodeRequiresStrictJSON(t *testing.T) {
	for _, document := range []string{
		`{"version":1,"version":1}`,
		`{"version":1} null`,
		`{"version":1,"manifest":{"name":"\ud800"}}`,
	} {
		if _, err := decodeMachineState([]byte(document)); err == nil {
			t.Fatalf("permissive machine-state input was accepted: %s", document)
		}
	}
}

func TestRetryBackoffIsBoundedAndTerminalTransitionsAreClosed(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	engine := &Engine{Options: Options{Now: func() time.Time { return now }}}
	policy := workflow.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 3 * time.Second}
	if got := engine.retryWake(policy, 3); !got.Equal(now.Add(3 * time.Second)) {
		t.Fatalf("bounded retry wake=%s", got)
	}
	if validTransition("Succeeded", "Running") {
		t.Fatal("terminal run was reopened")
	}
	if !validTransition("WaitingApproval", "Succeeded") {
		t.Fatal("approval completion was rejected")
	}
	if !validTransition("Pending", "Starting") {
		t.Fatal("initial scheduling transition was rejected")
	}
}

func TestOutputReferencesAreValidated(t *testing.T) {
	if err := testArtifact("artifact").Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (workflow.ArtifactRef{ID: "only-id"}).Validate(); err == nil {
		t.Fatal("incomplete artifact was accepted")
	}
	if !errors.Is(ErrNoWork, ErrNoWork) {
		t.Fatal("sentinel error is not stable")
	}
}
