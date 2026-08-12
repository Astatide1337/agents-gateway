package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextmaterializer"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextpack"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runplan"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type writerFake struct {
	calls   int
	body    []byte
	loadErr error
}

type contextArtifactReaderFake struct {
	body  []byte
	uri   string
	err   error
	calls int
}

func (f *contextArtifactReaderFake) LoadContextPack(context.Context, string, string, string) ([]byte, string, error) {
	f.calls++
	if f.err != nil {
		return nil, "", f.err
	}
	return append([]byte(nil), f.body...), f.uri, nil
}

func (w *writerFake) SaveResolvedSpec(_ context.Context, _ string, digest string, body []byte) (v1alpha1.ArtifactRef, error) {
	w.calls++
	w.body = append([]byte(nil), body...)
	return v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec", Name: "resolved.json", MediaType: "application/json", SizeBytes: int64(len(body))}, nil
}

func (w *writerFake) LoadResolvedSpec(_ context.Context, _, _ string) ([]byte, error) {
	if w.loadErr != nil {
		return nil, w.loadErr
	}
	return append([]byte(nil), w.body...), nil
}

func TestProjectContextPackUsesOnlyValidatedImmutableEvidence(t *testing.T) {
	run := testRun()
	run.Status.Phase = v1alpha1.PhaseWorking
	run.Status.SpecDigest = "sha256:" + strings.Repeat("a", 64)
	run.Status.BaseSHA = strings.Repeat("b", 40)
	body, err := json.Marshal(contextartifact.Bundle{
		SchemaVersion:      contextartifact.SchemaVersion,
		RunUID:             string(run.UID),
		ResolvedSpecDigest: run.Status.SpecDigest,
		BaseSHA:            run.Status.BaseSHA,
		ContextPack: contextartifact.PackIdentity{
			SchemaVersion: contextmaterializer.RefSchemaVersion,
			RunUID:        string(run.UID), ResolvedSpecDigest: run.Status.SpecDigest,
			BaseSHA: run.Status.BaseSHA, ManifestPath: contextartifact.ManifestPath,
			Digest: "sha256:" + strings.Repeat("c", 64),
		},
		Files:   []contextartifact.FileDescriptor{{Path: "AGENTS.md", Digest: "sha256:" + strings.Repeat("d", 64), SizeBytes: 1, TokenEstimate: 1, Kind: "instructions"}},
		Budgets: contextpack.Budgets{MaxInputBytes: 1, MaxOutputBytes: 1, MaxFileBytes: 1, MaxFiles: 1, MaxTokens: 1, MaxEntries: 1},
		Usage:   contextpack.BudgetUsage{FileCount: 2, OutputBytes: 2, InputBytes: 1, TokenEstimate: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err = strictjson.Normalize(body)
	if err != nil {
		t.Fatal(err)
	}
	reader := &contextArtifactReaderFake{body: body, uri: "s3://bucket/runs/run-uid/context-pack/context.json"}
	reconciler := &AgentRunReconciler{Artifacts: &writerFake{}, ContextArtifacts: reader}
	changed, err := reconciler.projectContextPack(context.Background(), run)
	if err != nil || !changed || run.Status.ContextPackRef == nil {
		t.Fatalf("projection changed=%v err=%v ref=%#v", changed, err, run.Status.ContextPackRef)
	}
	if run.Status.ContextPackRef.Kind != contextartifact.Kind || run.Status.ContextPackRef.Digest == "" {
		t.Fatalf("projected ref=%#v", run.Status.ContextPackRef)
	}
	reader.body = append(body, '\n')
	if _, err := reconciler.projectContextPack(context.Background(), run); err == nil {
		t.Fatal("tampered immutable evidence was accepted")
	}
}

type cleanerFake struct {
	refs        []sandbox.SandboxRef
	ensured     []sandbox.SandboxPlan
	observation sandbox.SandboxObservation
	ensureRef   *sandbox.SandboxRef
	orphaned    []struct {
		namespace string
		ownerUID  types.UID
		role      sandbox.Role
		digest    string
	}
}

func (c *cleanerFake) Ensure(_ context.Context, plan sandbox.SandboxPlan) (sandbox.SandboxRef, error) {
	c.ensured = append(c.ensured, plan)
	if c.ensureRef != nil {
		return *c.ensureRef, nil
	}
	name, err := sandbox.ChildName(plan.Owner.UID, plan.Role)
	if err != nil {
		return sandbox.SandboxRef{}, err
	}
	return sandbox.SandboxRef{Namespace: plan.Owner.Namespace, Name: name, Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"), OwnerUID: plan.Owner.UID, Role: plan.Role, SpecDigest: plan.SpecDigest, PlanFingerprint: testPlanFingerprint()}, nil
}

func testPlanFingerprint() string { return "sha256:" + strings.Repeat("f", 64) }

func testChildName(run *v1alpha1.AgentRun, role sandbox.Role) string {
	name, err := sandbox.ChildName(run.UID, role)
	if err != nil {
		panic(err)
	}
	return name
}

func TestSandboxChildReferenceRoundTripPreservesPlanFingerprint(t *testing.T) {
	run := testRun()
	original := sandbox.SandboxRef{
		Namespace: run.Namespace, Name: "agw-work-child", Kind: sandbox.ChildKindSandbox, UID: types.UID("sandbox-uid"),
		OwnerUID: run.UID, Role: sandbox.RoleWork,
		SpecDigest: "sha256:" + strings.Repeat("a", 64), PlanFingerprint: testPlanFingerprint(),
	}
	statusRef := childRef(original)
	if statusRef.PlanFingerprint != original.PlanFingerprint {
		t.Fatalf("status planFingerprint=%q, want %q", statusRef.PlanFingerprint, original.PlanFingerprint)
	}
	if statusRef.Kind != original.Kind {
		t.Fatalf("status kind=%q, want %q", statusRef.Kind, original.Kind)
	}
	restarted := sandboxRef(run, statusRef, sandbox.RoleWork)
	if restarted.PlanFingerprint != original.PlanFingerprint || restarted.SpecDigest != original.SpecDigest {
		t.Fatalf("restarted reference=%#v, want persisted digests from %#v", restarted, original)
	}
	statusRef.PlanFingerprint = ""
	if got := sandboxRef(run, statusRef, sandbox.RoleWork).PlanFingerprint; got != "" {
		t.Fatalf("missing plan fingerprint was synthesized as %q", got)
	}
}

func TestJobChildReferenceRoundTripPreservesKindAndPlanFingerprint(t *testing.T) {
	run := testRun()
	original := sandbox.SandboxRef{
		Namespace: run.Namespace, Name: "agw-work-child", Kind: sandbox.ChildKindJob, UID: types.UID("job-uid"),
		OwnerUID: run.UID, Role: sandbox.RoleWork,
		SpecDigest: "sha256:" + strings.Repeat("a", 64), PlanFingerprint: testPlanFingerprint(),
	}
	statusRef := childRef(original)
	restarted := sandboxRef(run, statusRef, sandbox.RoleWork)
	if restarted.Kind != sandbox.ChildKindJob || restarted.PlanFingerprint != original.PlanFingerprint {
		t.Fatalf("restarted reference=%#v, want Job kind and persisted fingerprint", restarted)
	}
}

func TestOwnedResourcesAreBackendSpecific(t *testing.T) {
	tests := []struct {
		name string
		kind sandbox.BackendKind
		want []string
	}{
		{name: "agent sandbox", kind: sandbox.BackendAgentSandbox, want: []string{"Job", "Sandbox", "NetworkPolicy"}},
		{name: "job fallback", kind: sandbox.BackendJob, want: []string{"Job", "PersistentVolumeClaim", "NetworkPolicy"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects, err := ownedResourcesForBackend(test.kind)
			if err != nil {
				t.Fatal(err)
			}
			// Unregistered typed objects do not carry a GVK, so compare their
			// concrete type names as the watch-construction contract.
			got := make([]string, 0, len(objects))
			for _, object := range objects {
				got = append(got, fmt.Sprintf("%T", object))
			}
			want := make([]string, 0, len(test.want))
			for _, name := range test.want {
				want = append(want, map[string]string{
					"Job":                   "*v1.Job",
					"Sandbox":               "*v1beta1.Sandbox",
					"PersistentVolumeClaim": "*v1.PersistentVolumeClaim",
					"NetworkPolicy":         "*v1.NetworkPolicy",
				}[name])
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("owned resource types=%v, want=%v", got, want)
			}
		})
	}
	if _, err := ownedResourcesForBackend("bogus"); err == nil {
		t.Fatal("unknown backend was accepted by watch construction")
	}
}

func (c *cleanerFake) Observe(_ context.Context, ref sandbox.SandboxRef) (sandbox.SandboxObservation, error) {
	observation := c.observation
	observation.Ref = ref
	return observation, nil
}

func (c *cleanerFake) Delete(_ context.Context, ref sandbox.SandboxRef) error {
	c.refs = append(c.refs, ref)
	return nil
}

func (c *cleanerFake) CleanupOwned(_ context.Context, namespace string, ownerUID types.UID, role sandbox.Role, digest string) error {
	c.orphaned = append(c.orphaned, struct {
		namespace string
		ownerUID  types.UID
		role      sandbox.Role
		digest    string
	}{namespace: namespace, ownerUID: ownerUID, role: role, digest: digest})
	return nil
}

type workPlanFake struct{ calls int }

func (w *workPlanFake) PlanWork(_ context.Context, run *v1alpha1.AgentRun, _ resolved.Snapshot) (sandbox.SandboxPlan, error) {
	w.calls++
	return sandbox.SandboxPlan{Owner: run.DeepCopy(), Role: sandbox.RoleWork, SpecDigest: run.Status.SpecDigest}, nil
}

type executionGateFake struct {
	allowed bool
	calls   int
}

type completionSourceFake struct {
	record         runtimeevents.CompletionRecord
	err            error
	calls          int
	finalizeRecord runtimeevents.CompletionRecord
	finalizeErr    error
	finalizeCalls  int
}

type processExitFake struct {
	exited bool
	exit   runtimeevents.ProcessExit
	err    error
	calls  int
}

type sourcePinnerFake struct {
	calls int
	err   error
}

type capturePhaseFake struct {
	outcome      CaptureOutcome
	err          error
	calls        int
	cleanupCalls int
}

type verifyPhaseFake struct {
	outcome VerifyOutcome
	err     error
	calls   int
}

type publishPhaseFake struct {
	outcome PublishOutcome
	err     error
	calls   int
}

func (f *publishPhaseFake) Publish(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (PublishOutcome, error) {
	f.calls++
	return f.outcome, f.err
}

func (f *verifyPhaseFake) Verify(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (VerifyOutcome, error) {
	f.calls++
	return f.outcome, f.err
}

func (f *capturePhaseFake) Capture(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (CaptureOutcome, error) {
	f.calls++
	return f.outcome, f.err
}

func (f *capturePhaseFake) Cleanup(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) error {
	f.cleanupCalls++
	return nil
}

func (p *sourcePinnerFake) PinSource(_ context.Context, input resolved.Result) (resolved.Result, error) {
	p.calls++
	if p.err != nil {
		return resolved.Result{}, p.err
	}
	return resolved.WithBaseSHA(input, strings.Repeat("e", 40))
}

func (s *completionSourceFake) LoadCompletion(context.Context, string, string) (runtimeevents.CompletionRecord, error) {
	s.calls++
	return s.record, s.err
}

func (s *completionSourceFake) FinalizeObservedExit(context.Context, string, string, string, runtimeevents.ProcessExit) (runtimeevents.CompletionRecord, error) {
	s.finalizeCalls++
	return s.finalizeRecord, s.finalizeErr
}

func (s *processExitFake) ObserveAgentExit(context.Context, sandbox.SandboxRef) (bool, runtimeevents.ProcessExit, error) {
	s.calls++
	return s.exited, s.exit, s.err
}

func (g *executionGateFake) CheckExecution(context.Context, time.Time) ExecutionDecision {
	g.calls++
	return ExecutionDecision{Allowed: g.allowed, Reason: "test"}
}

func TestReconcileAddsFinalizerThenPersistsOneImmutableResolution(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	writer := &writerFake{}
	cleaner := &cleanerFake{}
	pinner := &sourcePinnerFake{}
	resolves := 0
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: writer, Sandboxes: cleaner, Source: pinner, Clock: func() time.Time { return time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC) }, Resolve: func(_ context.Context, _ client.Reader, run *v1alpha1.AgentRun) (resolved.Result, error) {
		resolves++
		snapshot := testSnapshot(run, "", v1alpha1.GateEnforcing)
		body, err := canonical.CanonicalizeResolvedSpec(snapshot)
		if err != nil {
			return resolved.Result{}, err
		}
		digest, err := canonical.ResolvedSpecDigest(body)
		return resolved.Result{Snapshot: snapshot, Canonical: body, Digest: digest}, err
	}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if !contains(current.Finalizers, AgentRunFinalizer) || current.Status.Phase != v1alpha1.PhasePending || current.Status.SpecDigest == "" || current.Status.ResolvedSpecRef == nil {
		t.Fatalf("run not admitted: %#v", current)
	}
	if resolves != 1 || writer.calls != 1 || pinner.calls != 1 {
		t.Fatalf("resolve/write calls=%d/%d", resolves, writer.calls)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if resolves != 1 || writer.calls != 1 {
		t.Fatal("admitted run was re-resolved")
	}
}

func TestReconcileRecoversResolutionCheckpointWithoutReresolving(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.BaseSHA = strings.Repeat("e", 40)
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
	writer := &writerFake{body: body}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	resolves := 0
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: writer, Sandboxes: &cleanerFake{}, Source: &sourcePinnerFake{},
		Clock: func() time.Time { return time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC) },
		Resolve: func(context.Context, client.Reader, *v1alpha1.AgentRun) (resolved.Result, error) {
			resolves++
			return resolved.Result{}, errors.New("mutable resolution must not be retried")
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if resolves != 0 || writer.calls != 1 || current.Status.ResolvedSpecRef == nil {
		t.Fatalf("checkpoint recovery calls=%d/%d ref=%#v", resolves, writer.calls, current.Status.ResolvedSpecRef)
	}
}

func TestTerminalRunImmediatelyReapsPerRunCredentials(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhaseSucceeded
	secret := ownedRunSecret(run, workload.WorkSecretName(string(run.UID)))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run, secret).Build()
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatal(err)
	}
	err := kube.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("per-run credential Secret still exists or lookup failed unexpectedly: %v", err)
	}
}

func TestCleanupRefusesForeignPerRunCredentialSecret(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhaseSucceeded
	controller := true
	blockOwnerDeletion := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: run.Namespace,
		Name:      workload.WorkSecretName(string(run.UID)),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         v1alpha1.GroupVersion.String(),
			Kind:               "AgentRun",
			Name:               "other-run",
			UID:                types.UID("other-uid"),
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}},
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run, secret).Build()
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); !errors.Is(err, errForeignRunSecret) {
		t.Fatalf("cleanup error = %v, want foreign Secret ownership error", err)
	}
	current := &corev1.Secret{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(secret), current); err != nil {
		t.Fatalf("foreign Secret was deleted or lookup failed: %v", err)
	}
	currentRun := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), currentRun); err != nil {
		t.Fatal(err)
	}
	if !contains(currentRun.Finalizers, AgentRunFinalizer) {
		t.Fatal("finalizer was removed despite foreign Secret")
	}
}

