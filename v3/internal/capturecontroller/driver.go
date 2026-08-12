// Package capturecontroller drives the short-lived, networkless capture phase.
//
// The package is intentionally small and conservative. capture.Plan remains
// the source of truth for the Job and NetworkPolicy shape; this package owns
// only the Kubernetes lifecycle around that plan. In particular, it never
// patches an existing resource, treats AlreadyExists as success, mounts a
// Secret, or gives a capture pod an artifact credential.
package capturecontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/capture"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	RunUIDLabelKey   = "agents.astatide.com/run-uid"
	RoleLabelKey     = "agents.astatide.com/role"
	SpecDigestLabel  = "agents.astatide.com/spec-digest-short"
	EgressLabelKey   = "agents.astatide.com/egress"
	EgressNone       = "none"
	SpecDigest       = "agents.astatide.com/spec-digest"
	InputDigest      = "agents.astatide.com/capture-input-digest"
	OwnerAPIVersion  = "agents.astatide.com/v1alpha1"
	OwnerKind        = "AgentRun"
	JobOwnerAPIVer   = "batch/v1"
	JobOwnerKind     = "Job"
	CaptureContainer = capture.Role
)

var (
	ErrInvalidDriver = errors.New("capturecontroller: invalid driver")
	ErrInvalidPlan   = errors.New("capturecontroller: invalid capture plan")
	ErrMissing       = errors.New("capturecontroller: capture resource or pod is missing")
	ErrRunning       = errors.New("capturecontroller: capture job is running")
	ErrSucceeded     = errors.New("capturecontroller: capture job succeeded")
	ErrFailed        = errors.New("capturecontroller: capture job failed")
	ErrForeign       = errors.New("capturecontroller: capture resource or pod is foreign")
	ErrConflict      = errors.New("capturecontroller: capture resource or pod conflicts")
	ErrLogOutput     = errors.New("capturecontroller: capture log output is unavailable")
	ErrArtifactSink  = errors.New("capturecontroller: artifact sink is unavailable")
)

// ErrorClass identifies a bounded, controller-actionable observation result.
type ErrorClass string

const (
	ClassMissing   ErrorClass = "missing"
	ClassRunning   ErrorClass = "running"
	ClassSucceeded ErrorClass = "succeeded"
	ClassFailed    ErrorClass = "failed"
	ClassForeign   ErrorClass = "foreign"
	ClassConflict  ErrorClass = "conflict"
)

// ClassifiedError preserves the lifecycle classification while retaining a
// short diagnostic. errors.Is works with the exported sentinel for Class.
type ClassifiedError struct {
	Class    ErrorClass
	Resource string
	Message  string
	Cause    error
}

func (e *ClassifiedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := string(e.Class)
	if e.Resource != "" {
		base += " " + e.Resource
	}
	if e.Message != "" {
		base += ": " + e.Message
	}
	return "capturecontroller: " + base
}

func (e *ClassifiedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *ClassifiedError) Is(target error) bool {
	if e == nil {
		return false
	}
	return target == sentinelFor(e.Class)
}

// ClassOf returns the lifecycle class carried by err, if any.
func ClassOf(err error) (ErrorClass, bool) {
	var classified *ClassifiedError
	if errors.As(err, &classified) {
		return classified.Class, true
	}
	switch {
	case errors.Is(err, ErrMissing):
		return ClassMissing, true
	case errors.Is(err, ErrRunning):
		return ClassRunning, true
	case errors.Is(err, ErrSucceeded):
		return ClassSucceeded, true
	case errors.Is(err, ErrFailed):
		return ClassFailed, true
	case errors.Is(err, ErrForeign):
		return ClassForeign, true
	case errors.Is(err, ErrConflict):
		return ClassConflict, true
	default:
		return "", false
	}
}

func sentinelFor(class ErrorClass) error {
	switch class {
	case ClassMissing:
		return ErrMissing
	case ClassRunning:
		return ErrRunning
	case ClassSucceeded:
		return ErrSucceeded
	case ClassFailed:
		return ErrFailed
	case ClassForeign:
		return ErrForeign
	case ClassConflict:
		return ErrConflict
	default:
		return nil
	}
}

func classified(class ErrorClass, resource, format string, args ...any) error {
	return &ClassifiedError{Class: class, Resource: resource, Message: fmt.Sprintf(format, args...)}
}

// LogReader is the only log access required by the driver. Implementations
// should stream from the Kubernetes pod-log endpoint and stop after maxBytes.
// The driver supplies the fixed capture container name; callers cannot choose
// a different container or execute a command in the pod.
type LogReader interface {
	ReadCaptureLogs(context.Context, string, string, string, int64) ([]byte, error)
}

// LogReaderFunc adapts a function to LogReader and is useful for controller
// tests and for a client-go pod-log implementation in the operator package.
type LogReaderFunc func(context.Context, string, string, string, int64) ([]byte, error)

