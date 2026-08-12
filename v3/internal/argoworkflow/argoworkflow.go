// Package argoworkflow contains the deliberately small boundary between an
// admitted AgentRun and an Argo Workflow backend.
//
// This package does not own AgentRun lifecycle, Gate decisions, effect
// publication, or retries. It only builds an immutable, identity-bound Argo
// object and validates observations returned by that backend. In particular,
// an Argo phase is never converted into an agents.astatide.com domain phase.
package argoworkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// WorkflowAPIVersion and WorkflowKind are kept as strings intentionally.
	// The seam must not import Argo's Go API package or inherit its versioned
	// types. The operator pins the WorkflowTemplate separately.
	WorkflowAPIVersion = "argoproj.io/v1alpha1"
	WorkflowKind       = "Workflow"

	managedByLabel = "agents.astatide.com/managed-by"
	managedByValue = "argoworkflow"

	runUIDHashLabel     = "agents.astatide.com/run-uid-sha256"
	specDigestHashLabel = "agents.astatide.com/spec-digest-sha256"

	runUIDAnnotation          = "agents.astatide.com/run-uid"
	specDigestAnnotation      = "agents.astatide.com/spec-digest"
	baseSHAAnnotation         = "agents.astatide.com/base-sha"
	resolvedSpecURIAnnotation = "agents.astatide.com/resolved-spec-uri"
	resolvedDigestAnnotation  = "agents.astatide.com/resolved-spec-digest"
	resolvedRefHashAnnotation = "agents.astatide.com/resolved-ref-sha256"
	runGenerationAnnotation   = "agents.astatide.com/run-generation"
	templateUIDAnnotation     = "agents.astatide.com/workflow-template-uid"
	templateDigestAnnotation  = "agents.astatide.com/workflow-template-digest"

	parameterRunUID             = "agw-run-uid"
	parameterRunName            = "agw-run-name"
	parameterNamespace          = "agw-namespace"
	parameterResolvedSpecURI    = "agw-resolved-spec-uri"
	parameterResolvedSpecDigest = "agw-resolved-spec-digest"
	parameterBaseSHA            = "agw-base-sha"
	parameterRunGeneration      = "agw-run-generation"
	parameterTemplateUID        = "agw-workflow-template-uid"
	parameterTemplateDigest     = "agw-workflow-template-digest"

	workflowNamePrefix = "agw-"
)

// WorkflowNameForRunUID exposes the deterministic child-name seam to the
// lifecycle producer. It does not authorize creation or observation; callers
// still bind the live Workflow through Translate and Bind.
func WorkflowNameForRunUID(runUID string) (string, error) {
	if !validRunUID(runUID) {
		return "", fmt.Errorf("%w: run UID is unsafe", ErrInvalidInput)
	}
	return workflowName(runUID), nil
}

var (
	// ErrInvalidInput means the admitted contract or operator-owned config is
	// incomplete, malformed, or internally inconsistent.
	ErrInvalidInput = errors.New("argoworkflow: invalid input")
	// ErrBinding means a backend object is not the object for the expected
	// AgentRun/resolved contract.
	ErrBinding = errors.New("argoworkflow: workflow binding mismatch")
	// ErrInvalidObservation means the backend object cannot be safely observed.
	ErrInvalidObservation = errors.New("argoworkflow: invalid workflow observation")
	// ErrUnknownPhase is returned for every phase outside the deliberately
	// closed mapping below. Argo's Unknown phase is not silently treated as a
	// failure or success because that would invent domain semantics.
	ErrUnknownPhase = errors.New("argoworkflow: unknown Argo workflow phase")
)

var runUIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Config is operator-owned configuration. None of its values are taken from
// AgentRun user input. Namespace is deliberately required to equal the run's
// namespace; this seam does not support cross-namespace child creation.
type Config struct {
	Namespace              string
	WorkflowTemplateName   string
	WorkflowTemplateUID    string
	WorkflowTemplateDigest string
}

