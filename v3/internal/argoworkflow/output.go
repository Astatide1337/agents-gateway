package argoworkflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// LifecycleOutputParameterName is the only Workflow output that can cross
	// the Argo -> AGW boundary. The value is the complete canonical JSON
	// contract, not a result or verdict supplied by Argo.
	LifecycleOutputParameterName = "agw-lifecycle-output"

	LifecycleOutputKind          = "agents.astatide.com/argo-lifecycle-output"
	LifecycleOutputSchemaVersion = 1
	LifecycleOutputArtifactKind  = "argo-lifecycle-output"
	LifecycleOutputArtifactName  = "lifecycle-output.json"
	LifecycleOutputMediaType     = "application/json"

	// MaxLifecycleOutputBytes is intentionally below the Kubernetes status
	// budget. The Workflow output is a bounded descriptor; patches, logs,
	// completion records, and verification reports remain external artifacts.
	MaxLifecycleOutputBytes = 32 << 10
	maxWorkflowOutputsBytes = 128 << 10
	maxWorkflowOutputItems  = 64
)

var (
	ErrLifecycleOutputMissing        = errors.New("argoworkflow: lifecycle output is missing")
	ErrLifecycleOutputDuplicate      = errors.New("argoworkflow: lifecycle output is duplicated")
	ErrLifecycleOutputTooLarge       = errors.New("argoworkflow: lifecycle output is too large")
	ErrLifecycleOutputInvalid        = errors.New("argoworkflow: lifecycle output is invalid")
	ErrLifecycleOutputNonCanonical   = errors.New("argoworkflow: lifecycle output is not canonical")
	ErrLifecycleOutputDigestMismatch = errors.New("argoworkflow: lifecycle output digest mismatch")
	ErrLifecycleOutputBinding        = errors.New("argoworkflow: lifecycle output binding mismatch")
	ErrLifecycleOutputUnsupported    = errors.New("argoworkflow: lifecycle output contains unsupported data")
)

// LifecycleOutput is the pre-Gate handoff produced by an Argo lifecycle.
//
// It is deliberately not a Gate result. The controller may use a valid output
// to populate the immutable inputs for its own verifier, but it must still run
// the AGW-owned Gate before it can enter Gated, Publishing, or a terminal
// success state.
type LifecycleOutput struct {
	SchemaVersion          int                    `json:"schemaVersion"`
	Kind                   string                 `json:"kind"`
	RunUID                 string                 `json:"runUID"`
	RunGeneration          int64                  `json:"runGeneration"`
	SpecDigest             string                 `json:"specDigest"`
	WorkflowTemplateUID    string                 `json:"workflowTemplateUID"`
	WorkflowTemplateDigest string                 `json:"workflowTemplateDigest"`
	WorkflowUID            string                 `json:"workflowUID"`
	WorkflowGeneration     int64                  `json:"workflowGeneration"`
	BaseSHA                string                 `json:"baseSHA"`
	Patch                  LifecyclePatchOutput   `json:"patch"`
	Runtime                RuntimeCompletionProof `json:"runtime"`
	Verification           VerificationInputs     `json:"verification"`
}

// LifecyclePatchOutput binds both patch objects needed by AGW publication.
// Digest is repeated intentionally: it makes the contract self-describing and
// lets a consumer reject a ref whose digest was changed independently.
type LifecyclePatchOutput struct {
	Ref          v1alpha1.ArtifactRef `json:"ref"`
	ManifestRef  v1alpha1.ArtifactRef `json:"manifestRef"`
	Digest       string               `json:"digest"`
	FilesChanged int64                `json:"filesChanged"`
	LinesChanged int64                `json:"linesChanged"`
}

// RuntimeCompletionProof contains references to the independently persisted
// event stream and completion record plus the terminal event identity. The
// contract does not accept a free-form "completed: true" flag.
type RuntimeCompletionProof struct {
	EventStreamRef      v1alpha1.ArtifactRef `json:"eventStreamRef"`
	CompletionRef       v1alpha1.ArtifactRef `json:"completionRef"`
	TerminalEvent       string               `json:"terminalEvent"`
	TerminalSequence    uint64               `json:"terminalSequence"`
	TerminalEventDigest string               `json:"terminalEventDigest"`
	CompletionDigest    string               `json:"completionDigest"`
}

// VerificationInputs are the immutable inputs the AGW-owned verifier must
// consume after the workflow handoff. They carry no Gate verdict or effect
// state; those decisions stay outside the Argo output contract.
type VerificationInputs struct {
	InputRef   v1alpha1.ArtifactRef `json:"inputRef"`
	PatchRef   v1alpha1.ArtifactRef `json:"patchRef"`
	BaseSHA    string               `json:"baseSHA"`
	SpecDigest string               `json:"specDigest"`
}

