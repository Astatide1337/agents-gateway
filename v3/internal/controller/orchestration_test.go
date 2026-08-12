package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/argoworkflow"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type argoLifecycleStoreFake struct {
	calls int
	body  []byte
	err   error
}

// argoBootstrapWorkFake models the WorkPlanFactory contract relevant to this
// controller test: PlanWork is the owner of the deterministic per-run work
// Secret, while the controller passes the returned plan to SandboxBackend.
// Repeated PlanWork calls validate/reuse the same Secret rather than creating
// another credential projection.
type argoBootstrapWorkFake struct {
	client        client.Client
	calls         int
	secretCreates int
}

func (f *argoBootstrapWorkFake) PlanWork(ctx context.Context, run *v1alpha1.AgentRun, _ resolved.Snapshot) (sandbox.SandboxPlan, error) {
	f.calls++
	if f.client != nil {
		key := client.ObjectKey{Namespace: run.Namespace, Name: workload.WorkSecretName(string(run.UID))}
		current := &corev1.Secret{}
		err := f.client.Get(ctx, key, current)
		if apierrors.IsNotFound(err) {
			controller := true
			blockOwnerDeletion := true
			immutable := true
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Namespace: run.Namespace,
				Name:      key.Name,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "agents.astatide.com/v1alpha1", Kind: "AgentRun", Name: run.Name, UID: run.UID,
					Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion,
				}},
			}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: map[string][]byte{"token": []byte("test-token")}}
			if err := f.client.Create(ctx, secret); err != nil {
				return sandbox.SandboxPlan{}, err
			}
			f.secretCreates++
		} else if err != nil {
			return sandbox.SandboxPlan{}, err
		}
	}
	return sandbox.SandboxPlan{Owner: run.DeepCopy(), Role: sandbox.RoleWork, SpecDigest: run.Status.SpecDigest}, nil
}

// argoBootstrapSandboxFake implements create-or-get semantics so the test can
// distinguish repeated Ensure calls from duplicated Kubernetes children.
type argoBootstrapSandboxFake struct {
	ensureCalls int
	children    map[string]sandbox.SandboxRef
}

func (f *argoBootstrapSandboxFake) Ensure(_ context.Context, plan sandbox.SandboxPlan) (sandbox.SandboxRef, error) {
	f.ensureCalls++
	if plan.Owner == nil {
		return sandbox.SandboxRef{}, errors.New("missing owner")
	}
	name, err := sandbox.ChildName(plan.Owner.UID, plan.Role)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	if f.children == nil {
		f.children = make(map[string]sandbox.SandboxRef)
	}
	if ref, ok := f.children[name]; ok {
		return ref, nil
	}
	ref := sandbox.SandboxRef{
		Namespace: runNamespace(plan.Owner), Name: name, Kind: sandbox.ChildKindSandbox, UID: types.UID("work-sandbox-uid"),
		OwnerUID: plan.Owner.UID, Role: plan.Role, SpecDigest: plan.SpecDigest, PlanFingerprint: testControllerDigest("f"),
	}
	f.children[name] = ref
	return ref, nil
}

func runNamespace(run *v1alpha1.AgentRun) string {
	if run == nil {
		return ""
	}
	return run.Namespace
}

func (f *argoBootstrapSandboxFake) Observe(_ context.Context, ref sandbox.SandboxRef) (sandbox.SandboxObservation, error) {
	if f.children == nil {
		return sandbox.SandboxObservation{Ref: ref}, nil
	}
	_, exists := f.children[ref.Name]
	return sandbox.SandboxObservation{Ref: ref, Exists: exists}, nil
}

func (f *argoBootstrapSandboxFake) Delete(_ context.Context, ref sandbox.SandboxRef) error {
	delete(f.children, ref.Name)
	return nil
}

func (s *argoLifecycleStoreFake) SaveArgoLifecycleOutput(_ context.Context, runUID, _ string, body []byte) (v1alpha1.ArtifactRef, error) {
	s.calls++
	if s.err != nil {
		return v1alpha1.ArtifactRef{}, s.err
	}
	s.body = append([]byte(nil), body...)
	sum := sha256.Sum256(body)
	digest := canonical.DigestPrefix + hex.EncodeToString(sum[:])
	return v1alpha1.ArtifactRef{
		URI:       "s3://agw-artifacts/runs/" + runUID + "/orchestration/" + hex.EncodeToString(sum[:]) + ".json",
		Digest:    digest,
		Kind:      "argo-lifecycle-output",
		Name:      "lifecycle-output.json",
		MediaType: "application/json",
		SizeBytes: int64(len(body)),
	}, nil
}

