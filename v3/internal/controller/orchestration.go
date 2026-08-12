package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/argoworkflow"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	statusprojection "github.com/Astatide1337/agents-gateway/v3/internal/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
)

// OrchestrationBackendKind selects how the direct AgentRun lifecycle is
// sequenced. The zero value is deliberately direct for a safe rollback and
// backwards compatibility with existing controller construction.
type OrchestrationBackendKind string

const (
	OrchestrationBackendDirect OrchestrationBackendKind = "direct"
	OrchestrationBackendArgo   OrchestrationBackendKind = "argo"
)

// ParseOrchestrationBackend is the single configuration parser used by the
// operator. Unknown values fail startup instead of silently selecting direct.
func ParseOrchestrationBackend(raw string) (OrchestrationBackendKind, error) {
	switch OrchestrationBackendKind(strings.TrimSpace(raw)) {
	case "", OrchestrationBackendDirect:
		return OrchestrationBackendDirect, nil
	case OrchestrationBackendArgo:
		return OrchestrationBackendArgo, nil
	default:
		return "", fmt.Errorf("unsupported orchestration backend %q (want direct or argo)", raw)
	}
}

// OrchestrationDriver is intentionally smaller than the direct execution
// interfaces. Argo sequences lifecycle; the AgentRun controller still owns
// Gate/effect/publication semantics and never delegates those decisions to a
// backend phase.
type OrchestrationDriver interface {
	Ensure(context.Context, *v1alpha1.AgentRun, resolved.Snapshot) (argoworkflow.Binding, error)
	Observe(context.Context, argoworkflow.Binding) (argoworkflow.Observation, error)
	Delete(context.Context, argoworkflow.Binding) error
}

const (
	argoOrchestrationMessageBound   = "Argo Workflow identity and status are mirrored; Gate and effect semantics remain independent"
	argoOrchestrationSuccessMessage = "Argo Workflow output is validated and persisted; AGW verification and Gate semantics remain independent"
	argoOrchestrationFailureCode    = "ArgoWorkflowFailed"
	argoOrchestrationFailureMessage = "Argo Workflow did not complete successfully"
	argoExecutionRequeueAfter       = 10 * time.Second
)

const argoLifecycleOutputArtifactKind = argoworkflow.LifecycleOutputArtifactKind

var (
	orchestrationUIDPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	errArgoLifecycleOutputIdentity  = errors.New("Argo lifecycle output identity conflict")
	errArgoLifecycleOutputDuplicate = errors.New("Argo lifecycle output is duplicated in status")
)

func (r *AgentRunReconciler) validateOrchestrationConfig() error {
	backend := r.OrchestrationBackend
	if backend == "" {
		backend = OrchestrationBackendDirect
	}
	switch backend {
	case OrchestrationBackendDirect:
		return nil
	case OrchestrationBackendArgo:
		if r.Orchestration == nil {
			return errors.New("Argo orchestration backend is selected without an orchestration driver")
		}
		return nil
	default:
		return fmt.Errorf("unsupported orchestration backend %q", backend)
	}
}

func (r *AgentRunReconciler) usesArgoOrchestration() bool {
	return r.OrchestrationBackend == OrchestrationBackendArgo
}

// validateArgoBootstrapDependencies is intentionally stricter than the
// reconciler's general dependency check. Argo is only an orchestration
// backend; it does not own admission, work-plan construction, credential
// materialization, or Sandbox creation. Refuse the Argo path before any
// external child can be created when one of those controller-owned seams is
// absent.
func (r *AgentRunReconciler) validateArgoBootstrapDependencies() error {
	if r.Work == nil {
		return errors.New("Argo orchestration requires a WorkPlanFactory")
	}
	if r.Sandboxes == nil {
		return errors.New("Argo orchestration requires a SandboxBackend")
	}
	if r.Execution == nil {
		return errors.New("Argo orchestration requires an ExecutionGate")
	}
	return nil
}

// checkArgoExecution is called on every reconcile that can still create an
// operator-owned child. A denied check leaves the admitted run pending and
// deliberately does not invoke PlanWork, SandboxBackend.Ensure, or the Argo
// driver. The next reconcile gets a fresh Phase-0 decision.
func (r *AgentRunReconciler) checkArgoExecution(ctx context.Context, now time.Time) (ctrl.Result, bool) {
	decision := r.Execution.CheckExecution(ctx, now)
	if !decision.Allowed {
		return ctrl.Result{RequeueAfter: argoExecutionRequeueAfter}, false
	}
	return ctrl.Result{}, true
}

