package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestBindLifecycleOutputAcceptsCanonicalContentAddressedContract(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(4)
	workflow.Object["status"] = lifecycleStatus(t, workflow, translation.Binding)

	bound, err := BindLifecycleOutput(workflow, translation.Binding)
	if err != nil {
		t.Fatalf("BindLifecycleOutput: %v", err)
	}
	if bound.Contract.RunUID != translation.Binding.RunUID || bound.Contract.RunGeneration != translation.Binding.RunGeneration || bound.Contract.SpecDigest != translation.Binding.SpecDigest || bound.Contract.BaseSHA != translation.Binding.BaseSHA {
		t.Fatalf("bound identity=%#v, want binding identity", bound.Contract)
	}
	if bound.Contract.WorkflowUID != string(workflow.GetUID()) || bound.Contract.WorkflowGeneration != workflow.GetGeneration() {
		t.Fatalf("bound Workflow identity=%s/%d, want %s/%d", bound.Contract.WorkflowUID, bound.Contract.WorkflowGeneration, workflow.GetUID(), workflow.GetGeneration())
	}
	body, err := MarshalLifecycleOutput(bound.Contract)
	if err != nil {
		t.Fatalf("MarshalLifecycleOutput: %v", err)
	}
	if !bytes.Equal(body, bound.Canonical) {
		t.Fatalf("bound canonical bytes changed: got %s want %s", bound.Canonical, body)
	}
	digest, err := DigestLifecycleOutput(bound.Contract)
	if err != nil {
		t.Fatalf("DigestLifecycleOutput: %v", err)
	}
	if bound.Digest != digest || !canonical.ValidDigest(bound.Digest) {
		t.Fatalf("bound digest=%q, recomputed=%q", bound.Digest, digest)
	}
}

func TestBindLifecycleOutputRejectsMissingOutput(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(4)
	workflow.Object["status"] = map[string]any{"phase": "Succeeded"}
	if _, err := BindLifecycleOutput(workflow, translation.Binding); !errors.Is(err, ErrLifecycleOutputMissing) {
		t.Fatalf("BindLifecycleOutput error=%v, want ErrLifecycleOutputMissing", err)
	}
}