func TestParseOrchestrationBackendDefaultsToDirect(t *testing.T) {
	for _, raw := range []string{"", "direct", " direct "} {
		backend, err := ParseOrchestrationBackend(raw)
		if err != nil || backend != OrchestrationBackendDirect {
			t.Fatalf("ParseOrchestrationBackend(%q)=%q err=%v, want direct", raw, backend, err)
		}
	}
	if (&AgentRunReconciler{}).usesArgoOrchestration() {
		t.Fatal("zero-value reconciler selected Argo instead of the direct rollback path")
	}
	if _, err := ParseOrchestrationBackend("unknown"); err == nil {
		t.Fatal("unknown orchestration backend was accepted")
	}
}

func TestAcceptArgoLifecycleOutputIsIdempotentAndLeavesAGWDecisionsUntouched(t *testing.T) {
	run := argoHandoffRun(t)
	output := controllerLifecycleOutput(run, "workflow-uid", 4)
	store := &argoLifecycleStoreFake{}
	reconciler := &AgentRunReconciler{LifecycleOutputs: store}
	if err := reconciler.acceptArgoLifecycleOutput(context.Background(), run, output); err != nil {
		t.Fatalf("acceptArgoLifecycleOutput(first): %v", err)
	}
	if run.Status.Phase != v1alpha1.PhasePending || run.Status.Gate != nil || run.Status.Effect != nil || run.Status.Published {
		t.Fatalf("Argo handoff changed AGW decisions: phase=%s gate=%#v effect=%#v published=%v", run.Status.Phase, run.Status.Gate, run.Status.Effect, run.Status.Published)
	}
	if run.Status.Patch == nil || run.Status.Patch.Ref == nil || run.Status.Patch.ManifestRef == nil || run.Status.EventStreamRef == nil || len(run.Status.Artifacts) != 1 {
		t.Fatalf("handoff evidence projection incomplete: %#v", run.Status)
	}
	if !bytes.Equal(store.body, output.Canonical) || store.calls != 1 {
		t.Fatalf("stored calls/body: %d/%q", store.calls, store.body)
	}

	if err := reconciler.acceptArgoLifecycleOutput(context.Background(), run, output); err != nil {
		t.Fatalf("acceptArgoLifecycleOutput(retry): %v", err)
	}
	if len(run.Status.Artifacts) != 1 || store.calls != 2 {
		t.Fatalf("retry duplicated lifecycle output: artifacts=%d calls=%d", len(run.Status.Artifacts), store.calls)
	}
	run.Status.Phase = v1alpha1.PhaseVerifying
	if !argoLifecycleHandoffBound(run) {
		t.Fatal("validated lifecycle handoff was not recognized by routing guard")
	}
}

func TestAcceptArgoLifecycleOutputRejectsReplayTamperingBeforeProjection(t *testing.T) {
	run := argoHandoffRun(t)
	output := controllerLifecycleOutput(run, "workflow-uid", 4)
	store := &argoLifecycleStoreFake{}
	reconciler := &AgentRunReconciler{LifecycleOutputs: store}

	wrongCanonical := output
	wrongCanonical.Canonical = append(append([]byte(nil), output.Canonical...), '\n')
	if err := reconciler.acceptArgoLifecycleOutput(context.Background(), run, wrongCanonical); !errors.Is(err, argoworkflow.ErrLifecycleOutputNonCanonical) {
		t.Fatalf("non-canonical replay error=%v, want ErrLifecycleOutputNonCanonical", err)
	}
	if store.calls != 0 || run.Status.Patch != nil || run.Status.EventStreamRef != nil {
		t.Fatal("non-canonical replay was persisted or projected")
	}

	run.Status.Artifacts = []v1alpha1.ArtifactRef{{
		URI: "s3://agw-artifacts/runs/run-uid/orchestration/old.json", Digest: testControllerDigest("f"),
		Kind: "argo-lifecycle-output", Name: "lifecycle-output.json", MediaType: "application/json", SizeBytes: 1,
	}, {
		URI: "s3://agw-artifacts/runs/run-uid/orchestration/duplicate.json", Digest: testControllerDigest("e"),
		Kind: "argo-lifecycle-output", Name: "lifecycle-output.json", MediaType: "application/json", SizeBytes: 1,
	}}
	if err := reconciler.acceptArgoLifecycleOutput(context.Background(), run, output); !errors.Is(err, errArgoLifecycleOutputDuplicate) {
		t.Fatalf("duplicate replay error=%v, want duplicate", err)
	}
	if store.calls != 0 {
		t.Fatal("duplicate replay reached immutable storage")
	}
}