// Inputs are the only values accepted by Translate. Snapshot and ResolvedRef
// must be the immutable, admitted pair persisted by the operator. No secret
// values are accepted or serialized by this package.
type Inputs struct {
	Run         *v1alpha1.AgentRun
	Snapshot    resolved.Snapshot
	ResolvedRef v1alpha1.ArtifactRef
	Config      Config
}

// Reference identifies the backend object. UID is empty in a Translation
// result because Kubernetes assigns it; Observe requires a non-empty UID from
// the live object.
type Reference struct {
	Namespace string
	Name      string
	UID       types.UID
	// Generation is the live Workflow metadata.generation captured at bind
	// time. It is zero only before the API server has created the object.
	Generation int64
}

// Binding is the immutable identity expected on a Workflow observation. It is
// returned alongside the generated object so a controller can retain it
// without reconstructing security-sensitive assumptions from the object.
type Binding struct {
	Reference              Reference
	RunUID                 string
	RunName                string
	Namespace              string
	WorkflowTemplateName   string
	WorkflowTemplateUID    string
	WorkflowTemplateDigest string
	SpecDigest             string
	BaseSHA                string
	ResolvedRef            v1alpha1.ArtifactRef
	RunGeneration          int64
	// RunTimeoutSeconds is the immutable timeout copied to the generated
	// Workflow.spec.activeDeadlineSeconds. It is zero only while a binding is
	// reconstructed from AgentRun status; Backend.Observe rehydrates it from
	// the live AgentRun before validating the Workflow.
	RunTimeoutSeconds int64
}

// Translation is a backend object plus the exact binding used to validate its
// future observations.
type Translation struct {
	Workflow *unstructured.Unstructured
	Binding  Binding
}

// BackendPhase is intentionally not v1alpha1.Phase. It describes only what
// Argo reported and must be interpreted by the domain controller separately.
type BackendPhase string

const (
	BackendPending   BackendPhase = "Pending"
	BackendRunning   BackendPhase = "Running"
	BackendSucceeded BackendPhase = "Succeeded"
	BackendFailed    BackendPhase = "Failed"
	BackendError     BackendPhase = "Error"
	BackendSkipped   BackendPhase = "Skipped"
	BackendOmitted   BackendPhase = "Omitted"
	BackendSuspended BackendPhase = "Suspended"
)

// Observation is a bounded backend observation. Terminal reports whether the
// backend phase is terminal according to this package's closed Argo mapping;
// it is not an AgentRun completion or Gate/effect verdict.
type Observation struct {
	Reference          Reference
	Phase              BackendPhase
	RawPhase           string
	Terminal           bool
	ObservedGeneration int64
	Message            string
	// Output is present only after a successful Argo phase has supplied a
	// validated lifecycle handoff. It is never an Argo success verdict and is
	// intentionally not populated for non-terminal observations.
	Output *BoundLifecycleOutput
}

// Translate builds one deterministic Workflow object for an admitted run.
// The returned object has no Kubernetes-managed fields and no status. A
// caller may retry this function for the same logical run; it will produce the
// same object bytes.
func Translate(input Inputs) (Translation, error) {
	binding, err := validateInputs(input)
	if err != nil {
		return Translation{}, err
	}
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": WorkflowAPIVersion,
		"kind":       WorkflowKind,
		"metadata": map[string]any{
			"name":      binding.Reference.Name,
			"namespace": binding.Namespace,
			"labels": map[string]any{
				managedByLabel:      managedByValue,
				runUIDHashLabel:     hashHex(binding.RunUID),
				specDigestHashLabel: digestHex(binding.SpecDigest),
			},
			"annotations": map[string]any{
				runUIDAnnotation:          binding.RunUID,
				specDigestAnnotation:      binding.SpecDigest,
				baseSHAAnnotation:         binding.BaseSHA,
				resolvedSpecURIAnnotation: binding.ResolvedRef.URI,
				resolvedDigestAnnotation:  binding.ResolvedRef.Digest,
				resolvedRefHashAnnotation: resolvedRefHash(binding.ResolvedRef),
				runGenerationAnnotation:   strconv.FormatInt(binding.RunGeneration, 10),
				templateUIDAnnotation:     binding.WorkflowTemplateUID,
				templateDigestAnnotation:  binding.WorkflowTemplateDigest,
			},
			"ownerReferences": []any{ownerReference(input.Run)},
		},
		"spec": map[string]any{
			"activeDeadlineSeconds": binding.RunTimeoutSeconds,
			"workflowTemplateRef": map[string]any{
				"name": binding.WorkflowTemplateName,
			},
			"arguments": map[string]any{
				"parameters": workflowParameters(binding),
			},
		},
	}}
	workflow.SetGroupVersionKind(schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Workflow"})
	return Translation{Workflow: workflow, Binding: binding}, nil
}

