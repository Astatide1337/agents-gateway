// Package controller contains the Kubernetes control loops for v3.
package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/argoworkflow"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/fsm"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runplan"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	statusprojection "github.com/Astatide1337/agents-gateway/v3/internal/status"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const AgentRunFinalizer = "agw.astatide.com/cleanup"

var errForeignRunSecret = errors.New("per-run credential Secret is not owned by AgentRun")

type SnapshotStore interface {
	SaveResolvedSpec(context.Context, string, string, []byte) (v1alpha1.ArtifactRef, error)
	LoadResolvedSpec(context.Context, string, string) ([]byte, error)
}

// LifecycleOutputStore is the controller-owned persistence edge for the
// validated Argo handoff. It is intentionally separate from SnapshotStore so
// selecting the direct backend does not require or invoke Argo output storage.
type LifecycleOutputStore interface {
	SaveArgoLifecycleOutput(context.Context, string, string, []byte) (v1alpha1.ArtifactRef, error)
}

// ContextArtifactStore is deliberately a raw immutable-object read seam. The
// controller, not the store adapter, decodes and validates the evidence body
// against the current run/spec/base identities before status projection.
type ContextArtifactStore interface {
	LoadContextPack(context.Context, string, string, string) ([]byte, string, error)
}

type SandboxBackend interface {
	Ensure(context.Context, sandbox.SandboxPlan) (sandbox.SandboxRef, error)
	Observe(context.Context, sandbox.SandboxRef) (sandbox.SandboxObservation, error)
	Delete(context.Context, sandbox.SandboxRef) error
}