func TestAdvanceArgoToVerifyingDoesNotCreateGateOrEffectSuccess(t *testing.T) {
	status := v1alpha1.AgentRunStatus{Phase: v1alpha1.PhasePending}
	if err := advanceArgoToVerifying(&status, time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("advanceArgoToVerifying: %v", err)
	}
	if status.Phase != v1alpha1.PhaseVerifying || status.Gate != nil || status.Effect != nil || status.Published {
		t.Fatalf("advance projected domain success: %#v", status)
	}
	if err := advanceArgoToVerifying(&status, time.Time{}); err != nil {
		t.Fatalf("idempotent advance: %v", err)
	}
}

func TestArgoLifecycleOutputRejectsPreexistingDomainAuthority(t *testing.T) {
	base := v1alpha1.AgentRunStatus{Phase: v1alpha1.PhasePending}
	cases := []struct {
		name   string
		mutate func(*v1alpha1.AgentRunStatus)
	}{
		{"Gate", func(s *v1alpha1.AgentRunStatus) { s.Gate = &v1alpha1.GateResult{Verdict: "Accepted"} }},
		{"effect", func(s *v1alpha1.AgentRunStatus) { s.Effect = &v1alpha1.EffectSummary{State: v1alpha1.EffectPending} }},
		{"published", func(s *v1alpha1.AgentRunStatus) { s.Published = true }},
		{"verify child", func(s *v1alpha1.AgentRunStatus) { s.VerifySandboxRef = &v1alpha1.ChildRef{Name: "verify"} }},
		{"terminal phase", func(s *v1alpha1.AgentRunStatus) { s.Phase = v1alpha1.PhaseSucceeded }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status := base
			test.mutate(&status)
			if err := validateArgoPreGateState(status); !errors.Is(err, errArgoLifecycleOutputIdentity) {
				t.Fatalf("error=%v, want identity conflict", err)
			}
		})
	}
}

func TestDriveArgoBlocksBeforeBootstrapWhenExecutionEvidenceIsStale(t *testing.T) {
	scheme := testScheme(t)
	run, body := admittedArgoBootstrapRun(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	work := &argoBootstrapWorkFake{client: kube}
	sandboxes := &argoBootstrapSandboxFake{}
	execution := &executionGateFake{allowed: false}
	driver := &argoDriverFake{}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: &writerFake{body: body},
		Work: work, Sandboxes: sandboxes, Execution: execution,
		OrchestrationBackend: OrchestrationBackendArgo, Orchestration: driver,
	}

	result, err := reconciler.driveArgo(context.Background(), run, time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != argoExecutionRequeueAfter || execution.calls != 1 {
		t.Fatalf("stale execution evidence result=%#v calls=%d", result, execution.calls)
	}
	if work.calls != 0 || sandboxes.ensureCalls != 0 || driver.ensureCalls != 0 {
		t.Fatalf("Argo created work while blocked: plan=%d sandbox=%d workflow=%d", work.calls, sandboxes.ensureCalls, driver.ensureCalls)
	}
	if run.Status.WorkSandboxRef != nil || run.Status.Orchestration != nil {
		t.Fatalf("blocked Argo mutated child/workflow status: %#v", run.Status)
	}
}

func TestDriveArgoRefusesWithoutBootstrapDependencies(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*AgentRunReconciler)
	}{
		{name: "work plan", mutate: func(r *AgentRunReconciler) { r.Work = nil }},
		{name: "sandbox backend", mutate: func(r *AgentRunReconciler) { r.Sandboxes = nil }},
		{name: "execution gate", mutate: func(r *AgentRunReconciler) { r.Execution = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := testScheme(t)
			run, body := admittedArgoBootstrapRun(t)
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
			reconciler := &AgentRunReconciler{
				Client: kube, APIReader: kube, Artifacts: &writerFake{body: body},
				Work: &argoBootstrapWorkFake{client: kube}, Sandboxes: &argoBootstrapSandboxFake{},
				Execution: &executionGateFake{allowed: true}, OrchestrationBackend: OrchestrationBackendArgo,
				Orchestration: &argoDriverFake{},
			}
			test.mutate(reconciler)
			if _, err := reconciler.driveArgo(context.Background(), run, time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("Argo proceeded without a required bootstrap dependency")
			}
		})
	}
}