func (f LogReaderFunc) ReadCaptureLogs(ctx context.Context, namespace, podName, containerName string, maxBytes int64) ([]byte, error) {
	return f(ctx, namespace, podName, containerName, maxBytes)
}

// Options configures the controller seams. MaxLogBytes can only tighten the
// capture package's hard frame ceiling. A nil ArtifactSink is allowed for
// callers that only need Ensure/Observe/ReadAndValidate; Capture requires one.
type Options struct {
	Logs        LogReader
	Artifacts   capture.ArtifactSink
	MaxLogBytes int64
}

// Driver is a Kubernetes-backed capture-phase driver.
type Driver struct {
	client      client.Client
	logs        LogReader
	artifacts   capture.ArtifactSink
	maxLogBytes int64
}

// New constructs a driver. The controller-runtime client is used only for
// Jobs, Pods, and NetworkPolicies. Pod logs stay behind LogReader because
// controller-runtime intentionally does not expose a logs subresource client.
func New(c client.Client, options Options) (*Driver, error) {
	if c == nil || options.Logs == nil {
		return nil, ErrInvalidDriver
	}
	max := options.MaxLogBytes
	if max == 0 {
		max = capture.MaxEncodedFrameBytes(capture.HardMaxResultBytes, capture.HardMaxPatchBytes, capture.HardMaxManifestBytes)
	}
	hard := capture.MaxEncodedFrameBytes(capture.HardMaxResultBytes, capture.HardMaxPatchBytes, capture.HardMaxManifestBytes)
	if max <= 0 || hard <= 0 || max > hard {
		return nil, fmt.Errorf("%w: MaxLogBytes must be in (0,%d]", ErrInvalidDriver, hard)
	}
	return &Driver{client: c, logs: options.Logs, artifacts: options.Artifacts, maxLogBytes: max}, nil
}

// EnsureResult contains the fresh objects returned by Ensure. The Job object
// is important to observation because its UID is the only safe pod ownership
// key. It is not safe to infer ownership from labels alone.
type EnsureResult struct {
	Job           *batchv1.Job
	NetworkPolicy *networkingv1.NetworkPolicy
}

// Observation is a bounded projection of the capture Job and its one owned
// Pod. Observe returns the matching classified lifecycle error for every
// state, including ErrSucceeded; this lets a reconciler switch on errors.Is
// without trusting unstructured Job status fields.
type Observation struct {
	State    ErrorClass
	Job      *batchv1.Job
	Pod      *corev1.Pod
	PodCount int
}

// CaptureResult contains the validated output and both controller-persisted
// immutable artifact references. Artifact remains the patch reference for
// compatibility with the current AgentRun controller boundary.
type CaptureResult struct {
	Observation      Observation
	Validated        capture.ValidatedOutput
	Artifact         v1alpha1.ArtifactRef
	ManifestArtifact v1alpha1.ArtifactRef
}

// Ensure creates the deterministic Job and deny-all NetworkPolicy, or gets
// and validates each existing object. A create AlreadyExists race is followed
// by a fresh GET and full validation; it is never treated as success by itself.
func (d *Driver) Ensure(ctx context.Context, plan capture.Plan) (EnsureResult, error) {
	contract, err := validatePlan(plan)
	if err != nil {
		return EnsureResult{}, err
	}
	job, err := d.ensureJob(ctx, plan.Job, contract)
	if err != nil {
		return EnsureResult{}, err
	}
	policy, err := d.ensureNetworkPolicy(ctx, plan.NetworkPolicy, contract)
	if err != nil {
		return EnsureResult{}, err
	}
	return EnsureResult{Job: job, NetworkPolicy: policy}, nil
}