// Bind validates only the immutable identity and specification of a live
// Workflow and records the API-server UID/generation. It intentionally does
// not inspect Workflow status. Controllers use this once, then persist the
// returned binding before any backend phase is trusted.
func Bind(workflow *unstructured.Unstructured, expected Binding) (Binding, error) {
	if err := validateWorkflowIdentity(workflow, expected); err != nil {
		return Binding{}, err
	}
	bound := expected
	bound.Reference.UID = workflow.GetUID()
	bound.Reference.Generation = workflow.GetGeneration()
	if err := validateBinding(bound); err != nil {
		return Binding{}, fmt.Errorf("%w: bound Workflow: %v", ErrBinding, err)
	}
	return bound, nil
}

// Observe validates identity and parses the backend-only status of a live
// Workflow. It rejects an object that is missing a required binding even if
// its Argo phase is otherwise valid.
func Observe(workflow *unstructured.Unstructured, expected Binding) (observation Observation, err error) {
	// Unstructured helpers deep-copy values and may panic when handed an
	// in-memory object containing a non-JSON-compatible value. Objects read from
	// the API server are JSON-compatible, but this boundary must also fail
	// closed for malformed fakes, cache data, or adapters rather than taking the
	// process down.
	defer func() {
		if recovered := recover(); recovered != nil {
			observation = Observation{}
			err = fmt.Errorf("%w: malformed Workflow: %v", ErrInvalidObservation, recovered)
		}
	}()
	if err := validateBinding(expected); err != nil {
		return Observation{}, fmt.Errorf("%w: %v", ErrInvalidObservation, err)
	}
	if err := validateWorkflowIdentity(workflow, expected); err != nil {
		return Observation{}, err
	}

	status, found, err := unstructured.NestedMap(workflow.Object, "status")
	if err != nil {
		return Observation{}, fmt.Errorf("%w: malformed status: %v", ErrInvalidObservation, err)
	}
	phase := ""
	message := ""
	if found {
		if value, exists := status["phase"]; exists {
			var ok bool
			phase, ok = value.(string)
			if !ok {
				return Observation{}, fmt.Errorf("%w: status.phase is not a string", ErrInvalidObservation)
			}
		}
		if value, exists := status["message"]; exists {
			var ok bool
			message, ok = value.(string)
			if !ok {
				return Observation{}, fmt.Errorf("%w: status.message is not a string", ErrInvalidObservation)
			}
			if len(message) > v1alpha1.MaxStatusMessage {
				return Observation{}, fmt.Errorf("%w: status.message exceeds %d bytes", ErrInvalidObservation, v1alpha1.MaxStatusMessage)
			}
		}
	}
	backendPhase, terminal, ok := mapPhase(phase)
	if !ok {
		return Observation{}, fmt.Errorf("%w: %q", ErrUnknownPhase, phase)
	}
	var output *BoundLifecycleOutput
	if backendPhase == BackendSucceeded {
		bound, bindErr := BindLifecycleOutput(workflow, expected)
		if bindErr != nil {
			return Observation{}, bindErr
		}
		output = &bound
	}
	return Observation{
		Reference: Reference{
			Namespace:  workflow.GetNamespace(),
			Name:       workflow.GetName(),
			UID:        workflow.GetUID(),
			Generation: workflow.GetGeneration(),
		},
		Phase:              backendPhase,
		RawPhase:           phase,
		Terminal:           terminal,
		ObservedGeneration: workflow.GetGeneration(),
		Message:            message,
		Output:             output,
	}, nil
}