func TestDriveArgoBootstrapsWorkBeforeWorkflowAndRetriesIdempotently(t *testing.T) {
	scheme := testScheme(t)
	run, body := admittedArgoBootstrapRun(t)
	binding := argoBootstrapBinding(run)
	driver := &argoDriverFake{binding: binding}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	work := &argoBootstrapWorkFake{client: kube}
	sandboxes := &argoBootstrapSandboxFake{}
	execution := &executionGateFake{allowed: true}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: &writerFake{body: body},
		Work: work, Sandboxes: sandboxes, Execution: execution,
		OrchestrationBackend: OrchestrationBackendArgo, Orchestration: driver,
	}
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)

	first, err := reconciler.driveArgo(context.Background(), run, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequeueAfter != 2*time.Second || driver.ensureCalls != 0 {
		t.Fatalf("first Argo reconcile created Workflow or returned wrong retry: result=%#v workflow=%d", first, driver.ensureCalls)
	}
	current := &v1alpha1.AgentRun{}
	key := client.ObjectKeyFromObject(run)
	if err := kube.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.WorkSandboxRef == nil || current.Status.Phase != v1alpha1.PhaseCloning || current.Status.Orchestration != nil {
		t.Fatalf("work/status bootstrap was not persisted before Workflow: phase=%s ref=%#v orchestration=%#v", current.Status.Phase, current.Status.WorkSandboxRef, current.Status.Orchestration)
	}
	secret := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: workload.WorkSecretName(string(run.UID))}, secret); err != nil {
		t.Fatalf("work Secret was not materialized by PlanWork: %v", err)
	}

	second, err := reconciler.driveArgo(context.Background(), current, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.RequeueAfter != 2*time.Second || driver.ensureCalls != 1 || len(driver.ensureSawWorkRef) != 1 || !driver.ensureSawWorkRef[0] {
		t.Fatalf("Workflow was not created after persisted child bootstrap: result=%#v workflow=%d sawRef=%v", second, driver.ensureCalls, driver.ensureSawWorkRef)
	}
	if work.calls != 2 || work.secretCreates != 1 || sandboxes.ensureCalls != 2 || len(sandboxes.children) != 1 {
		t.Fatalf("retry duplicated bootstrap effects: planCalls=%d secretCreates=%d ensureCalls=%d children=%d", work.calls, work.secretCreates, sandboxes.ensureCalls, len(sandboxes.children))
	}
	if err := kube.Get(context.Background(), key, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Orchestration == nil || current.Status.Orchestration.Phase != v1alpha1.OrchestrationPending {
		t.Fatalf("Workflow binding was not persisted after bootstrap: %#v", current.Status.Orchestration)
	}
	if execution.calls != 3 {
		t.Fatalf("fresh execution was not checked before child and Workflow creation: calls=%d, want 3", execution.calls)
	}
}

func TestDriveArgoSuccessOnlyResumesIndependentVerification(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Generation = 3
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.BaseSHA = strings.Repeat("b", 40)
	snapshot := testSnapshot(run, run.Status.BaseSHA, v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatalf("canonical snapshot: %v", err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatalf("snapshot digest: %v", err)
	}
	run.Status.SpecDigest = digest
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/runs/run-uid/resolved.json", Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json", MediaType: "application/json", SizeBytes: int64(len(body))}
	workName, err := sandbox.ChildName(run.UID, sandbox.RoleWork)
	if err != nil {
		t.Fatalf("work child name: %v", err)
	}
	run.Status.Phase = v1alpha1.PhaseCloning
	run.Status.WorkSandboxRef = &v1alpha1.ChildRef{
		Name: workName, Kind: sandbox.ChildKindSandbox, UID: "work-sandbox-uid", Role: string(sandbox.RoleWork),
		SpecDigest: digest, PlanFingerprint: testControllerDigest("f"),
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	writer := &writerFake{body: body}
	store := &argoLifecycleStoreFake{}
	binding := argoworkflow.Binding{
		Reference: argoworkflow.Reference{Namespace: run.Namespace, Name: "agw-run-uid", UID: types.UID("workflow-uid"), Generation: 4},
		RunUID:    string(run.UID), RunName: run.Name, Namespace: run.Namespace,
		WorkflowTemplateName: "agentrun-go-default", WorkflowTemplateUID: "template-uid-1", WorkflowTemplateDigest: testControllerDigest("d"), SpecDigest: digest, BaseSHA: run.Status.BaseSHA,
		ResolvedRef: *run.Status.ResolvedSpecRef, RunGeneration: run.Generation,
	}
	driver := &argoDriverFake{binding: binding}
	lifecycleOutput := controllerLifecycleOutput(run, string(binding.Reference.UID), binding.Reference.Generation)
	driver.observation = argoworkflow.Observation{Reference: binding.Reference, Phase: argoworkflow.BackendSucceeded, RawPhase: "Succeeded", Terminal: true, ObservedGeneration: 4, Output: &lifecycleOutput}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: writer, LifecycleOutputs: store,
		Work: &workPlanFake{}, Sandboxes: &argoBootstrapSandboxFake{}, Execution: &executionGateFake{allowed: true},
		OrchestrationBackend: OrchestrationBackendArgo, Orchestration: driver,
	}
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	if result, err := reconciler.driveArgo(context.Background(), run, now); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("initial driveArgo result=%#v err=%v", result, err)
	}
	if run.Status.Phase != v1alpha1.PhaseCloning || run.Status.Gate != nil || run.Status.Effect != nil {
		t.Fatalf("initial Argo bind changed AGW state: %#v", run.Status)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatalf("get bound run: %v", err)
	}
	if _, err := reconciler.driveArgo(context.Background(), current, now); err != nil {
		t.Fatalf("successful driveArgo: %v", err)
	}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatalf("get handed-off run: %v", err)
	}
	if current.Status.Phase != v1alpha1.PhaseVerifying || current.Status.Gate != nil || current.Status.Effect != nil || current.Status.Published {
		t.Fatalf("Argo success implied domain success: phase=%s gate=%#v effect=%#v published=%v", current.Status.Phase, current.Status.Gate, current.Status.Effect, current.Status.Published)
	}
	if len(current.Status.Artifacts) != 1 || current.Status.Patch == nil || current.Status.EventStreamRef == nil || store.calls != 1 {
		t.Fatalf("validated handoff projection incomplete: status=%#v storeCalls=%d", current.Status, store.calls)
	}
}