// WorkPlanFactory resolves controller-owned, short-lived inputs and returns a
// complete work Sandbox plan. It is deliberately optional: an operator may
// admit runs while remaining fail closed at Pending until Phase-0 succeeds.
type WorkPlanFactory interface {
	PlanWork(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (sandbox.SandboxPlan, error)
}

// ExecutionDecision is the controller-side isolation admission result.  It is
// intentionally smaller than the admission package's decision so the
// reconciler cannot accidentally start work based on an unknown value.
type ExecutionDecision struct {
	Allowed bool
	Reason  string
}

// ExecutionGate re-checks fresh Phase-0 evidence immediately before creating
// a work Sandbox. Admission protects new API writes; this second check closes
// the race where an already-admitted Pending run is reconciled after the
// attestation has expired or the node identity changed.
type ExecutionGate interface {
	CheckExecution(context.Context, time.Time) ExecutionDecision
}

// CompletionSource is the durable runtime boundary. A Sandbox becoming
// Finished is never treated as success; only a verified immutable completion
// record can advance Working.
type CompletionSource interface {
	LoadCompletion(context.Context, string, string) (runtimeevents.CompletionRecord, error)
	FinalizeObservedExit(context.Context, string, string, string, runtimeevents.ProcessExit) (runtimeevents.CompletionRecord, error)
}

// ProcessExitObserver reads the Kubernetes-owned agent container state. It is
// deliberately separate from runtime events: model-authored output cannot
// manufacture this observation.
type ProcessExitObserver interface {
	ObserveAgentExit(context.Context, sandbox.SandboxRef) (exited bool, exit runtimeevents.ProcessExit, err error)
}

// SourcePinner resolves a mutable baseRef to an exact commit through an
// operator-owned credential and re-canonicalizes the resolved run. The
// resulting baseSHA is therefore covered by status.specDigest.
type SourcePinner interface {
	PinSource(context.Context, resolved.Result) (resolved.Result, error)
}

// CapturePhase owns the controller-side, credential-free patch capture Job.
// It returns only independently validated evidence and an immutable artifact.
type CapturePhase interface {
	Capture(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (CaptureOutcome, error)
	Cleanup(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) error
}

type CaptureOutcome struct {
	Pending          bool
	Failed           bool
	Reason           string
	Artifact         v1alpha1.ArtifactRef
	ManifestArtifact v1alpha1.ArtifactRef
	Validated        capture.ValidatedOutput
}

// VerifyPhase owns the independent, fresh-checkout verification Sandbox and
// returns only the controller-safe projection of its signed Gate decision.
// A completed verification always enters Gated; the AgentRun controller is
// the only component allowed to interpret shadow versus enforcing policy.
type VerifyPhase interface {
	Verify(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (VerifyOutcome, error)
}

type VerifyOutcome struct {
	Pending          bool
	Complete         bool
	RequeueAfter     time.Duration
	VerifySandboxRef *v1alpha1.ChildRef
	Gate             *v1alpha1.GateResult
	Failure          *v1alpha1.FailureStatus
	Artifacts        []v1alpha1.ArtifactRef
}

type PublishPhase interface {
	Publish(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (PublishOutcome, error)
}

type PublishOutcome struct {
	Pending   bool
	Succeeded bool
	Rejected  bool
	Failed    bool
	Unknown   bool
	Reason    string
	Effect    *v1alpha1.EffectSummary
	// Findings publication is an independent output. These bounded booleans
	// let the Kubernetes projection distinguish a findings-only success and a
	// partial OutputBoth result without importing the publication package into
	// the core controller.
	FindingsAttempted bool
	FindingsPublished bool
	FindingsUnknown   bool
	RequeueAfter      time.Duration
}

type ResolveFunc func(context.Context, client.Reader, *v1alpha1.AgentRun) (resolved.Result, error)

// AgentRunReconciler owns immutable admission, finalization, and cancellation.
// Work execution is attached as a separate phase driver so this foundation can
// fail closed until the Phase-0 isolation proof succeeds.
type AgentRunReconciler struct {
	client.Client
	APIReader            client.Reader
	OrchestrationBackend OrchestrationBackendKind
	Orchestration        OrchestrationDriver
	Artifacts            SnapshotStore
	LifecycleOutputs     LifecycleOutputStore
	ContextArtifacts     ContextArtifactStore
	Sandboxes            SandboxBackend
	Work                 WorkPlanFactory
	Execution            ExecutionGate
	Events               CompletionSource
	Processes            ProcessExitObserver
	Source               SourcePinner
	Capture              CapturePhase
	Verify               VerifyPhase
	Publish              PublishPhase
	Resolve              ResolveFunc
	Clock                func() time.Time
}

func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager, backendKind sandbox.BackendKind) error {
	if err := r.validateOrchestrationConfig(); err != nil {
		return err
	}
	owned, err := ownedResourcesForBackend(backendKind)
	if err != nil {
		return err
	}
	builder := ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.AgentRun{})
	for _, object := range owned {
		builder = builder.Owns(object)
	}
	return builder.Complete(r)
}

func ownedResourcesForBackend(backendKind sandbox.BackendKind) ([]client.Object, error) {
	if !backendKind.Valid() {
		return nil, fmt.Errorf("unsupported sandbox backend %q", backendKind)
	}
	// Capture and verification use Jobs in both modes. The execution child is
	// the only optional REST mapping: job mode deliberately never registers an
	// Owns(Sandbox) watch, so the manager can start without that CRD installed.
	owned := []client.Object{&batchv1.Job{}}
	switch backendKind {
	case sandbox.BackendAgentSandbox:
		owned = append(owned, &sandboxv1beta1.Sandbox{})
	case sandbox.BackendJob:
		owned = append(owned, &corev1.PersistentVolumeClaim{})
	}
	owned = append(owned, &networkingv1.NetworkPolicy{})
	return owned, nil
}

func (r *AgentRunReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if r.Client == nil || r.APIReader == nil || r.Artifacts == nil || (!r.usesArgoOrchestration() && r.Sandboxes == nil) {
		return ctrl.Result{}, errors.New("AgentRun reconciler dependencies are incomplete")
	}
	if err := r.validateOrchestrationConfig(); err != nil {
		return ctrl.Result{}, err
	}
	resolve := r.Resolve
	if resolve == nil {
		resolve = resolved.Resolve
	}
	now := time.Now().UTC()
	if r.Clock != nil {
		now = r.Clock().UTC()
	}

	run := &v1alpha1.AgentRun{}
	if err := r.Get(ctx, request.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, run)
	}
	if !contains(run.Finalizers, AgentRunFinalizer) {
		base := run.DeepCopy()
		run.Finalizers = append(run.Finalizers, AgentRunFinalizer)
		if err := r.Client.SubResource("finalizers").Patch(ctx, run, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if run.Spec.CancelRequested {
		if err := r.cleanupChildren(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		if run.Status.Phase == "" {
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhasePending, now); err != nil {
				return ctrl.Result{}, err
			}
		}
		if !fsm.IsTerminal(run.Status.Phase) {
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCancelled, now); err != nil {
				return ctrl.Result{}, err
			}
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{}, r.updateStatus(ctx, run)
	}
	if fsm.IsTerminal(run.Status.Phase) {
		return ctrl.Result{}, r.cleanupChildren(ctx, run)
	}
	if run.Status.SpecDigest != "" {
		// Reference resolution is intentionally one-shot. Mutable Agent/Gate/
		// ToolSet/ModelRoute updates cannot change an admitted run.
		//
		// The digest is persisted before the immutable object is written. A
		// controller crash can therefore leave a durable resolution checkpoint
		// without its status reference; recover that reference from the
		// content-addressed object instead of resolving mutable references again.
		if run.Status.ResolvedSpecRef == nil {
			return r.recoverResolvedSpecRef(ctx, run, now)
		}
		return r.driveAdmitted(ctx, run, now)
	}

	result, err := resolve(ctx, r.APIReader, run)
	if err != nil {
		if permanentResolutionError(err) {
			return ctrl.Result{}, r.failAdmission(ctx, run, now)
		}
		return ctrl.Result{}, fmt.Errorf("resolve immutable execution inputs: %w", err)
	}
	if r.Source == nil {
		return ctrl.Result{}, errors.New("source pinning is not configured")
	}
	result, err = r.Source.PinSource(ctx, result)
	if err != nil {
		if runplan.IsPermanentSourceResolutionError(err) {
			return ctrl.Result{}, r.failAdmission(ctx, run, now)
		}
		return ctrl.Result{}, fmt.Errorf("pin immutable source revision: %w", err)
	}
	if !resolved.ValidBaseSHA(result.Snapshot.BaseSHA) {
		return ctrl.Result{}, errors.New("source pinner returned an invalid base SHA")
	}
	pinnedSnapshot, err := resolved.Decode(result.Canonical, result.Digest)
	if err != nil || pinnedSnapshot.BaseSHA != result.Snapshot.BaseSHA || pinnedSnapshot.Run.UID != string(run.UID) {
		return ctrl.Result{}, errors.New("source pinner returned an invalid canonical execution contract")
	}
	if run.Status.Phase == "" {
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhasePending, now); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Persist the digest and pinned base before the object-store write. This is
	// the execution lock: if the process dies after this status update, a
	// restart must wait for or recover the exact object rather than re-resolve
	// mutable refs or a moving base branch.
	run.Status.SpecDigest = result.Digest
	run.Status.BaseSHA = result.Snapshot.BaseSHA
	run.Status.ObservedGeneration = run.Generation
	if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
		Type: string(v1alpha1.ConditionAdmitted), Status: v1alpha1.ConditionUnknown,
		Reason: "ResolutionLocked", Message: "immutable execution inputs locked; awaiting artifact persistence",
		ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.updateStatus(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	ref, err := r.Artifacts.SaveResolvedSpec(ctx, string(run.UID), result.Digest, result.Canonical)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("persist immutable resolved spec: %w", err)
	}
	if err := statusprojection.SetResolvedSpecRef(&run.Status, ref); err != nil {
		return ctrl.Result{}, err
	}
	if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
		Type: string(v1alpha1.ConditionAdmitted), Status: v1alpha1.ConditionTrue,
		Reason: "Resolved", Message: "immutable execution inputs resolved",
		ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)
}

// recoverResolvedSpecRef completes the second half of admission after a
// crash between the status checkpoint and the object-store write/status
// projection. It is deliberately fail-closed: a missing or invalid object is
// retried, never replaced by a fresh resolution of mutable references.
func (r *AgentRunReconciler) recoverResolvedSpecRef(ctx context.Context, run *v1alpha1.AgentRun, now time.Time) (ctrl.Result, error) {
	if !canonical.ValidDigest(run.Status.SpecDigest) || !resolved.ValidBaseSHA(run.Status.BaseSHA) {
		return ctrl.Result{}, errors.New("immutable resolution checkpoint is incomplete")
	}
	body, err := r.Artifacts.LoadResolvedSpec(ctx, string(run.UID), run.Status.SpecDigest)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("recover immutable resolved spec: %w", err)
	}
	snapshot, err := resolved.Decode(body, run.Status.SpecDigest)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("verify recovered immutable resolved spec: %w", err)
	}
	if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) || snapshot.Run.Generation != run.Generation || snapshot.BaseSHA != run.Status.BaseSHA {
		return ctrl.Result{}, errors.New("recovered immutable resolved spec does not belong to AgentRun")
	}
	ref, err := r.Artifacts.SaveResolvedSpec(ctx, string(run.UID), run.Status.SpecDigest, body)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile immutable resolved spec reference: %w", err)
	}
	if err := statusprojection.SetResolvedSpecRef(&run.Status, ref); err != nil {
		return ctrl.Result{}, err
	}
	if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
		Type: string(v1alpha1.ConditionAdmitted), Status: v1alpha1.ConditionTrue,
		Reason: "Resolved", Message: "immutable execution inputs resolved",
		ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)
}

