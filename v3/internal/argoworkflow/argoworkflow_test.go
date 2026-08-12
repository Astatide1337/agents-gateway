package argoworkflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestTranslateIsDeterministicAndCredentialFree(t *testing.T) {
	input := fixture(t)
	first, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate(first): %v", err)
	}
	second, err := Translate(input)
	if err != nil {
		t.Fatalf("Translate(second): %v", err)
	}
	firstBytes, err := NormalizedBytes(first.Workflow)
	if err != nil {
		t.Fatalf("NormalizedBytes(first): %v", err)
	}
	secondBytes, err := NormalizedBytes(second.Workflow)
	if err != nil {
		t.Fatalf("NormalizedBytes(second): %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("retry translation changed bytes:\n%s\n%s", firstBytes, secondBytes)
	}
	if first.Binding != second.Binding {
		t.Fatalf("retry binding changed: %#v vs %#v", first.Binding, second.Binding)
	}

	encoded, err := json.Marshal(first.Workflow.Object)
	if err != nil {
		t.Fatalf("marshal workflow: %v", err)
	}
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "credential-value") {
		t.Fatalf("workflow contains credential material: %s", encoded)
	}
	if first.Workflow.GetAPIVersion() != WorkflowAPIVersion || first.Workflow.GetKind() != WorkflowKind {
		t.Fatalf("unexpected GVK: %s/%s", first.Workflow.GetAPIVersion(), first.Workflow.GetKind())
	}
	if first.Workflow.GetName() != workflowName(string(input.Run.UID)) {
		t.Fatalf("workflow name=%q, want %q", first.Workflow.GetName(), workflowName(string(input.Run.UID)))
	}
	if len(first.Workflow.GetOwnerReferences()) != 1 || first.Workflow.GetOwnerReferences()[0].UID != input.Run.UID {
		t.Fatalf("owner reference=%#v", first.Workflow.GetOwnerReferences())
	}
	if first.Workflow.GetAnnotations()[resolvedRefHashAnnotation] != resolvedRefHash(input.ResolvedRef) {
		t.Fatalf("resolved ref hash annotation=%q, want %q", first.Workflow.GetAnnotations()[resolvedRefHashAnnotation], resolvedRefHash(input.ResolvedRef))
	}
	assertClosedWorkflowShape(t, first.Workflow)
}

func TestTranslateProducesOnlyOperatorControlledWorkflowInputs(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow

	if _, found, _ := unstructured.NestedFieldNoCopy(workflow.Object, "status"); found {
		t.Fatal("translated Workflow contains backend status")
	}
	parameters, found, err := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
	if err != nil || !found {
		t.Fatalf("parameters: found=%v err=%v", found, err)
	}
	for index, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("parameter %d has type %T", index, raw)
		}
		if _, found := parameter["valueFrom"]; found {
			t.Fatalf("parameter %d contains valueFrom", index)
		}
		if strings.Contains(strings.ToLower(string(mustJSON(t, parameter))), "secret") || strings.Contains(strings.ToLower(string(mustJSON(t, parameter))), "credential") {
			t.Fatalf("parameter %d contains credential material: %#v", index, parameter)
		}
	}
}

func TestObserveAcceptsBackendPhasesWithoutDomainClaims(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	for _, test := range []struct {
		raw      string
		phase    BackendPhase
		terminal bool
	}{
		{raw: "", phase: BackendPending},
		{raw: "Pending", phase: BackendPending},
		{raw: "Running", phase: BackendRunning},
		{raw: "Succeeded", phase: BackendSucceeded, terminal: true},
		{raw: "Failed", phase: BackendFailed, terminal: true},
		{raw: "Error", phase: BackendError, terminal: true},
		{raw: "Skipped", phase: BackendSkipped, terminal: true},
		{raw: "Omitted", phase: BackendOmitted, terminal: true},
		{raw: "Suspended", phase: BackendSuspended},
	} {
		t.Run(test.raw, func(t *testing.T) {
			workflow := translation.Workflow.DeepCopy()
			workflow.SetUID(types.UID("workflow-uid-123"))
			workflow.SetGeneration(4)
			if test.raw != "" {
				status := map[string]any{"phase": test.raw, "message": "backend observation"}
				if test.phase == BackendSucceeded {
					status = lifecycleStatus(t, workflow, translation.Binding)
				}
				workflow.Object["status"] = status
			}
			observation, err := Observe(workflow, translation.Binding)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if observation.Phase != test.phase || observation.RawPhase != test.raw || observation.Terminal != test.terminal {
				t.Fatalf("observation=%#v", observation)
			}
			wantMessage := ""
			if test.raw != "" {
				wantMessage = "backend observation"
			}
			if observation.Reference.UID != workflow.GetUID() || observation.ObservedGeneration != 4 || observation.Message != wantMessage {
				t.Fatalf("observation reference/metadata=%#v", observation)
			}
		})
	}
}