type argoDriverFake struct {
	binding          argoworkflow.Binding
	observation      argoworkflow.Observation
	ensureCalls      int
	ensureSawWorkRef []bool
}

func (d *argoDriverFake) Ensure(_ context.Context, run *v1alpha1.AgentRun, _ resolved.Snapshot) (argoworkflow.Binding, error) {
	d.ensureCalls++
	d.ensureSawWorkRef = append(d.ensureSawWorkRef, run != nil && run.Status.WorkSandboxRef != nil)
	return d.binding, nil
}

func (d *argoDriverFake) Observe(context.Context, argoworkflow.Binding) (argoworkflow.Observation, error) {
	return d.observation, nil
}

func (d *argoDriverFake) Delete(context.Context, argoworkflow.Binding) error { return nil }

func argoHandoffRun(t *testing.T) *v1alpha1.AgentRun {
	t.Helper()
	run := testRun()
	run.Generation = 3
	run.Status = v1alpha1.AgentRunStatus{
		Phase:      v1alpha1.PhasePending,
		SpecDigest: testControllerDigest("b"),
		BaseSHA:    strings.Repeat("c", 40),
		Orchestration: &v1alpha1.OrchestrationStatus{
			Backend: v1alpha1.OrchestrationBackendArgo, Phase: v1alpha1.OrchestrationSucceeded,
			Message: "workflow output is bound",
			Reference: v1alpha1.OrchestrationRef{
				APIVersion: argoworkflow.WorkflowAPIVersion, Kind: argoworkflow.WorkflowKind,
				Namespace: run.Namespace, Name: "agw-run-uid", UID: "workflow-uid", Generation: 4,
				RunGeneration: run.Generation, SpecDigest: testControllerDigest("b"), WorkflowTemplateName: "agentrun-go-default", WorkflowTemplateUID: "template-uid-1", WorkflowTemplateDigest: testControllerDigest("d"),
			},
		},
		Published: false,
	}
	return run
}