func validateWorkflowIdentity(workflow *unstructured.Unstructured, expected Binding) error {
	if workflow == nil {
		return fmt.Errorf("%w: workflow is nil", ErrInvalidObservation)
	}
	if workflow.GetAPIVersion() != WorkflowAPIVersion || workflow.GetKind() != WorkflowKind {
		return fmt.Errorf("%w: expected %s/%s", ErrInvalidObservation, WorkflowAPIVersion, WorkflowKind)
	}
	if workflow.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%w: Workflow is being deleted", ErrBinding)
	}
	if workflow.GetNamespace() != expected.Namespace || workflow.GetName() != expected.Reference.Name {
		return fmt.Errorf("%w: namespace/name does not match expected reference", ErrBinding)
	}
	if !validRunUID(string(workflow.GetUID())) {
		return fmt.Errorf("%w: live Workflow UID is missing or unsafe", ErrInvalidObservation)
	}
	if workflow.GetGeneration() <= 0 {
		return fmt.Errorf("%w: live Workflow generation is missing or invalid", ErrInvalidObservation)
	}
	if expected.Reference.UID != "" && workflow.GetUID() != expected.Reference.UID {
		return fmt.Errorf("%w: live Workflow UID does not match the bound UID", ErrBinding)
	}
	if expected.Reference.Generation > 0 && workflow.GetGeneration() != expected.Reference.Generation {
		return fmt.Errorf("%w: live Workflow generation does not match the bound generation", ErrBinding)
	}
	if err := validateOwner(workflow.GetOwnerReferences(), expected); err != nil {
		return err
	}
	if err := validateMetadataBinding(workflow, expected); err != nil {
		return err
	}
	return validateWorkflowSpec(workflow, expected)
}

// NormalizedBytes returns deterministic JSON for a Workflow after removing
// fields Kubernetes owns or the Argo controller continuously updates. It is
// intended for retry equivalence tests and create/update comparisons; it is
// not an object-store canonicalization format.
func NormalizedBytes(workflow *unstructured.Unstructured) (encoded []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			encoded = nil
			err = fmt.Errorf("%w: malformed Workflow: %v", ErrInvalidInput, recovered)
		}
	}()
	if workflow == nil {
		return nil, fmt.Errorf("%w: workflow is nil", ErrInvalidInput)
	}
	copy := workflow.DeepCopy()
	metadata, found, err := unstructured.NestedMap(copy.Object, "metadata")
	if err != nil {
		return nil, fmt.Errorf("%w: malformed metadata: %v", ErrInvalidInput, err)
	}
	if found {
		for _, field := range []string{
			"creationTimestamp", "deletionGracePeriodSeconds", "deletionTimestamp",
			"generation", "managedFields", "resourceVersion", "selfLink", "uid",
		} {
			delete(metadata, field)
		}
		copy.Object["metadata"] = metadata
	}
	delete(copy.Object, "status")
	return json.Marshal(copy.Object)
}