func TestObserveRejectsUnknownTerminalPhase(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(1)
	workflow.Object["status"] = map[string]any{"phase": "Terminated"}
	_, err = Observe(workflow, translation.Binding)
	if !errors.Is(err, ErrUnknownPhase) {
		t.Fatalf("Observe error=%v, want ErrUnknownPhase", err)
	}
}

func TestObserveRejectsIdentityAndContractTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{name: "wrong workflow name", mutate: func(workflow *unstructured.Unstructured) { workflow.SetName("agw-tampered") }},
		{name: "wrong workflow uid is allowed only backend uid", mutate: func(workflow *unstructured.Unstructured) { workflow.SetUID(types.UID("")) }},
		{name: "wrong owner uid", mutate: func(workflow *unstructured.Unstructured) {
			owners := workflow.GetOwnerReferences()
			owners[0].UID = types.UID("foreign-run")
			workflow.SetOwnerReferences(owners)
		}},
		{name: "wrong owner api version", mutate: func(workflow *unstructured.Unstructured) {
			owners := workflow.GetOwnerReferences()
			owners[0].APIVersion = "v1"
			workflow.SetOwnerReferences(owners)
		}},
		{name: "missing controller owner", mutate: func(workflow *unstructured.Unstructured) {
			owners := workflow.GetOwnerReferences()
			value := false
			owners[0].Controller = &value
			workflow.SetOwnerReferences(owners)
		}},
		{name: "missing owner deletion protection", mutate: func(workflow *unstructured.Unstructured) {
			owners := workflow.GetOwnerReferences()
			value := false
			owners[0].BlockOwnerDeletion = &value
			workflow.SetOwnerReferences(owners)
		}},
		{name: "wrong run annotation", mutate: func(workflow *unstructured.Unstructured) {
			annotations := workflow.GetAnnotations()
			annotations[runUIDAnnotation] = "foreign-run"
			workflow.SetAnnotations(annotations)
		}},
		{name: "wrong digest label", mutate: func(workflow *unstructured.Unstructured) {
			labels := workflow.GetLabels()
			labels[specDigestHashLabel] = strings.Repeat("f", 64)
			workflow.SetLabels(labels)
		}},
		{name: "wrong template", mutate: func(workflow *unstructured.Unstructured) {
			ref, _, _ := unstructured.NestedMap(workflow.Object, "spec", "workflowTemplateRef")
			ref["name"] = "foreign-template"
			_ = unstructured.SetNestedMap(workflow.Object, ref, "spec", "workflowTemplateRef")
		}},
		{name: "extra parameter", mutate: func(workflow *unstructured.Unstructured) {
			parameters, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
			parameters = append(parameters, map[string]any{"name": "secret", "value": "credential-value"})
			_ = unstructured.SetNestedSlice(workflow.Object, parameters, "spec", "arguments", "parameters")
		}},
		{name: "valueFrom parameter", mutate: func(workflow *unstructured.Unstructured) {
			parameters, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
			parameter := parameters[0].(map[string]any)
			delete(parameter, "value")
			parameter["valueFrom"] = map[string]any{"secretKeyRef": map[string]any{"name": "secret"}}
			_ = unstructured.SetNestedSlice(workflow.Object, parameters, "spec", "arguments", "parameters")
		}},
		{name: "wrong resolved uri", mutate: func(workflow *unstructured.Unstructured) {
			parameters, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "arguments", "parameters")
			parameter := parameters[3].(map[string]any)
			parameter["value"] = "s3://foreign/bad.json"
			_ = unstructured.SetNestedSlice(workflow.Object, parameters, "spec", "arguments", "parameters")
		}},
		{name: "wrong resolved ref hash", mutate: func(workflow *unstructured.Unstructured) {
			annotations := workflow.GetAnnotations()
			annotations[resolvedRefHashAnnotation] = strings.Repeat("f", 64)
			workflow.SetAnnotations(annotations)
		}},
		{name: "extra workflow spec field", mutate: func(workflow *unstructured.Unstructured) {
			workflow.Object["spec"].(map[string]any)["serviceAccountName"] = "foreign"
		}},
		{name: "extra workflow arguments field", mutate: func(workflow *unstructured.Unstructured) {
			workflow.Object["spec"].(map[string]any)["arguments"].(map[string]any)["parametersFrom"] = []any{}
		}},
		{name: "extra template ref field", mutate: func(workflow *unstructured.Unstructured) {
			workflow.Object["spec"].(map[string]any)["workflowTemplateRef"].(map[string]any)["clusterScope"] = false
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			translation, err := Translate(fixture(t))
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			workflow := translation.Workflow.DeepCopy()
			workflow.SetUID(types.UID("workflow-uid-123"))
			test.mutate(workflow)
			if _, err := Observe(workflow, translation.Binding); err == nil {
				t.Fatal("Observe unexpectedly accepted tampered Workflow")
			}
		})
	}
}