func (r *AgentRunReconciler) driveAdmitted(ctx context.Context, run *v1alpha1.AgentRun, now time.Time) (ctrl.Result, error) {
	// Argo owns only the opt-in work-sequencing backend until its immutable
	// lifecycle output has been validated and persisted. Once that handoff is
	// bound, the normal AGW-owned verifier/gate/publication path resumes from
	// the projected evidence. A Workflow phase alone never selects this path.
	if r.usesArgoOrchestration() && !argoLifecycleHandoffBound(run) {
		return r.driveArgo(ctx, run, now)
	}
	if r.Work == nil {
		// The missing executor is intentional during Phase-0: admission and
		// immutable evidence work, but no workload starts before isolation has
		// been proved and a work-plan factory is explicitly configured.
		return ctrl.Result{}, nil
	}
	if r.Execution == nil {
		return ctrl.Result{}, errors.New("work execution is configured without a Phase-0 execution gate")
	}
	if r.Events == nil {
		return ctrl.Result{}, errors.New("work execution is configured without a durable runtime completion source")
	}

	switch run.Status.Phase {
	case v1alpha1.PhasePending:
		decision := r.Execution.CheckExecution(ctx, now)
		if !decision.Allowed {
			// Keep the run Pending. The admission webhook has already recorded
			// that its immutable inputs are valid; stale or failed isolation
			// evidence is an operational block, not an agent failure.
			changed, err := setExecutionCondition(&run.Status, run.Generation, now, v1alpha1.ConditionFalse, decision.Reason)
			if err != nil {
				return ctrl.Result{}, err
			}
			if changed {
				run.Status.ObservedGeneration = run.Generation
				if err := r.updateStatus(ctx, run); err != nil {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		body, err := r.Artifacts.LoadResolvedSpec(ctx, string(run.UID), run.Status.SpecDigest)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("load immutable resolved spec: %w", err)
		}
		snapshot, err := resolved.Decode(body, run.Status.SpecDigest)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("verify immutable resolved spec: %w", err)
		}
		if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) || snapshot.Run.Generation != run.Generation {
			return ctrl.Result{}, errors.New("immutable resolved spec does not belong to AgentRun")
		}
		plan, err := r.Work.PlanWork(ctx, run, snapshot)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("construct work Sandbox: %w", err)
		}
		if plan.Owner == nil || plan.Owner.UID != run.UID || plan.Role != sandbox.RoleWork || plan.SpecDigest != run.Status.SpecDigest {
			return ctrl.Result{}, errors.New("work plan identity does not match admitted AgentRun")
		}
		ref, err := r.Sandboxes.Ensure(ctx, plan)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure work Sandbox: %w", err)
		}
		if err := validateSandboxRefForRun(run, ref, sandbox.RoleWork); err != nil {
			return ctrl.Result{}, fmt.Errorf("validate work Sandbox reference: %w", err)
		}
		run.Status.WorkSandboxRef = childRef(ref)
		if _, err := setExecutionCondition(&run.Status, run.Generation, now, v1alpha1.ConditionTrue, "allowed"); err != nil {
			return ctrl.Result{}, err
		}
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCloning, now); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.updateStatus(ctx, run)

	case v1alpha1.PhaseCloning:
		if run.Status.WorkSandboxRef == nil {
			return ctrl.Result{}, errors.New("Cloning run has no work child reference")
		}
		if err := validateStatusChildRefForRun(run, run.Status.WorkSandboxRef, sandbox.RoleWork); err != nil {
			return ctrl.Result{}, fmt.Errorf("validate persisted work Sandbox reference: %w", err)
		}
		observation, err := r.Sandboxes.Observe(ctx, sandboxRef(run, run.Status.WorkSandboxRef, sandbox.RoleWork))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("observe work Sandbox: %w", err)
		}
		if !observation.Exists {
			return ctrl.Result{}, r.failRun(ctx, run, now, "WorkSandboxMissing", "work Sandbox disappeared before becoming ready")
		}
		if observation.Finished && !observation.Ready {
			return ctrl.Result{}, r.failRun(ctx, run, now, "WorkSandboxSetupFailed", "work Sandbox finished before becoming ready")
		}
		if !observation.Ready {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// Ready means the upstream Sandbox and all ordered init containers are
		// ready for execution. It does not mean the harness completed its work.
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseWorking, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
			Type: string(v1alpha1.ConditionWorkReady), Status: v1alpha1.ConditionTrue,
			Reason: "SandboxReady", Message: "work Sandbox is ready",
			ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
		}); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{RequeueAfter: 2 * time.Second}, r.updateStatus(ctx, run)

	case v1alpha1.PhaseWorking:
		if run.Status.WorkSandboxRef == nil {
			return ctrl.Result{}, errors.New("Working run has no work child reference")
		}
		if err := validateStatusChildRefForRun(run, run.Status.WorkSandboxRef, sandbox.RoleWork); err != nil {
			return ctrl.Result{}, fmt.Errorf("validate persisted work Sandbox reference: %w", err)
		}
		contextProjected, err := r.projectContextPack(ctx, run)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("load and validate immutable context artifact: %w", err)
		}
		if contextProjected {
			run.Status.ObservedGeneration = run.Generation
			return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)
		}
		record, err := r.Events.LoadCompletion(ctx, string(run.UID), run.Status.SpecDigest)
		if errors.Is(err, runtimeevents.ErrMissing) || errors.Is(err, runtimeevents.ErrNotReady) {
			workRef := sandboxRef(run, run.Status.WorkSandboxRef, sandbox.RoleWork)
			observation, observeErr := r.Sandboxes.Observe(ctx, workRef)
			if observeErr != nil {
				return ctrl.Result{}, fmt.Errorf("observe working Sandbox: %w", observeErr)
			}
			if r.Processes == nil {
				return ctrl.Result{}, errors.New("Working run has no independent process-exit observer")
			}
			exited, processExit, processErr := r.Processes.ObserveAgentExit(ctx, workRef)
			if processErr != nil {
				return ctrl.Result{}, fmt.Errorf("observe agent container exit: %w", processErr)
			}
			if exited {
				if !processExit.Known {
					return ctrl.Result{}, r.failRun(ctx, run, now, "WorkExitUnknown", "agent container exit status could not be proven")
				}
				record, err = r.Events.FinalizeObservedExit(ctx, string(run.UID), run.Status.SpecDigest, run.Status.BaseSHA, processExit)
				switch {
				case errors.Is(err, runtimeevents.ErrNoTerminal):
					return ctrl.Result{}, r.failRun(ctx, run, now, "WorkExitedWithoutCompletion", "agent container exited without a durable terminal runtime event")
				case errors.Is(err, runtimeevents.ErrProcessExitFailed):
					return ctrl.Result{}, r.failRun(ctx, run, now, "WorkExitContradiction", "agent reported completion but its container exited non-zero")
				case err != nil:
					return ctrl.Result{}, fmt.Errorf("finalize runtime completion from Kubernetes exit: %w", err)
				}
			} else if !observation.Exists || observation.Finished {
				return ctrl.Result{}, r.failRun(ctx, run, now, "WorkExitedWithoutCompletion", "work Sandbox ended without a durable terminal runtime event")
			} else {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("load runtime completion: %w", err)
		}
		if record.RunUID != string(run.UID) || record.SpecDigest != run.Status.SpecDigest || record.BaseSHA != run.Status.BaseSHA {
			return ctrl.Result{}, errors.New("runtime completion identity does not match AgentRun")
		}
		ref := v1alpha1.ArtifactRef{
			URI: record.EventStream.URI, Digest: record.EventStream.Digest,
			Kind: record.EventStream.Kind, Name: record.EventStream.Name,
			MediaType: record.EventStream.MediaType, SizeBytes: record.EventStream.SizeBytes,
		}
		if err := statusprojection.SetEventStreamRef(&run.Status, ref); err != nil {
			return ctrl.Result{}, err
		}
		switch record.TerminalType {
		case runtimeevents.TerminalCompleted:
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCapturing, now); err != nil {
				return ctrl.Result{}, err
			}
			if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
				Type: string(v1alpha1.ConditionWorkComplete), Status: v1alpha1.ConditionTrue,
				Reason: "RuntimeCompleted", Message: "durable terminal runtime event verified",
				ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
			}); err != nil {
				return ctrl.Result{}, err
			}
		case runtimeevents.TerminalFailed:
			return ctrl.Result{}, r.failRun(ctx, run, now, "AgentFailed", "agent runtime reported a terminal failure")
		case runtimeevents.TerminalCancelled:
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCancelled, now); err != nil {
				return ctrl.Result{}, err
			}
		case runtimeevents.TerminalUnknownEffect:
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseUnknownEffect, now); err != nil {
				return ctrl.Result{}, err
			}
			run.Status.Effect = &v1alpha1.EffectSummary{State: v1alpha1.EffectUnknown}
		default:
			return ctrl.Result{}, errors.New("runtime completion contains an unsupported terminal type")
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{Requeue: run.Status.Phase == v1alpha1.PhaseCapturing}, r.updateStatus(ctx, run)

	case v1alpha1.PhaseCapturing:
		if r.Capture == nil {
			return ctrl.Result{}, errors.New("Capturing run has no capture phase driver")
		}
		if run.Status.WorkSandboxRef == nil {
			return ctrl.Result{}, errors.New("captured run has no work child reference")
		}
		if err := validateStatusChildRefForRun(run, run.Status.WorkSandboxRef, sandbox.RoleWork); err != nil {
			return ctrl.Result{}, fmt.Errorf("validate persisted captured work Sandbox reference: %w", err)
		}
		snapshot, err := r.loadSnapshot(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if run.Status.Patch == nil {
			outcome, err := r.Capture.Capture(ctx, run, snapshot)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("capture immutable patch: %w", err)
			}
			if outcome.Pending {
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if outcome.Failed {
				reason := outcome.Reason
				if reason == "" {
					reason = "patch capture failed"
				}
				return ctrl.Result{}, r.failRun(ctx, run, now, "CaptureFailed", reason)
			}
			envelope := outcome.Validated.Envelope
			manifest := outcome.ManifestArtifact
			if outcome.Artifact.Digest == "" || outcome.Artifact.Digest != envelope.PatchDigest || manifest.Digest == "" || manifest.Digest != envelope.ManifestDigest || envelope.FilesChanged > math.MaxInt32 || envelope.LinesChanged > math.MaxInt32 {
				return ctrl.Result{}, errors.New("capture outcome is not safe for status projection")
			}
			artifact := outcome.Artifact
			manifestRef := manifest
			run.Status.Patch = &v1alpha1.PatchSummary{Ref: &artifact, ManifestRef: &manifestRef, FilesChanged: int32(envelope.FilesChanged), LinesChanged: int32(envelope.LinesChanged)}
			// Persist the capture checkpoint before any work child is reaped. If
			// the controller stops after this write, the next reconcile can skip
			// recapture and safely retry cleanup from the durable patch reference.
			run.Status.ObservedGeneration = run.Generation
			if err := r.updateStatus(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
		}
		if run.Status.Patch.Ref == nil || run.Status.Patch.ManifestRef == nil || run.Status.Patch.Ref.Digest == "" || run.Status.Patch.ManifestRef.Digest == "" {
			return ctrl.Result{}, errors.New("captured patch checkpoint is incomplete")
		}
		// Patch and publish evidence are durable before the work environment is
		// reaped. Verification starts from a fresh clone and must never depend on
		// the agent-touched PVC. On restart, the persisted Patch skips Capture and
		// retries only this cleanup boundary.
		if err := r.Capture.Cleanup(ctx, run, snapshot); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleanup completed capture phase: %w", err)
		}
		if err := r.Sandboxes.Delete(ctx, sandboxRef(run, run.Status.WorkSandboxRef, sandbox.RoleWork)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("reap captured work Sandbox: %w", err)
		}
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseVerifying, now); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)

	case v1alpha1.PhaseVerifying:
		if r.Verify == nil {
			return ctrl.Result{}, errors.New("Verifying run has no independent verification driver")
		}
		snapshot, err := r.loadSnapshot(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if run.Status.Patch == nil || run.Status.Patch.Ref == nil {
			return ctrl.Result{}, errors.New("Verifying run has no immutable patch artifact")
		}
		if run.Status.VerifySandboxRef != nil {
			if err := validateStatusChildRefForRun(run, run.Status.VerifySandboxRef, sandbox.RoleVerify); err != nil {
				return ctrl.Result{}, fmt.Errorf("validate persisted verify Sandbox reference: %w", err)
			}
		}
		outcome, err := r.Verify.Verify(ctx, run, snapshot)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("verify immutable patch: %w", err)
		}
		if outcome.VerifySandboxRef != nil {
			if err := validateStatusChildRefForRun(run, outcome.VerifySandboxRef, sandbox.RoleVerify); err != nil {
				return ctrl.Result{}, fmt.Errorf("validate verify Sandbox reference: %w", err)
			}
			ref := *outcome.VerifySandboxRef
			run.Status.VerifySandboxRef = &ref
		}
		if outcome.Gate != nil {
			result := *outcome.Gate
			result.Checks = append([]v1alpha1.GateCheck(nil), outcome.Gate.Checks...)
			if outcome.Gate.ReportRef != nil {
				ref := *outcome.Gate.ReportRef
				result.ReportRef = &ref
			}
			run.Status.Gate = &result
		}
		if outcome.Failure != nil {
			failure := *outcome.Failure
			run.Status.Failure = &failure
		} else if outcome.Complete {
			// A retryable verification error is a transient observation, not
			// durable terminal evidence. Do not carry it into a later accepted
			// Gate result.
			run.Status.Failure = nil
		}
		if len(outcome.Artifacts) > 0 {
			run.Status.Artifacts = mergeArtifacts(run.Status.Artifacts, outcome.Artifacts)
		}
		if outcome.Pending || !outcome.Complete {
			after := outcome.RequeueAfter
			if after <= 0 {
				after = 2 * time.Second
			}
			run.Status.ObservedGeneration = run.Generation
			if err := r.updateStatus(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: after}, nil
		}
		if run.Status.VerifySandboxRef == nil || run.Status.Gate == nil || run.Status.Gate.ReportRef == nil {
			return ctrl.Result{}, errors.New("completed verification lacks a child reference or signed Gate report")
		}
		if run.Status.Gate.Verdict != "Accepted" && run.Status.Gate.Verdict != "Rejected" {
			return ctrl.Result{}, errors.New("completed verification returned an unsupported Gate verdict")
		}
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseGated, now); err != nil {
			return ctrl.Result{}, err
		}
		conditionStatus := v1alpha1.ConditionTrue
		conditionReason := "GateAccepted"
		conditionMessage := "independent verification accepted the patch"
		if run.Status.Gate.Verdict == "Rejected" {
			conditionStatus = v1alpha1.ConditionFalse
			conditionReason = "GateRejected"
			conditionMessage = "independent verification rejected the patch"
		}
		if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
			Type: string(v1alpha1.ConditionVerified), Status: conditionStatus,
			Reason: conditionReason, Message: conditionMessage,
			ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
		}); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)

	case v1alpha1.PhaseGated:
		if run.Status.Gate == nil || run.Status.Gate.ReportRef == nil {
			return ctrl.Result{}, errors.New("Gated run has no signed Gate result")
		}
		accepted := run.Status.Gate.Verdict == "Accepted"
		rejected := run.Status.Gate.Verdict == "Rejected"
		if !accepted && !rejected {
			return ctrl.Result{}, errors.New("Gated run has an unsupported Gate verdict")
		}
		snapshot, err := r.loadSnapshot(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if snapshot.Gate.Mode != v1alpha1.GateShadow && snapshot.Gate.Mode != v1alpha1.GateEnforcing {
			return ctrl.Result{}, errors.New("Gated run has an unsupported immutable Gate mode")
		}
		if rejected && snapshot.Gate.Mode == v1alpha1.GateEnforcing {
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseRejected, now); err != nil {
				return ctrl.Result{}, err
			}
			run.Status.ObservedGeneration = run.Generation
			return ctrl.Result{}, r.updateStatus(ctx, run)
		}
		if snapshot.Spec.Publish.Mode == v1alpha1.PublishNone {
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseSucceeded, now); err != nil {
				return ctrl.Result{}, err
			}
			run.Status.ObservedGeneration = run.Generation
			return ctrl.Result{}, r.updateStatus(ctx, run)
		}
		if snapshot.Spec.Publish.Mode != v1alpha1.PublishPullRequest {
			return ctrl.Result{}, errors.New("Gated run has an unsupported publish mode")
		}
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhasePublishing, now); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)

	case v1alpha1.PhasePublishing:
		if r.Publish == nil {
			return ctrl.Result{}, errors.New("Publishing run has no controller-owned publication driver")
		}
		snapshot, err := r.loadSnapshot(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		outcome, err := r.Publish.Publish(ctx, run, snapshot)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("publish verified patch: %w", err)
		}
		if outcome.Effect != nil {
			effect := *outcome.Effect
			run.Status.Effect = &effect
		}
		if publishOutcomeStateCount(outcome) != 1 {
			return ctrl.Result{}, errors.New("publication driver returned an ambiguous state projection")
		}
		if err := projectPublicationStatus(&run.Status, snapshot, outcome, run.Generation, now); err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case outcome.Unknown:
			patchRequested := publicationRequestsPatch(snapshot)
			if patchRequested && run.Status.Effect == nil {
				run.Status.Effect = &v1alpha1.EffectSummary{State: v1alpha1.EffectUnknown}
			} else if patchRequested && run.Status.Effect != nil && run.Status.Effect.State == v1alpha1.EffectUnknown {
				run.Status.Effect.State = v1alpha1.EffectUnknown
				run.Status.Effect.PullRequestURL = ""
			}
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseUnknownEffect, now); err != nil {
				return ctrl.Result{}, err
			}
		case outcome.Failed:
			reason := outcome.Reason
			if reason == "" {
				reason = "controller-owned publication failed deterministically"
			}
			return ctrl.Result{}, r.failRun(ctx, run, now, "PublishFailed", reason)
		case outcome.Rejected:
			if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseRejected, now); err != nil {
				return ctrl.Result{}, err
			}
		case outcome.Succeeded:
			terminal := v1alpha1.PhaseSucceeded
			if snapshot.Gate.Mode == v1alpha1.GateShadow && run.Status.Gate != nil && run.Status.Gate.Verdict == "Rejected" {
				terminal = v1alpha1.PhaseRejected
			}
			if err := statusprojection.SetPhaseAt(&run.Status, terminal, now); err != nil {
				return ctrl.Result{}, err
			}
		case outcome.Pending:
			after := outcome.RequeueAfter
			if after <= 0 {
				after = 2 * time.Second
			}
			run.Status.ObservedGeneration = run.Generation
			if err := r.updateStatus(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: after}, nil
		default:
			return ctrl.Result{}, errors.New("publication driver returned no definitive state")
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{}, r.updateStatus(ctx, run)
	default:
		return ctrl.Result{}, nil
	}
}