func validateInputs(input Inputs) (Binding, error) {
	if input.Run == nil {
		return Binding{}, fmt.Errorf("%w: AgentRun is nil", ErrInvalidInput)
	}
	if err := validateConfig(input.Config); err != nil {
		return Binding{}, err
	}
	run := input.Run
	if run.Namespace != input.Config.Namespace {
		return Binding{}, fmt.Errorf("%w: config namespace must equal AgentRun namespace", ErrInvalidInput)
	}
	if len(validation.IsDNS1123Subdomain(run.Name)) != 0 {
		return Binding{}, fmt.Errorf("%w: AgentRun name is not a DNS subdomain", ErrInvalidInput)
	}
	if !validRunUID(string(run.UID)) {
		return Binding{}, fmt.Errorf("%w: AgentRun UID is missing or unsafe", ErrInvalidInput)
	}
	if run.Generation <= 0 {
		return Binding{}, fmt.Errorf("%w: AgentRun generation is missing or invalid", ErrInvalidInput)
	}
	if !admitted(run) {
		return Binding{}, fmt.Errorf("%w: AgentRun is not admitted", ErrInvalidInput)
	}
	if !resolved.ValidBaseSHA(input.Snapshot.BaseSHA) {
		return Binding{}, fmt.Errorf("%w: snapshot baseSHA is invalid", ErrInvalidInput)
	}
	if run.Status.BaseSHA != input.Snapshot.BaseSHA {
		return Binding{}, fmt.Errorf("%w: status baseSHA does not match snapshot", ErrInvalidInput)
	}
	if !canonical.ValidDigest(run.Status.SpecDigest) {
		return Binding{}, fmt.Errorf("%w: status specDigest is invalid", ErrInvalidInput)
	}
	if input.ResolvedRef.Digest != run.Status.SpecDigest {
		return Binding{}, fmt.Errorf("%w: resolved ref digest does not match status specDigest", ErrInvalidInput)
	}
	if run.Status.ResolvedSpecRef == nil || !reflect.DeepEqual(*run.Status.ResolvedSpecRef, input.ResolvedRef) {
		return Binding{}, fmt.Errorf("%w: resolved ref is not the admitted status ref", ErrInvalidInput)
	}
	if err := validateResolvedRef(input.ResolvedRef); err != nil {
		return Binding{}, err
	}
	if input.Snapshot.SchemaVersion != resolved.SchemaVersion || input.Snapshot.Run.Namespace != run.Namespace || input.Snapshot.Run.Name != run.Name || input.Snapshot.Run.UID != string(run.UID) || input.Snapshot.Run.Generation != run.Generation {
		return Binding{}, fmt.Errorf("%w: snapshot identity does not match AgentRun", ErrInvalidInput)
	}
	canonicalSnapshot, err := canonical.CanonicalizeResolvedSpec(input.Snapshot)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: canonicalize snapshot: %v", ErrInvalidInput, err)
	}
	digest, err := canonical.ResolvedSpecDigest(canonicalSnapshot)
	if err != nil || digest != run.Status.SpecDigest {
		return Binding{}, fmt.Errorf("%w: snapshot digest does not match status specDigest", ErrInvalidInput)
	}
	decoded, err := resolved.Decode(canonicalSnapshot, digest)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: immutable snapshot failed validation: %v", ErrInvalidInput, err)
	}
	if !reflect.DeepEqual(decoded, input.Snapshot) {
		return Binding{}, fmt.Errorf("%w: snapshot is not canonical and credential-free", ErrInvalidInput)
	}
	if input.ResolvedRef.SizeBytes > 0 && input.ResolvedRef.SizeBytes != int64(len(canonicalSnapshot)) {
		return Binding{}, fmt.Errorf("%w: resolved ref size does not match snapshot", ErrInvalidInput)
	}
	deadlineSeconds, err := workflowDeadlineSeconds(input.Snapshot.Spec.Limits.Timeout)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: invalid AgentRun workflow timeout: %v", ErrInvalidInput, err)
	}

	binding := Binding{
		Reference:              Reference{Namespace: run.Namespace, Name: workflowName(string(run.UID))},
		RunUID:                 string(run.UID),
		RunName:                run.Name,
		Namespace:              run.Namespace,
		WorkflowTemplateName:   input.Config.WorkflowTemplateName,
		WorkflowTemplateUID:    input.Config.WorkflowTemplateUID,
		WorkflowTemplateDigest: input.Config.WorkflowTemplateDigest,
		SpecDigest:             run.Status.SpecDigest,
		BaseSHA:                input.Snapshot.BaseSHA,
		ResolvedRef:            input.ResolvedRef,
		RunGeneration:          run.Generation,
		RunTimeoutSeconds:      deadlineSeconds,
	}
	if err := validateBinding(binding); err != nil {
		return Binding{}, fmt.Errorf("%w: generated binding: %v", ErrInvalidInput, err)
	}
	return binding, nil
}

