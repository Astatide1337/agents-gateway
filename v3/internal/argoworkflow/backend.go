package argoworkflow

import (
	"context"
	"errors"
	"fmt"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrWorkflowMissing means a previously bound Workflow disappeared. It is
// deliberately distinct from a transient API error so the controller can fail
// closed without recreating a possibly partially-executed workflow.
var ErrWorkflowMissing = errors.New("argoworkflow: bound Workflow is missing")

// Backend is the narrow Kubernetes client boundary for the opt-in Argo
// orchestration backend. It creates one immutable Workflow and never updates
// or replaces it. Argo's controller owns the Workflow status; this package
// only reads it after the binding has been persisted by the AgentRun
// controller.
type Backend struct {
	client client.Client
	config Config
}

// NewBackend validates operator-owned configuration and returns an Argo
// backend. The client is expected to be scoped to the operator's normal
// namespace permissions; no Secret access is required here.
func NewBackend(c client.Client, config Config) (*Backend, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: Kubernetes client is nil", ErrInvalidInput)
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Backend{client: c, config: config}, nil
}

// ValidateTemplate proves that the named WorkflowTemplate still has the
// operator-pinned UID and content digest. A mutable or replaced template is a
// different orchestration program and is never accepted for a run.
func (b *Backend) ValidateTemplate(ctx context.Context) error {
	if b == nil || b.client == nil {
		return fmt.Errorf("%w: Argo backend is not configured", ErrInvalidInput)
	}
	template := workflowTemplateObject()
	key := client.ObjectKey{Namespace: b.config.Namespace, Name: b.config.WorkflowTemplateName}
	if err := b.client.Get(ctx, key, template); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: pinned WorkflowTemplate %q/%q is missing", ErrBinding, key.Namespace, key.Name)
		}
		return fmt.Errorf("get pinned WorkflowTemplate %q/%q: %w", key.Namespace, key.Name, err)
	}
	if string(template.GetUID()) != b.config.WorkflowTemplateUID {
		return fmt.Errorf("%w: WorkflowTemplate UID does not match operator pin", ErrBinding)
	}
	digest, err := WorkflowTemplateContentDigest(template)
	if err != nil {
		return err
	}
	if digest != b.config.WorkflowTemplateDigest {
		return fmt.Errorf("%w: WorkflowTemplate content digest does not match operator pin", ErrBinding)
	}
	if annotation := template.GetAnnotations()[templateDigestAnnotation]; annotation != "" && annotation != digest {
		return fmt.Errorf("%w: WorkflowTemplate digest annotation does not match content", ErrBinding)
	}
	return nil
}