// BoundLifecycleOutput is the validated immutable handoff. Canonical is a
// defensive copy of the exact bytes hashed by Digest. A caller may persist it
// using create-if-absent semantics; it must not reconstruct the contract from
// individual fields.
type BoundLifecycleOutput struct {
	Contract  LifecycleOutput
	Canonical []byte
	Digest    string
}

// MarshalLifecycleOutput validates and encodes one output in its canonical
// representation. It is also the producer-side helper used by tests and
// Workflow-template generators; the consumer still validates the raw Argo
// parameter independently.
func MarshalLifecycleOutput(output LifecycleOutput) ([]byte, error) {
	if err := validateLifecycleOutputShape(output); err != nil {
		return nil, err
	}
	body, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrLifecycleOutputInvalid, err)
	}
	if len(body) > MaxLifecycleOutputBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrLifecycleOutputTooLarge, len(body), MaxLifecycleOutputBytes)
	}
	if err := strictjson.ValidateObject(body); err != nil {
		return nil, fmt.Errorf("%w: encoded JSON: %v", ErrLifecycleOutputInvalid, err)
	}
	normalized, err := strictjson.Normalize(body)
	if err != nil {
		return nil, fmt.Errorf("%w: normalize: %v", ErrLifecycleOutputInvalid, err)
	}
	return normalized, nil
}

// DigestLifecycleOutput returns the content address of the canonical output
// bytes. The digest covers the complete contract and no mutable Workflow
// status field.
func DigestLifecycleOutput(output LifecycleOutput) (string, error) {
	body, err := MarshalLifecycleOutput(output)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// BindLifecycleOutput extracts, strictly decodes, and identity-binds the
// reserved Workflow output. It accepts exactly one parameter with the
// reserved name and rejects duplicate parameter/artifact names, malformed
// output collections, oversized data, non-canonical JSON, and all identity
// mismatches. It intentionally requires the caller to pass the already-bound
// Workflow reference; a same-name Workflow cannot be used as a substitute.
func BindLifecycleOutput(workflow *unstructured.Unstructured, expected Binding) (BoundLifecycleOutput, error) {
	var zero BoundLifecycleOutput
	if workflow == nil {
		return zero, fmt.Errorf("%w: Workflow is nil", ErrLifecycleOutputInvalid)
	}
	if err := validateWorkflowIdentity(workflow, expected); err != nil {
		return zero, fmt.Errorf("%w: Workflow identity: %v", ErrLifecycleOutputBinding, err)
	}
	outputs, found, err := unstructured.NestedMap(workflow.Object, "status", "outputs")
	if err != nil {
		return zero, fmt.Errorf("%w: outputs object: %v", ErrLifecycleOutputInvalid, err)
	}
	if !found {
		return zero, ErrLifecycleOutputMissing
	}
	encodedOutputs, err := json.Marshal(outputs)
	if err != nil {
		return zero, fmt.Errorf("%w: encode outputs: %v", ErrLifecycleOutputInvalid, err)
	}
	if len(encodedOutputs) > maxWorkflowOutputsBytes {
		return zero, fmt.Errorf("%w: workflow outputs exceed %d bytes", ErrLifecycleOutputTooLarge, maxWorkflowOutputsBytes)
	}

	value, err := reservedParameter(outputs)
	if err != nil {
		return zero, err
	}
	if len(value) > MaxLifecycleOutputBytes {
		return zero, fmt.Errorf("%w: parameter exceeds %d bytes", ErrLifecycleOutputTooLarge, MaxLifecycleOutputBytes)
	}
	if err := strictjson.ValidateObject([]byte(value)); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrLifecycleOutputInvalid, err)
	}
	normalized, err := strictjson.Normalize([]byte(value))
	if err != nil {
		return zero, fmt.Errorf("%w: normalize: %v", ErrLifecycleOutputInvalid, err)
	}
	if !bytes.Equal(normalized, []byte(value)) {
		return zero, ErrLifecycleOutputNonCanonical
	}

	contract, err := decodeLifecycleOutput(normalized)
	if err != nil {
		return zero, err
	}
	if err := bindLifecycleOutput(contract, workflow, expected); err != nil {
		return zero, err
	}
	sum := sha256.Sum256(normalized)
	return BoundLifecycleOutput{
		Contract:  contract,
		Canonical: append([]byte(nil), normalized...),
		Digest:    canonical.DigestPrefix + hex.EncodeToString(sum[:]),
	}, nil
}