func validateConfig(config Config) error {
	if len(validation.IsDNS1123Label(config.Namespace)) != 0 {
		return fmt.Errorf("%w: operator namespace is not a DNS label", ErrInvalidInput)
	}
	if len(validation.IsDNS1123Subdomain(config.WorkflowTemplateName)) != 0 {
		return fmt.Errorf("%w: pinned WorkflowTemplate name is not a DNS subdomain", ErrInvalidInput)
	}
	if !validRunUID(config.WorkflowTemplateUID) {
		return fmt.Errorf("%w: pinned WorkflowTemplate UID is missing or unsafe", ErrInvalidInput)
	}
	if !canonical.ValidDigest(config.WorkflowTemplateDigest) {
		return fmt.Errorf("%w: pinned WorkflowTemplate content digest is invalid", ErrInvalidInput)
	}
	return nil
}

func validateBinding(binding Binding) error {
	if len(validation.IsDNS1123Label(binding.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(binding.RunName)) != 0 {
		return fmt.Errorf("%w: binding namespace/name is unsafe", ErrBinding)
	}
	if !validRunUID(binding.RunUID) || binding.Reference.Namespace != binding.Namespace || binding.Reference.Name != workflowName(binding.RunUID) {
		return fmt.Errorf("%w: binding run identity is unsafe", ErrBinding)
	}
	if len(validation.IsDNS1123Subdomain(binding.WorkflowTemplateName)) != 0 {
		return fmt.Errorf("%w: binding WorkflowTemplate name is unsafe", ErrBinding)
	}
	if !validRunUID(binding.WorkflowTemplateUID) || !canonical.ValidDigest(binding.WorkflowTemplateDigest) {
		return fmt.Errorf("%w: binding WorkflowTemplate revision is unsafe", ErrBinding)
	}
	if binding.RunTimeoutSeconds < 0 || binding.RunTimeoutSeconds > int64((24*time.Hour)/time.Second) {
		return fmt.Errorf("%w: workflow timeout seconds are outside the supported bound", ErrBinding)
	}
	if binding.Reference.UID != "" && !validRunUID(string(binding.Reference.UID)) {
		return fmt.Errorf("%w: binding Workflow UID is unsafe", ErrBinding)
	}
	if binding.Reference.UID == "" && binding.Reference.Generation != 0 {
		return fmt.Errorf("%w: unbound Workflow reference has a generation", ErrBinding)
	}
	if binding.Reference.UID != "" && binding.Reference.Generation <= 0 {
		return fmt.Errorf("%w: bound Workflow reference is missing generation", ErrBinding)
	}
	if binding.RunGeneration <= 0 {
		return fmt.Errorf("%w: AgentRun generation is invalid", ErrBinding)
	}
	if !canonical.ValidDigest(binding.SpecDigest) || !resolved.ValidBaseSHA(binding.BaseSHA) || binding.ResolvedRef.Digest != binding.SpecDigest {
		return fmt.Errorf("%w: binding digest/baseSHA is unsafe", ErrBinding)
	}
	if err := validateResolvedRef(binding.ResolvedRef); err != nil {
		return fmt.Errorf("%w: %v", ErrBinding, err)
	}
	return nil
}

func validateResolvedRef(ref v1alpha1.ArtifactRef) error {
	if !canonical.ValidDigest(ref.Digest) {
		return fmt.Errorf("%w: resolved ref digest is invalid", ErrInvalidInput)
	}
	if ref.Kind != "resolved-spec" {
		return fmt.Errorf("%w: resolved ref kind must be resolved-spec", ErrInvalidInput)
	}
	if ref.MediaType != "" && ref.MediaType != "application/json" {
		return fmt.Errorf("%w: resolved ref media type must be application/json", ErrInvalidInput)
	}
	if !safeArtifactURI(ref.URI) {
		return fmt.Errorf("%w: resolved ref URI is unsafe", ErrInvalidInput)
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("%w: resolved ref size is negative", ErrInvalidInput)
	}
	return nil
}

func validateOwner(owners []metav1.OwnerReference, expected Binding) error {
	if len(owners) != 1 {
		return fmt.Errorf("%w: Workflow must have exactly one AgentRun owner", ErrBinding)
	}
	owner := owners[0]
	if owner.APIVersion != v1alpha1.GroupName+"/"+v1alpha1.Version || owner.Kind != "AgentRun" || owner.Name != expected.RunName || owner.UID != types.UID(expected.RunUID) || owner.Controller == nil || !*owner.Controller || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return fmt.Errorf("%w: Workflow owner reference does not match AgentRun", ErrBinding)
	}
	return nil
}

