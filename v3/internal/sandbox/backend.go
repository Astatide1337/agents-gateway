// Package sandbox contains the Kubernetes-backed execution sandbox seam.
//
// The backend deliberately does not build credentials, clone configuration, or
// tool policy. The operator assembles a complete, hardened upstream Sandbox
// object and hands it to this package. This package owns child identity,
// ownership, idempotency, and validation of the small set of invariants that
// must never be relaxed.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	DefaultMaxShutdownDuration = 24 * time.Hour
	MaxObservedConditions      = 16

	SpecDigestLabelKey      = "agents.astatide.com/spec-digest"
	SpecDigestAnnotationKey = "agents.astatide.com/spec-digest"
	// SandboxSpecFingerprintAnnotationKey is an integrity check for the
	// controller-owned Sandbox spec. It is never trusted as the sole proof of
	// idempotency: the actual normalized spec and this annotation must both
	// match the controller-computed fingerprint persisted in AgentRun status.
	SandboxSpecFingerprintAnnotationKey = "agents.astatide.com/sandbox-spec-fingerprint"
	RunUIDLabelKey                      = "agents.astatide.com/run-uid"
	RoleLabelKey                        = "agents.astatide.com/role"

	ownerAPIVersion = "agents.astatide.com/v1alpha1"
	ownerKind       = "AgentRun"

	// ChildKindSandbox is the upstream agent-sandbox child kind.
	ChildKindSandbox = "Sandbox"
	// ChildKindJob is the built-in batch fallback child kind.
	ChildKindJob = "Job"
)

var (
	ErrInvalidPlan            = errors.New("invalid sandbox plan")
	ErrInvalidSecurityPosture = errors.New("invalid sandbox security posture")
	ErrForeignChild           = errors.New("sandbox child is foreign")
	ErrSpecDigestConflict     = errors.New("sandbox spec digest conflict")
	ErrSandboxSpecConflict    = errors.New("sandbox controller-owned spec conflict")
	ErrReferenceConflict      = errors.New("sandbox reference conflict")
	ErrObservationTooLarge    = errors.New("sandbox observation exceeds bound")
)

// BackendKind selects the Kubernetes child resource used for execution.
// Keeping this value in the sandbox package makes the controller, status, and
// CLI agree on one finite set of backends instead of passing free-form kinds.
type BackendKind string

const (
	BackendAgentSandbox BackendKind = "agent-sandbox"
	BackendJob          BackendKind = "job"
)

func (k BackendKind) Valid() bool { return k == BackendAgentSandbox || k == BackendJob }

func ParseBackendKind(value string) (BackendKind, error) {
	kind := BackendKind(value)
	if !kind.Valid() {
		return "", fmt.Errorf("unsupported sandbox backend %q (want agent-sandbox or job)", value)
	}
	return kind, nil
}

func (k BackendKind) ChildKind() string {
	switch k {
	case BackendAgentSandbox:
		return ChildKindSandbox
	case BackendJob:
		return ChildKindJob
	default:
		return ""
	}
}

// ValidChildKind is used at controller/CLI boundaries where the child kind is
// carried in status and must remain one of the two supported backend kinds.
func ValidChildKind(kind string) bool { return validChildKind(kind) }

// Role identifies the two v3 child sandboxes.
type Role string

const (
	RoleWork   Role = "work"
	RoleVerify Role = "verify"
)

func (r Role) valid() bool { return r == RoleWork || r == RoleVerify }

// SandboxPlan is a complete upstream Sandbox request. The caller owns the
// pod template, volumes, containers, and credential references; this package
// does not invent any of them.
type SandboxPlan struct {
	Owner      *v1alpha1.AgentRun
	Role       Role
	SpecDigest string
	Sandbox    *sandboxv1beta1.Sandbox
}

// SandboxRef is a stable, ownership-bound child handle.
type SandboxRef struct {
	Namespace       string
	Name            string
	Kind            string
	UID             types.UID
	OwnerUID        types.UID
	Role            Role
	SpecDigest      string
	PlanFingerprint string
}

// ConditionObservation is a bounded projection that omits unbounded messages.
type ConditionObservation struct {
	Type               string
	Status             metav1.ConditionStatus
	Reason             string
	ObservedGeneration int64
	LastTransitionTime metav1.Time
}