func TestTerminalCleanupRevokesCredentialsAndSandboxesWhenArtifactStoreIsUnavailable(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhaseSucceeded
	run.Status.SpecDigest = "sha256:" + strings.Repeat("b", 64)
	run.Status.WorkSandboxRef = &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleWork), Kind: "Sandbox", UID: "work-uid", Role: string(sandbox.RoleWork), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint()}
	run.Status.VerifySandboxRef = &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleVerify), Kind: "Sandbox", UID: "verify-uid", Role: string(sandbox.RoleVerify), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint()}
	workSecret := ownedRunSecret(run, workload.WorkSecretName(string(run.UID)))
	verifySecret := ownedRunSecret(run, workload.VerifySecretName(string(run.UID)))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run, workSecret, verifySecret).Build()
	cleaner := &cleanerFake{}
	captureDriver := &capturePhaseFake{}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube,
		Artifacts: &writerFake{loadErr: errors.New("object store unavailable")},
		Sandboxes: cleaner, Capture: captureDriver,
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil || !strings.Contains(err.Error(), "load immutable resolved spec") {
		t.Fatalf("cleanup error = %v, want retained finalizer due unavailable immutable snapshot", err)
	}
	for _, secret := range []*corev1.Secret{workSecret, verifySecret} {
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("credential Secret %s survived independent cleanup: %v", secret.Name, err)
		}
	}
	if len(cleaner.refs) != 2 || cleaner.refs[0].Role != sandbox.RoleWork || cleaner.refs[1].Role != sandbox.RoleVerify {
		t.Fatalf("Sandbox cleanup refs = %#v, want work and verify", cleaner.refs)
	}
	if captureDriver.cleanupCalls != 0 {
		t.Fatalf("capture cleanup ran without its immutable plan: %d calls", captureDriver.cleanupCalls)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatal(err)
	}
	if !contains(current.Finalizers, AgentRunFinalizer) {
		t.Fatal("cleanup finalizer was removed despite incomplete capture cleanup")
	}
}