// Observe validates the Job and NetworkPolicy contract, then lists Pods using
// the Job's fixed selector. It accepts exactly one Pod whose controller owner
// is the current Job UID. Missing, running, succeeded, failed, foreign, and
// conflict outcomes are all typed through ClassifiedError.
func (d *Driver) Observe(ctx context.Context, plan capture.Plan) (Observation, error) {
	contract, err := validatePlan(plan)
	if err != nil {
		return Observation{}, err
	}
	var job batchv1.Job
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: contract.namespace, Name: contract.jobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			return Observation{State: ClassMissing}, classified(ClassMissing, "Job", "capture Job %s/%s does not exist", contract.namespace, contract.jobName)
		}
		return Observation{}, fmt.Errorf("get capture Job %s/%s: %w", contract.namespace, contract.jobName, err)
	}
	if err := validateExistingJob(&job, plan.Job, contract); err != nil {
		return Observation{}, err
	}
	var policy networkingv1.NetworkPolicy
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: contract.namespace, Name: contract.policyName}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return Observation{State: ClassMissing, Job: job.DeepCopy()}, classified(ClassMissing, "NetworkPolicy", "capture deny-all NetworkPolicy %s/%s does not exist", contract.namespace, contract.policyName)
		}
		return Observation{}, fmt.Errorf("get capture NetworkPolicy %s/%s: %w", contract.namespace, contract.policyName, err)
	}
	if err := validateExistingNetworkPolicy(&policy, plan.NetworkPolicy, contract); err != nil {
		return Observation{}, err
	}

	if job.UID == "" {
		return Observation{}, classified(ClassConflict, "Job", "capture Job has no UID; pod ownership cannot be proven")
	}
	selector, err := fixedSelector(job.Spec.Selector)
	if err != nil {
		return Observation{}, err
	}
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods, client.InNamespace(contract.namespace), client.MatchingLabels(selector)); err != nil {
		return Observation{}, fmt.Errorf("list capture Pods for Job %s/%s: %w", contract.namespace, contract.jobName, err)
	}
	owned := make([]*corev1.Pod, 0, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		if podOwnedByJob(pod, &job) {
			owned = append(owned, pod)
		}
	}
	if len(owned) == 0 {
		if len(pods.Items) > 0 {
			return Observation{State: ClassForeign, Job: job.DeepCopy(), PodCount: len(pods.Items)}, classified(ClassForeign, "Pod", "no pod matching the fixed selector is controlled by Job UID %q", job.UID)
		}
		return Observation{State: ClassMissing, Job: job.DeepCopy()}, classified(ClassMissing, "Pod", "capture Job has no owned Pod")
	}
	if len(owned) != 1 || len(pods.Items) != 1 {
		return Observation{State: ClassConflict, Job: job.DeepCopy(), PodCount: len(pods.Items)}, classified(ClassConflict, "Pod", "expected exactly one fixed-selector owned Pod, found %d owned and %d total", len(owned), len(pods.Items))
	}

	state, err := jobState(&job)
	if err != nil {
		return Observation{}, err
	}
	observation := Observation{State: state, Job: job.DeepCopy(), Pod: owned[0].DeepCopy(), PodCount: 1}
	return observation, classified(state, "Job", "capture Job %s/%s is %s", contract.namespace, contract.jobName, state)
}

// ReadAndValidate observes a succeeded Job and reads only its fixed capture
// container through the bounded LogReader seam. It does not persist anything.
func (d *Driver) ReadAndValidate(ctx context.Context, plan capture.Plan, snapshot resolved.Snapshot, baseSHA string) (capture.ValidatedOutput, Observation, error) {
	observation, err := d.Observe(ctx, plan)
	if err != nil && !errors.Is(err, ErrSucceeded) {
		return capture.ValidatedOutput{}, observation, err
	}
	if observation.State != ClassSucceeded || observation.Pod == nil {
		return capture.ValidatedOutput{}, observation, classified(observation.State, "Job", "capture output is available only after Job success")
	}
	if plan.Output.MaxFrameBytes <= 0 || plan.Output.MaxFrameBytes > d.maxLogBytes {
		return capture.ValidatedOutput{}, observation, fmt.Errorf("%w: plan log bound %d exceeds driver bound %d", ErrLogOutput, plan.Output.MaxFrameBytes, d.maxLogBytes)
	}
	reader := fixedOutputReader{logs: d.logs, maxBytes: d.maxLogBytes}
	limits := capture.ValidationLimits{MaxResultBytes: plan.Output.MaxResultBytes, MaxPatchBytes: plan.Output.MaxPatchBytes, MaxManifestBytes: plan.Output.MaxManifestBytes}
	validated, err := capture.ReadAndValidate(ctx, reader, snapshot, baseSHA, observation.Pod.Namespace, observation.Pod.Name, limits)
	if err != nil {
		return capture.ValidatedOutput{}, observation, err
	}
	return validated, observation, nil
}

// Persist delegates to capture.Persist. The sink is controller-owned and is
// never placed in the Job or Pod.
func (d *Driver) Persist(ctx context.Context, output capture.ValidatedOutput) (capture.PersistedArtifacts, error) {
	if d.artifacts == nil {
		return capture.PersistedArtifacts{}, ErrArtifactSink
	}
	return capture.Persist(ctx, d.artifacts, output)
}

// Capture observes, validates, and persists one successful capture output.
func (d *Driver) Capture(ctx context.Context, plan capture.Plan, snapshot resolved.Snapshot, baseSHA string) (CaptureResult, error) {
	validated, observation, err := d.ReadAndValidate(ctx, plan, snapshot, baseSHA)
	if err != nil {
		return CaptureResult{Observation: observation}, err
	}
	artifacts, err := d.Persist(ctx, validated)
	if err != nil {
		return CaptureResult{Observation: observation, Validated: validated}, err
	}
	validated, err = validated.WithManifestArtifact(artifacts.Manifest)
	if err != nil {
		return CaptureResult{Observation: observation, Validated: validated, Artifact: artifacts.Patch, ManifestArtifact: artifacts.Manifest}, err
	}
	return CaptureResult{Observation: observation, Validated: validated, Artifact: artifacts.Patch, ManifestArtifact: artifacts.Manifest}, nil
}