// bootstrapArgoWork creates or validates the operator-owned work inputs. The
// WorkPlanFactory is also the owner of the deterministic per-run work Secret;
// SandboxBackend.Ensure is create-or-get for the work child. Calling both on a
// retry is intentional: their contracts validate the existing resources and
// make a crash between resource creation and status persistence safe.
//
// The bool is true only when a previously persisted, validated WorkSandboxRef
// is ready for Workflow creation. When the reference is first persisted, this
// returns false so the API-server status write is a hard ordering boundary.
func (r *AgentRunReconciler) bootstrapArgoWork(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot, now time.Time) (bool, error) {
	if run.Status.WorkSandboxRef != nil {
		if err := validateArgoWorkStatusRef(run, run.Status.WorkSandboxRef); err != nil {
			return false, err
		}
	}

	plan, err := r.Work.PlanWork(ctx, run, snapshot)
	if err != nil {
		return false, fmt.Errorf("construct Argo work Sandbox: %w", err)
	}
	if plan.Owner == nil || plan.Owner.Namespace != run.Namespace || plan.Owner.Name != run.Name || plan.Owner.UID != run.UID || plan.Role != sandbox.RoleWork || plan.SpecDigest != run.Status.SpecDigest {
		return false, errors.New("Argo work plan identity does not match admitted AgentRun")
	}

	ref, err := r.Sandboxes.Ensure(ctx, plan)
	if err != nil {
		return false, fmt.Errorf("ensure Argo work Sandbox: %w", err)
	}
	if err := validateArgoWorkRef(run, ref); err != nil {
		return false, err
	}

	if run.Status.WorkSandboxRef == nil {
		run.Status.WorkSandboxRef = childRef(ref)
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCloning, now); err != nil {
			return false, fmt.Errorf("record Argo work bootstrap phase: %w", err)
		}
		run.Status.ObservedGeneration = run.Generation
		if err := r.updateStatus(ctx, run); err != nil {
			return false, fmt.Errorf("persist Argo work Sandbox reference: %w", err)
		}
		return false, nil
	}

	if err := validateArgoWorkStatusRefMatches(run, run.Status.WorkSandboxRef, ref); err != nil {
		return false, err
	}
	// A reference can have been persisted by a previous controller version or
	// by a crash-recovery write while the phase was still Pending. Persist the
	// phase transition before allowing the Workflow create, too.
	if run.Status.Phase == v1alpha1.PhasePending {
		if err := statusprojection.SetPhaseAt(&run.Status, v1alpha1.PhaseCloning, now); err != nil {
			return false, fmt.Errorf("record Argo work bootstrap phase: %w", err)
		}
		run.Status.ObservedGeneration = run.Generation
		if err := r.updateStatus(ctx, run); err != nil {
			return false, fmt.Errorf("persist Argo work bootstrap phase: %w", err)
		}
		return false, nil
	}
	if run.Status.Phase != v1alpha1.PhaseCloning {
		return false, fmt.Errorf("Argo work bootstrap has unexpected AgentRun phase %q", run.Status.Phase)
	}
	return true, nil
}

func validateArgoWorkRef(run *v1alpha1.AgentRun, ref sandbox.SandboxRef) error {
	if run == nil || run.Status.SpecDigest == "" {
		return errors.New("Argo work child cannot be validated without an admitted AgentRun")
	}
	expectedName, err := sandbox.ChildName(run.UID, sandbox.RoleWork)
	if err != nil {
		return fmt.Errorf("derive deterministic Argo work child name: %w", err)
	}
	if ref.Namespace != run.Namespace || ref.Name != expectedName || ref.UID == "" || ref.OwnerUID != run.UID || ref.Role != sandbox.RoleWork || !sandbox.ValidChildKind(ref.Kind) || ref.SpecDigest != run.Status.SpecDigest || !canonical.ValidDigest(ref.PlanFingerprint) {
		return errors.New("Argo work Sandbox reference does not match the admitted AgentRun")
	}
	return nil
}