// Ensure translates and creates the deterministic Workflow for the admitted
// run. An existing object is accepted only when its immutable identity and
// spec match. Create is attempted at most once: after a successful create, or
// an AlreadyExists response, the object is read back and bound by UID and
// generation before it can be observed.
func (b *Backend) Ensure(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (Binding, error) {
	if b == nil || b.client == nil {
		return Binding{}, fmt.Errorf("%w: Argo backend is not configured", ErrInvalidInput)
	}
	if run == nil || run.Status.ResolvedSpecRef == nil {
		return Binding{}, fmt.Errorf("%w: admitted AgentRun has no resolved spec reference", ErrInvalidInput)
	}
	if err := b.ValidateTemplate(ctx); err != nil {
		return Binding{}, err
	}
	translation, err := Translate(Inputs{
		Run:         run,
		Snapshot:    snapshot,
		ResolvedRef: *run.Status.ResolvedSpecRef,
		Config:      b.config,
	})
	if err != nil {
		return Binding{}, err
	}

	key := client.ObjectKey{Namespace: translation.Binding.Namespace, Name: translation.Binding.Reference.Name}
	live := workflowObject()
	err = b.client.Get(ctx, key, live)
	switch {
	case err == nil:
		return Bind(live, translation.Binding)
	case !apierrors.IsNotFound(err):
		return Binding{}, fmt.Errorf("get immutable Argo Workflow %q/%q: %w", key.Namespace, key.Name, err)
	}

	createErr := b.client.Create(ctx, translation.Workflow)
	switch {
	case createErr == nil:
		// Do not trust the object returned by Create for identity binding. A
		// read-after-create proves the API-server UID and generation that are
		// persisted for later status observations.
	case apierrors.IsAlreadyExists(createErr):
		// Another reconciliation may have won the create race. It is safe to
		// bind only after reading the single deterministic object back.
	default:
		return Binding{}, fmt.Errorf("create immutable Argo Workflow %q/%q: %w", key.Namespace, key.Name, createErr)
	}

	live = workflowObject()
	if err := b.client.Get(ctx, key, live); err != nil {
		return Binding{}, fmt.Errorf("read back created Argo Workflow %q/%q: %w", key.Namespace, key.Name, err)
	}
	return Bind(live, translation.Binding)
}

// Observe reads one already-bound Workflow and validates every immutable
// identity field before parsing its bounded backend status. A missing bound
// Workflow is terminally unsafe to recreate and is returned as
// ErrWorkflowMissing.
func (b *Backend) Observe(ctx context.Context, binding Binding) (Observation, error) {
	if b == nil || b.client == nil {
		return Observation{}, fmt.Errorf("%w: Argo backend is not configured", ErrInvalidObservation)
	}
	if err := validateBinding(binding); err != nil {
		return Observation{}, err
	}
	if err := b.ValidateTemplate(ctx); err != nil {
		return Observation{}, err
	}
	// Orchestration status intentionally stores only the backend Workflow
	// identity, not a duplicate copy of the immutable run timeout. Rehydrate
	// that value from the live AgentRun before validating activeDeadlineSeconds
	// so a tampered Workflow cannot silently extend its own deadline.
	run := &v1alpha1.AgentRun{}
	if err := b.client.Get(ctx, client.ObjectKey{Namespace: binding.Namespace, Name: binding.RunName}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return Observation{}, fmt.Errorf("%w: bound AgentRun is missing", ErrInvalidObservation)
		}
		return Observation{}, fmt.Errorf("%w: get bound AgentRun: %v", ErrInvalidObservation, err)
	}
	if string(run.UID) != binding.RunUID || run.Generation != binding.RunGeneration || run.Status.SpecDigest != binding.SpecDigest || run.Status.BaseSHA != binding.BaseSHA {
		return Observation{}, fmt.Errorf("%w: bound AgentRun identity does not match Workflow binding", ErrBinding)
	}
	deadlineSeconds, err := workflowDeadlineSeconds(run.Spec.Limits.Timeout)
	if err != nil {
		return Observation{}, fmt.Errorf("%w: bound AgentRun timeout is invalid", ErrBinding)
	}
	binding.RunTimeoutSeconds = deadlineSeconds
	live := workflowObject()
	key := client.ObjectKey{Namespace: binding.Namespace, Name: binding.Reference.Name}
	if err := b.client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return Observation{}, fmt.Errorf("%w: %q/%q", ErrWorkflowMissing, key.Namespace, key.Name)
		}
		return Observation{}, fmt.Errorf("get bound Argo Workflow %q/%q: %w", key.Namespace, key.Name, err)
	}
	return Observe(live, binding)
}

// Delete removes only the Workflow whose persisted identity was previously
// bound. UID preconditions prevent a delete from racing with replacement, and
// Bind rejects a same-name object with a different owner/spec before the
// delete is attempted.
func (b *Backend) Delete(ctx context.Context, binding Binding) error {
	if b == nil || b.client == nil {
		return fmt.Errorf("%w: Argo backend is not configured", ErrInvalidInput)
	}
	if err := validateBinding(binding); err != nil {
		return err
	}
	if binding.Reference.UID == "" || binding.Reference.Generation <= 0 {
		return fmt.Errorf("%w: cannot delete an unbound Workflow", ErrBinding)
	}
	live := workflowObject()
	key := client.ObjectKey{Namespace: binding.Namespace, Name: binding.Reference.Name}
	if err := b.client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get bound Argo Workflow for deletion %q/%q: %w", key.Namespace, key.Name, err)
	}
	if _, err := Bind(live, binding); err != nil {
		return err
	}
	uid := binding.Reference.UID
	if err := b.client.Delete(ctx, live, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete bound Argo Workflow %q/%q: %w", key.Namespace, key.Name, err)
	}
	return nil
}

func workflowObject() *unstructured.Unstructured {
	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: WorkflowKind})
	return workflow
}