func ownedRunSecret(run *v1alpha1.AgentRun, name string) *corev1.Secret {
	controller := true
	blockOwnerDeletion := true
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: run.Namespace,
		Name:      name,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion:         v1alpha1.GroupVersion.String(),
			Kind:               "AgentRun",
			Name:               run.Name,
			UID:                run.UID,
			Controller:         &controller,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}},
	}}
}

func TestCancellationCleansChildrenAndBecomesTerminalWithoutPreflight(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Spec.CancelRequested = true
	run.Status.Phase = v1alpha1.PhaseWorking
	run.Status.SpecDigest = "sha256:" + strings.Repeat("b", 64)
	run.Status.WorkSandboxRef = &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleWork), Kind: "Sandbox", UID: "work-uid", Role: string(sandbox.RoleWork), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint()}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	cleaner := &cleanerFake{}
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: cleaner}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PhaseCancelled || len(cleaner.refs) != 1 || cleaner.refs[0].OwnerUID != run.UID {
		t.Fatalf("cancel result=%s refs=%#v", current.Status.Phase, cleaner.refs)
	}
}

func TestResolutionFailureIsBoundedTerminalFailure(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}, Resolve: func(context.Context, client.Reader, *v1alpha1.AgentRun) (resolved.Result, error) {
		return resolved.Result{}, resolved.ErrReferenceMissing
	}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	_ = kube.Get(context.Background(), request.NamespacedName, current)
	if current.Status.Phase != v1alpha1.PhaseFailed || current.Status.CompletedAt == nil || current.Status.Failure == nil || strings.Contains(current.Status.Failure.Message, "missing") {
		t.Fatalf("unsafe failure projection=%#v", current.Status)
	}
}