func TestBindLifecycleOutputRejectsIdentityAndShapeTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(LifecycleOutput, *unstructured.Unstructured) (string, error)
		want   error
	}{
		{
			name: "wrong run uid",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.RunUID = "foreign-run"
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "wrong generation",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.RunGeneration++
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "wrong spec digest",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.SpecDigest = "sha256:" + strings.Repeat("f", 64)
				output.Verification.SpecDigest = output.SpecDigest
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "wrong Workflow uid",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.WorkflowUID = "foreign-workflow"
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "wrong Workflow generation",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.WorkflowGeneration++
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "wrong base SHA",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.BaseSHA = strings.Repeat("b", 40)
				output.Verification.BaseSHA = output.BaseSHA
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "patch digest does not match ref",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.Patch.Digest = "sha256:" + strings.Repeat("f", 64)
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputInvalid,
		},
		{
			name: "verification patch ref changed",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.Verification.PatchRef = output.Patch.ManifestRef
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputBinding,
		},
		{
			name: "missing completion evidence",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.Runtime.TerminalEvent = ""
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputInvalid,
		},
		{
			name: "oversized artifact descriptor",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				output.Patch.Ref.SizeBytes = 1<<40 + 1
				return marshalTestOutput(output)
			},
			want: ErrLifecycleOutputInvalid,
		},
		{
			name: "unsupported top-level field",
			mutate: func(output LifecycleOutput, workflow *unstructured.Unstructured) (string, error) {
				body, err := MarshalLifecycleOutput(output)
				if err != nil {
					return "", err
				}
				var object map[string]any
				if err := json.Unmarshal(body, &object); err != nil {
					return "", err
				}
				object["gateVerdict"] = "Accepted"
				encoded, err := json.Marshal(object)
				return string(encoded), err
			},
			want: ErrLifecycleOutputUnsupported,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := fixture(t)
			translation, err := Translate(input)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			workflow := translation.Workflow.DeepCopy()
			workflow.SetUID(types.UID("workflow-uid-123"))
			workflow.SetGeneration(4)
			output := testLifecycleOutput(translation.Binding, workflow)
			encoded, err := test.mutate(output, workflow)
			if err != nil {
				t.Fatalf("mutate output: %v", err)
			}
			workflow.Object["status"] = lifecycleStatusWithValue("Succeeded", encoded)
			if _, err := BindLifecycleOutput(workflow, translation.Binding); !errors.Is(err, test.want) {
				t.Fatalf("BindLifecycleOutput error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestBindLifecycleOutputRejectsDuplicateAndOversizedWorkflowOutputs(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(4)
	body, err := MarshalLifecycleOutput(testLifecycleOutput(translation.Binding, workflow))
	if err != nil {
		t.Fatalf("MarshalLifecycleOutput: %v", err)
	}

	tests := []struct {
		name   string
		status map[string]any
		want   error
	}{
		{
			name: "duplicate reserved parameter",
			status: map[string]any{
				"phase": "Succeeded",
				"outputs": map[string]any{"parameters": []any{
					map[string]any{"name": LifecycleOutputParameterName, "value": string(body)},
					map[string]any{"name": LifecycleOutputParameterName, "value": string(body)},
				}},
			},
			want: ErrLifecycleOutputDuplicate,
		},
		{
			name: "duplicate ordinary parameter",
			status: map[string]any{
				"phase": "Succeeded",
				"outputs": map[string]any{"parameters": []any{
					map[string]any{"name": LifecycleOutputParameterName, "value": string(body)},
					map[string]any{"name": "other", "value": "one"},
					map[string]any{"name": "other", "value": "two"},
				}},
			},
			want: ErrLifecycleOutputDuplicate,
		},
		{
			name: "reserved artifact name",
			status: map[string]any{
				"phase": "Succeeded",
				"outputs": map[string]any{
					"parameters": []any{map[string]any{"name": LifecycleOutputParameterName, "value": string(body)}},
					"artifacts":  []any{map[string]any{"name": LifecycleOutputParameterName}},
				},
			},
			want: ErrLifecycleOutputDuplicate,
		},
		{
			name: "too many parameters",
			status: map[string]any{
				"phase":   "Succeeded",
				"outputs": map[string]any{"parameters": outputParameters(body, maxWorkflowOutputItems+1)},
			},
			want: ErrLifecycleOutputTooLarge,
		},
		{
			name:   "oversized reserved value",
			status: lifecycleStatusWithValue("Succeeded", strings.Repeat("x", MaxLifecycleOutputBytes+1)),
			want:   ErrLifecycleOutputTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow.Object["status"] = test.status
			if _, err := BindLifecycleOutput(workflow, translation.Binding); !errors.Is(err, test.want) {
				t.Fatalf("BindLifecycleOutput error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestBindLifecycleOutputRejectsNonCanonicalAndDuplicateJSON(t *testing.T) {
	input := fixture(t)
	translation, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(4)
	body, err := MarshalLifecycleOutput(testLifecycleOutput(translation.Binding, workflow))
	if err != nil {
		t.Fatalf("MarshalLifecycleOutput: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("indent output: %v", err)
	}

	tests := []struct {
		name  string
		value string
		want  error
	}{
		{name: "pretty JSON", value: pretty.String(), want: ErrLifecycleOutputNonCanonical},
		{name: "duplicate top-level member", value: duplicateJSONMember(body), want: ErrLifecycleOutputInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workflow.Object["status"] = lifecycleStatusWithValue("Succeeded", test.value)
			if _, err := BindLifecycleOutput(workflow, translation.Binding); !errors.Is(err, test.want) {
				t.Fatalf("BindLifecycleOutput error=%v, want %v", err, test.want)
			}
		})
	}
}

func lifecycleStatus(t *testing.T, workflow *unstructured.Unstructured, binding Binding) map[string]any {
	t.Helper()
	body, err := MarshalLifecycleOutput(testLifecycleOutput(binding, workflow))
	if err != nil {
		t.Fatalf("MarshalLifecycleOutput: %v", err)
	}
	return lifecycleStatusWithValue("Succeeded", string(body))
}

func lifecycleStatusWithValue(phase, value string) map[string]any {
	return map[string]any{
		"phase":   phase,
		"message": "backend observation",
		"outputs": map[string]any{
			"parameters": []any{map[string]any{"name": LifecycleOutputParameterName, "value": value}},
		},
	}
}

func testLifecycleOutput(binding Binding, workflow *unstructured.Unstructured) LifecycleOutput {
	patch := testArtifactRef(binding.RunUID, "patch", "1")
	patchManifest := testArtifactRef(binding.RunUID, "patch-manifest", "2")
	eventStream := testArtifactRef(binding.RunUID, "runtime-event-stream", "3")
	completion := testArtifactRef(binding.RunUID, "runtime-completion", "4")
	verificationInput := testArtifactRef(binding.RunUID, "verification-input", "6")
	return LifecycleOutput{
		SchemaVersion: LifecycleOutputSchemaVersion,
		Kind:          LifecycleOutputKind,
		RunUID:        binding.RunUID, RunGeneration: binding.RunGeneration,
		SpecDigest: binding.SpecDigest, WorkflowTemplateUID: binding.WorkflowTemplateUID, WorkflowTemplateDigest: binding.WorkflowTemplateDigest,
		WorkflowUID: string(workflow.GetUID()), WorkflowGeneration: workflow.GetGeneration(),
		BaseSHA: binding.BaseSHA,
		Patch: LifecyclePatchOutput{
			Ref: patch, ManifestRef: patchManifest, Digest: patch.Digest,
			FilesChanged: 4, LinesChanged: 118,
		},
		Runtime: RuntimeCompletionProof{
			EventStreamRef: eventStream, CompletionRef: completion,
			TerminalEvent: "run.completed", TerminalSequence: 31,
			TerminalEventDigest: testDigest("5"), CompletionDigest: completion.Digest,
		},
		Verification: VerificationInputs{
			InputRef: verificationInput, PatchRef: patch,
			BaseSHA: binding.BaseSHA, SpecDigest: binding.SpecDigest,
		},
	}
}

func testArtifactRef(runUID, kind, digit string) v1alpha1.ArtifactRef {
	digest := testDigest(digit)
	return v1alpha1.ArtifactRef{
		URI:       fmt.Sprintf("s3://agw-artifacts/runs/%s/%s/%s.json", runUID, kind, strings.TrimPrefix(digest, "sha256:")),
		Digest:    digest,
		Kind:      kind,
		Name:      kind + ".json",
		MediaType: "application/json",
		SizeBytes: 1,
	}
}

func testDigest(digit string) string {
	return "sha256:" + strings.Repeat(digit, 64)
}

func marshalTestOutput(output LifecycleOutput) (string, error) {
	body, err := json.Marshal(output)
	if err != nil {
		return "", err
	}
	canonicalBody, err := strictjson.Normalize(body)
	if err != nil {
		return "", err
	}
	return string(canonicalBody), nil
}

func outputParameters(body []byte, count int) []any {
	parameters := make([]any, 0, count)
	parameters = append(parameters, map[string]any{"name": LifecycleOutputParameterName, "value": string(body)})
	for index := 1; index < count; index++ {
		parameters = append(parameters, map[string]any{"name": fmt.Sprintf("parameter-%d", index), "value": "x"})
	}
	return parameters
}

func duplicateJSONMember(body []byte) string {
	trimmed := strings.TrimSuffix(string(body), "}")
	return trimmed + `,"kind":"agents.astatide.com/argo-lifecycle-output"}`
}