func validateMetadataBinding(workflow *unstructured.Unstructured, expected Binding) error {
	labels := workflow.GetLabels()
	if labels[managedByLabel] != managedByValue || labels[runUIDHashLabel] != hashHex(expected.RunUID) || labels[specDigestHashLabel] != digestHex(expected.SpecDigest) {
		return fmt.Errorf("%w: Workflow labels do not match immutable binding", ErrBinding)
	}
	annotations := workflow.GetAnnotations()
	for key, want := range map[string]string{
		runUIDAnnotation:          expected.RunUID,
		specDigestAnnotation:      expected.SpecDigest,
		baseSHAAnnotation:         expected.BaseSHA,
		resolvedSpecURIAnnotation: expected.ResolvedRef.URI,
		resolvedDigestAnnotation:  expected.ResolvedRef.Digest,
		resolvedRefHashAnnotation: resolvedRefHash(expected.ResolvedRef),
		runGenerationAnnotation:   strconv.FormatInt(expected.RunGeneration, 10),
		templateUIDAnnotation:     expected.WorkflowTemplateUID,
		templateDigestAnnotation:  expected.WorkflowTemplateDigest,
	} {
		if annotations[key] != want {
			return fmt.Errorf("%w: Workflow annotation %q does not match immutable binding", ErrBinding, key)
		}
	}
	return nil
}

func validateWorkflowSpec(workflow *unstructured.Unstructured, expected Binding) error {
	spec, found, err := unstructured.NestedMap(workflow.Object, "spec")
	if err != nil || !found || len(spec) != 3 {
		return fmt.Errorf("%w: Workflow spec is missing or malformed", ErrBinding)
	}
	deadlineSeconds, found, err := unstructured.NestedInt64(workflow.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found || deadlineSeconds <= 0 || deadlineSeconds > int64((24*time.Hour)/time.Second) {
		return fmt.Errorf("%w: Workflow activeDeadlineSeconds is missing or outside the supported bound", ErrBinding)
	}
	if expected.RunTimeoutSeconds > 0 && deadlineSeconds != expected.RunTimeoutSeconds {
		return fmt.Errorf("%w: Workflow activeDeadlineSeconds does not match the immutable run timeout", ErrBinding)
	}
	ref, found, err := unstructured.NestedMap(spec, "workflowTemplateRef")
	if err != nil || !found || len(ref) != 1 {
		return fmt.Errorf("%w: WorkflowTemplateRef is missing or contains unsupported fields", ErrBinding)
	}
	if name, ok := ref["name"].(string); !ok || name != expected.WorkflowTemplateName {
		return fmt.Errorf("%w: WorkflowTemplateRef name does not match operator config", ErrBinding)
	}
	arguments, found, err := unstructured.NestedMap(spec, "arguments")
	if err != nil || !found || len(arguments) != 1 {
		return fmt.Errorf("%w: Workflow arguments are missing or contain unsupported fields", ErrBinding)
	}
	parameters, ok := arguments["parameters"].([]any)
	if !ok || len(parameters) != len(parameterNames) {
		return fmt.Errorf("%w: Workflow parameter set is not closed", ErrBinding)
	}
	want := parameterValues(expected)
	for index, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok || len(parameter) != 2 {
			return fmt.Errorf("%w: Workflow parameter %d is malformed", ErrBinding, index)
		}
		name, nameOK := parameter["name"].(string)
		value, valueOK := parameter["value"].(string)
		if !nameOK || !valueOK || name != parameterNames[index] || value != want[name] {
			return fmt.Errorf("%w: Workflow parameter %d does not match immutable binding", ErrBinding, index)
		}
	}
	return nil
}

var parameterNames = []string{
	parameterRunUID,
	parameterRunName,
	parameterNamespace,
	parameterResolvedSpecURI,
	parameterResolvedSpecDigest,
	parameterBaseSHA,
	parameterRunGeneration,
	parameterTemplateUID,
	parameterTemplateDigest,
}