func TestTransientResolutionFailureIsRetriedWithoutTerminalMutation(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}, Resolve: func(context.Context, client.Reader, *v1alpha1.AgentRun) (resolved.Result, error) {
		return resolved.Result{}, errors.New("temporary API transport failure")
	}}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil {
		t.Fatal("transient resolution failure was swallowed")
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != "" || current.Status.Failure != nil {
		t.Fatalf("transient failure became terminal: %#v", current.Status)
	}
}

func TestPermanentSourcePinFailureBecomesTerminalWithoutRetry(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	pinner := &sourcePinnerFake{err: runplan.ErrInvalidRepository}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}, Source: pinner,
		Resolve: func(_ context.Context, _ client.Reader, run *v1alpha1.AgentRun) (resolved.Result, error) {
			snapshot := testSnapshot(run, "", v1alpha1.GateEnforcing)
			body, err := canonical.CanonicalizeResolvedSpec(snapshot)
			if err != nil {
				return resolved.Result{}, err
			}
			digest, err := canonical.ResolvedSpecDigest(body)
			if err != nil {
				return resolved.Result{}, err
			}
			return resolved.Result{Snapshot: snapshot, Canonical: body, Digest: digest}, nil
		},
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PhaseFailed || current.Status.Failure == nil || current.Status.CompletedAt == nil {
		t.Fatalf("permanent source failure was not terminal: %#v", current.Status)
	}
	if pinner.calls != 1 || current.Status.SpecDigest != "" {
		t.Fatalf("source failure was retried or partially admitted: calls=%d digest=%q", pinner.calls, current.Status.SpecDigest)
	}
}