func TestObserveBindsLiveWorkflowUIDWhenProvided(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(1)

	wrong := translation.Binding
	wrong.Reference.UID = types.UID("workflow-uid-foreign")
	wrong.Reference.Generation = 1
	if _, err := Observe(workflow, wrong); !errors.Is(err, ErrBinding) {
		t.Fatalf("Observe with foreign bound UID error=%v, want ErrBinding", err)
	}

	right := translation.Binding
	right.Reference.UID = workflow.GetUID()
	right.Reference.Generation = 1
	if _, err := Observe(workflow, right); err != nil {
		t.Fatalf("Observe with matching bound UID: %v", err)
	}
}

func TestObserveBindsCompleteResolvedRef(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.SetGeneration(1)

	foreignRef := translation.Binding
	foreignRef.ResolvedRef.Name = "foreign-resolved-spec.json"
	if _, err := Observe(workflow, foreignRef); !errors.Is(err, ErrBinding) {
		t.Fatalf("Observe with foreign resolved-ref metadata error=%v, want ErrBinding", err)
	}
}

func TestObserveRejectsUnboundedStatusMessage(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	workflow := translation.Workflow.DeepCopy()
	workflow.SetUID(types.UID("workflow-uid-123"))
	workflow.Object["status"] = map[string]any{
		"phase":   "Running",
		"message": strings.Repeat("x", v1alpha1.MaxStatusMessage+1),
	}
	if _, err := Observe(workflow, translation.Binding); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("Observe error=%v, want ErrInvalidObservation", err)
	}
}

func TestObserveRejectsMalformedStatusFields(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
	}{
		{name: "status is not an object", status: nil},
		{name: "phase is not a string", status: map[string]any{"phase": true}},
		{name: "message is not a string", status: map[string]any{"phase": "Running", "message": 7}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			translation, err := Translate(fixture(t))
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			workflow := translation.Workflow.DeepCopy()
			workflow.SetUID(types.UID("workflow-uid-123"))
			if test.status == nil {
				workflow.Object["status"] = []any{"not-an-object"}
			} else {
				workflow.Object["status"] = test.status
			}
			if _, err := Observe(workflow, translation.Binding); !errors.Is(err, ErrInvalidObservation) {
				t.Fatalf("Observe error=%v, want ErrInvalidObservation", err)
			}
		})
	}
}

func TestTranslateRequiresCurrentAdmissionGeneration(t *testing.T) {
	input := fixture(t)
	input.Run.Status.Conditions[0].ObservedGeneration--
	if _, err := Translate(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Translate error=%v, want ErrInvalidInput", err)
	}

	input = fixture(t)
	input.Run.Status.Conditions = append(input.Run.Status.Conditions, v1alpha1.Condition{
		Type:               string(v1alpha1.ConditionAdmitted),
		Status:             v1alpha1.ConditionTrue,
		ObservedGeneration: input.Run.Generation,
	})
	if _, err := Translate(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Translate with duplicate admission condition error=%v, want ErrInvalidInput", err)
	}
}

func TestTranslateFailsClosedForInputTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inputs)
	}{
		{name: "not admitted", mutate: func(input *Inputs) { input.Run.Status.Conditions[0].Status = v1alpha1.ConditionFalse }},
		{name: "status digest", mutate: func(input *Inputs) { input.Run.Status.SpecDigest = "sha256:" + strings.Repeat("f", 64) }},
		{name: "status base sha", mutate: func(input *Inputs) { input.Run.Status.BaseSHA = strings.Repeat("b", 40) }},
		{name: "snapshot base sha", mutate: func(input *Inputs) { input.Snapshot.BaseSHA = strings.Repeat("b", 40) }},
		{name: "snapshot run uid", mutate: func(input *Inputs) { input.Snapshot.Run.UID = "foreign-run" }},
		{name: "resolved digest", mutate: func(input *Inputs) { input.ResolvedRef.Digest = "sha256:" + strings.Repeat("f", 64) }},
		{name: "resolved uri userinfo", mutate: func(input *Inputs) { input.ResolvedRef.URI = "https://user:pass@example.test/resolved.json" }},
		{name: "wrong namespace", mutate: func(input *Inputs) { input.Config.Namespace = "other" }},
		{name: "arbitrary template", mutate: func(input *Inputs) { input.Config.WorkflowTemplateName = "../template" }},
		{name: "unsafe uid", mutate: func(input *Inputs) { input.Run.UID = types.UID("uid/with/slash") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := fixture(t)
			test.mutate(&input)
			if _, err := Translate(input); err == nil {
				t.Fatal("Translate unexpectedly accepted invalid input")
			}
		})
	}
}