// SandboxObservation is the bounded state consumed by the AgentRun
// reconciler.
type SandboxObservation struct {
	Ref        SandboxRef
	Exists     bool
	Ready      bool
	Finished   bool
	Conditions []ConditionObservation
}

type BackendOptions struct {
	Now                 func() time.Time
	MaxShutdownDuration time.Duration
}

// SandboxBackend is the controller seam. AgentSandboxBackend is the primary
// implementation; JobBackend is the tested fallback and uses the same
// contract without changing the AgentRun reconciler.
type SandboxBackend interface {
	Ensure(context.Context, SandboxPlan) (SandboxRef, error)
	Observe(context.Context, SandboxRef) (SandboxObservation, error)
	Delete(context.Context, SandboxRef) error
}

// OrphanCleaner is an optional recovery seam used when a controller process
// dies after creating a child but before it can persist the ChildRef. It looks
// up only the deterministic owner/role name and derives the live fingerprint
// after validating ownership and security posture; it never accepts a caller
// supplied arbitrary child name or spec fingerprint.
type OrphanCleaner interface {
	CleanupOwned(context.Context, string, types.UID, Role, string) error
}

// AgentSandboxBackend is the direct agents.x-k8s.io/v1beta1 implementation.
type AgentSandboxBackend struct {
	client              client.Client
	now                 func() time.Time
	maxShutdownDuration time.Duration
}

var _ SandboxBackend = (*AgentSandboxBackend)(nil)

func NewAgentSandboxBackend(c client.Client, options BackendOptions) (*AgentSandboxBackend, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: nil Kubernetes client", ErrInvalidPlan)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	maxShutdown := options.MaxShutdownDuration
	if maxShutdown == 0 {
		maxShutdown = DefaultMaxShutdownDuration
	}
	if maxShutdown <= 0 || maxShutdown > DefaultMaxShutdownDuration {
		return nil, fmt.Errorf("%w: max shutdown duration must be in (0,%s]", ErrInvalidPlan, DefaultMaxShutdownDuration)
	}
	return &AgentSandboxBackend{client: c, now: now, maxShutdownDuration: maxShutdown}, nil
}

// ChildName derives a stable DNS-safe name from an AgentRun UID and role.
func ChildName(ownerUID types.UID, role Role) (string, error) {
	if ownerUID == "" {
		return "", fmt.Errorf("%w: owner UID is required", ErrInvalidPlan)
	}
	if !role.valid() {
		return "", fmt.Errorf("%w: unsupported role %q", ErrInvalidPlan, role)
	}
	hash := sha256.Sum256([]byte(string(ownerUID) + "\x00" + string(role)))
	return "agw-" + string(role) + "-" + hex.EncodeToString(hash[:10]), nil
}