func TestAdmittedRunCreatesAndObservesWorkSandbox(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	snapshot := testSnapshot(run, strings.Repeat("c", 40), v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.SpecDigest = digest
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec"}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	store := &writerFake{body: body}
	backend := &cleanerFake{observation: sandbox.SandboxObservation{Exists: true, Ready: true}}
	work := &workPlanFake{}
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: store, Sandboxes: backend, Work: work, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{err: runtimeevents.ErrMissing}, Clock: func() time.Time { return time.Date(2026, 8, 11, 17, 0, 0, 0, time.UTC) }}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PhaseCloning || current.Status.WorkSandboxRef == nil || work.calls != 1 || len(backend.ensured) != 1 {
		t.Fatalf("work was not started safely: phase=%s ref=%#v plans=%d", current.Status.Phase, current.Status.WorkSandboxRef, len(backend.ensured))
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.PhaseWorking {
		t.Fatalf("ready Sandbox did not advance to Working: %s", current.Status.Phase)
	}
}

func TestDirectChildReferenceSurvivesControllerRestartAndTerminalCleanup(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	snapshot := testSnapshot(run, strings.Repeat("c", 40), v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.SpecDigest = digest
	run.Status.BaseSHA = snapshot.BaseSHA
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec"}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	backend := &cleanerFake{observation: sandbox.SandboxObservation{Exists: true, Ready: true}}
	work := &workPlanFake{}
	common := func() *AgentRunReconciler {
		return &AgentRunReconciler{
			Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: backend,
			Work: work, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{err: runtimeevents.ErrMissing},
		}
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}

	// Reconcile once as the original controller. The status write is the
	// restart boundary; the child is already present and has a deterministic
	// owner-bound handle.
	if _, err := common().Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	persisted := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), request.NamespacedName, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != v1alpha1.PhaseCloning || persisted.Status.WorkSandboxRef == nil {
		t.Fatalf("first reconcile status=%#v", persisted.Status)
	}
	firstRef := *persisted.Status.WorkSandboxRef

	// A fresh reconciler instance must consume the bounded status handle rather
	// than re-resolve or create a second logical child.
	if _, err := common().Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(context.Background(), request.NamespacedName, persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status.Phase != v1alpha1.PhaseWorking || *persisted.Status.WorkSandboxRef != firstRef {
		t.Fatalf("restart changed child identity or phase: phase=%s ref=%#v first=%#v", persisted.Status.Phase, persisted.Status.WorkSandboxRef, firstRef)
	}
	if len(backend.ensured) != 1 {
		t.Fatalf("restart created %d logical work children, want 1", len(backend.ensured))
	}

	// Terminal reconciliation uses the same validated status reference for
	// cleanup, so it cannot delete an arbitrary same-name/foreign child.
	persisted.Status.Phase = v1alpha1.PhaseSucceeded
	if err := kube.Status().Update(context.Background(), persisted); err != nil {
		t.Fatal(err)
	}
	if _, err := common().Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(backend.refs) != 1 || backend.refs[0].Name != firstRef.Name || backend.refs[0].OwnerUID != run.UID || backend.refs[0].PlanFingerprint != firstRef.PlanFingerprint {
		t.Fatalf("terminal cleanup reference=%#v, want %#v", backend.refs, firstRef)
	}
}

func TestDirectControllerRejectsForeignChildBeforeStatusProjection(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	snapshot := testSnapshot(run, strings.Repeat("c", 40), v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.SpecDigest = digest
	run.Status.BaseSHA = snapshot.BaseSHA
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec"}
	foreign := &sandbox.SandboxRef{
		Namespace: "other-namespace", Name: "foreign-child", Kind: sandbox.ChildKindSandbox,
		UID: types.UID("foreign-uid"), OwnerUID: run.UID, Role: sandbox.RoleWork,
		SpecDigest: digest, PlanFingerprint: testPlanFingerprint(),
	}
	backend := &cleanerFake{ensureRef: foreign}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: backend,
		Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{err: runtimeevents.ErrMissing},
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil || !strings.Contains(err.Error(), "validate work Sandbox reference") {
		t.Fatalf("foreign child was accepted: %v", err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.WorkSandboxRef != nil || current.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("foreign child was projected into status: %#v", current.Status)
	}
}

func TestTerminalCleanupRefusesUnboundPersistedChildReference(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhaseSucceeded
	run.Status.SpecDigest = "sha256:" + strings.Repeat("b", 64)
	run.Status.WorkSandboxRef = &v1alpha1.ChildRef{
		Name: "foreign-child", Kind: sandbox.ChildKindSandbox, UID: "foreign-uid",
		Role: string(sandbox.RoleWork), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint(),
	}
	backend := &cleanerFake{}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: backend}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err == nil || !strings.Contains(err.Error(), "validate work child reference during cleanup") {
		t.Fatalf("unbound child was deleted: %v", err)
	}
	if len(backend.refs) != 0 {
		t.Fatalf("cleanup backend was called for unbound child: %#v", backend.refs)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatal(err)
	}
	if !contains(current.Finalizers, AgentRunFinalizer) {
		t.Fatal("finalizer was removed after refusing an unbound child")
	}
}

func TestPendingRunDoesNotStartWithoutFreshExecutionEvidence(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	snapshot := testSnapshot(run, strings.Repeat("c", 40), v1alpha1.GateEnforcing)
	body, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := canonical.ResolvedSpecDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	run.Status.Phase = v1alpha1.PhasePending
	run.Status.SpecDigest = digest
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec"}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	backend := &cleanerFake{}
	work := &workPlanFake{}
	gate := &executionGateFake{allowed: false}
	reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: backend, Work: work, Execution: gate, Events: &completionSourceFake{err: runtimeevents.ErrMissing}}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 10*time.Second || gate.calls != 1 || work.calls != 0 || len(backend.ensured) != 0 {
		t.Fatalf("execution was not fail-closed: result=%#v gate=%d work=%d sandboxes=%d", result, gate.calls, work.calls, len(backend.ensured))
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatal(err)
	}
	foundBlocked := false
	for _, condition := range current.Status.Conditions {
		if condition.Type == string(v1alpha1.ConditionReady) && condition.Status == v1alpha1.ConditionFalse && condition.Reason == "test" {
			foundBlocked = true
			break
		}
	}
	if !foundBlocked {
		t.Fatalf("execution block was not projected into Ready condition: %#v", current.Status.Conditions)
	}
}

func TestWorkingAdvancesOnlyFromDurableTerminalCompletion(t *testing.T) {
	scheme := testScheme(t)
	now := time.Date(2026, 8, 11, 18, 0, 0, 0, time.UTC)
	newWorkingRun := func() *v1alpha1.AgentRun {
		run := testRun()
		run.Finalizers = []string{AgentRunFinalizer}
		run.Status.Phase = v1alpha1.PhaseWorking
		run.Status.SpecDigest = "sha256:" + strings.Repeat("a", 64)
		run.Status.BaseSHA = strings.Repeat("c", 40)
		run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: run.Status.SpecDigest, Kind: "resolved-spec", Name: "resolved-spec.json"}
		run.Status.WorkSandboxRef = &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleWork), Kind: "Sandbox", UID: "work-uid", Role: string(sandbox.RoleWork), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint()}
		return run
	}
	t.Run("completed", func(t *testing.T) {
		run := newWorkingRun()
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
		events := &completionSourceFake{record: runtimeevents.CompletionRecord{
			RunUID: string(run.UID), SpecDigest: run.Status.SpecDigest, BaseSHA: strings.Repeat("c", 40), TerminalType: runtimeevents.TerminalCompleted,
			EventStream: runtimeevents.ArtifactRef{URI: "s3://bucket/events.json", Digest: "sha256:" + strings.Repeat("b", 64), Kind: "runtime-event-stream", Name: "events.jsonl", MediaType: "application/x-ndjson", SizeBytes: 128},
		}}
		reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{}, Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: events, Clock: func() time.Time { return now }}
		result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
		if err != nil {
			t.Fatal(err)
		}
		current := &v1alpha1.AgentRun{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
			t.Fatal(err)
		}
		if !result.Requeue || current.Status.Phase != v1alpha1.PhaseCapturing || current.Status.EventStreamRef == nil || events.calls != 1 {
			t.Fatalf("completion was not projected: result=%#v status=%#v", result, current.Status)
		}
	})

	t.Run("process exit without terminal event fails", func(t *testing.T) {
		run := newWorkingRun()
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
		events := &completionSourceFake{err: runtimeevents.ErrMissing, finalizeErr: runtimeevents.ErrNoTerminal}
		reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: &cleanerFake{observation: sandbox.SandboxObservation{Exists: true}}, Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: events, Processes: &processExitFake{exited: true, exit: runtimeevents.ProcessExit{Known: true, Code: 0}}, Clock: func() time.Time { return now }}
		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
			t.Fatal(err)
		}
		current := &v1alpha1.AgentRun{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase != v1alpha1.PhaseFailed || current.Status.Failure == nil || current.Status.Failure.Code != "WorkExitedWithoutCompletion" {
			t.Fatalf("process exit was mistaken for completion: %#v", current.Status)
		}
	})

	t.Run("Kubernetes exit finalizes durable terminal stream", func(t *testing.T) {
		run := newWorkingRun()
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
		record := runtimeevents.CompletionRecord{
			RunUID: string(run.UID), SpecDigest: run.Status.SpecDigest, BaseSHA: run.Status.BaseSHA,
			TerminalType: runtimeevents.TerminalCompleted,
			EventStream:  runtimeevents.ArtifactRef{URI: "s3://bucket/events.json", Digest: "sha256:" + strings.Repeat("b", 64), Kind: "runtime-event-stream", Name: "events.jsonl", MediaType: "application/x-ndjson", SizeBytes: 128},
		}
		events := &completionSourceFake{err: runtimeevents.ErrMissing, finalizeRecord: record}
		processes := &processExitFake{exited: true, exit: runtimeevents.ProcessExit{Known: true, Code: 0}}
		reconciler := &AgentRunReconciler{
			Client: kube, APIReader: kube, Artifacts: &writerFake{},
			Sandboxes: &cleanerFake{observation: sandbox.SandboxObservation{Exists: true}},
			Work:      &workPlanFake{}, Execution: &executionGateFake{allowed: true},
			Events: events, Processes: processes, Clock: func() time.Time { return now },
		}
		if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
			t.Fatal(err)
		}
		current := &v1alpha1.AgentRun{}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase != v1alpha1.PhaseCapturing || events.finalizeCalls != 1 || processes.calls != 1 || current.Status.EventStreamRef == nil {
			t.Fatalf("status=%#v events=%#v processes=%#v", current.Status, events, processes)
		}
	})
}