// setExecutionCondition makes a failed Phase-0 decision visible without
// turning an operational isolation block into an AgentRun failure. The
// reason is intentionally copied only from the bounded preflight vocabulary;
// unknown or empty values use a stable generic diagnostic.
func setExecutionCondition(status *v1alpha1.AgentRunStatus, generation int64, now time.Time, conditionStatus v1alpha1.ConditionStatus, reason string) (bool, error) {
	if conditionStatus == v1alpha1.ConditionFalse {
		if reason == "" {
			reason = "ExecutionEvidenceUnavailable"
		}
	} else {
		reason = "ExecutionAllowed"
	}
	message := "Phase-0 execution evidence is unavailable; work remains pending"
	if conditionStatus == v1alpha1.ConditionTrue {
		message = "Phase-0 execution evidence permits work"
	}
	want := v1alpha1.Condition{
		Type: string(v1alpha1.ConditionReady), Status: conditionStatus,
		Reason: reason, Message: message, ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now),
	}
	for _, current := range status.Conditions {
		if current.Type == want.Type && current.Status == want.Status && current.Reason == want.Reason && current.Message == want.Message && current.ObservedGeneration == want.ObservedGeneration {
			return false, nil
		}
	}
	if err := statusprojection.SetCondition(status, want); err != nil {
		return false, err
	}
	return true, nil
}