func workflowParameters(binding Binding) []any {
	values := parameterValues(binding)
	parameters := make([]any, 0, len(parameterNames))
	for _, name := range parameterNames {
		parameters = append(parameters, map[string]any{"name": name, "value": values[name]})
	}
	return parameters
}

func parameterValues(binding Binding) map[string]string {
	return map[string]string{
		parameterRunUID:             binding.RunUID,
		parameterRunName:            binding.RunName,
		parameterNamespace:          binding.Namespace,
		parameterResolvedSpecURI:    binding.ResolvedRef.URI,
		parameterResolvedSpecDigest: binding.ResolvedRef.Digest,
		parameterBaseSHA:            binding.BaseSHA,
		parameterRunGeneration:      strconv.FormatInt(binding.RunGeneration, 10),
		parameterTemplateUID:        binding.WorkflowTemplateUID,
		parameterTemplateDigest:     binding.WorkflowTemplateDigest,
	}
}

func ownerReference(run *v1alpha1.AgentRun) map[string]any {
	controller := true
	blockOwnerDeletion := true
	return map[string]any{
		"apiVersion":         v1alpha1.GroupName + "/" + v1alpha1.Version,
		"blockOwnerDeletion": blockOwnerDeletion,
		"controller":         controller,
		"kind":               "AgentRun",
		"name":               run.Name,
		"uid":                string(run.UID),
	}
}

func admitted(run *v1alpha1.AgentRun) bool {
	found := false
	for _, condition := range run.Status.Conditions {
		if condition.Type != string(v1alpha1.ConditionAdmitted) {
			continue
		}
		if found || condition.Status != v1alpha1.ConditionTrue || condition.ObservedGeneration != run.Generation {
			return false
		}
		found = true
	}
	return found
}

func mapPhase(raw string) (BackendPhase, bool, bool) {
	switch raw {
	case "", "Pending":
		return BackendPending, false, true
	case "Running":
		return BackendRunning, false, true
	case "Succeeded":
		return BackendSucceeded, true, true
	case "Failed":
		return BackendFailed, true, true
	case "Error":
		return BackendError, true, true
	case "Skipped":
		return BackendSkipped, true, true
	case "Omitted":
		return BackendOmitted, true, true
	case "Suspended":
		return BackendSuspended, false, true
	default:
		return "", false, false
	}
}

func workflowName(runUID string) string {
	sum := sha256.Sum256([]byte("agents.astatide.com/argo-workflow\x00" + runUID))
	// 4 + 56 = 60, below the 63-character DNS label ceiling. The exact run UID
	// remains in the owner reference and annotation; the name is only a stable
	// Kubernetes identity derived from it.
	return workflowNamePrefix + hex.EncodeToString(sum[:])[:56]
}

func hashHex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func digestHex(value string) string {
	return strings.TrimPrefix(value, canonical.DigestPrefix)
}

func resolvedRefHash(ref v1alpha1.ArtifactRef) string {
	encoded, err := json.Marshal(ref)
	if err != nil {
		// ArtifactRef is a fixed Go struct containing only JSON scalar fields;
		// this is unreachable unless that contract changes. Keep the helper
		// deterministic and fail closed at validation instead of panicking.
		return ""
	}
	sum := sha256.Sum256(append([]byte("agents.astatide.com/resolved-ref\x00"), encoded...))
	return hex.EncodeToString(sum[:])
}

func validRunUID(value string) bool {
	return runUIDPattern.MatchString(value)
}

// workflowDeadlineSeconds converts the admitted duration to the integer
// seconds required by Argo's WorkflowSpec. Sub-second values are rounded up
// so the workflow deadline never expires before the requested timeout.
func workflowDeadlineSeconds(value string) (int64, error) {
	if value == "" || len(value) > 32 || strings.TrimSpace(value) != value {
		return 0, errors.New("timeout is empty or oversized")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return 0, errors.New("timeout is not a positive duration within 24h")
	}
	seconds := int64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds <= 0 {
		seconds = 1
	}
	return seconds, nil
}

func safeArtifactURI(value string) bool {
	if value == "" || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n@?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "s3" || parsed.Scheme == "https"
}