func TestCloningDoesNotLoopForeverAfterSandboxDisappearsOrFinishes(t *testing.T) {
	for _, test := range []struct {
		name        string
		observation sandbox.SandboxObservation
		failureCode string
	}{
		{name: "missing", observation: sandbox.SandboxObservation{}, failureCode: "WorkSandboxMissing"},
		{name: "finished before ready", observation: sandbox.SandboxObservation{Exists: true, Finished: true}, failureCode: "WorkSandboxSetupFailed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := testScheme(t)
			run := testRun()
			run.Finalizers = []string{AgentRunFinalizer}
			run.Status.Phase = v1alpha1.PhaseCloning
			run.Status.SpecDigest = "sha256:" + strings.Repeat("a", 64)
			run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: run.Status.SpecDigest, Kind: "resolved-spec", Name: "resolved-spec.json"}
			run.Status.WorkSandboxRef = &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleWork), Kind: "Sandbox", UID: "work-uid", Role: string(sandbox.RoleWork), SpecDigest: run.Status.SpecDigest, PlanFingerprint: testPlanFingerprint()}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
			backend := &cleanerFake{observation: test.observation}
			reconciler := &AgentRunReconciler{Client: kube, APIReader: kube, Artifacts: &writerFake{}, Sandboxes: backend, Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{err: runtimeevents.ErrMissing}, Clock: func() time.Time { return time.Date(2026, 8, 11, 17, 0, 0, 0, time.UTC) }}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			current := &v1alpha1.AgentRun{}
			if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != v1alpha1.PhaseFailed || current.Status.Failure == nil || current.Status.Failure.Code != test.failureCode {
				t.Fatalf("unexpected terminal status: %#v", current.Status)
			}
		})
	}
}