// Cleanup validates both resources before deleting either one. It is
// idempotent for missing resources and refuses to delete a foreign or
// conflicting object. UID preconditions prevent deleting a replacement that
// raced with the validation GET.
func (d *Driver) Cleanup(ctx context.Context, plan capture.Plan) error {
	contract, err := validatePlan(plan)
	if err != nil {
		return err
	}
	var job batchv1.Job
	jobFound := true
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: contract.namespace, Name: contract.jobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			jobFound = false
		} else {
			return fmt.Errorf("get capture Job for cleanup %s/%s: %w", contract.namespace, contract.jobName, err)
		}
	} else if err := validateExistingJob(&job, plan.Job, contract); err != nil {
		return err
	}
	var policy networkingv1.NetworkPolicy
	policyFound := true
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: contract.namespace, Name: contract.policyName}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			policyFound = false
		} else {
			return fmt.Errorf("get capture NetworkPolicy for cleanup %s/%s: %w", contract.namespace, contract.policyName, err)
		}
	} else if err := validateExistingNetworkPolicy(&policy, plan.NetworkPolicy, contract); err != nil {
		return err
	}

	if policyFound {
		if err := d.deleteObject(ctx, &policy); err != nil {
			return fmt.Errorf("delete capture NetworkPolicy %s/%s: %w", policy.Namespace, policy.Name, err)
		}
	}
	if jobFound {
		if err := d.deleteObject(ctx, &job); err != nil {
			return fmt.Errorf("delete capture Job %s/%s: %w", job.Namespace, job.Name, err)
		}
	}
	return nil
}

type fixedOutputReader struct {
	logs     LogReader
	maxBytes int64
}

func (r fixedOutputReader) ReadCaptureOutput(ctx context.Context, namespace, podName string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > r.maxBytes {
		return nil, fmt.Errorf("%w: requested log bound %d is outside driver bound %d", ErrLogOutput, maxBytes, r.maxBytes)
	}
	output, err := r.logs.ReadCaptureLogs(ctx, namespace, podName, CaptureContainer, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: read container %q logs: %v", ErrLogOutput, CaptureContainer, err)
	}
	if int64(len(output)) > maxBytes {
		return nil, fmt.Errorf("%w: container %q returned %d bytes, cap is %d", ErrLogOutput, CaptureContainer, len(output), maxBytes)
	}
	return output, nil
}

type planContract struct {
	namespace   string
	jobName     string
	policyName  string
	runUID      types.UID
	specDigest  string
	inputDigest string
	owner       metav1.OwnerReference
	selector    map[string]string
}