// projectContextPack is the only path that writes status.contextPackRef. It
// reads the deterministic object from the immutable store and validates its
// canonical bytes, artifact digest, and identities independently. No runtime
// event, agent workspace file, or context-pack marker is trusted here.
func (r *AgentRunReconciler) projectContextPack(ctx context.Context, run *v1alpha1.AgentRun) (bool, error) {
	if r == nil || run == nil || r.Artifacts == nil || run.Status.SpecDigest == "" || run.Status.BaseSHA == "" {
		return false, nil
	}
	store := r.ContextArtifacts
	if store == nil {
		var ok bool
		store, ok = r.Artifacts.(ContextArtifactStore)
		if !ok {
			// Unit-test and migration fakes that predate context evidence do not
			// provide the seam. Production wiring supplies artifacts.Writer.
			return false, nil
		}
	}
	body, uri, err := store.LoadContextPack(ctx, string(run.UID), run.Status.SpecDigest, run.Status.BaseSHA)
	if err != nil {
		return false, err
	}
	_, ref, err := contextartifact.Decode(body, uri, string(run.UID), run.Status.SpecDigest, run.Status.BaseSHA)
	if err != nil {
		return false, err
	}
	if run.Status.ContextPackRef != nil && *run.Status.ContextPackRef != ref {
		return false, errors.New("immutable context artifact reference changed")
	}
	if run.Status.ContextPackRef != nil {
		return false, nil
	}
	if err := statusprojection.SetContextPackRef(&run.Status, ref); err != nil {
		return false, err
	}
	return true, nil
}