func TestNormalizedBytesStripsOnlyManagedFields(t *testing.T) {
	translation, err := Translate(fixture(t))
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	first := translation.Workflow.DeepCopy()
	second := translation.Workflow.DeepCopy()
	first.SetUID(types.UID("uid-one"))
	first.SetResourceVersion("1")
	first.SetGeneration(1)
	first.SetCreationTimestamp(metav1.Now())
	first.Object["status"] = map[string]any{"phase": "Running"}
	second.SetUID(types.UID("uid-two"))
	second.SetResourceVersion("2")
	second.SetGeneration(9)
	second.SetCreationTimestamp(metav1.Now())
	second.Object["status"] = map[string]any{"phase": "Failed"}
	firstBytes, err := NormalizedBytes(first)
	if err != nil {
		t.Fatalf("NormalizedBytes(first): %v", err)
	}
	secondBytes, err := NormalizedBytes(second)
	if err != nil {
		t.Fatalf("NormalizedBytes(second): %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatalf("managed fields changed normalized bytes:\n%s\n%s", firstBytes, secondBytes)
	}
}

func TestNormalizedBytesRejectsMalformedUnstructuredValues(t *testing.T) {
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": WorkflowAPIVersion,
		"kind":       WorkflowKind,
		"metadata":   map[string]any{"name": "workflow"},
		"spec":       map[string]any{"unsupported": int(1)},
	}}
	if _, err := NormalizedBytes(workflow); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NormalizedBytes error=%v, want ErrInvalidInput", err)
	}
}

func assertClosedWorkflowShape(t *testing.T, workflow *unstructured.Unstructured) {
	t.Helper()
	if len(workflow.Object) != 4 {
		t.Fatalf("top-level Workflow keys=%v, want exactly apiVersion/kind/metadata/spec", sortedKeys(workflow.Object))
	}
	metadata, found, err := unstructured.NestedMap(workflow.Object, "metadata")
	if err != nil || !found || len(metadata) != 5 {
		t.Fatalf("metadata keys=%v found=%v err=%v, want exactly five generated fields", sortedKeys(metadata), found, err)
	}
	spec, found, err := unstructured.NestedMap(workflow.Object, "spec")
	if err != nil || !found || len(spec) != 3 {
		t.Fatalf("spec keys=%v found=%v err=%v, want exactly activeDeadlineSeconds/workflowTemplateRef/arguments", sortedKeys(spec), found, err)
	}
	deadline, found, err := unstructured.NestedInt64(workflow.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found || deadline != 2700 {
		t.Fatalf("activeDeadlineSeconds=%d found=%v err=%v, want 2700", deadline, found, err)
	}
	ref, found, err := unstructured.NestedMap(spec, "workflowTemplateRef")
	if err != nil || !found || len(ref) != 1 {
		t.Fatalf("workflowTemplateRef=%#v found=%v err=%v, want only name", ref, found, err)
	}
	arguments, found, err := unstructured.NestedMap(spec, "arguments")
	if err != nil || !found || len(arguments) != 1 {
		t.Fatalf("arguments=%#v found=%v err=%v, want only parameters", arguments, found, err)
	}
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test value: %v", err)
	}
	return encoded
}