func validatePlan(plan capture.Plan) (planContract, error) {
	var contract planContract
	if plan.Job == nil || plan.NetworkPolicy == nil {
		return contract, fmt.Errorf("%w: Job and NetworkPolicy are required", ErrInvalidPlan)
	}
	job := plan.Job
	policy := plan.NetworkPolicy
	if job.Namespace == "" || policy.Namespace != job.Namespace || job.Name == "" || policy.Name == "" {
		return contract, fmt.Errorf("%w: resources must have matching namespace and names", ErrInvalidPlan)
	}
	if plan.JobName == "" || job.Name != plan.JobName || policy.Name != plan.JobName+"-deny" {
		return contract, fmt.Errorf("%w: resource names are not the deterministic capture names", ErrInvalidPlan)
	}
	if !canonical.ValidDigest(plan.SpecDigest) {
		return contract, fmt.Errorf("%w: plan spec digest is invalid", ErrInvalidPlan)
	}
	owner, err := singleAgentRunOwner(job.OwnerReferences)
	if err != nil {
		return contract, err
	}
	deterministicName, err := capture.JobName(string(owner.UID))
	if err != nil || plan.JobName != deterministicName {
		return contract, fmt.Errorf("%w: Job name is not deterministic for AgentRun UID %q", ErrInvalidPlan, owner.UID)
	}
	if job.Labels == nil || job.Labels[RunUIDLabelKey] == "" || types.UID(job.Labels[RunUIDLabelKey]) != owner.UID {
		return contract, fmt.Errorf("%w: Job run UID label does not match its AgentRun owner", ErrInvalidPlan)
	}
	if job.Labels[RoleLabelKey] != capture.Role || job.Labels[EgressLabelKey] != EgressNone {
		return contract, fmt.Errorf("%w: Job role or egress label is unsafe", ErrInvalidPlan)
	}
	if job.Labels[SpecDigestLabel] == "" || job.Labels[SpecDigestLabel] != shortDigest(plan.SpecDigest) {
		return contract, fmt.Errorf("%w: Job spec digest label is not bound to the plan", ErrInvalidPlan)
	}
	annotations := job.Annotations
	if annotations == nil || annotations[SpecDigest] != plan.SpecDigest || !canonical.ValidDigest(annotations[InputDigest]) {
		return contract, fmt.Errorf("%w: Job spec/input digest annotations are invalid", ErrInvalidPlan)
	}
	if !sameStringMap(job.Labels, policy.Labels) || !sameStringMap(annotations, policy.Annotations) {
		return contract, fmt.Errorf("%w: Job and NetworkPolicy metadata contracts differ", ErrInvalidPlan)
	}
	if !sameOwner(policy.OwnerReferences, owner) {
		return contract, fmt.Errorf("%w: NetworkPolicy owner is not the same AgentRun controller", ErrInvalidPlan)
	}
	if err := validateSelector(job.Spec.Selector, job.Labels); err != nil {
		return contract, err
	}
	if !sameStringMap(job.Spec.Template.Labels, job.Labels) || !sameStringMap(job.Spec.Template.Annotations, annotations) {
		return contract, fmt.Errorf("%w: capture Pod template metadata is not fixed to the plan", ErrInvalidPlan)
	}
	if plan.Output.Protocol != capture.OutputProtocol || plan.Output.ResultPath != capture.ResultPath || plan.Output.PatchPath != capture.PatchPath || plan.Output.ManifestPath != capture.ManifestPath || plan.Output.MaxFrameBytes <= 0 || plan.Output.MaxFrameBytes != capture.MaxEncodedFrameBytes(plan.Output.MaxResultBytes, plan.Output.MaxPatchBytes, plan.Output.MaxManifestBytes) {
		return contract, fmt.Errorf("%w: capture output contract is not bounded or does not match the frame protocol", ErrInvalidPlan)
	}
	if err := validateJobShape(job, plan.Job); err != nil {
		return contract, err
	}
	if err := validateNetworkPolicyShape(policy, plan.NetworkPolicy); err != nil {
		return contract, err
	}
	if !sameStringMap(policy.Spec.PodSelector.MatchLabels, job.Spec.Selector.MatchLabels) {
		return contract, fmt.Errorf("%w: deny-all NetworkPolicy selector does not cover the capture Job", ErrInvalidPlan)
	}
	contract = planContract{
		namespace: job.Namespace, jobName: job.Name, policyName: policy.Name,
		runUID: owner.UID, specDigest: plan.SpecDigest, inputDigest: annotations[InputDigest],
		owner: owner, selector: copyStringMap(job.Spec.Selector.MatchLabels),
	}
	return contract, nil
}

func (d *Driver) ensureJob(ctx context.Context, expected *batchv1.Job, contract planContract) (*batchv1.Job, error) {
	key := client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name}
	var current batchv1.Job
	if err := d.client.Get(ctx, key, &current); err == nil {
		if err := validateExistingJob(&current, expected, contract); err != nil {
			return nil, err
		}
		return current.DeepCopy(), nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get capture Job %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := d.client.Create(ctx, expected.DeepCopy()); err == nil {
		// Always GET after create so the returned UID and server defaults are
		// the same values that Observe will later validate.
		if err := d.client.Get(ctx, key, &current); err != nil {
			return nil, fmt.Errorf("get capture Job after create %s/%s: %w", key.Namespace, key.Name, err)
		}
		if err := validateExistingJob(&current, expected, contract); err != nil {
			return nil, err
		}
		return current.DeepCopy(), nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create capture Job %s/%s: %w", key.Namespace, key.Name, err)
	}
	current = batchv1.Job{}
	if err := d.client.Get(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, classified(ClassConflict, "Job", "Job was reported AlreadyExists but fresh validation GET found no object")
		}
		return nil, fmt.Errorf("get capture Job after AlreadyExists %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := validateExistingJob(&current, expected, contract); err != nil {
		return nil, err
	}
	return current.DeepCopy(), nil
}

func (d *Driver) ensureNetworkPolicy(ctx context.Context, expected *networkingv1.NetworkPolicy, contract planContract) (*networkingv1.NetworkPolicy, error) {
	key := client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name}
	var current networkingv1.NetworkPolicy
	if err := d.client.Get(ctx, key, &current); err == nil {
		if err := validateExistingNetworkPolicy(&current, expected, contract); err != nil {
			return nil, err
		}
		return current.DeepCopy(), nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get capture NetworkPolicy %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := d.client.Create(ctx, expected.DeepCopy()); err == nil {
		if err := d.client.Get(ctx, key, &current); err != nil {
			return nil, fmt.Errorf("get capture NetworkPolicy after create %s/%s: %w", key.Namespace, key.Name, err)
		}
		if err := validateExistingNetworkPolicy(&current, expected, contract); err != nil {
			return nil, err
		}
		return current.DeepCopy(), nil
	} else if !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create capture NetworkPolicy %s/%s: %w", key.Namespace, key.Name, err)
	}
	current = networkingv1.NetworkPolicy{}
	if err := d.client.Get(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, classified(ClassConflict, "NetworkPolicy", "NetworkPolicy was reported AlreadyExists but fresh validation GET found no object")
		}
		return nil, fmt.Errorf("get capture NetworkPolicy after AlreadyExists %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := validateExistingNetworkPolicy(&current, expected, contract); err != nil {
		return nil, err
	}
	return current.DeepCopy(), nil
}