func mergeArtifacts(existing, additions []v1alpha1.ArtifactRef) []v1alpha1.ArtifactRef {
	out := append([]v1alpha1.ArtifactRef(nil), existing...)
	for _, addition := range additions {
		found := false
		for _, current := range out {
			if current.Digest == addition.Digest && current.Kind == addition.Kind && current.URI == addition.URI {
				found = true
				break
			}
		}
		if !found {
			out = append(out, addition)
		}
	}
	return out
}

func publishOutcomeStateCount(outcome PublishOutcome) int {
	count := 0
	for _, value := range []bool{outcome.Pending, outcome.Succeeded, outcome.Rejected, outcome.Failed, outcome.Unknown} {
		if value {
			count++
		}
	}
	return count
}

func publicationRequestsPatch(snapshot resolved.Snapshot) bool {
	if snapshot.Spec.Output == nil || snapshot.Spec.Output.Mode == "" {
		return true
	}
	return snapshot.Spec.Output.Mode == v1alpha1.OutputPatch || snapshot.Spec.Output.Mode == v1alpha1.OutputBoth
}

func publicationRequestsFindings(snapshot resolved.Snapshot) bool {
	return snapshot.Spec.Output != nil && (snapshot.Spec.Output.Mode == v1alpha1.OutputFindings || snapshot.Spec.Output.Mode == v1alpha1.OutputBoth)
}

