package orchestration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/httpapi"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

type fakeTemporal struct {
	options     client.StartWorkflowOptions
	name        interface{}
	input       agentworkflow.WorkflowInput
	err         error
	calls       int
	signalCalls []struct {
		workflowID string
		signalName string
		argument   interface{}
	}
	signalErr error
}

func (f *fakeTemporal) ExecuteWorkflow(_ context.Context, options client.StartWorkflowOptions, name interface{}, args ...interface{}) (client.WorkflowRun, error) {
	f.calls++
	f.options, f.name = options, name
	if len(args) != 1 {
		return nil, errors.New("unexpected workflow arguments")
	}
	input, ok := args[0].(agentworkflow.WorkflowInput)
	if !ok {
		return nil, errors.New("unexpected workflow input")
	}
	f.input = input
	return nil, f.err
}

func (f *fakeTemporal) SignalWorkflow(_ context.Context, workflowID, _ string, signalName string, argument interface{}) error {
	f.signalCalls = append(f.signalCalls, struct {
		workflowID string
		signalName string
		argument   interface{}
	}{workflowID: workflowID, signalName: signalName, argument: argument})
	return f.signalErr
}

func TestStarterCompilesAndStartsWorkflowRevision(t *testing.T) {
	ctx := context.Background()
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	digest := "sha256:" + strings.Repeat("a", 64)
	definition := &spec.Workflow{
		ResourceMeta: spec.ResourceMeta{TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: spec.KindWorkflow}, Metadata: spec.ObjectMeta{Name: "release", Namespace: "project-a"}},
		Spec: spec.WorkflowSpec{Steps: []spec.WorkflowStep{
			{ID: "test", Agent: "tester", Timeout: "5m", Retries: 2, Input: map[string]any{"suite": "smoke"}},
			{ID: "approve", Needs: []string{"test"}, Approval: &spec.ApprovalStep{Reason: "release", Role: "approver"}},
		}},
	}
	document, err := spec.AsJSON(definition)
	if err != nil {
		t.Fatal(err)
	}
	memory := store.NewMemory()
	applyExecutableAgent(t, memory, scope, "tester", "sha256:"+strings.Repeat("e", 64))
	if _, err := memory.ApplyResource(ctx, store.Resource{Scope: scope, Kind: spec.KindWorkflow, Name: "release", Digest: digest, Document: document, AppliedBy: "user-a"}); err != nil {
		t.Fatal(err)
	}
	temporal := &fakeTemporal{}
	starter := Starter{Store: memory, Temporal: temporal, TaskQueue: "runner-queue"}
	run := store.Run{Scope: scope, ID: "run-1", Kind: spec.KindWorkflowRun, DefinitionDigest: digest, RequestedBy: "user-a"}
	if err := starter.StartRun(ctx, httpapi.RunStartRequest{Scope: scope, Run: run, WorkflowRef: "release", InputRef: "s3://input"}); err != nil {
		t.Fatal(err)
	}
	if temporal.calls != 1 || temporal.options.ID != "run-1" || temporal.options.TaskQueue != "runner-queue" || temporal.name != agentworkflow.WorkflowName {
		t.Fatalf("unexpected Temporal start: %#v name=%v", temporal.options, temporal.name)
	}
	var testStep agentworkflow.Step
	for _, step := range temporal.input.Manifest.Steps {
		if step.ID == "test" {
			testStep = step
		}
	}
	if temporal.input.InputRef != "s3://input" || len(temporal.input.Manifest.Steps) != 2 || testStep.InputRef == "" || testStep.Retry.MaxAttempts != 3 {
		t.Fatalf("unexpected compiled input: %#v", temporal.input)
	}
}

func TestStarterRejectsRevisionMismatchAndConfirmsDuplicate(t *testing.T) {
	ctx := context.Background()
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	digest := "sha256:" + strings.Repeat("b", 64)
	memory := store.NewMemory()
	applyExecutableAgent(t, memory, scope, "fixer", digest)
	temporal := &fakeTemporal{err: serviceerror.NewWorkflowExecutionAlreadyStarted("already started", "request", "run")}
	starter := Starter{Store: memory, Temporal: temporal}
	run := store.Run{Scope: scope, ID: "run-2", Kind: spec.KindAgentRun, DefinitionDigest: digest, RequestedBy: "user-a"}
	if err := starter.StartRun(ctx, httpapi.RunStartRequest{Scope: scope, Run: run, AgentRef: "fixer"}); err != nil {
		t.Fatalf("duplicate start should confirm success: %v", err)
	}

	run.DefinitionDigest = "sha256:" + strings.Repeat("c", 64)
	if err := starter.StartRun(ctx, httpapi.RunStartRequest{Scope: scope, Run: run, AgentRef: "fixer"}); err == nil {
		t.Fatal("expected revision mismatch rejection")
	}
}