func validateExistingJob(current, expected *batchv1.Job, contract planContract) error {
	if current.Namespace != expected.Namespace || current.Name != expected.Name {
		return classified(ClassForeign, "Job", "object identity is %s/%s, expected %s/%s", current.Namespace, current.Name, expected.Namespace, expected.Name)
	}
	if !sameOwner(current.OwnerReferences, contract.owner) {
		return classified(ClassForeign, "Job", "Job is not controlled by AgentRun UID %q", contract.runUID)
	}
	if !sameStringMap(current.Labels, expected.Labels) {
		return classified(ClassConflict, "Job", "Job labels differ from the immutable plan")
	}
	if !sameStringMap(current.Annotations, expected.Annotations) {
		return classified(ClassConflict, "Job", "Job spec/input digest annotations differ from the immutable plan")
	}
	if err := validateJobShape(current, expected); err != nil {
		return err
	}
	return nil
}

func validateExistingNetworkPolicy(current, expected *networkingv1.NetworkPolicy, contract planContract) error {
	if current.Namespace != expected.Namespace || current.Name != expected.Name {
		return classified(ClassForeign, "NetworkPolicy", "object identity is %s/%s, expected %s/%s", current.Namespace, current.Name, expected.Namespace, expected.Name)
	}
	if !sameOwner(current.OwnerReferences, contract.owner) {
		return classified(ClassForeign, "NetworkPolicy", "NetworkPolicy is not controlled by AgentRun UID %q", contract.runUID)
	}
	if !sameStringMap(current.Labels, expected.Labels) || !sameStringMap(current.Annotations, expected.Annotations) {
		return classified(ClassConflict, "NetworkPolicy", "NetworkPolicy metadata differs from the immutable plan")
	}
	if err := validateNetworkPolicyShape(current, expected); err != nil {
		return err
	}
	return nil
}

func validateJobShape(current, expected *batchv1.Job) error {
	if current.Spec.Parallelism == nil || *current.Spec.Parallelism != 1 || current.Spec.Completions == nil || *current.Spec.Completions != 1 || current.Spec.BackoffLimit == nil || *current.Spec.BackoffLimit != 0 {
		return classified(ClassConflict, "Job", "parallelism, completions, and backoff policy changed")
	}
	if current.Spec.ActiveDeadlineSeconds == nil || expected.Spec.ActiveDeadlineSeconds == nil || *current.Spec.ActiveDeadlineSeconds != *expected.Spec.ActiveDeadlineSeconds {
		return classified(ClassConflict, "Job", "active deadline changed")
	}
	if current.Spec.TTLSecondsAfterFinished != nil || current.Spec.ManualSelector == nil || !*current.Spec.ManualSelector {
		return classified(ClassConflict, "Job", "Job retry/selector/retention policy is unsafe")
	}
	if err := validateSelector(current.Spec.Selector, expected.Labels); err != nil {
		return err
	}
	if !sameStringMap(current.Spec.Template.Labels, expected.Spec.Template.Labels) || !sameStringMap(current.Spec.Template.Annotations, expected.Spec.Template.Annotations) {
		return classified(ClassConflict, "Job", "Pod template metadata changed")
	}
	if err := validateCapturePod(current.Spec.Template.Spec, expected.Spec.Template.Spec); err != nil {
		return err
	}
	return nil
}