func reservedParameter(outputs map[string]any) (string, error) {
	parameters, parametersFound := outputs["parameters"]
	artifacts, artifactsFound := outputs["artifacts"]
	if parametersFound {
		if err := validateOutputList(parameters, "parameter"); err != nil {
			return "", err
		}
	}
	if artifactsFound {
		if err := validateOutputList(artifacts, "artifact"); err != nil {
			return "", err
		}
		if containsOutputName(artifacts, LifecycleOutputParameterName) {
			return "", ErrLifecycleOutputDuplicate
		}
	}
	if !parametersFound {
		return "", ErrLifecycleOutputMissing
	}
	values, ok := parameters.([]any)
	if !ok {
		return "", fmt.Errorf("%w: parameters is not a list", ErrLifecycleOutputInvalid)
	}
	matches := 0
	value := ""
	for _, raw := range values {
		parameter, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%w: parameter is not an object", ErrLifecycleOutputInvalid)
		}
		if parameter["name"] != LifecycleOutputParameterName {
			continue
		}
		matches++
		if len(parameter) != 2 {
			return "", fmt.Errorf("%w: reserved parameter contains unsupported fields", ErrLifecycleOutputUnsupported)
		}
		candidate, ok := parameter["value"].(string)
		if !ok || candidate == "" {
			return "", fmt.Errorf("%w: reserved parameter value is missing or not a string", ErrLifecycleOutputInvalid)
		}
		value = candidate
	}
	if matches == 0 {
		return "", ErrLifecycleOutputMissing
	}
	if matches != 1 {
		return "", ErrLifecycleOutputDuplicate
	}
	return value, nil
}

func validateOutputList(raw any, kind string) error {
	items, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%w: %s outputs is not a list", ErrLifecycleOutputInvalid, kind)
	}
	if len(items) > maxWorkflowOutputItems {
		return fmt.Errorf("%w: %s output count exceeds %d", ErrLifecycleOutputTooLarge, kind, maxWorkflowOutputItems)
	}
	seen := make(map[string]struct{}, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s output is not an object", ErrLifecycleOutputInvalid, kind)
		}
		name, ok := item["name"].(string)
		if !ok || name == "" || len(name) > 128 {
			return fmt.Errorf("%w: %s output name is invalid", ErrLifecycleOutputInvalid, kind)
		}
		if _, exists := seen[name]; exists {
			return ErrLifecycleOutputDuplicate
		}
		seen[name] = struct{}{}
	}
	return nil
}

func containsOutputName(raw any, name string) bool {
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemName, nameOK := item["name"].(string)
		if nameOK && itemName == name {
			return true
		}
	}
	return false
}

func decodeLifecycleOutput(body []byte) (LifecycleOutput, error) {
	var zero LifecycleOutput
	keys, err := objectKeys(body)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrLifecycleOutputInvalid, err)
	}
	if err := requireExactKeys(keys, []string{"baseSHA", "kind", "patch", "runGeneration", "runUID", "runtime", "schemaVersion", "specDigest", "verification", "workflowGeneration", "workflowTemplateDigest", "workflowTemplateUID", "workflowUID"}); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var output LifecycleOutput
	if err := decoder.Decode(&output); err != nil {
		return zero, fmt.Errorf("%w: decode: %v", ErrLifecycleOutputInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return zero, fmt.Errorf("%w: multiple JSON values", ErrLifecycleOutputInvalid)
	}
	if err := validateLifecycleOutputShape(output); err != nil {
		return zero, err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return zero, ErrLifecycleOutputNonCanonical
	}
	canonicalBody, err := strictjson.Normalize(encoded)
	if err != nil || !bytes.Equal(canonicalBody, body) {
		return zero, ErrLifecycleOutputNonCanonical
	}
	return output, nil
}

func objectKeys(body []byte) (map[string]struct{}, error) {
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("multiple JSON values")
	}
	keys := make(map[string]struct{}, len(object))
	for key := range object {
		keys[key] = struct{}{}
	}
	return keys, nil
}

func requireExactKeys(got map[string]struct{}, required []string) error {
	want := make(map[string]struct{}, len(required))
	for _, key := range required {
		want[key] = struct{}{}
	}
	if len(got) != len(want) {
		return fmt.Errorf("%w: top-level fields are not closed", ErrLifecycleOutputUnsupported)
	}
	for key := range want {
		if _, ok := got[key]; !ok {
			return fmt.Errorf("%w: required field %q is missing", ErrLifecycleOutputMissing, key)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			return fmt.Errorf("%w: field %q is unsupported", ErrLifecycleOutputUnsupported, key)
		}
	}
	return nil
}