func TestStarterRoutesParentAndAgentSignalsDeterministically(t *testing.T) {
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	temporal := &fakeTemporal{}
	starter := Starter{Signals: temporal}
	if err := starter.SignalRun(context.Background(), scope, "run-1", httpapi.RunSignal{
		Kind: "approval", Target: "workflow", ApprovalID: "approval-1", StepID: "release", Decision: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	if err := starter.SignalRun(context.Background(), scope, "run-1", httpapi.RunSignal{
		Kind: "reply", Target: "agent", StepID: "test", TaskID: "task-1", ReplyRef: "s3://reply/r1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := starter.SignalRun(context.Background(), scope, "run-1", httpapi.RunSignal{Kind: "cancel", Reason: "operator requested stop"}); err != nil {
		t.Fatal(err)
	}
	if len(temporal.signalCalls) != 3 {
		t.Fatalf("signal calls=%#v", temporal.signalCalls)
	}
	if temporal.signalCalls[0].workflowID != "run-1" || temporal.signalCalls[0].signalName != agentworkflow.SignalApproval {
		t.Fatalf("workflow approval was not sent to parent: %#v", temporal.signalCalls[0])
	}
	if temporal.signalCalls[1].workflowID != "run-1/agent/test" || temporal.signalCalls[1].signalName != agentworkflow.SignalUserReply {
		t.Fatalf("agent reply was not sent to deterministic child: %#v", temporal.signalCalls[1])
	}
	reply, ok := temporal.signalCalls[1].argument.(agentworkflow.UserReplySignal)
	if !ok || reply.RunID != "run-1" || reply.StepID != "test" || reply.ReplyRef != "s3://reply/r1" {
		t.Fatalf("unexpected child reply argument=%#v", temporal.signalCalls[1].argument)
	}
	if temporal.signalCalls[2].workflowID != "run-1" || temporal.signalCalls[2].signalName != agentworkflow.SignalCancel {
		t.Fatalf("cancel was not sent to parent: %#v", temporal.signalCalls[2])
	}
}

func TestStarterMapsMissingWorkflowToHTTPBoundaryError(t *testing.T) {
	temporal := &fakeTemporal{signalErr: serviceerror.NewNotFound("run-1")}
	starter := Starter{Signals: temporal}
	err := starter.SignalRun(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "run-1", httpapi.RunSignal{Kind: "cancel"})
	if !errors.Is(err, httpapi.ErrWorkflowNotFound) {
		t.Fatalf("expected workflow not found mapping, got %v", err)
	}
}

func TestCompilerUsesConfiguredRootlessSandboxUser(t *testing.T) {
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	digest := "sha256:" + strings.Repeat("9", 64)
	memory := store.NewMemory()
	applyExecutableAgent(t, memory, scope, "local", digest)
	manifest, err := (Compiler{Store: memory, SandboxUser: "1001:1001"}).CompileRun(context.Background(), httpapi.RunStartRequest{
		Scope: scope,
		Run: store.Run{Scope: scope, ID: "run-local", Kind: spec.KindAgentRun,
			DefinitionDigest: digest, RequestedBy: "owner"},
		AgentRef: "local",
	})
	if err != nil {
		t.Fatalf("compile local agent: %v", err)
	}
	if got := manifest.Steps[0].Execution.RunAsUser; got != "1001:1001" {
		t.Fatalf("run_as_user=%q", got)
	}
}

func applyExecutableAgent(t *testing.T, memory *store.Memory, scope store.Scope, name, agentDigest string) {
	t.Helper()
	imageDigest := "sha256:" + strings.Repeat("f", 64)
	profile := &spec.SandboxProfile{
		ResourceMeta: spec.ResourceMeta{TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: spec.KindSandboxProfile}, Metadata: spec.ObjectMeta{Name: "secure", Namespace: "project-a"}},
		Spec: spec.SandboxProfileSpec{
			Backend: "gvisor", Image: "ghcr.io/example/agent@" + imageDigest,
			Resources:  spec.ResourceLimits{CPU: "1", Memory: "512Mi", Disk: "2Gi", PIDs: 128},
			Filesystem: spec.FilesystemSpec{Root: "read-only", Workspace: "/workspace"},
			Network:    spec.NetworkSpec{Mode: "none"},
		},
	}
	agent := &spec.Agent{
		ResourceMeta: spec.ResourceMeta{TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: spec.KindAgent}, Metadata: spec.ObjectMeta{Name: name, Namespace: "project-a"}},
		Spec: spec.AgentSpec{
			Runtime:           spec.RuntimeSpec{Harness: "codex", Image: "ghcr.io/example/agent@" + imageDigest},
			Instructions:      spec.InstructionsSpec{Inline: "Run the requested test task."},
			SandboxProfileRef: "secure", Limits: spec.RunLimits{Timeout: "5m"},
		},
	}
	for kind, resource := range map[string]spec.Resource{spec.KindSandboxProfile: profile, spec.KindAgent: agent} {
		document, err := spec.AsJSON(resource)
		if err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("d", 64)
		resourceName := resource.Meta().Metadata.Name
		if kind == spec.KindAgent {
			digest = agentDigest
		}
		if _, err := memory.ApplyResource(context.Background(), store.Resource{Scope: scope, Kind: kind, Name: resourceName, Digest: digest, Document: document, AppliedBy: "user-a"}); err != nil {
			t.Fatal(err)
		}
	}
}