func validateCapturePod(current, expected corev1.PodSpec) error {
	if current.HostUsers == nil || *current.HostUsers || current.HostNetwork || current.HostPID || current.HostIPC || current.AutomountServiceAccountToken == nil || *current.AutomountServiceAccountToken || current.ServiceAccountName != "" {
		return classified(ClassConflict, "Job", "capture pod host or service-account isolation changed")
	}
	if current.RestartPolicy != corev1.RestartPolicyNever || current.EnableServiceLinks == nil || *current.EnableServiceLinks || current.DNSPolicy != corev1.DNSNone {
		return classified(ClassConflict, "Job", "capture pod lifecycle or DNS policy changed")
	}
	if current.SecurityContext == nil || current.SecurityContext.RunAsNonRoot == nil || !*current.SecurityContext.RunAsNonRoot || current.SecurityContext.SeccompProfile == nil || current.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		return classified(ClassConflict, "Job", "capture pod security context changed")
	}
	if len(current.Volumes) != 1 || current.Volumes[0].Name != capture.WorkspaceVolumeName || current.Volumes[0].PersistentVolumeClaim == nil || current.Volumes[0].PersistentVolumeClaim.ClaimName == "" {
		return classified(ClassConflict, "Job", "capture pod must mount exactly one workspace PVC")
	}
	if len(current.InitContainers) != 0 || len(current.EphemeralContainers) != 0 || len(current.Containers) != 1 || current.Containers[0].Name != CaptureContainer {
		return classified(ClassConflict, "Job", "capture pod container topology changed")
	}
	container := current.Containers[0]
	if !resolved.ValidPinnedImage(container.Image) || !reflectStringSlice(container.Command, []string{capture.CaptureEntrypoint}) || container.WorkingDir != capture.RepoPath || len(container.VolumeMounts) != 1 || container.VolumeMounts[0].Name != capture.WorkspaceVolumeName || container.VolumeMounts[0].MountPath != capture.WorkspaceMountPath || container.VolumeMounts[0].ReadOnly {
		return classified(ClassConflict, "Job", "capture container execution contract changed")
	}
	security := container.SecurityContext
	if security == nil || security.RunAsUser == nil || *security.RunAsUser != 1000 || security.RunAsGroup == nil || *security.RunAsGroup != 1000 || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Privileged == nil || *security.Privileged || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || security.Capabilities == nil || len(security.Capabilities.Add) != 0 || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != corev1.Capability("ALL") {
		return classified(ClassConflict, "Job", "capture container security context changed")
	}
	if err := rejectSecrets(current); err != nil {
		return err
	}
	for _, volume := range current.Volumes {
		if volume.HostPath != nil {
			return classified(ClassConflict, "Job", "capture pod may not mount hostPath")
		}
	}
	if !samePodSpec(current, expected) {
		return classified(ClassConflict, "Job", "capture pod execution contract changed")
	}
	return nil
}

func rejectSecrets(pod corev1.PodSpec) error {
	if len(pod.ImagePullSecrets) != 0 {
		return classified(ClassConflict, "Job", "capture pod may not use imagePullSecrets")
	}
	for _, volume := range pod.Volumes {
		if volume.Secret != nil || volume.Projected != nil {
			return classified(ClassConflict, "Job", "capture pod contains a Secret or projected volume")
		}
	}
	for _, container := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		if len(container.EnvFrom) != 0 {
			return classified(ClassConflict, "Job", "capture pod contains EnvFrom credentials")
		}
		for _, env := range container.Env {
			if env.ValueFrom != nil || looksLikeCredentialName(env.Name) {
				return classified(ClassConflict, "Job", "capture pod contains a projected or credential-like environment variable")
			}
		}
	}
	return nil
}

func looksLikeCredentialName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"AWS_ACCESS_KEY", "AWS_SECRET", "AWS_SESSION", "AWS_WEB_IDENTITY", "ARTIFACT", "OBJECT_STORE", "S3_", "R2_", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func samePodSpec(current, expected corev1.PodSpec) bool {
	// The API server may add these harmless defaults after creation. They are
	// normalized only after rejecting a non-default override.
	if current.SchedulerName != "" && current.SchedulerName != expected.SchedulerName && current.SchedulerName != "default-scheduler" {
		return false
	}
	if current.PreemptionPolicy != nil && expected.PreemptionPolicy == nil && string(*current.PreemptionPolicy) != string(corev1.PreemptLowerPriority) {
		return false
	}
	current.SchedulerName = expected.SchedulerName
	current.PreemptionPolicy = expected.PreemptionPolicy
	if current.SchedulerName == "" {
		current.SchedulerName = expected.SchedulerName
	}
	for index := range current.Containers {
		if index >= len(expected.Containers) {
			return false
		}
		if current.Containers[index].TerminationMessagePath == "/dev/termination-log" && expected.Containers[index].TerminationMessagePath == "" {
			current.Containers[index].TerminationMessagePath = ""
		}
		if current.Containers[index].TerminationMessagePolicy == corev1.TerminationMessageReadFile && expected.Containers[index].TerminationMessagePolicy == "" {
			current.Containers[index].TerminationMessagePolicy = ""
		}
	}
	return apiequality.Semantic.DeepEqual(current, expected)
}