func validateArgoWorkStatusRef(run *v1alpha1.AgentRun, ref *v1alpha1.ChildRef) error {
	if run == nil || ref == nil {
		return errors.New("Argo work Sandbox reference cannot be validated without an AgentRun")
	}
	if ref.Role != string(sandbox.RoleWork) || ref.UID == "" || ref.SpecDigest != run.Status.SpecDigest || !canonical.ValidDigest(ref.PlanFingerprint) {
		return errors.New("persisted Argo work Sandbox reference is invalid")
	}
	if ref.Kind != sandbox.ChildKindSandbox && ref.Kind != sandbox.ChildKindJob {
		return errors.New("persisted Argo work Sandbox reference has an unsupported child kind")
	}
	expectedName, err := sandbox.ChildName(run.UID, sandbox.RoleWork)
	if err != nil {
		return fmt.Errorf("derive deterministic Argo work child name: %w", err)
	}
	if ref.Name != expectedName {
		return errors.New("persisted Argo work Sandbox reference has a non-deterministic name")
	}
	return nil
}

func validateArgoWorkStatusRefMatches(run *v1alpha1.AgentRun, statusRef *v1alpha1.ChildRef, liveRef sandbox.SandboxRef) error {
	if err := validateArgoWorkStatusRef(run, statusRef); err != nil {
		return err
	}
	if statusRef.Name != liveRef.Name || statusRef.Kind != liveRef.Kind || statusRef.UID != string(liveRef.UID) || statusRef.Role != string(liveRef.Role) || statusRef.SpecDigest != liveRef.SpecDigest || statusRef.PlanFingerprint != liveRef.PlanFingerprint {
		return errors.New("persisted Argo work Sandbox reference changed on retry")
	}
	return nil
}