// projectPublicationStatus keeps the two output effects independent at the
// Kubernetes boundary. In particular, findings-only output never masquerades
// as a controller-created PR, while a successful patch remains Published even
// when the independent findings effect later fails or becomes unknown.
func projectPublicationStatus(status *v1alpha1.AgentRunStatus, snapshot resolved.Snapshot, outcome PublishOutcome, generation int64, now time.Time) error {
	if status == nil {
		return errors.New("publication status is required")
	}
	patchRequested := publicationRequestsPatch(snapshot)
	if !patchRequested {
		// Findings-only has no controller-owned patch effect. Clear a stale
		// value as well as rejecting a new one so status cannot claim that a
		// patch PR was published for a review-only run.
		status.Effect = nil
	}
	patchPublished := patchRequested && status.Effect != nil && status.Effect.State == v1alpha1.EffectSucceeded && status.Effect.PullRequestURL != ""
	status.Published = patchPublished

	publishedCondition := v1alpha1.Condition{Type: string(v1alpha1.ConditionPublished), Status: v1alpha1.ConditionFalse, Reason: "PullRequestNotCreated", Message: "no controller-owned patch pull request was created"}
	if patchPublished {
		publishedCondition.Status = v1alpha1.ConditionTrue
		publishedCondition.Reason = "PullRequestCreated"
		publishedCondition.Message = "controller-owned patch publication completed"
	} else if !patchRequested {
		publishedCondition.Reason = "NoPatchPublication"
		publishedCondition.Message = "findings output does not create a patch pull request"
	} else if outcome.Unknown {
		publishedCondition.Status = v1alpha1.ConditionUnknown
		publishedCondition.Reason = "UnknownEffect"
		publishedCondition.Message = "patch publication outcome requires manual reconciliation"
	}
	publishedCondition.ObservedGeneration = generation
	if err := statusprojection.SetCondition(status, publishedCondition); err != nil {
		return err
	}

	if !publicationRequestsFindings(snapshot) {
		return nil
	}
	findingsCondition := v1alpha1.Condition{Type: string(v1alpha1.ConditionFindingsPublished), Status: v1alpha1.ConditionFalse, Reason: "FindingsNotPublished", Message: "findings publication did not complete"}
	switch {
	case outcome.FindingsPublished:
		findingsCondition.Status = v1alpha1.ConditionTrue
		findingsCondition.Reason = "FindingsPublished"
		findingsCondition.Message = "controller-owned findings publication completed"
	case outcome.FindingsUnknown:
		findingsCondition.Status = v1alpha1.ConditionUnknown
		findingsCondition.Reason = "UnknownEffect"
		findingsCondition.Message = "findings publication outcome requires manual reconciliation"
	case !outcome.FindingsAttempted:
		findingsCondition.Status = v1alpha1.ConditionUnknown
		findingsCondition.Reason = "FindingsNotAttempted"
		findingsCondition.Message = "findings publication is waiting for a proven patch publication"
	}
	findingsCondition.ObservedGeneration = generation
	return statusprojection.SetCondition(status, findingsCondition)
}

func (r *AgentRunReconciler) loadSnapshot(ctx context.Context, run *v1alpha1.AgentRun) (resolved.Snapshot, error) {
	body, err := r.Artifacts.LoadResolvedSpec(ctx, string(run.UID), run.Status.SpecDigest)
	if err != nil {
		return resolved.Snapshot{}, fmt.Errorf("load immutable resolved spec: %w", err)
	}
	snapshot, err := resolved.Decode(body, run.Status.SpecDigest)
	if err != nil {
		return resolved.Snapshot{}, fmt.Errorf("verify immutable resolved spec: %w", err)
	}
	if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) || snapshot.Run.Generation != run.Generation || snapshot.BaseSHA != run.Status.BaseSHA {
		return resolved.Snapshot{}, errors.New("immutable resolved spec does not belong to AgentRun")
	}
	return snapshot, nil
}

func (r *AgentRunReconciler) failAdmission(ctx context.Context, run *v1alpha1.AgentRun, now time.Time) error {
	if run.Status.Phase == "" {
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhasePending, now); err != nil {
			return err
		}
	}
	if err := statusprojection.SetFailureAt(&run.Status, "ResolutionFailed", "execution references could not be resolved safely", now); err != nil {
		return err
	}
	run.Status.ObservedGeneration = run.Generation
	return r.updateStatus(ctx, run)
}

func (r *AgentRunReconciler) failRun(ctx context.Context, run *v1alpha1.AgentRun, now time.Time, code, message string) error {
	if err := statusprojection.SetFailureAt(&run.Status, code, message, now); err != nil {
		return err
	}
	run.Status.ObservedGeneration = run.Generation
	return r.updateStatus(ctx, run)
}

func (r *AgentRunReconciler) finalize(ctx context.Context, run *v1alpha1.AgentRun) error {
	if !contains(run.Finalizers, AgentRunFinalizer) {
		return nil
	}
	if err := r.cleanupChildren(ctx, run); err != nil {
		return err
	}
	base := run.DeepCopy()
	run.Finalizers = remove(run.Finalizers, AgentRunFinalizer)
	return r.Client.SubResource("finalizers").Patch(ctx, run, client.MergeFrom(base))
}

func (r *AgentRunReconciler) cleanupChildren(ctx context.Context, run *v1alpha1.AgentRun) error {
	// Credential and execution cleanup must not depend on the object store.
	// In particular, a transient failure loading the immutable snapshot must
	// never leave model/MCP credentials or an executable Sandbox alive. Capture
	// resources still require the snapshot-bound plan and therefore remain
	// fail-closed; the finalizer is retained until every independent cleanup
	// action succeeds.
	var cleanupErrs []error
	if err := r.cleanupOrchestration(ctx, run); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	for _, secretName := range []string{workload.WorkSecretName(string(run.UID)), workload.VerifySecretName(string(run.UID))} {
		if err := r.deleteOwnedRunSecret(ctx, run, secretName); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if r.Sandboxes != nil {
		if err := r.cleanupSandboxes(ctx, run); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if r.Capture != nil && run.Status.SpecDigest != "" {
		snapshot, err := r.loadSnapshot(ctx, run)
		if err != nil {
			cleanupErrs = append(cleanupErrs, err)
		} else if err := r.Capture.Cleanup(ctx, run, snapshot); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("delete capture children: %w", err))
		}
	}
	return errors.Join(cleanupErrs...)
}

func (r *AgentRunReconciler) deleteOwnedRunSecret(ctx context.Context, run *v1alpha1.AgentRun, name string) error {
	if r == nil || r.Client == nil || run == nil || name == "" {
		return errors.New("cannot clean up per-run credential Secret without reconciler, run, and name")
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: run.Namespace, Name: name}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get per-run credential Secret %q: %w", name, err)
	}
	if len(secret.OwnerReferences) != 1 {
		return fmt.Errorf("%w: Secret %q has %d owner references", errForeignRunSecret, name, len(secret.OwnerReferences))
	}
	owner := secret.OwnerReferences[0]
	if owner.APIVersion != v1alpha1.GroupVersion.String() ||
		owner.Kind != "AgentRun" ||
		owner.Name != run.Name ||
		owner.UID != run.UID ||
		owner.Controller == nil || !*owner.Controller ||
		owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return fmt.Errorf("%w: Secret %q has unexpected owner", errForeignRunSecret, name)
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete per-run credential Secret %q: %w", name, err)
	}
	return nil
}