func validateNetworkPolicyShape(current, expected *networkingv1.NetworkPolicy) error {
	if !sameStringMap(current.Spec.PodSelector.MatchLabels, expected.Spec.PodSelector.MatchLabels) || len(current.Spec.PodSelector.MatchExpressions) != 0 || len(expected.Spec.PodSelector.MatchExpressions) != 0 {
		return classified(ClassConflict, "NetworkPolicy", "NetworkPolicy selector changed")
	}
	if !samePolicyTypes(current.Spec.PolicyTypes, expected.Spec.PolicyTypes) || !denyAllPolicyTypes(current.Spec.PolicyTypes) || len(current.Spec.Ingress) != 0 || len(current.Spec.Egress) != 0 {
		return classified(ClassConflict, "NetworkPolicy", "NetworkPolicy is not an explicit deny-all policy")
	}
	return nil
}

func denyAllPolicyTypes(policyTypes []networkingv1.PolicyType) bool {
	return len(policyTypes) == 2 && ((policyTypes[0] == networkingv1.PolicyTypeIngress && policyTypes[1] == networkingv1.PolicyTypeEgress) || (policyTypes[0] == networkingv1.PolicyTypeEgress && policyTypes[1] == networkingv1.PolicyTypeIngress))
}

func validateSelector(selector *metav1.LabelSelector, expected map[string]string) error {
	if selector == nil || len(selector.MatchExpressions) != 0 || !sameStringMap(selector.MatchLabels, expected) {
		return fmt.Errorf("%w: fixed capture selector differs from resource labels", ErrInvalidPlan)
	}
	return nil
}

func fixedSelector(selector *metav1.LabelSelector) (map[string]string, error) {
	if selector == nil || len(selector.MatchExpressions) != 0 || len(selector.MatchLabels) == 0 {
		return nil, fmt.Errorf("%w: capture Job selector must be fixed MatchLabels", ErrInvalidPlan)
	}
	if _, err := metav1.LabelSelectorAsSelector(selector); err != nil {
		return nil, fmt.Errorf("%w: invalid capture selector: %v", ErrInvalidPlan, err)
	}
	return copyStringMap(selector.MatchLabels), nil
}

func jobState(job *batchv1.Job) (ErrorClass, error) {
	complete, failed := false, false
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
		}
	}
	if complete && failed {
		return "", classified(ClassConflict, "Job", "Job has both Complete=True and Failed=True")
	}
	if complete {
		return ClassSucceeded, nil
	}
	if failed {
		return ClassFailed, nil
	}
	return ClassRunning, nil
}

func podOwnedByJob(pod *corev1.Pod, job *batchv1.Job) bool {
	if pod.Namespace != job.Namespace || pod.Labels == nil || job.Spec.Selector == nil {
		return false
	}
	for key, value := range job.Spec.Selector.MatchLabels {
		if pod.Labels[key] != value {
			return false
		}
	}
	for _, owner := range pod.OwnerReferences {
		if owner.APIVersion == JobOwnerAPIVer && owner.Kind == JobOwnerKind && owner.Name == job.Name && owner.UID == job.UID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func singleAgentRunOwner(owners []metav1.OwnerReference) (metav1.OwnerReference, error) {
	if len(owners) != 1 {
		return metav1.OwnerReference{}, fmt.Errorf("%w: capture resource must have exactly one AgentRun controller owner", ErrInvalidPlan)
	}
	owner := owners[0]
	if owner.APIVersion != OwnerAPIVersion || owner.Kind != OwnerKind || owner.Name == "" || owner.UID == "" || owner.Controller == nil || !*owner.Controller {
		return metav1.OwnerReference{}, fmt.Errorf("%w: capture resource owner is not a valid AgentRun controller reference", ErrInvalidPlan)
	}
	return owner, nil
}

func sameOwner(current []metav1.OwnerReference, expected metav1.OwnerReference) bool {
	if len(current) != 1 {
		return false
	}
	owner := current[0]
	return owner.APIVersion == expected.APIVersion && owner.Kind == expected.Kind && owner.Name == expected.Name && owner.UID == expected.UID && owner.Controller != nil && *owner.Controller && expected.Controller != nil && *expected.Controller
}

func samePolicyTypes(left, right []networkingv1.PolicyType) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[networkingv1.PolicyType]bool, len(left))
	for _, value := range left {
		if seen[value] {
			return false
		}
		seen[value] = true
	}
	for _, value := range right {
		if !seen[value] {
			return false
		}
	}
	return true
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func reflectStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func copyStringMap(input map[string]string) map[string]string {
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func shortDigest(value string) string {
	value = strings.TrimPrefix(value, canonical.DigestPrefix)
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func (d *Driver) deleteObject(ctx context.Context, object client.Object) error {
	options := &client.DeleteOptions{}
	if uid := object.GetUID(); uid != "" {
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := d.client.Delete(ctx, object, options); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsConflict(err) {
			return classified(ClassConflict, object.GetObjectKind().GroupVersionKind().Kind, "resource changed after validation")
		}
		return err
	}
	return nil
}

var _ client.Object = (*batchv1.Job)(nil)
var _ client.Object = (*networkingv1.NetworkPolicy)(nil)