func TestCapturingProjectsOnlyValidatedImmutablePatch(t *testing.T) {
	scheme := testScheme(t)
	run := testRun()
	run.Finalizers = []string{AgentRunFinalizer}
	run.Status.Phase = v1alpha1.PhaseCapturing
	run.Status.WorkSandboxRef = &v1alpha1.ChildRef{Name: "pending", Kind: "Sandbox", Role: string(sandbox.RoleWork), PlanFingerprint: testPlanFingerprint()}
	run.Status.BaseSHA = strings.Repeat("c", 40)
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
	run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json"}
	run.Status.WorkSandboxRef.Name = testChildName(run, sandbox.RoleWork)
	run.Status.WorkSandboxRef.UID = "work-uid"
	run.Status.WorkSandboxRef.SpecDigest = digest
	patchDigest := "sha256:" + strings.Repeat("d", 64)
	manifestDigest := "sha256:" + strings.Repeat("e", 64)
	phase := &capturePhaseFake{outcome: CaptureOutcome{
		Artifact:         v1alpha1.ArtifactRef{URI: "s3://bucket/patch.diff", Digest: patchDigest, Kind: "patch", Name: "patch.diff", MediaType: "text/x-diff", SizeBytes: 12},
		ManifestArtifact: v1alpha1.ArtifactRef{URI: "s3://bucket/patch-manifest.json", Digest: manifestDigest, Kind: "patch-manifest", Name: "patch-manifest.json", MediaType: "application/json", SizeBytes: 24},
		Validated:        capture.ValidatedOutput{Envelope: capture.ResultEnvelope{PatchDigest: patchDigest, ManifestDigest: manifestDigest, FilesChanged: 2, LinesChanged: 7}},
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
	backend := &cleanerFake{}
	reconciler := &AgentRunReconciler{
		Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: backend,
		Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{}, Capture: phase,
	}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatal(err)
	}
	current := &v1alpha1.AgentRun{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
		t.Fatal(err)
	}
	if !result.Requeue || phase.calls != 1 || phase.cleanupCalls != 1 || len(backend.refs) != 1 || backend.refs[0].Role != sandbox.RoleWork || current.Status.Phase != v1alpha1.PhaseVerifying || current.Status.Patch == nil || current.Status.Patch.Ref == nil || current.Status.Patch.Ref.Digest != patchDigest || current.Status.Patch.ManifestRef == nil || current.Status.Patch.ManifestRef.Digest != manifestDigest || current.Status.Patch.FilesChanged != 2 || current.Status.Patch.LinesChanged != 7 {
		t.Fatalf("capture result was not projected safely: result=%#v status=%#v", result, current.Status)
	}
}

func TestVerificationAndGatePolicyAreDrivenFromImmutableSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 11, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		mode         v1alpha1.GateMode
		verdict      string
		publish      v1alpha1.PublishMode
		finalPhase   v1alpha1.Phase
		intermediate v1alpha1.Phase
	}{
		{name: "accepted without publish succeeds", mode: v1alpha1.GateEnforcing, verdict: "Accepted", publish: v1alpha1.PublishNone, intermediate: v1alpha1.PhaseGated, finalPhase: v1alpha1.PhaseSucceeded},
		{name: "enforcing rejection is terminal", mode: v1alpha1.GateEnforcing, verdict: "Rejected", publish: v1alpha1.PublishPullRequest, intermediate: v1alpha1.PhaseGated, finalPhase: v1alpha1.PhaseRejected},
		{name: "shadow rejection proceeds to publishing", mode: v1alpha1.GateShadow, verdict: "Rejected", publish: v1alpha1.PublishPullRequest, intermediate: v1alpha1.PhaseGated, finalPhase: v1alpha1.PhasePublishing},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := testScheme(t)
			run := testRun()
			run.Finalizers = []string{AgentRunFinalizer}
			run.Spec.Publish.Mode = test.publish
			run.Status.Phase = v1alpha1.PhaseVerifying
			run.Status.BaseSHA = strings.Repeat("c", 40)
			patchDigest := "sha256:" + strings.Repeat("d", 64)
			run.Status.Patch = &v1alpha1.PatchSummary{Ref: &v1alpha1.ArtifactRef{URI: "s3://bucket/patch.diff", Digest: patchDigest, Kind: "patch"}}
			snapshot := testSnapshot(run, run.Status.BaseSHA, test.mode)
			body, err := canonical.CanonicalizeResolvedSpec(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := canonical.ResolvedSpecDigest(body)
			if err != nil {
				t.Fatal(err)
			}
			run.Status.SpecDigest = digest
			run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json"}
			reportDigest := "sha256:" + strings.Repeat("e", 64)
			phase := &verifyPhaseFake{outcome: VerifyOutcome{
				Complete:         true,
				VerifySandboxRef: &v1alpha1.ChildRef{Name: testChildName(run, sandbox.RoleVerify), Kind: "Sandbox", UID: "verify-uid", Role: string(sandbox.RoleVerify), SpecDigest: digest, PlanFingerprint: testPlanFingerprint()},
				Gate:             &v1alpha1.GateResult{Verdict: test.verdict, ReportRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/report.json", Digest: reportDigest, Kind: "verification-report"}},
				Artifacts:        []v1alpha1.ArtifactRef{{URI: "s3://bucket/evidence.json", Digest: "sha256:" + strings.Repeat("f", 64), Kind: "verification-evidence"}},
			}}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
			reconciler := &AgentRunReconciler{
				Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: &cleanerFake{},
				Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{}, Verify: phase,
				Clock: func() time.Time { return now },
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			current := &v1alpha1.AgentRun{}
			if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != test.intermediate || current.Status.Gate == nil || current.Status.Gate.Verdict != test.verdict || current.Status.VerifySandboxRef == nil || len(current.Status.Artifacts) != 1 {
				t.Fatalf("verification projection=%#v", current.Status)
			}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if err := kube.Get(context.Background(), request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != test.finalPhase || phase.calls != 1 {
				t.Fatalf("final phase=%s calls=%d, want %s/1", current.Status.Phase, phase.calls, test.finalPhase)
			}
		})
	}
}

func TestPublishingLifecycleIsTerminalAndNeverRetriesUnknownEffects(t *testing.T) {
	now := time.Date(2026, 8, 11, 21, 0, 0, 0, time.UTC)
	effectKey := "sha256:" + strings.Repeat("9", 64)
	prURL := "https://github.com/Astatide1337/agents-gateway/pull/42"
	for _, test := range []struct {
		name        string
		gateMode    v1alpha1.GateMode
		verdict     string
		outcome     PublishOutcome
		wantPhase   v1alpha1.Phase
		wantURL     string
		published   bool
		failureCode string
	}{
		{
			name: "accepted publication succeeds", gateMode: v1alpha1.GateEnforcing, verdict: "Accepted",
			outcome:   PublishOutcome{Succeeded: true, Effect: &v1alpha1.EffectSummary{Key: effectKey, State: v1alpha1.EffectSucceeded, PullRequestURL: prURL}},
			wantPhase: v1alpha1.PhaseSucceeded, wantURL: prURL, published: true,
		},
		{
			name: "shadow rejection still publishes evidence then remains rejected", gateMode: v1alpha1.GateShadow, verdict: "Rejected",
			outcome:   PublishOutcome{Succeeded: true, Effect: &v1alpha1.EffectSummary{Key: effectKey, State: v1alpha1.EffectSucceeded, PullRequestURL: prURL}},
			wantPhase: v1alpha1.PhaseRejected, wantURL: prURL, published: true,
		},
		{
			name: "ambiguous result is terminal unknown with no trusted URL", gateMode: v1alpha1.GateEnforcing, verdict: "Accepted",
			outcome:   PublishOutcome{Unknown: true, Effect: &v1alpha1.EffectSummary{Key: effectKey, State: v1alpha1.EffectUnknown, PullRequestURL: prURL}},
			wantPhase: v1alpha1.PhaseUnknownEffect,
		},
		{
			name: "deterministic publish failure is failed", gateMode: v1alpha1.GateEnforcing, verdict: "Accepted",
			outcome:   PublishOutcome{Failed: true, Effect: &v1alpha1.EffectSummary{Key: effectKey, State: v1alpha1.EffectFailed}, Reason: "publication conflict"},
			wantPhase: v1alpha1.PhaseFailed, failureCode: "PublishFailed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := testScheme(t)
			run := testRun()
			run.Finalizers = []string{AgentRunFinalizer}
			run.Status.Phase = v1alpha1.PhasePublishing
			run.Status.BaseSHA = strings.Repeat("c", 40)
			run.Status.Gate = &v1alpha1.GateResult{Verdict: test.verdict, ReportRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/report", Digest: "sha256:" + strings.Repeat("e", 64)}}
			snapshot := testSnapshot(run, run.Status.BaseSHA, test.gateMode)
			body, err := canonical.CanonicalizeResolvedSpec(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := canonical.ResolvedSpecDigest(body)
			if err != nil {
				t.Fatal(err)
			}
			run.Status.SpecDigest = digest
			run.Status.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: "s3://bucket/resolved.json", Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json"}
			phase := &publishPhaseFake{outcome: test.outcome}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.AgentRun{}).WithObjects(run).Build()
			reconciler := &AgentRunReconciler{
				Client: kube, APIReader: kube, Artifacts: &writerFake{body: body}, Sandboxes: &cleanerFake{},
				Work: &workPlanFake{}, Execution: &executionGateFake{allowed: true}, Events: &completionSourceFake{}, Publish: phase,
				Clock: func() time.Time { return now },
			}
			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
			if err != nil {
				t.Fatal(err)
			}
			current := &v1alpha1.AgentRun{}
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), current); err != nil {
				t.Fatal(err)
			}
			if result.Requeue || result.RequeueAfter != 0 || phase.calls != 1 || current.Status.Phase != test.wantPhase || current.Status.Published != test.published {
				t.Fatalf("result=%#v calls=%d status=%#v", result, phase.calls, current.Status)
			}
			gotURL := ""
			if current.Status.Effect != nil {
				gotURL = current.Status.Effect.PullRequestURL
			}
			if gotURL != test.wantURL {
				t.Fatalf("pullRequestURL=%q, want %q", gotURL, test.wantURL)
			}
			if test.failureCode != "" && (current.Status.Failure == nil || current.Status.Failure.Code != test.failureCode) {
				t.Fatalf("failure=%#v, want code %q", current.Status.Failure, test.failureCode)
			}
		})
	}
}