func fixture(t *testing.T) Inputs {
	t.Helper()
	runUID := types.UID("1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15")
	task := "fix the nil dereference"
	instructions := "Work only within the admitted scope."
	run := &v1alpha1.AgentRun{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version, Kind: "AgentRun"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "agw-runs",
			Name:       "jobmark-fix-427",
			UID:        runUID,
			Generation: 3,
		},
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "issue-fixer", GateRef: "go-default",
			Source:    v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Task:      v1alpha1.TaskSpec{Inline: &task},
			Scope:     v1alpha1.ScopeSpec{Paths: []string{"src/**", "tests/**"}, Forbidden: []string{"Dockerfile"}},
			Workspace: v1alpha1.WorkspaceSpec{Size: "8Gi"},
			Publish:   v1alpha1.PublishSpec{Mode: v1alpha1.PublishNone},
			Limits:    v1alpha1.LimitsSpec{Timeout: "45m", MaxToolCalls: 60, MaxCostUSD: "2.00"},
		},
	}
	baseSHA := strings.Repeat("a", 40)
	snapshot := resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		Spec:          *run.Spec.DeepCopy(),
		BaseSHA:       baseSHA,
		Task:          task,
		Agent: v1alpha1.AgentSpec{
			Runtime:      v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime-codex@sha256:" + strings.Repeat("1", 64)},
			Instructions: v1alpha1.InstructionsSpec{Inline: &instructions}, ToolSetRef: "github-readonly", ModelRouteRef: "default-codex", ContextStrategyRef: "go-context",
		},
		Instructions: instructions,
		Gate: v1alpha1.GateSpec{
			Verify:  v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + strings.Repeat("2", 64), FromCleanCheckout: true, Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m"},
			Require: v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 25, MaxDiffLines: 800, NoBinaryFiles: true},
			OnFail:  "Rejected", Mode: v1alpha1.GateShadow,
		},
		ToolSet: v1alpha1.ToolSetSpec{
			Servers:          []v1alpha1.ToolServer{{Name: "github", Ref: "https://mcp.example.test/mcp", CredentialsRef: "github-mcp", Tools: []v1alpha1.ToolDefinition{{Name: "get_me", Effect: v1alpha1.EffectRead}}}},
			Profiles:         []v1alpha1.ToolProfile{{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}}, {Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}}, {Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "get_me"}}}},
			MaxToolsPerPhase: 1,
		},
		ModelRoute:      v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "codex", Kind: "openai-responses", Model: "gpt-5.6-luna", Family: "openai", CredentialRef: "openai", Priority: 1}}, Budget: v1alpha1.ModelBudget{MaxCostUSD: "2.00"}},
		ContextStrategy: v1alpha1.ContextStrategySpec{RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 800}, Budget: v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 1048576}},
		References: resolved.References{
			Agent: v1alpha1ObjectVersion("issue-fixer", "agent-uid", "11"), Gate: v1alpha1ObjectVersion("go-default", "gate-uid", "12"), ToolSet: v1alpha1ObjectVersion("github-readonly", "tool-uid", "13"), ModelRoute: v1alpha1ObjectVersion("default-codex", "route-uid", "14"), ContextStrategy: v1alpha1ObjectVersion("go-context", "context-uid", "15"),
		},
	}
	canonicalSnapshot, err := canonical.CanonicalizeResolvedSpec(snapshot)
	if err != nil {
		t.Fatalf("canonicalize fixture: %v", err)
	}
	digest, err := canonical.ResolvedSpecDigest(canonicalSnapshot)
	if err != nil {
		t.Fatalf("digest fixture: %v", err)
	}
	ref := v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/runs/" + string(runUID) + "/resolved/" + strings.TrimPrefix(digest, "sha256:") + ".json", Digest: digest, Kind: "resolved-spec", Name: "resolved-spec.json", MediaType: "application/json"}
	run.Status = v1alpha1.AgentRunStatus{Phase: v1alpha1.PhasePending, SpecDigest: digest, ResolvedSpecRef: &ref, BaseSHA: baseSHA, Conditions: []v1alpha1.Condition{{Type: string(v1alpha1.ConditionAdmitted), Status: v1alpha1.ConditionTrue, ObservedGeneration: run.Generation}}}
	template := workflowTemplateFixture(run.Namespace, "agentrun-go-default", "template-uid-1")
	templateDigest, err := WorkflowTemplateContentDigest(template)
	if err != nil {
		t.Fatalf("template digest fixture: %v", err)
	}
	return Inputs{Run: run, Snapshot: snapshot, ResolvedRef: ref, Config: Config{
		Namespace:              run.Namespace,
		WorkflowTemplateName:   "agentrun-go-default",
		WorkflowTemplateUID:    "template-uid-1",
		WorkflowTemplateDigest: templateDigest,
	}}
}

func workflowTemplateFixture(namespace, name, uid string) *unstructured.Unstructured {
	template := workflowTemplateObject()
	template.SetNamespace(namespace)
	template.SetName(name)
	template.SetUID(types.UID(uid))
	template.Object["spec"] = map[string]any{"entrypoint": "lifecycle", "templates": []any{map[string]any{"name": "lifecycle"}}}
	return template
}

func v1alpha1ObjectVersion(name, uid, resourceVersion string) resolved.ObjectVersion {
	return resolved.ObjectVersion{Name: name, UID: uid, ResourceVersion: resourceVersion, Generation: 1}
}