func controllerLifecycleOutput(run *v1alpha1.AgentRun, workflowUID string, workflowGeneration int64) argoworkflow.BoundLifecycleOutput {
	patch := controllerArtifactRef(string(run.UID), "patch", "1")
	manifest := controllerArtifactRef(string(run.UID), "patch-manifest", "2")
	events := controllerArtifactRef(string(run.UID), "runtime-event-stream", "3")
	completion := controllerArtifactRef(string(run.UID), "runtime-completion", "4")
	input := controllerArtifactRef(string(run.UID), "verification-input", "6")
	contract := argoworkflow.LifecycleOutput{
		SchemaVersion: argoworkflow.LifecycleOutputSchemaVersion, Kind: argoworkflow.LifecycleOutputKind,
		RunUID: string(run.UID), RunGeneration: run.Generation, SpecDigest: run.Status.SpecDigest,
		WorkflowTemplateUID: "template-uid-1", WorkflowTemplateDigest: testControllerDigest("d"),
		WorkflowUID: workflowUID, WorkflowGeneration: workflowGeneration, BaseSHA: run.Status.BaseSHA,
		Patch:        argoworkflow.LifecyclePatchOutput{Ref: patch, ManifestRef: manifest, Digest: patch.Digest, FilesChanged: 2, LinesChanged: 20},
		Runtime:      argoworkflow.RuntimeCompletionProof{EventStreamRef: events, CompletionRef: completion, TerminalEvent: "run.completed", TerminalSequence: 3, TerminalEventDigest: testControllerDigest("5"), CompletionDigest: completion.Digest},
		Verification: argoworkflow.VerificationInputs{InputRef: input, PatchRef: patch, BaseSHA: run.Status.BaseSHA, SpecDigest: run.Status.SpecDigest},
	}
	body, err := argoworkflow.MarshalLifecycleOutput(contract)
	if err != nil {
		panic(err)
	}
	digest, err := argoworkflow.DigestLifecycleOutput(contract)
	if err != nil {
		panic(err)
	}
	return argoworkflow.BoundLifecycleOutput{Contract: contract, Canonical: body, Digest: digest}
}

func controllerArtifactRef(runUID, kind, digit string) v1alpha1.ArtifactRef {
	digest := testControllerDigest(digit)
	return v1alpha1.ArtifactRef{
		URI:    "s3://agw-artifacts/runs/" + runUID + "/" + kind + "/" + strings.TrimPrefix(digest, canonical.DigestPrefix) + ".json",
		Digest: digest, Kind: kind, Name: kind + ".json", MediaType: "application/json", SizeBytes: 1,
	}
}

func admittedArgoBootstrapRun(t *testing.T) (*v1alpha1.AgentRun, []byte) {
	t.Helper()
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.BaseSHA = strings.Repeat("b", 40)
	snapshot := testSnapshot(run, run.Status.BaseSHA, v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.SpecDigest = digest
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{
		URI: "s3://agw-artifacts/runs/" + string(run.UID) + "/resolved.json", Digest: digest,
		Kind: "resolved-spec", Name: "resolved-spec.json", MediaType: "application/json", SizeBytes: int64(len(body)),
	}
	return run, body
}

func argoBootstrapBinding(run *v1alpha1.AgentRun) argoworkflow.Binding {
	return argoworkflow.Binding{
		Reference: argoworkflow.Reference{Namespace: run.Namespace, Name: "agw-run-uid", UID: types.UID("workflow-uid"), Generation: 4},
		RunUID:    string(run.UID), RunName: run.Name, Namespace: run.Namespace,
		WorkflowTemplateName: "agentrun-go-default", WorkflowTemplateUID: "template-uid-1", WorkflowTemplateDigest: testControllerDigest("d"),
		SpecDigest: run.Status.SpecDigest, BaseSHA: run.Status.BaseSHA, ResolvedRef: *run.Status.ResolvedSpecRef, RunGeneration: run.Generation,
	}
}

func testControllerDigest(digit string) string {
	return canonical.DigestPrefix + strings.Repeat(digit, 64)
}

var _ LifecycleOutputStore = (*argoLifecycleStoreFake)(nil)