func validateLifecycleOutputShape(output LifecycleOutput) error {
	if output.SchemaVersion != LifecycleOutputSchemaVersion || output.Kind != LifecycleOutputKind {
		return fmt.Errorf("%w: schema version or kind is unsupported", ErrLifecycleOutputInvalid)
	}
	if !validRunUID(output.RunUID) || output.RunGeneration <= 0 || !validRunUID(output.WorkflowTemplateUID) || !canonical.ValidDigest(output.WorkflowTemplateDigest) || !validRunUID(output.WorkflowUID) || output.WorkflowGeneration <= 0 {
		return fmt.Errorf("%w: run or Workflow identity is invalid", ErrLifecycleOutputInvalid)
	}
	if !canonical.ValidDigest(output.SpecDigest) || !resolved.ValidBaseSHA(output.BaseSHA) {
		return fmt.Errorf("%w: spec digest or base SHA is invalid", ErrLifecycleOutputInvalid)
	}
	if err := validatePatchOutput(output.Patch); err != nil {
		return err
	}
	if err := validateRuntimeProof(output.Runtime); err != nil {
		return err
	}
	if err := validateVerificationInputs(output.Verification, output.Patch, output.SpecDigest, output.BaseSHA); err != nil {
		return err
	}
	return nil
}

func validatePatchOutput(output LifecyclePatchOutput) error {
	if output.FilesChanged < 0 || output.LinesChanged < 0 || !canonical.ValidDigest(output.Digest) || output.Ref.Digest != output.Digest {
		return fmt.Errorf("%w: patch digest or counters are invalid", ErrLifecycleOutputInvalid)
	}
	if err := validateOutputArtifact(output.Ref, "patch"); err != nil {
		return err
	}
	if err := validateOutputArtifact(output.ManifestRef, "patch-manifest"); err != nil {
		return err
	}
	return nil
}

func validateRuntimeProof(proof RuntimeCompletionProof) error {
	if proof.TerminalEvent != "run.completed" || proof.TerminalSequence == 0 || !canonical.ValidDigest(proof.TerminalEventDigest) || !canonical.ValidDigest(proof.CompletionDigest) || proof.CompletionRef.Digest != proof.CompletionDigest {
		return fmt.Errorf("%w: terminal completion evidence is invalid", ErrLifecycleOutputInvalid)
	}
	if err := validateOutputArtifact(proof.EventStreamRef, "runtime-event-stream"); err != nil {
		return err
	}
	if err := validateOutputArtifact(proof.CompletionRef, "runtime-completion"); err != nil {
		return err
	}
	return nil
}

func validateVerificationInputs(inputs VerificationInputs, patch LifecyclePatchOutput, specDigest, baseSHA string) error {
	if inputs.BaseSHA != baseSHA || inputs.SpecDigest != specDigest || inputs.PatchRef != patch.Ref {
		return fmt.Errorf("%w: verification inputs do not bind patch/base/spec", ErrLifecycleOutputBinding)
	}
	if err := validateOutputArtifact(inputs.InputRef, "verification-input"); err != nil {
		return err
	}
	if err := validateOutputArtifact(inputs.PatchRef, "patch"); err != nil {
		return err
	}
	return nil
}

func bindLifecycleOutput(output LifecycleOutput, workflow *unstructured.Unstructured, expected Binding) error {
	if output.RunUID != expected.RunUID || output.RunGeneration != expected.RunGeneration || output.SpecDigest != expected.SpecDigest || output.BaseSHA != expected.BaseSHA || output.WorkflowTemplateUID != expected.WorkflowTemplateUID || output.WorkflowTemplateDigest != expected.WorkflowTemplateDigest {
		return fmt.Errorf("%w: AgentRun identity does not match binding", ErrLifecycleOutputBinding)
	}
	if output.WorkflowUID != string(workflow.GetUID()) || output.WorkflowGeneration != workflow.GetGeneration() {
		return fmt.Errorf("%w: Workflow identity does not match live object", ErrLifecycleOutputBinding)
	}
	return nil
}

func validateOutputArtifact(ref v1alpha1.ArtifactRef, kind string) error {
	if ref.Kind != kind || ref.URI == "" || len(ref.URI) > 1024 || ref.SizeBytes <= 0 || ref.SizeBytes > 1<<40 || !canonical.ValidDigest(ref.Digest) || !safeArtifactURI(ref.URI) {
		return fmt.Errorf("%w: %s artifact reference is invalid", ErrLifecycleOutputInvalid, kind)
	}
	if ref.Name == "" || len(ref.Name) > 128 || ref.MediaType == "" || len(ref.MediaType) > 128 || strings.ContainsAny(ref.Name+ref.MediaType, "\x00\r\n") {
		return fmt.Errorf("%w: %s artifact descriptor is incomplete", ErrLifecycleOutputInvalid, kind)
	}
	return nil
}