func (r *AgentRunReconciler) cleanupSandboxes(ctx context.Context, run *v1alpha1.AgentRun) error {
	discoverOrphans := run.Spec.CancelRequested || !run.DeletionTimestamp.IsZero()
	for _, item := range []struct {
		ref  *v1alpha1.ChildRef
		role sandbox.Role
	}{{run.Status.WorkSandboxRef, sandbox.RoleWork}, {run.Status.VerifySandboxRef, sandbox.RoleVerify}} {
		if item.ref == nil {
			if !discoverOrphans || run.Status.SpecDigest == "" {
				continue
			}
			cleaner, ok := r.Sandboxes.(sandbox.OrphanCleaner)
			if !ok {
				return fmt.Errorf("sandbox backend cannot recover an unreferenced %s child", item.role)
			}
			if err := cleaner.CleanupOwned(ctx, run.Namespace, run.UID, item.role, run.Status.SpecDigest); err != nil {
				return fmt.Errorf("cleanup unreferenced %s child: %w", item.role, err)
			}
			continue
		}
		if err := validateStatusChildRefForRun(run, item.ref, item.role); err != nil {
			return fmt.Errorf("validate %s child reference during cleanup: %w", item.role, err)
		}
		digest := item.ref.SpecDigest
		if digest == "" {
			digest = run.Status.SpecDigest
		}
		ref := sandbox.SandboxRef{Namespace: run.Namespace, Name: item.ref.Name, Kind: item.ref.Kind, UID: types.UID(item.ref.UID), OwnerUID: run.UID, Role: item.role, SpecDigest: digest, PlanFingerprint: item.ref.PlanFingerprint}
		if err := r.Sandboxes.Delete(ctx, ref); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s child (%s): %w", item.role, item.ref.Kind, err)
		}
	}
	return nil
}

// validateSandboxRefForRun is the controller-side status boundary for a
// backend-created child. Backend implementations validate the Kubernetes
// object they create, but the reconciler must not persist an unbound handle
// returned by a faulty or stale implementation. The deterministic name and
// immutable identities make the handle safe to replay after a controller
// restart and safe to use during finalizer cleanup.
func validateSandboxRefForRun(run *v1alpha1.AgentRun, ref sandbox.SandboxRef, role sandbox.Role) error {
	if run == nil {
		return errors.New("AgentRun is required")
	}
	if run.Status.SpecDigest == "" || !canonical.ValidDigest(run.Status.SpecDigest) {
		return errors.New("admitted AgentRun has no valid spec digest")
	}
	expectedName, err := sandbox.ChildName(run.UID, role)
	if err != nil {
		return err
	}
	if ref.Namespace != run.Namespace || ref.Name != expectedName {
		return fmt.Errorf("child identity is %s/%s, want %s/%s", ref.Namespace, ref.Name, run.Namespace, expectedName)
	}
	if ref.UID == "" || ref.OwnerUID != run.UID || ref.Role != role || ref.SpecDigest != run.Status.SpecDigest {
		return errors.New("child owner, role, or spec identity does not match AgentRun")
	}
	if !sandbox.ValidChildKind(ref.Kind) {
		return fmt.Errorf("unsupported child kind %q", ref.Kind)
	}
	if !canonical.ValidDigest(ref.PlanFingerprint) {
		return errors.New("child plan fingerprint is not a valid digest")
	}
	return nil
}

// validateStatusChildRefForRun applies the same identity contract after a
// restart, when the only available handle is the bounded status projection.
// It is intentionally stricter than the API's shape validation: a syntactically
// valid ChildRef is not enough to authorize observation or deletion.
func validateStatusChildRefForRun(run *v1alpha1.AgentRun, ref *v1alpha1.ChildRef, role sandbox.Role) error {
	if ref == nil {
		return errors.New("child reference is required")
	}
	if run == nil {
		return errors.New("AgentRun is required")
	}
	if run.Status.SpecDigest == "" || !canonical.ValidDigest(run.Status.SpecDigest) {
		return errors.New("admitted AgentRun has no valid spec digest")
	}
	expectedName, err := sandbox.ChildName(run.UID, role)
	if err != nil {
		return err
	}
	if ref.Name != expectedName {
		return fmt.Errorf("child name %q is not deterministic name %q", ref.Name, expectedName)
	}
	if ref.UID == "" || ref.Role != string(role) || ref.SpecDigest != run.Status.SpecDigest {
		return errors.New("persisted child owner, role, or spec identity does not match AgentRun")
	}
	if !sandbox.ValidChildKind(ref.Kind) {
		return fmt.Errorf("unsupported persisted child kind %q", ref.Kind)
	}
	if !canonical.ValidDigest(ref.PlanFingerprint) {
		return errors.New("persisted child plan fingerprint is not a valid digest")
	}
	return nil
}

func childRef(ref sandbox.SandboxRef) *v1alpha1.ChildRef {
	return &v1alpha1.ChildRef{Name: ref.Name, Kind: ref.Kind, UID: string(ref.UID), Role: string(ref.Role), SpecDigest: ref.SpecDigest, PlanFingerprint: ref.PlanFingerprint}
}

func sandboxRef(run *v1alpha1.AgentRun, ref *v1alpha1.ChildRef, role sandbox.Role) sandbox.SandboxRef {
	digest := ref.SpecDigest
	if digest == "" {
		digest = run.Status.SpecDigest
	}
	return sandbox.SandboxRef{Namespace: run.Namespace, Name: ref.Name, Kind: ref.Kind, UID: types.UID(ref.UID), OwnerUID: run.UID, Role: role, SpecDigest: digest, PlanFingerprint: ref.PlanFingerprint}
}

func (r *AgentRunReconciler) updateStatus(ctx context.Context, run *v1alpha1.AgentRun) error {
	bounded := statusprojection.Bounded(run.Status)
	if err := validateOrchestrationStatusValue(run, bounded.Orchestration); err != nil {
		return err
	}
	if err := statusprojection.Validate(bounded); err != nil {
		return err
	}
	run.Status = bounded
	return r.Status().Update(ctx, run)
}

func permanentResolutionError(err error) bool {
	return errors.Is(err, resolved.ErrInvalidRun) ||
		errors.Is(err, resolved.ErrReferenceMissing) ||
		errors.Is(err, resolved.ErrReferenceUnsafe) ||
		errors.Is(err, resolved.ErrConfiguration)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func remove(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

var _ SnapshotStore = (*artifacts.Writer)(nil)
var _ LifecycleOutputStore = (*artifacts.Writer)(nil)
var _ SandboxBackend = (sandbox.SandboxBackend)(nil)
var _ OrchestrationDriver = (*argoworkflow.Backend)(nil)