func TestPublicationStatusDoesNotConfuseFindingsWithPatchPublication(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	prURL := "https://github.com/Astatide1337/agents-gateway/pull/42"

	t.Run("findings only", func(t *testing.T) {
		status := v1alpha1.AgentRunStatus{Effect: &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: prURL}}
		snapshot := resolved.Snapshot{Spec: v1alpha1.AgentRunSpec{Output: &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings}}}
		outcome := PublishOutcome{
			Succeeded: true, FindingsAttempted: true, FindingsPublished: true,
			Effect: &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: prURL},
		}
		if err := projectPublicationStatus(&status, snapshot, outcome, 3, now); err != nil {
			t.Fatal(err)
		}
		published := conditionByType(status.Conditions, v1alpha1.ConditionPublished)
		findings := conditionByType(status.Conditions, v1alpha1.ConditionFindingsPublished)
		if status.Published || published.Status != v1alpha1.ConditionFalse || published.Reason != "NoPatchPublication" || findings.Status != v1alpha1.ConditionTrue || status.Effect != nil {
			t.Fatalf("findings-only status=%#v conditions=%#v", status, status.Conditions)
		}
	})

	t.Run("both retains patch publication when findings is unknown", func(t *testing.T) {
		effect := &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: prURL}
		status := v1alpha1.AgentRunStatus{Effect: effect}
		snapshot := resolved.Snapshot{Spec: v1alpha1.AgentRunSpec{Output: &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth}}}
		outcome := PublishOutcome{Unknown: true, Effect: effect, FindingsAttempted: true, FindingsUnknown: true}
		if err := projectPublicationStatus(&status, snapshot, outcome, 3, now); err != nil {
			t.Fatal(err)
		}
		published := conditionByType(status.Conditions, v1alpha1.ConditionPublished)
		findings := conditionByType(status.Conditions, v1alpha1.ConditionFindingsPublished)
		if !status.Published || published.Status != v1alpha1.ConditionTrue || findings.Status != v1alpha1.ConditionUnknown || status.Effect.PullRequestURL != prURL {
			t.Fatalf("OutputBoth status=%#v conditions=%#v", status, status.Conditions)
		}
	})
}

func conditionByType(conditions []v1alpha1.Condition, conditionType v1alpha1.ConditionType) v1alpha1.Condition {
	for _, condition := range conditions {
		if condition.Type == string(conditionType) {
			return condition
		}
	}
	return v1alpha1.Condition{}
}

func testRun() *v1alpha1.AgentRun {
	return &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "agw-runs", UID: types.UID("run-uid"), Generation: 1}, Spec: v1alpha1.AgentRunSpec{AgentRef: "agent", GateRef: "gate"}}
}

func testSnapshot(run *v1alpha1.AgentRun, baseSHA string, gateMode v1alpha1.GateMode) resolved.Snapshot {
	instructions := "test instructions"
	return resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		Spec:          *run.Spec.DeepCopy(),
		BaseSHA:       baseSHA,
		Task:          "test task",
		Instructions:  instructions,
		Agent: v1alpha1.AgentSpec{
			Runtime:            v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/runtime@sha256:" + strings.Repeat("a", 64)},
			Instructions:       v1alpha1.InstructionsSpec{Inline: &instructions},
			ContextStrategyRef: "context",
			ToolSetRef:         "tools",
			ModelRouteRef:      "models",
		},
		Gate: v1alpha1.GateSpec{
			Mode:   gateMode,
			Verify: v1alpha1.VerifySpec{Image: "ghcr.io/astatide/verify@sha256:" + strings.Repeat("b", 64)},
		},
		ToolSet: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{
			Name: "github", Ref: "https://mcp.example.test/mcp",
			Tools: []v1alpha1.ToolDefinition{{Name: "get_issue", Effect: v1alpha1.EffectRead}},
		}}, Profiles: []v1alpha1.ToolProfile{
			{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
			{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
			{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_issue"}}},
		}, MaxToolsPerPhase: v1alpha1.MaxToolsPerPhase},
		ModelRoute: v1alpha1.ModelRouteSpec{
			Providers: []v1alpha1.ModelProvider{{Name: "default", Kind: "openai-responses", Model: "test-model", Family: "openai", Priority: 1}},
			Budget:    v1alpha1.ModelBudget{MaxCostUSD: "2.00"},
		},
		ContextStrategy: v1alpha1.ContextStrategySpec{
			RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 8000},
			Budget:  v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 8 << 20},
		},
		References: resolved.References{
			Agent:           resolved.ObjectVersion{Name: "agent", UID: "agent-uid", ResourceVersion: "1", Generation: 1},
			Gate:            resolved.ObjectVersion{Name: "gate", UID: "gate-uid", ResourceVersion: "2", Generation: 1},
			ToolSet:         resolved.ObjectVersion{Name: "tools", UID: "tools-uid", ResourceVersion: "3", Generation: 1},
			ModelRoute:      resolved.ObjectVersion{Name: "models", UID: "models-uid", ResourceVersion: "4", Generation: 1},
			ContextStrategy: resolved.ObjectVersion{Name: "context", UID: "context-uid", ResourceVersion: "5", Generation: 1},
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}