// Ensure is create-or-get and never mutates an existing child.
func (b *AgentSandboxBackend) Ensure(ctx context.Context, plan SandboxPlan) (SandboxRef, error) {
	prepared, expected, err := b.prepare(plan)
	if err != nil {
		return SandboxRef{}, err
	}
	key := client.ObjectKey{Namespace: prepared.Namespace, Name: prepared.Name}
	var current sandboxv1beta1.Sandbox
	if err := b.client.Get(ctx, key, &current); err == nil {
		if err := b.validateExisting(&current, expected, prepared); err != nil {
			return SandboxRef{}, err
		}
		return refFromSandbox(&current, expected), nil
	} else if !apierrors.IsNotFound(err) {
		return SandboxRef{}, fmt.Errorf("get Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}

	err = b.client.Create(ctx, prepared)
	if err == nil {
		// Read back the object so API-server defaults are included in the
		// same validation path used by retries and create races.
		current = sandboxv1beta1.Sandbox{}
		if err := b.client.Get(ctx, key, &current); err != nil {
			return SandboxRef{}, fmt.Errorf("get Sandbox after create %s/%s: %w", key.Namespace, key.Name, err)
		}
		if err := b.validateExisting(&current, expected, prepared); err != nil {
			return SandboxRef{}, err
		}
		return refFromSandbox(&current, expected), nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return SandboxRef{}, fmt.Errorf("create Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	// A concurrent creator is accepted only after a fresh ownership/digest
	// validation; AlreadyExists alone is not proof of idempotency.
	current = sandboxv1beta1.Sandbox{}
	if err := b.client.Get(ctx, key, &current); err != nil {
		return SandboxRef{}, fmt.Errorf("get Sandbox after create race %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := b.validateExisting(&current, expected, prepared); err != nil {
		return SandboxRef{}, err
	}
	return refFromSandbox(&current, expected), nil
}

// Observe returns Exists=false for a missing child and an error for a foreign
// one. Existing security invariants are checked before any state is trusted.
func (b *AgentSandboxBackend) Observe(ctx context.Context, ref SandboxRef) (SandboxObservation, error) {
	if err := validateRefKind(ref, ChildKindSandbox); err != nil {
		return SandboxObservation{}, err
	}
	var current sandboxv1beta1.Sandbox
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := b.client.Get(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return SandboxObservation{Ref: ref}, nil
		}
		return SandboxObservation{}, fmt.Errorf("get Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := b.validateExisting(&current, ref, nil); err != nil {
		return SandboxObservation{}, err
	}
	return observationFromSandbox(&current, ref)
}

// Delete is idempotent for an absent child and refuses to delete a foreign one.
func (b *AgentSandboxBackend) Delete(ctx context.Context, ref SandboxRef) error {
	if err := validateRefKind(ref, ChildKindSandbox); err != nil {
		return err
	}
	var current sandboxv1beta1.Sandbox
	key := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := b.client.Get(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Sandbox for delete %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := b.validateExisting(&current, ref, nil); err != nil {
		return err
	}
	if err := b.client.Delete(ctx, &current, client.Preconditions{UID: &ref.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

// CleanupOwned removes a deterministic child whose status reference was lost
// during a crash. It is intentionally narrower than a general Delete: the
// backend reconstructs the name, validates the live owner/labels/spec posture,
// and uses the live UID as the delete precondition.
func (b *AgentSandboxBackend) CleanupOwned(ctx context.Context, namespace string, ownerUID types.UID, role Role, specDigest string) error {
	if namespace == "" || ownerUID == "" || !role.valid() || !validDigest(specDigest) {
		return fmt.Errorf("%w: namespace, owner UID, role, and spec digest are required", ErrReferenceConflict)
	}
	name, err := ChildName(ownerUID, role)
	if err != nil {
		return err
	}
	var current sandboxv1beta1.Sandbox
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := b.client.Get(ctx, key, &current); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get orphan Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	ref := SandboxRef{
		Namespace: namespace, Name: name, Kind: ChildKindSandbox, UID: current.UID,
		OwnerUID: ownerUID, Role: role, SpecDigest: specDigest,
		PlanFingerprint: sandboxSpecFingerprint(current.Spec),
	}
	if err := b.validateExisting(&current, ref, nil); err != nil {
		return err
	}
	if err := b.client.Delete(ctx, &current, client.Preconditions{UID: &current.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete orphan Sandbox %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}

func (b *AgentSandboxBackend) prepare(plan SandboxPlan) (*sandboxv1beta1.Sandbox, SandboxRef, error) {
	if plan.Owner == nil {
		return nil, SandboxRef{}, fmt.Errorf("%w: AgentRun owner is required", ErrInvalidPlan)
	}
	if plan.Owner.Namespace == "" || plan.Owner.Name == "" || plan.Owner.UID == "" {
		return nil, SandboxRef{}, fmt.Errorf("%w: owner namespace, name, and UID are required", ErrInvalidPlan)
	}
	if !plan.Role.valid() {
		return nil, SandboxRef{}, fmt.Errorf("%w: unsupported role %q", ErrInvalidPlan, plan.Role)
	}
	if !validDigest(plan.SpecDigest) {
		return nil, SandboxRef{}, fmt.Errorf("%w: spec digest must be sha256:<64 lowercase hex characters>", ErrInvalidPlan)
	}
	if plan.Sandbox == nil {
		return nil, SandboxRef{}, fmt.Errorf("%w: fully constructed upstream Sandbox is required", ErrInvalidPlan)
	}
	name, err := ChildName(plan.Owner.UID, plan.Role)
	if err != nil {
		return nil, SandboxRef{}, err
	}
	prepared := plan.Sandbox.DeepCopy()
	if prepared.Namespace != "" && prepared.Namespace != plan.Owner.Namespace {
		return nil, SandboxRef{}, fmt.Errorf("%w: Sandbox namespace %q does not match owner namespace %q", ErrInvalidPlan, prepared.Namespace, plan.Owner.Namespace)
	}
	if prepared.Name != "" && prepared.Name != name {
		return nil, SandboxRef{}, fmt.Errorf("%w: Sandbox name %q is not deterministic name %q", ErrInvalidPlan, prepared.Name, name)
	}
	prepared.APIVersion = sandboxv1beta1.GroupVersion.String()
	prepared.Kind = sandboxv1beta1.SandboxKind
	prepared.Namespace = plan.Owner.Namespace
	prepared.Name = name
	if err := stampMetadata(prepared, plan.Owner, plan.Role, plan.SpecDigest); err != nil {
		return nil, SandboxRef{}, err
	}
	if err := validateSandbox(prepared, plan.Role, b.now(), b.maxShutdownDuration, false); err != nil {
		return nil, SandboxRef{}, err
	}
	if err := stampSandboxSpecFingerprint(prepared); err != nil {
		return nil, SandboxRef{}, err
	}
	return prepared, SandboxRef{
		Namespace:       prepared.Namespace,
		Name:            prepared.Name,
		Kind:            ChildKindSandbox,
		OwnerUID:        plan.Owner.UID,
		Role:            plan.Role,
		SpecDigest:      plan.SpecDigest,
		PlanFingerprint: sandboxSpecFingerprint(prepared.Spec),
	}, nil
}

func stampMetadata(sandbox *sandboxv1beta1.Sandbox, owner *v1alpha1.AgentRun, role Role, digest string) error {
	if sandbox.Labels == nil {
		sandbox.Labels = make(map[string]string, 3)
	}
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string, 1)
	}
	for key, value := range map[string]string{SpecDigestLabelKey: digestLabelValue(digest), RunUIDLabelKey: string(owner.UID), RoleLabelKey: string(role)} {
		if current, ok := sandbox.Labels[key]; ok && current != value {
			if key == SpecDigestLabelKey {
				return fmt.Errorf("%w: planned label %q is %q, want %q", ErrSpecDigestConflict, key, current, value)
			}
			return fmt.Errorf("%w: planned label %q is %q, want %q", ErrInvalidPlan, key, current, value)
		}
		sandbox.Labels[key] = value
	}
	if current, ok := sandbox.Annotations[SpecDigestAnnotationKey]; ok && current != digest {
		return fmt.Errorf("%w: planned annotation %q is %q, want %q", ErrSpecDigestConflict, SpecDigestAnnotationKey, current, digest)
	}
	sandbox.Annotations[SpecDigestAnnotationKey] = digest
	want := metav1.OwnerReference{APIVersion: ownerAPIVersion, Kind: ownerKind, Name: owner.Name, UID: owner.UID, Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}
	if len(sandbox.OwnerReferences) > 0 && (len(sandbox.OwnerReferences) != 1 || !sameOwnerReference(sandbox.OwnerReferences[0], want)) {
		return fmt.Errorf("%w: planned Sandbox must have no owner reference or the matching AgentRun controller reference", ErrInvalidPlan)
	}
	sandbox.OwnerReferences = []metav1.OwnerReference{want}
	return nil
}

func stampSandboxSpecFingerprint(sandbox *sandboxv1beta1.Sandbox) error {
	fingerprint := sandboxSpecFingerprint(sandbox.Spec)
	if current, ok := sandbox.Annotations[SandboxSpecFingerprintAnnotationKey]; ok && current != fingerprint {
		return fmt.Errorf("%w: planned annotation %q is %q, want %q", ErrSandboxSpecConflict, SandboxSpecFingerprintAnnotationKey, current, fingerprint)
	}
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string, 1)
	}
	sandbox.Annotations[SandboxSpecFingerprintAnnotationKey] = fingerprint
	return nil
}

// sameSandboxMetadata compares the metadata owned by this backend. Kubernetes
// identity fields such as UID/resourceVersion and the upstream controller's
// documented pod-name/trace annotations are intentionally outside this
// contract. Unknown labels or annotations are not silently accepted.
func sameSandboxMetadata(current, expected *sandboxv1beta1.Sandbox) bool {
	if !apiequality.Semantic.DeepEqual(current.OwnerReferences, expected.OwnerReferences) {
		return false
	}
	if !apiequality.Semantic.DeepEqual(current.Labels, expected.Labels) {
		return false
	}
	return apiequality.Semantic.DeepEqual(
		normalizedSandboxAnnotations(current.Annotations),
		normalizedSandboxAnnotations(expected.Annotations),
	)
}

func normalizedSandboxAnnotations(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	// agent-sandbox owns these annotations and may change them while a
	// Sandbox is running. They are not part of the v3 controller's desired
	// metadata contract, but every other annotation remains exact.
	delete(output, sandboxv1beta1.SandboxPodNameAnnotation)
	delete(output, "opentelemetry.io/trace-context")
	if len(output) == 0 {
		return nil
	}
	return output
}

func sameSandboxSpec(current, expected sandboxv1beta1.SandboxSpec) bool {
	return apiequality.Semantic.DeepEqual(normalizedSandboxSpec(current), normalizedSandboxSpec(expected))
}

// normalizedSandboxSpec admits only defaults documented by Kubernetes or the
// upstream agent-sandbox API. It must not be expanded to hide arbitrary
// controller or user mutations.
func normalizedSandboxSpec(input sandboxv1beta1.SandboxSpec) sandboxv1beta1.SandboxSpec {
	output := *input.DeepCopy()
	if output.OperatingMode == "" {
		output.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
	}
	if output.ShutdownPolicy == nil {
		policy := sandboxv1beta1.ShutdownPolicyRetain
		output.ShutdownPolicy = &policy
	}
	normalizePodSpec(&output.PodTemplate.Spec)
	for index := range output.VolumeClaimTemplates {
		if output.VolumeClaimTemplates[index].Spec.VolumeMode == nil {
			mode := corev1.PersistentVolumeFilesystem
			output.VolumeClaimTemplates[index].Spec.VolumeMode = &mode
		}
	}
	return output
}

func sandboxSpecFingerprint(spec sandboxv1beta1.SandboxSpec) string {
	encoded, err := json.Marshal(normalizedSandboxSpec(spec))
	if err != nil {
		panic(fmt.Sprintf("sandbox spec fingerprint JSON encoding failed: %v", err))
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (b *AgentSandboxBackend) validateExisting(sandbox *sandboxv1beta1.Sandbox, ref SandboxRef, expected *sandboxv1beta1.Sandbox) error {
	if sandbox.Namespace != ref.Namespace || sandbox.Name != ref.Name {
		return fmt.Errorf("%w: object is %s/%s, reference is %s/%s", ErrReferenceConflict, sandbox.Namespace, sandbox.Name, ref.Namespace, ref.Name)
	}
	expectedName, err := ChildName(ref.OwnerUID, ref.Role)
	if err != nil {
		return err
	}
	if sandbox.Name != expectedName {
		return fmt.Errorf("%w: object name %q is not deterministic name %q", ErrForeignChild, sandbox.Name, expectedName)
	}
	if ref.UID != "" && sandbox.UID != "" && ref.UID != sandbox.UID {
		return fmt.Errorf("%w: object UID %q differs from reference UID %q", ErrReferenceConflict, sandbox.UID, ref.UID)
	}
	if !hasExpectedOwner(sandbox, ref) {
		return fmt.Errorf("%w: Sandbox %s/%s is not controlled by AgentRun UID %q", ErrForeignChild, sandbox.Namespace, sandbox.Name, ref.OwnerUID)
	}
	if sandbox.Labels[RunUIDLabelKey] != string(ref.OwnerUID) || sandbox.Labels[RoleLabelKey] != string(ref.Role) {
		return fmt.Errorf("%w: ownership labels do not match reference", ErrForeignChild)
	}
	if sandbox.Labels[SpecDigestLabelKey] != digestLabelValue(ref.SpecDigest) || sandbox.Annotations[SpecDigestAnnotationKey] != ref.SpecDigest {
		return fmt.Errorf("%w: expected %q, found label %q and annotation %q", ErrSpecDigestConflict, ref.SpecDigest, sandbox.Labels[SpecDigestLabelKey], sandbox.Annotations[SpecDigestAnnotationKey])
	}
	if expected != nil {
		if !sameSandboxMetadata(sandbox, expected) {
			return fmt.Errorf("%w: Sandbox %s/%s controller-owned metadata differs from the intended contract", ErrSandboxSpecConflict, sandbox.Namespace, sandbox.Name)
		}
		if !sameSandboxSpec(sandbox.Spec, expected.Spec) {
			return fmt.Errorf("%w: Sandbox %s/%s spec fingerprint differs from the intended plan (actual=%s expected=%s)", ErrSandboxSpecConflict, sandbox.Namespace, sandbox.Name, sandboxSpecFingerprint(sandbox.Spec), sandboxSpecFingerprint(expected.Spec))
		}
	}
	actualFingerprint := sandboxSpecFingerprint(sandbox.Spec)
	if actualFingerprint != ref.PlanFingerprint {
		return fmt.Errorf("%w: Sandbox %s/%s actual spec fingerprint %q differs from persisted plan fingerprint %q", ErrSandboxSpecConflict, sandbox.Namespace, sandbox.Name, actualFingerprint, ref.PlanFingerprint)
	}
	if sandbox.Annotations[SandboxSpecFingerprintAnnotationKey] != ref.PlanFingerprint {
		return fmt.Errorf("%w: Sandbox %s/%s fingerprint annotation differs from persisted plan fingerprint", ErrSandboxSpecConflict, sandbox.Namespace, sandbox.Name)
	}
	return validateSandbox(sandbox, ref.Role, b.now(), b.maxShutdownDuration, true)
}

func validateRef(ref SandboxRef) error {
	if ref.Namespace == "" || ref.Name == "" || ref.OwnerUID == "" || !ref.Role.valid() || !validChildKind(ref.Kind) || !validDigest(ref.SpecDigest) || !validDigest(ref.PlanFingerprint) {
		return fmt.Errorf("%w: namespace, name, kind, owner UID, role, valid spec digest, and valid plan fingerprint are required", ErrReferenceConflict)
	}
	expected, err := ChildName(ref.OwnerUID, ref.Role)
	if err != nil {
		return err
	}
	if ref.Name != expected {
		return fmt.Errorf("%w: name %q is not deterministic name %q", ErrReferenceConflict, ref.Name, expected)
	}
	return nil
}

func validChildKind(kind string) bool { return kind == ChildKindSandbox || kind == ChildKindJob }

func validateRefKind(ref SandboxRef, want string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if ref.Kind != want {
		return fmt.Errorf("%w: backend expects child kind %q, reference has %q", ErrReferenceConflict, want, ref.Kind)
	}
	return nil
}

func hasExpectedOwner(sandbox *sandboxv1beta1.Sandbox, ref SandboxRef) bool {
	if len(sandbox.OwnerReferences) != 1 {
		return false
	}
	owner := sandbox.OwnerReferences[0]
	return owner.APIVersion == ownerAPIVersion && owner.Kind == ownerKind && owner.Name != "" && owner.UID == ref.OwnerUID && boolValue(owner.Controller) && boolValue(owner.BlockOwnerDeletion)
}

func observationFromSandbox(sandbox *sandboxv1beta1.Sandbox, expected SandboxRef) (SandboxObservation, error) {
	if len(sandbox.Status.Conditions) > MaxObservedConditions {
		return SandboxObservation{}, fmt.Errorf("%w: %d conditions exceeds %d", ErrObservationTooLarge, len(sandbox.Status.Conditions), MaxObservedConditions)
	}
	observation := SandboxObservation{Ref: refFromSandbox(sandbox, expected), Exists: true, Conditions: make([]ConditionObservation, 0, len(sandbox.Status.Conditions))}
	for _, condition := range sandbox.Status.Conditions {
		observation.Conditions = append(observation.Conditions, ConditionObservation{Type: string(condition.Type), Status: condition.Status, Reason: condition.Reason, ObservedGeneration: condition.ObservedGeneration, LastTransitionTime: condition.LastTransitionTime})
		switch condition.Type {
		case string(sandboxv1beta1.SandboxConditionReady):
			observation.Ready = condition.Status == metav1.ConditionTrue
		case string(sandboxv1beta1.SandboxConditionFinished):
			observation.Finished = condition.Status == metav1.ConditionTrue
		}
	}
	return observation, nil
}

func refFromSandbox(sandbox *sandboxv1beta1.Sandbox, expected SandboxRef) SandboxRef {
	expected.Namespace = sandbox.Namespace
	expected.Name = sandbox.Name
	expected.UID = sandbox.UID
	return expected
}

func validateSandbox(sandbox *sandboxv1beta1.Sandbox, role Role, now time.Time, maxShutdown time.Duration, allowExpired bool) error {
	if !role.valid() {
		return fmt.Errorf("%w: unsupported role %q", ErrInvalidSecurityPosture, role)
	}
	pod := sandbox.Spec.PodTemplate.Spec
	if pod.HostUsers == nil || *pod.HostUsers {
		return fmt.Errorf("%w: podTemplate.spec.hostUsers must be explicitly false", ErrInvalidSecurityPosture)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		return fmt.Errorf("%w: automountServiceAccountToken must be explicitly false", ErrInvalidSecurityPosture)
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		return fmt.Errorf("%w: hostNetwork, hostPID, and hostIPC must all be false", ErrInvalidSecurityPosture)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		return fmt.Errorf("%w: restartPolicy must be Never", ErrInvalidSecurityPosture)
	}
	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		return fmt.Errorf("%w: pod seccompProfile must be RuntimeDefault", ErrInvalidSecurityPosture)
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			return fmt.Errorf("%w: volume %q uses hostPath", ErrInvalidSecurityPosture, volume.Name)
		}
	}
	if len(pod.EphemeralContainers) > 0 {
		return fmt.Errorf("%w: ephemeral containers are not supported", ErrInvalidSecurityPosture)
	}
	if err := validateRoleShape(pod, role); err != nil {
		return err
	}
	for index, container := range pod.InitContainers {
		if err := validateContainerSecurity(container, container.Name == "lockdown" && index == len(pod.InitContainers)-1, false); err != nil {
			return err
		}
	}
	for _, container := range pod.Containers {
		if err := validateContainerSecurity(container, false, true); err != nil {
			return err
		}
	}
	if err := validateRoleUIDs(pod, role); err != nil {
		return err
	}
	if sandbox.Spec.ShutdownTime == nil || sandbox.Spec.ShutdownTime.IsZero() {
		return fmt.Errorf("%w: shutdownTime is required", ErrInvalidSecurityPosture)
	}
	shutdown := sandbox.Spec.ShutdownTime.Time
	if (!allowExpired && shutdown.Before(now)) || shutdown.After(now.Add(maxShutdown)) {
		return fmt.Errorf("%w: shutdownTime is outside the allowed bound", ErrInvalidSecurityPosture)
	}
	if sandbox.Spec.ShutdownPolicy == nil || (*sandbox.Spec.ShutdownPolicy != sandboxv1beta1.ShutdownPolicyDelete && *sandbox.Spec.ShutdownPolicy != sandboxv1beta1.ShutdownPolicyRetain) {
		return fmt.Errorf("%w: shutdownPolicy must be explicitly Delete or Retain", ErrInvalidSecurityPosture)
	}
	return nil
}

// validateRoleShape enforces the exact topology needed for the network
// airlock and keeps a verify Sandbox from accidentally becoming another work
// Sandbox. Setup init containers are controller-owned too: an added or
// reordered init container could run code before the airlock, so it is never
// tolerated.
func validateRoleShape(pod corev1.PodSpec, role Role) error {
	var allowedInit [][]string
	var wantContainers []string
	switch role {
	case RoleWork:
		// Context is required; the skills init container is optional, and both
		// must remain before lockdown. No other setup ordering is valid.
		allowedInit = [][]string{
			{"clone", "context", "lockdown"},
			{"clone", "skills", "context", "lockdown"},
		}
		wantContainers = []string{"agent", "broker"}
	case RoleVerify:
		allowedInit = [][]string{{"fetch", "apply", "lockdown"}}
		wantContainers = []string{"verify"}
	default:
		return fmt.Errorf("%w: unsupported role %q", ErrInvalidSecurityPosture, role)
	}
	if !matchesAnyContainerNameSequence(pod.InitContainers, allowedInit) {
		return fmt.Errorf("%w: %s Sandbox init container order is not an allowed controller sequence", ErrInvalidSecurityPosture, role)
	}
	if !containerNameSequenceMatches(pod.Containers, wantContainers) {
		return fmt.Errorf("%w: %s Sandbox regular container order must be %v", ErrInvalidSecurityPosture, role, wantContainers)
	}
	return nil
}

func matchesAnyContainerNameSequence(containers []corev1.Container, allowed [][]string) bool {
	for _, expected := range allowed {
		if containerNameSequenceMatches(containers, expected) {
			return true
		}
	}
	return false
}

func containerNameSequenceMatches(containers []corev1.Container, expected []string) bool {
	if len(containers) != len(expected) {
		return false
	}
	for index, container := range containers {
		if container.Name != expected[index] {
			return false
		}
	}
	return true
}

func validateRoleUIDs(pod corev1.PodSpec, role Role) error {
	want := map[string]int64{"agent": 1000, "broker": 1337, "verify": 1000}
	for _, container := range pod.Containers {
		uid, required := want[container.Name]
		if !required {
			continue
		}
		security := container.SecurityContext
		if security == nil || security.RunAsUser == nil || *security.RunAsUser != uid || security.RunAsGroup == nil || *security.RunAsGroup != uid {
			return fmt.Errorf("%w: %s container must run as UID/GID %d", ErrInvalidSecurityPosture, container.Name, uid)
		}
	}
	return nil
}

func validateContainerSecurity(container corev1.Container, lockdown bool, regular bool) error {
	security := container.SecurityContext
	if security == nil {
		return fmt.Errorf("%w: container %q must declare a securityContext", ErrInvalidSecurityPosture, container.Name)
	}
	if security.Privileged != nil && *security.Privileged {
		return fmt.Errorf("%w: container %q is privileged", ErrInvalidSecurityPosture, container.Name)
	}
	if security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation {
		return fmt.Errorf("%w: container %q must explicitly set allowPrivilegeEscalation=false", ErrInvalidSecurityPosture, container.Name)
	}
	if security.SeccompProfile != nil && security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		return fmt.Errorf("%w: container %q overrides seccompProfile", ErrInvalidSecurityPosture, container.Name)
	}
	if security.ProcMount != nil && *security.ProcMount != corev1.DefaultProcMount {
		return fmt.Errorf("%w: container %q has procMount %q", ErrInvalidSecurityPosture, container.Name, *security.ProcMount)
	}
	if security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
		return fmt.Errorf("%w: container %q must use a read-only root filesystem", ErrInvalidSecurityPosture, container.Name)
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			return fmt.Errorf("%w: container %q requests host port %d", ErrInvalidSecurityPosture, container.Name, port.HostPort)
		}
	}
	if security.Capabilities == nil || !hasCapability(security.Capabilities.Drop, corev1.Capability("ALL")) {
		return fmt.Errorf("%w: container %q must explicitly drop ALL capabilities", ErrInvalidSecurityPosture, container.Name)
	}

	if lockdown {
		if len(security.Capabilities.Add) != 1 || security.Capabilities.Add[0] != corev1.Capability("NET_ADMIN") {
			return fmt.Errorf("%w: lockdown must add only NET_ADMIN", ErrInvalidSecurityPosture)
		}
		if security.RunAsUser == nil || *security.RunAsUser != 0 {
			return fmt.Errorf("%w: lockdown must run as UID 0 inside the user namespace", ErrInvalidSecurityPosture)
		}
		if security.RunAsNonRoot == nil || *security.RunAsNonRoot {
			return fmt.Errorf("%w: lockdown must explicitly run as root inside the user namespace", ErrInvalidSecurityPosture)
		}
		return nil
	}

	if len(security.Capabilities.Add) != 0 {
		return fmt.Errorf("%w: container %q may not add capabilities", ErrInvalidSecurityPosture, container.Name)
	}
	if !regular {
		return nil
	}
	if security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
		return fmt.Errorf("%w: container %q must explicitly set runAsNonRoot=true", ErrInvalidSecurityPosture, container.Name)
	}
	if security.RunAsUser != nil && *security.RunAsUser <= 0 {
		return fmt.Errorf("%w: container %q must not run as UID 0", ErrInvalidSecurityPosture, container.Name)
	}
	return nil
}

func hasCapability(capabilities []corev1.Capability, wanted corev1.Capability) bool {
	for _, capability := range capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func digestLabelValue(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 63 {
		return value[:63]
	}
	return value
}

func sameOwnerReference(left, right metav1.OwnerReference) bool {
	return left.APIVersion == right.APIVersion && left.Kind == right.Kind && left.Name == right.Name && left.UID == right.UID && boolValue(left.Controller) && boolValue(left.BlockOwnerDeletion) && boolValue(right.Controller) && boolValue(right.BlockOwnerDeletion)
}

func boolPtr(value bool) *bool { return &value }

func boolValue(value *bool) bool { return value != nil && *value }