// driveArgo intentionally has no AgentRun lifecycle state machine. It binds a
// deterministic Workflow once, mirrors its bounded backend phase, and then
// stops. A successful Workflow is not an AGW success: no Gate, Effect,
// Published, or AgentRun terminal field is changed by this path.
func (r *AgentRunReconciler) driveArgo(ctx context.Context, run *v1alpha1.AgentRun, now time.Time) (ctrl.Result, error) {
	if r.Orchestration == nil {
		return ctrl.Result{}, errors.New("Argo orchestration backend has no driver")
	}
	if err := r.validateArgoBootstrapDependencies(); err != nil {
		return ctrl.Result{}, err
	}
	snapshot, err := r.loadSnapshot(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if run.Status.Orchestration == nil {
		if result, allowed := r.checkArgoExecution(ctx, now); !allowed {
			return result, nil
		}
		ready, err := r.bootstrapArgoWork(ctx, run, snapshot, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// The work child and Secret may have taken time to validate. Check the
		// current execution evidence again immediately before the Workflow
		// create; this closes the child-to-Workflow race as well as the initial
		// Pending admission race.
		if result, allowed := r.checkArgoExecution(ctx, now); !allowed {
			return result, nil
		}
		binding, err := r.Orchestration.Ensure(ctx, run, snapshot)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("ensure immutable Argo Workflow: %w", err)
		}
		if err := validateOrchestrationBinding(run, snapshot, binding); err != nil {
			return ctrl.Result{}, err
		}
		// This is the trust boundary: persist the API-server name, UID,
		// generation, owner/spec identity, and resolved contract before calling
		// Observe. A crash after Create is safe because the next Ensure reads the
		// same deterministic object rather than creating another one.
		run.Status.Orchestration = &v1alpha1.OrchestrationStatus{
			Backend:   v1alpha1.OrchestrationBackendArgo,
			Phase:     v1alpha1.OrchestrationPending,
			Message:   "Argo Workflow identity bound; backend status will be observed after persistence",
			Reference: orchestrationRef(binding),
		}
		run.Status.ObservedGeneration = run.Generation
		if err := r.updateStatus(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	binding, err := bindingFromOrchestrationStatus(run)
	if err != nil {
		return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowBindingInvalid", "persisted Argo Workflow binding is invalid")
	}
	if err := validateOrchestrationBinding(run, snapshot, binding); err != nil {
		return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowBindingInvalid", "persisted Argo Workflow binding no longer matches the admitted run")
	}
	observation, err := r.Orchestration.Observe(ctx, binding)
	if err != nil {
		switch {
		case errors.Is(err, argoworkflow.ErrWorkflowMissing):
			return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowMissing", "bound Argo Workflow disappeared and will not be recreated")
		case errors.Is(err, argoworkflow.ErrBinding), errors.Is(err, argoworkflow.ErrInvalidObservation), errors.Is(err, argoworkflow.ErrUnknownPhase):
			return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowObservationInvalid", "bound Argo Workflow identity or status failed validation")
		case errors.Is(err, argoworkflow.ErrLifecycleOutputMissing), errors.Is(err, argoworkflow.ErrLifecycleOutputDuplicate), errors.Is(err, argoworkflow.ErrLifecycleOutputTooLarge), errors.Is(err, argoworkflow.ErrLifecycleOutputInvalid), errors.Is(err, argoworkflow.ErrLifecycleOutputNonCanonical), errors.Is(err, argoworkflow.ErrLifecycleOutputDigestMismatch), errors.Is(err, argoworkflow.ErrLifecycleOutputBinding), errors.Is(err, argoworkflow.ErrLifecycleOutputUnsupported), errors.Is(err, artifacts.ErrInvalidArtifact), errors.Is(err, artifacts.ErrObjectConflict):
			return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoLifecycleOutputInvalid", "Argo lifecycle output failed validation")
		default:
			return ctrl.Result{}, fmt.Errorf("observe bound Argo Workflow: %w", err)
		}
	}
	if err := validateOrchestrationObservation(binding, observation); err != nil {
		return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowObservationInvalid", "bound Argo Workflow observation failed validation")
	}

	next := &v1alpha1.OrchestrationStatus{
		Backend:   v1alpha1.OrchestrationBackendArgo,
		Phase:     v1alpha1.OrchestrationPhase(observation.Phase),
		Message:   argoOrchestrationMessage(observation),
		Reference: orchestrationRefFromObservation(binding, observation),
	}
	if err := validateOrchestrationStatusValue(run, next); err != nil {
		return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoWorkflowStatusInvalid", "Argo Workflow status could not be safely projected")
	}
	changed := !reflect.DeepEqual(run.Status.Orchestration, next)
	run.Status.Orchestration = next
	run.Status.ObservedGeneration = run.Generation

	switch observation.Phase {
	case argoworkflow.BackendFailed, argoworkflow.BackendError, argoworkflow.BackendSkipped, argoworkflow.BackendOmitted:
		if err := statusprojection.SetFailureAt(&run.Status, argoOrchestrationFailureCode, argoOrchestrationFailureMessage, now); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.updateStatus(ctx, run)
	case argoworkflow.BackendSucceeded:
		// A successful Argo phase is not sufficient. Observe has already
		// required the reserved, canonical lifecycle output and bound it to the
		// live Workflow/AgentRun identities. Persist that exact byte string as
		// a controller-owned content-addressed artifact before any AGW-owned
		// verifier can consume its patch or completion evidence.
		if observation.Output == nil {
			return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoLifecycleOutputMissing", "successful Argo Workflow has no validated lifecycle output")
		}
		if r.LifecycleOutputs == nil {
			return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoLifecycleOutputStoreMissing", "Argo lifecycle output storage is not configured")
		}
		if err := r.acceptArgoLifecycleOutput(ctx, run, *observation.Output); err != nil {
			if errors.Is(err, errArgoLifecycleOutputIdentity) || errors.Is(err, errArgoLifecycleOutputDuplicate) || errors.Is(err, argoworkflow.ErrLifecycleOutputMissing) || errors.Is(err, argoworkflow.ErrLifecycleOutputDuplicate) || errors.Is(err, argoworkflow.ErrLifecycleOutputTooLarge) || errors.Is(err, argoworkflow.ErrLifecycleOutputInvalid) || errors.Is(err, argoworkflow.ErrLifecycleOutputNonCanonical) || errors.Is(err, argoworkflow.ErrLifecycleOutputDigestMismatch) || errors.Is(err, argoworkflow.ErrLifecycleOutputBinding) || errors.Is(err, argoworkflow.ErrLifecycleOutputUnsupported) {
				return ctrl.Result{}, r.failRun(ctx, run, now, "ArgoLifecycleOutputInvalid", "Argo lifecycle output failed controller binding")
			}
			return ctrl.Result{}, fmt.Errorf("persist Argo lifecycle output: %w", err)
		}
		// The handoff advances only to AGW verification. Gate, effect, and
		// publication fields remain untouched. The explicit sequence preserves
		// the existing AgentRun FSM while recording that the Argo output proved
		// each earlier boundary.
		if err := advanceArgoToVerifying(&run.Status, now); err != nil {
			return ctrl.Result{}, err
		}
		if err := statusprojection.SetCondition(&run.Status, v1alpha1.Condition{
			Type: string(v1alpha1.ConditionWorkComplete), Status: v1alpha1.ConditionTrue,
			Reason: "ArgoLifecycleOutputValidated", Message: "controller validated immutable Argo lifecycle output",
			ObservedGeneration: run.Generation, LastTransitionTime: metav1.NewTime(now),
		}); err != nil {
			return ctrl.Result{}, err
		}
		run.Status.ObservedGeneration = run.Generation
		return ctrl.Result{Requeue: true}, r.updateStatus(ctx, run)
	default:
		if changed {
			if err := r.updateStatus(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
}

// argoLifecycleHandoffBound reports whether the controller has already
// accepted the Argo handoff and resumed the AGW-owned lifecycle. It is a
// routing guard only: the content is validated at the Argo observation edge
// and the reference is persisted as an immutable artifact before this returns
// true. A Workflow phase alone can never make this true.
func argoLifecycleHandoffBound(run *v1alpha1.AgentRun) bool {
	if run == nil || run.Status.Orchestration == nil {
		return false
	}
	switch run.Status.Phase {
	case v1alpha1.PhaseVerifying, v1alpha1.PhaseGated, v1alpha1.PhasePublishing,
		v1alpha1.PhaseSucceeded, v1alpha1.PhaseRejected, v1alpha1.PhaseFailed,
		v1alpha1.PhaseCancelled, v1alpha1.PhaseUnknownEffect:
	default:
		return false
	}
	if run.Status.Patch == nil || run.Status.Patch.Ref == nil || run.Status.Patch.ManifestRef == nil || run.Status.EventStreamRef == nil {
		return false
	}
	count := 0
	for _, ref := range run.Status.Artifacts {
		if ref.Kind == argoLifecycleOutputArtifactKind {
			if ref.Name != argoworkflow.LifecycleOutputArtifactName {
				return false
			}
			count++
		}
	}
	return count == 1
}

// acceptArgoLifecycleOutput is the controller-side adapter between the
// neutral Argo boundary and AGW-owned verification. It performs no Gate,
// effect, or publication transition. It first checks already-projected
// references for conflicting retries, then persists the exact canonical bytes
// under their content digest, and only then projects the handoff references.
func (r *AgentRunReconciler) acceptArgoLifecycleOutput(ctx context.Context, run *v1alpha1.AgentRun, output argoworkflow.BoundLifecycleOutput) error {
	if r == nil || r.LifecycleOutputs == nil || run == nil {
		return fmt.Errorf("%w: controller dependencies are incomplete", errArgoLifecycleOutputIdentity)
	}
	if err := validateArgoPreGateState(run.Status); err != nil {
		return err
	}
	contract := output.Contract
	if output.Digest == "" || len(output.Canonical) == 0 || contract.RunUID != string(run.UID) || contract.RunGeneration != run.Generation || contract.SpecDigest != run.Status.SpecDigest || contract.BaseSHA != run.Status.BaseSHA {
		return fmt.Errorf("%w: output does not match AgentRun", errArgoLifecycleOutputIdentity)
	}
	canonicalBody, err := argoworkflow.MarshalLifecycleOutput(contract)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonicalBody, output.Canonical) {
		return argoworkflow.ErrLifecycleOutputNonCanonical
	}
	computedDigest, err := argoworkflow.DigestLifecycleOutput(contract)
	if err != nil {
		return err
	}
	if computedDigest != output.Digest {
		return argoworkflow.ErrLifecycleOutputDigestMismatch
	}
	statusOrchestration := run.Status.Orchestration
	if statusOrchestration == nil || statusOrchestration.Phase != v1alpha1.OrchestrationSucceeded || statusOrchestration.Reference.UID != contract.WorkflowUID || statusOrchestration.Reference.Generation != contract.WorkflowGeneration || statusOrchestration.Reference.RunGeneration != run.Generation || statusOrchestration.Reference.SpecDigest != run.Status.SpecDigest || statusOrchestration.Reference.WorkflowTemplateUID != contract.WorkflowTemplateUID || statusOrchestration.Reference.WorkflowTemplateDigest != contract.WorkflowTemplateDigest {
		return fmt.Errorf("%w: output does not match persisted Workflow reference", errArgoLifecycleOutputIdentity)
	}

	var existingLifecycle *v1alpha1.ArtifactRef
	for index := range run.Status.Artifacts {
		ref := run.Status.Artifacts[index]
		if ref.Kind != argoLifecycleOutputArtifactKind {
			continue
		}
		if ref.Name != "lifecycle-output.json" {
			return fmt.Errorf("%w: reserved lifecycle artifact has an unexpected name", errArgoLifecycleOutputIdentity)
		}
		if existingLifecycle != nil {
			return errArgoLifecycleOutputDuplicate
		}
		copy := ref
		existingLifecycle = &copy
	}

	patch := &v1alpha1.PatchSummary{
		Ref:          artifactRefCopy(contract.Patch.Ref),
		ManifestRef:  artifactRefCopy(contract.Patch.ManifestRef),
		FilesChanged: int32(contract.Patch.FilesChanged),
		LinesChanged: int32(contract.Patch.LinesChanged),
	}
	if contract.Patch.FilesChanged > math.MaxInt32 || contract.Patch.LinesChanged > math.MaxInt32 {
		return fmt.Errorf("%w: patch counters exceed status bounds", errArgoLifecycleOutputIdentity)
	}
	if run.Status.Patch != nil && !patchSummaryEqual(run.Status.Patch, patch) {
		return fmt.Errorf("%w: projected patch changed on retry", errArgoLifecycleOutputIdentity)
	}
	if run.Status.EventStreamRef != nil && *run.Status.EventStreamRef != contract.Runtime.EventStreamRef {
		return fmt.Errorf("%w: projected event stream changed on retry", errArgoLifecycleOutputIdentity)
	}

	lifecycleRef, err := r.LifecycleOutputs.SaveArgoLifecycleOutput(ctx, string(run.UID), run.Status.SpecDigest, output.Canonical)
	if err != nil {
		return fmt.Errorf("save immutable lifecycle output: %w", err)
	}
	if lifecycleRef.Digest != output.Digest || lifecycleRef.Kind != argoLifecycleOutputArtifactKind || lifecycleRef.Name != argoworkflow.LifecycleOutputArtifactName || lifecycleRef.MediaType != argoworkflow.LifecycleOutputMediaType || lifecycleRef.SizeBytes != int64(len(output.Canonical)) {
		return fmt.Errorf("%w: persistence adapter returned a different lifecycle reference", errArgoLifecycleOutputIdentity)
	}
	if existingLifecycle != nil && *existingLifecycle != lifecycleRef {
		return fmt.Errorf("%w: persisted lifecycle reference changed on retry", errArgoLifecycleOutputIdentity)
	}
	if existingLifecycle == nil && len(run.Status.Artifacts) >= statusprojection.MaxStatusConditions {
		return fmt.Errorf("%w: status artifact budget is exhausted", errArgoLifecycleOutputIdentity)
	}

	if run.Status.Patch == nil {
		run.Status.Patch = patch
	}
	if run.Status.EventStreamRef == nil {
		ref := contract.Runtime.EventStreamRef
		if err := statusprojection.SetEventStreamRef(&run.Status, ref); err != nil {
			return err
		}
	}
	if existingLifecycle == nil {
		run.Status.Artifacts = append(run.Status.Artifacts, lifecycleRef)
	}
	return nil
}

func validateArgoPreGateState(status v1alpha1.AgentRunStatus) error {
	switch status.Phase {
	case v1alpha1.PhasePending, v1alpha1.PhaseCloning, v1alpha1.PhaseWorking, v1alpha1.PhaseCapturing:
	default:
		return fmt.Errorf("%w: lifecycle output arrived outside the pre-Gate phases", errArgoLifecycleOutputIdentity)
	}
	if status.Gate != nil || status.Effect != nil || status.Published || status.VerifySandboxRef != nil || status.CompletedAt != nil {
		return fmt.Errorf("%w: lifecycle output cannot coexist with prior Gate, effect, verification, publication, or completion state", errArgoLifecycleOutputIdentity)
	}
	return nil
}

func artifactRefCopy(ref v1alpha1.ArtifactRef) *v1alpha1.ArtifactRef {
	copy := ref
	return &copy
}

func patchSummaryEqual(left, right *v1alpha1.PatchSummary) bool {
	if left == nil || right == nil || left.FilesChanged != right.FilesChanged || left.LinesChanged != right.LinesChanged {
		return left == nil && right == nil
	}
	if left.Ref == nil || right.Ref == nil || *left.Ref != *right.Ref {
		return left.Ref == nil && right.Ref == nil
	}
	return left.ManifestRef != nil && right.ManifestRef != nil && *left.ManifestRef == *right.ManifestRef
}

// advanceArgoToVerifying preserves the existing FSM instead of inventing a
// Pending -> Verifying transition. The validated lifecycle contract proves
// the earlier boundaries, so the controller records those sequentially in
// memory and persists only the resulting AGW-owned verification handoff.
func advanceArgoToVerifying(status *v1alpha1.AgentRunStatus, now time.Time) error {
	if status == nil {
		return errors.New("cannot advance a nil AgentRun status")
	}
	if status.Phase == v1alpha1.PhaseVerifying {
		return nil
	}
	phases := []v1alpha1.Phase{v1alpha1.PhaseCloning, v1alpha1.PhaseWorking, v1alpha1.PhaseCapturing, v1alpha1.PhaseVerifying}
	start := 0
	switch status.Phase {
	case v1alpha1.PhasePending:
	case v1alpha1.PhaseCloning:
		start = 1
	case v1alpha1.PhaseWorking:
		start = 2
	case v1alpha1.PhaseCapturing:
		start = 3
	default:
		return fmt.Errorf("cannot advance Argo handoff from phase %s", status.Phase)
	}
	for _, phase := range phases[start:] {
		if err := statusprojection.SetPhaseAt(status, phase, now); err != nil {
			return fmt.Errorf("advance Argo handoff to %s: %w", phase, err)
		}
	}
	return nil
}

func argoOrchestrationMessage(observation argoworkflow.Observation) string {
	if observation.Phase == argoworkflow.BackendSucceeded {
		return argoOrchestrationSuccessMessage
	}
	return argoOrchestrationMessageBound
}

func orchestrationRef(binding argoworkflow.Binding) v1alpha1.OrchestrationRef {
	return v1alpha1.OrchestrationRef{
		APIVersion:             argoworkflow.WorkflowAPIVersion,
		Kind:                   argoworkflow.WorkflowKind,
		Namespace:              binding.Reference.Namespace,
		Name:                   binding.Reference.Name,
		UID:                    string(binding.Reference.UID),
		Generation:             binding.Reference.Generation,
		RunGeneration:          binding.RunGeneration,
		SpecDigest:             binding.SpecDigest,
		WorkflowTemplateName:   binding.WorkflowTemplateName,
		WorkflowTemplateUID:    binding.WorkflowTemplateUID,
		WorkflowTemplateDigest: binding.WorkflowTemplateDigest,
	}
}

func orchestrationRefFromObservation(binding argoworkflow.Binding, observation argoworkflow.Observation) v1alpha1.OrchestrationRef {
	ref := binding
	ref.Reference = observation.Reference
	return orchestrationRef(ref)
}

func bindingFromOrchestrationStatus(run *v1alpha1.AgentRun) (argoworkflow.Binding, error) {
	if run == nil || run.Status.Orchestration == nil {
		return argoworkflow.Binding{}, errors.New("Argo orchestration status is missing")
	}
	status := run.Status.Orchestration
	if status.Backend != v1alpha1.OrchestrationBackendArgo {
		return argoworkflow.Binding{}, errors.New("Argo orchestration status has an unsupported backend")
	}
	ref := status.Reference
	if ref.APIVersion != argoworkflow.WorkflowAPIVersion || ref.Kind != argoworkflow.WorkflowKind || ref.Namespace != run.Namespace || ref.Name == "" || ref.UID == "" || ref.Generation <= 0 || ref.RunGeneration != run.Generation || ref.SpecDigest != run.Status.SpecDigest || ref.WorkflowTemplateName == "" || ref.WorkflowTemplateUID == "" || !canonical.ValidDigest(ref.WorkflowTemplateDigest) {
		return argoworkflow.Binding{}, errors.New("Argo orchestration status reference does not match AgentRun")
	}
	if run.Status.ResolvedSpecRef == nil || run.Status.ResolvedSpecRef.Digest != ref.SpecDigest {
		return argoworkflow.Binding{}, errors.New("Argo orchestration status has no matching resolved reference")
	}
	return argoworkflow.Binding{
		Reference: argoworkflow.Reference{
			Namespace:  ref.Namespace,
			Name:       ref.Name,
			UID:        typesUID(ref.UID),
			Generation: ref.Generation,
		},
		RunUID:                 string(run.UID),
		RunName:                run.Name,
		Namespace:              run.Namespace,
		WorkflowTemplateName:   ref.WorkflowTemplateName,
		WorkflowTemplateUID:    ref.WorkflowTemplateUID,
		WorkflowTemplateDigest: ref.WorkflowTemplateDigest,
		SpecDigest:             run.Status.SpecDigest,
		BaseSHA:                run.Status.BaseSHA,
		ResolvedRef:            *run.Status.ResolvedSpecRef,
		RunGeneration:          run.Generation,
	}, nil
}

// typesUID keeps the controller file independent of Kubernetes object
// construction details while retaining the exact UID type required by the
// argoworkflow boundary.
func typesUID(value string) types.UID {
	return types.UID(value)
}

func validateOrchestrationBinding(run *v1alpha1.AgentRun, snapshot resolved.Snapshot, binding argoworkflow.Binding) error {
	if run == nil || run.Status.ResolvedSpecRef == nil {
		return errors.New("Argo binding cannot be validated without an admitted resolved reference")
	}
	if binding.Reference.UID == "" || binding.Reference.Generation <= 0 || binding.RunGeneration != run.Generation || binding.Namespace != run.Namespace || binding.RunName != run.Name || binding.RunUID != string(run.UID) || binding.SpecDigest != run.Status.SpecDigest || binding.BaseSHA != run.Status.BaseSHA || binding.ResolvedRef != *run.Status.ResolvedSpecRef || binding.WorkflowTemplateUID == "" || !canonical.ValidDigest(binding.WorkflowTemplateDigest) {
		return errors.New("Argo binding does not match AgentRun identity or immutable snapshot")
	}
	if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) || snapshot.Run.Generation != run.Generation || snapshot.BaseSHA != run.Status.BaseSHA {
		return errors.New("Argo binding snapshot identity does not match AgentRun")
	}
	return nil
}

func validateOrchestrationObservation(binding argoworkflow.Binding, observation argoworkflow.Observation) error {
	if !observation.Terminal && observation.Phase != argoworkflow.BackendPending && observation.Phase != argoworkflow.BackendRunning && observation.Phase != argoworkflow.BackendSuspended {
		return errors.New("Argo observation has an unsupported non-terminal phase")
	}
	if observation.Reference != binding.Reference {
		return errors.New("Argo observation reference changed after binding")
	}
	return nil
}

func validateOrchestrationStatusValue(run *v1alpha1.AgentRun, status *v1alpha1.OrchestrationStatus) error {
	if status == nil {
		return nil
	}
	if run == nil || status.Backend != v1alpha1.OrchestrationBackendArgo || status.Message == "" || len(status.Message) > v1alpha1.MaxStatusMessage || strings.IndexByte(status.Message, 0) >= 0 || len(validation.IsDNS1123Label(status.Reference.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(status.Reference.Name)) != 0 || status.Reference.APIVersion != argoworkflow.WorkflowAPIVersion || status.Reference.Kind != argoworkflow.WorkflowKind || !orchestrationUIDPattern.MatchString(status.Reference.UID) || status.Reference.Generation <= 0 || status.Reference.RunGeneration <= 0 || !canonical.ValidDigest(status.Reference.SpecDigest) || len(validation.IsDNS1123Subdomain(status.Reference.WorkflowTemplateName)) != 0 || !orchestrationUIDPattern.MatchString(status.Reference.WorkflowTemplateUID) || !canonical.ValidDigest(status.Reference.WorkflowTemplateDigest) {
		return errors.New("invalid bounded Argo orchestration status")
	}
	if status.Reference.Namespace != run.Namespace || status.Reference.RunGeneration != run.Generation || status.Reference.SpecDigest != run.Status.SpecDigest || run.Status.ResolvedSpecRef == nil || run.Status.ResolvedSpecRef.Digest != status.Reference.SpecDigest {
		return errors.New("Argo orchestration status identity does not match AgentRun")
	}
	switch status.Phase {
	case v1alpha1.OrchestrationPending, v1alpha1.OrchestrationRunning, v1alpha1.OrchestrationSucceeded, v1alpha1.OrchestrationFailed, v1alpha1.OrchestrationError, v1alpha1.OrchestrationSkipped, v1alpha1.OrchestrationOmitted, v1alpha1.OrchestrationSuspended:
		return nil
	default:
		return errors.New("invalid Argo orchestration phase")
	}
}

func (r *AgentRunReconciler) cleanupOrchestration(ctx context.Context, run *v1alpha1.AgentRun) error {
	if !r.usesArgoOrchestration() || run == nil || run.Status.Orchestration == nil {
		return nil
	}
	binding, err := bindingFromOrchestrationStatus(run)
	if err != nil {
		return err
	}
	if err := r.Orchestration.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete bound Argo Workflow: %w", err)
	}
	return nil
}
