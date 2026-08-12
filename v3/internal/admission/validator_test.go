package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestValidatorDeniesMissingPreflight(t *testing.T) {
	scheme := testScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	validator := NewValidator(scheme, reader, time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
	validator.Clock = func() time.Time { return time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC) }
	response := validator.Handle(context.Background(), requestForRun(t))
	if response.Allowed {
		t.Fatal("missing preflight was allowed")
	}
}

func TestValidatorAllowsFreshPreflightAndReferences(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	scheme := testScheme(t)
	run := validRun()
	objects := []client.Object{
		preflightMap(now, "node"),
		&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"}, Spec: validAgentSpec()},
		validGate(),
		&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
		&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
		validContextStrategy(),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	validator := NewValidator(scheme, reader, 10*time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
	validator.Clock = func() time.Time { return now }
	response := validator.Handle(context.Background(), requestForRunObject(t, run))
	if !response.Allowed {
		t.Fatalf("valid run was denied: %v", response.Result)
	}
}

func TestValidatorEnforcesTaskContentAtAdmission(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		configure   func(*v1alpha1.AgentRun)
		taskData    map[string]string
		wantAllowed bool
		wantCode    Code
	}{
		{
			name: "empty inline task",
			configure: func(run *v1alpha1.AgentRun) {
				empty := " \t\n "
				run.Spec.Task = v1alpha1.TaskSpec{Inline: &empty}
			},
			wantCode: CodeTaskContentEmpty,
		},
		{
			name: "missing exact ConfigMap key",
			configure: func(run *v1alpha1.AgentRun) {
				ref := "task-input"
				run.Spec.Task = v1alpha1.TaskSpec{ConfigMapRef: &ref}
			},
			taskData: map[string]string{"not-task": "content"},
			wantCode: CodeConfigMapKeyMissing,
		},
		{
			name: "valid ConfigMap task",
			configure: func(run *v1alpha1.AgentRun) {
				ref := "task-input"
				run.Spec.Task = v1alpha1.TaskSpec{ConfigMapRef: &ref}
			},
			taskData:    map[string]string{resolved.TaskConfigMapKey: "fix the issue"},
			wantAllowed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := validRun()
			test.configure(run)
			objects := []client.Object{
				preflightMap(now, "node"),
				&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"}, Spec: validAgentSpec()},
				validGate(),
				&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
				&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
				validContextStrategy(),
			}
			if run.Spec.Task.ConfigMapRef != nil {
				objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: *run.Spec.Task.ConfigMapRef, Namespace: "agw-runs"}, Data: test.taskData})
			}
			scheme := testScheme(t)
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			validator := NewValidator(scheme, reader, 10*time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
			validator.Clock = func() time.Time { return now }
			response := validator.Handle(context.Background(), requestForRunObject(t, run))
			if response.Allowed != test.wantAllowed {
				t.Fatalf("allowed=%t, want %t; response=%#v", response.Allowed, test.wantAllowed, response.Result)
			}
			if !test.wantAllowed && (response.Result == nil || !strings.Contains(response.Result.Message, string(test.wantCode))) {
				t.Fatalf("response=%#v, want code %q", response.Result, test.wantCode)
			}
		})
	}
}

func TestValidatorAllowsCancellationWhenPreflightIsUnavailable(t *testing.T) {
	scheme := testScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	validator := NewValidator(scheme, reader, time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
	previous := validRun()
	current := previous.DeepCopy()
	current.Spec.CancelRequested = true
	response := validator.Handle(context.Background(), updateRequestForRuns(t, previous, current))
	if !response.Allowed {
		t.Fatalf("one-way cancellation was denied during degraded preflight: %v", response.Result)
	}
}

func TestValidatorDoesNotBypassImmutableSpecOnCancellation(t *testing.T) {
	scheme := testScheme(t)
	validator := NewValidator(scheme, fake.NewClientBuilder().WithScheme(scheme).Build(), time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
	previous := validRun()
	current := previous.DeepCopy()
	current.Spec.CancelRequested = true
	current.Spec.AgentRef = "different-agent"
	response := validator.Handle(context.Background(), updateRequestForRuns(t, previous, current))
	if response.Allowed {
		t.Fatal("cancellation with an immutable-spec mutation was allowed")
	}
	if response.Result == nil || !strings.Contains(response.Result.Message, string(CodeSpecImmutable)) {
		t.Fatalf("response=%#v, want %q", response.Result, CodeSpecImmutable)
	}
}

func TestValidatorFailsClosedWhenUpdateOmitsOldObject(t *testing.T) {
	scheme := testScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	validator := NewValidator(scheme, reader, time.Minute, preflight.Fingerprint{Node: "node", Runtime: "runtime"})
	request := requestForRun(t)
	request.Operation = admissionv1.Update
	response := validator.Handle(context.Background(), request)
	if response.Allowed || response.Result == nil || response.Result.Code != 400 {
		t.Fatalf("update without old object did not fail closed: %#v", response)
	}
}

func TestValidateAgentRunDynamicChecksConfigMapReferences(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	scheme := testScheme(t)
	taskConfigMap := "task-input"
	instructionsConfigMap := "agent-instructions"
	run := validRun()
	run.Spec.Task = v1alpha1.TaskSpec{ConfigMapRef: &taskConfigMap}
	objects := []client.Object{
		preflightMap(now, "node"),
		&v1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"},
			Spec: v1alpha1.AgentSpec{
				Runtime:            validAgentSpec().Runtime,
				Instructions:       v1alpha1.InstructionsSpec{ConfigMapRef: &instructionsConfigMap},
				ToolSetRef:         "readonly",
				ModelRouteRef:      "default",
				ContextStrategyRef: "default-context",
			},
		},
		validGate(),
		&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
		&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: taskConfigMap, Namespace: "agw-runs"}, Data: map[string]string{resolved.TaskConfigMapKey: "fix the issue"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: instructionsConfigMap, Namespace: "agw-runs"}, Data: map[string]string{resolved.InstructionsMapKey: "follow the repository conventions"}},
		validContextStrategy(),
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	err := ValidateAgentRunDynamic(context.Background(), reader, run, DynamicOptions{
		Now: now, PreflightTTL: 10 * time.Minute, ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
	})
	if err != nil {
		t.Fatalf("existing ConfigMaps were rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*v1alpha1.AgentRun, *v1alpha1.Agent)
		code Code
	}{
		{
			name: "missing task ConfigMap",
			edit: func(run *v1alpha1.AgentRun, _ *v1alpha1.Agent) {
				missing := "missing-task"
				run.Spec.Task.ConfigMapRef = &missing
			},
			code: CodeConfigMapMissing,
		},
		{
			name: "missing instructions ConfigMap",
			edit: func(_ *v1alpha1.AgentRun, agent *v1alpha1.Agent) {
				missing := "missing-instructions"
				agent.Spec.Instructions.ConfigMapRef = &missing
			},
			code: CodeConfigMapMissing,
		},
		{
			name: "invalid task ConfigMap reference",
			edit: func(run *v1alpha1.AgentRun, _ *v1alpha1.Agent) {
				invalid := "not/a-name"
				run.Spec.Task.ConfigMapRef = &invalid
			},
			code: CodeConfigMapReferenceInvalid,
		},
		{
			name: "missing task key",
			edit: func(_ *v1alpha1.AgentRun, _ *v1alpha1.Agent) {},
			code: CodeConfigMapKeyMissing,
		},
		{
			name: "empty task key",
			edit: func(_ *v1alpha1.AgentRun, _ *v1alpha1.Agent) {},
			code: CodeTaskContentEmpty,
		},
		{
			name: "oversized task key",
			edit: func(_ *v1alpha1.AgentRun, _ *v1alpha1.Agent) {},
			code: CodeTaskContentTooLong,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testRun := run.DeepCopy()
			testAgent := &v1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"},
				Spec: v1alpha1.AgentSpec{
					Runtime:      validAgentSpec().Runtime,
					Instructions: v1alpha1.InstructionsSpec{ConfigMapRef: &instructionsConfigMap},
					ToolSetRef:   "readonly", ModelRouteRef: "default",
					ContextStrategyRef: "default-context",
				},
			}
			test.edit(testRun, testAgent)
			taskData := map[string]string{resolved.TaskConfigMapKey: "fix the issue"}
			switch test.name {
			case "missing task key":
				taskData = map[string]string{"instructions": "wrong key"}
			case "empty task key":
				taskData = map[string]string{resolved.TaskConfigMapKey: " \t\n "}
			case "oversized task key":
				taskData = map[string]string{resolved.TaskConfigMapKey: strings.Repeat("x", v1alpha1.MaxTaskLength+1)}
			}
			testReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				preflightMap(now, "node"),
				testAgent,
				validGate(),
				&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
				&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: taskConfigMap, Namespace: "agw-runs"}, Data: taskData},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: instructionsConfigMap, Namespace: "agw-runs"}, Data: map[string]string{resolved.InstructionsMapKey: "follow the repository conventions"}},
				validContextStrategy(),
			).Build()
			violations := requireViolations(t, ValidateAgentRunDynamic(context.Background(), testReader, testRun, DynamicOptions{
				Now: now, PreflightTTL: 10 * time.Minute, ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
			}))
			if !hasCode(violations, test.code) {
				t.Fatalf("violations=%#v, want %q", violations, test.code)
			}
		})
	}
}

func TestValidateAgentRunDynamicValidatesResolvedToolSet(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	scheme := testScheme(t)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		preflightMap(now, "node"),
		&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"}, Spec: validAgentSpec()},
		validGate(),
		&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}, Spec: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{Ref: "https://127.0.0.1/mcp"}}}},
		&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
		validContextStrategy(),
	).Build()
	violations := requireViolations(t, ValidateAgentRunDynamic(context.Background(), reader, validRun(), DynamicOptions{
		Now: now, PreflightTTL: 10 * time.Minute, ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
	}))
	if !hasCode(violations, CodeMCPServerEndpointAddress) {
		t.Fatalf("violations=%#v, want ToolSet endpoint rejection", violations)
	}
}

func TestValidateAgentRunDynamicRejectsMutableOrPlaceholderImages(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	for _, image := range []string{"ghcr.io/astatide/runtime:latest", "ghcr.io/astatide/runtime@sha256:" + strings.Repeat("0", 64)} {
		t.Run(image, func(t *testing.T) {
			scheme := testScheme(t)
			agentSpec := validAgentSpec()
			agentSpec.Runtime.Image = image
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				preflightMap(now, "node"),
				&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"}, Spec: agentSpec},
				validGate(),
				&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
				&v1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"}},
				validContextStrategy(),
			).Build()
			violations := requireViolations(t, ValidateAgentRunDynamic(context.Background(), reader, validRun(), DynamicOptions{
				Now: now, PreflightTTL: 10 * time.Minute, ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
			}))
			if !hasCode(violations, CodeImageNotPinned) {
				t.Fatalf("violations=%#v, want image rejection", violations)
			}
		})
	}
}

func TestValidateAgentRunDynamicRejectsCriticRouteFamilyOverlap(t *testing.T) {
	now := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	run := validRun()
	gate := validGate()
	gate.Spec.Signals = &v1alpha1.GateSignalsSpec{
		ExecutionWeightBasisPoints: 7000,
		Critic: &v1alpha1.GateCriticSignalSpec{
			WeightBasisPoints: 3000,
			ModelRouteRef:     "critic-route",
			MaxFindings:       8,
		},
		MinScoreBasisPoints: 8500,
	}
	worker := &v1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "agw-runs"},
		Spec: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{
			Name: "worker", Kind: "openrouter-responses", Model: "worker-model", Family: "same-family", Priority: 1,
		}}},
	}
	critic := &v1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "critic-route", Namespace: "agw-runs"},
		Spec: v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{
			Name: "critic", Kind: "anthropic-messages", Model: "critic-model", Family: "same-family", Priority: 1,
		}}},
	}
	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		preflightMap(now, "node"),
		&v1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "issue-fixer", Namespace: "agw-runs"}, Spec: validAgentSpec()},
		gate,
		&v1alpha1.ToolSet{ObjectMeta: metav1.ObjectMeta{Name: "readonly", Namespace: "agw-runs"}},
		worker,
		critic,
		validContextStrategy(),
	).Build()
	violations := requireViolations(t, ValidateAgentRunDynamic(context.Background(), reader, run, DynamicOptions{
		Now: now, PreflightTTL: 10 * time.Minute, ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
	}))
	if !hasCode(violations, CodeCriticRouteNotDistinct) {
		t.Fatalf("violations=%#v, want critic route separation rejection", violations)
	}
}

func TestValidateDynamicStateFailsClosedWhenRouteIdentityIsUnknown(t *testing.T) {
	state := DynamicState{
		Agent:               ReferenceFound,
		Gate:                ReferenceFound,
		ToolSet:             ReferenceFound,
		ModelRoute:          ReferenceFound,
		CriticModelRoute:    ReferenceFound,
		ContextStrategy:     ReferenceFound,
		ToolSetRef:          "tools",
		ModelRouteRef:       "worker",
		CriticModelRouteRef: "critic",
		ContextStrategyRef:  "context",
		GateSignals: &v1alpha1.GateSignalsSpec{
			ExecutionWeightBasisPoints: 7000,
			Critic:                     &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 3000, ModelRouteRef: "critic", MaxFindings: 8},
			MinScoreBasisPoints:        8500,
		},
		PolicyRefsMatch: true,
		PolicyRefs:      []string{},
		PolicyStates:    []ReferenceState{},
		Preflight:       preflight.Decision{Allowed: true, Reason: preflight.ReasonAllowed},
	}
	violations := ValidateDynamicState(state)
	if !hasCode(violations, CodeCriticRouteNotDistinct) {
		t.Fatalf("violations=%#v, want fail-closed route identity violation", violations)
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func requestForRun(t *testing.T) webhookadmission.Request {
	return requestForRunObject(t, validRun())
}

func requestForRunObject(t *testing.T, run *v1alpha1.AgentRun) webhookadmission.Request {
	t.Helper()
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	return webhookadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: types.UID("run-uid"), Operation: admissionv1.Create, Namespace: run.Namespace,
		Object: runtime.RawExtension{Raw: raw},
	}}
}

func updateRequestForRuns(t *testing.T, previous, current *v1alpha1.AgentRun) webhookadmission.Request {
	t.Helper()
	request := requestForRunObject(t, current)
	oldRaw, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	request.Operation = admissionv1.Update
	request.OldObject = runtime.RawExtension{Raw: oldRaw}
	return request
}

func validRun() *v1alpha1.AgentRun {
	task := "fix the issue"
	return &v1alpha1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "agw-runs"}, Spec: v1alpha1.AgentRunSpec{
		AgentRef: "issue-fixer", GateRef: "go-default", Task: v1alpha1.TaskSpec{Inline: &task},
	}}
}

func validAgentSpec() v1alpha1.AgentSpec {
	return v1alpha1.AgentSpec{
		Runtime:            v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("a", 64)},
		ToolSetRef:         "readonly",
		ModelRouteRef:      "default",
		ContextStrategyRef: "default-context",
	}
}

func validContextStrategy() *v1alpha1.ContextStrategy {
	return &v1alpha1.ContextStrategy{
		ObjectMeta: metav1.ObjectMeta{Name: "default-context", Namespace: "agw-runs"},
		Spec: v1alpha1.ContextStrategySpec{
			RepoMap: &v1alpha1.ContextRepoMapSpec{Kind: "tree-sitter", Budget: 8000},
			Budget:  v1alpha1.ContextBudgetSpec{TotalTokens: 40000, MaxBytes: 8 << 20},
		},
	}
}

func validGate() *v1alpha1.Gate {
	return &v1alpha1.Gate{
		ObjectMeta: metav1.ObjectMeta{Name: "go-default", Namespace: "agw-runs"},
		Spec:       v1alpha1.GateSpec{Verify: v1alpha1.VerifySpec{Image: "ghcr.io/astatide/agw-verify@sha256:" + strings.Repeat("b", 64)}},
	}
}

func preflightMap(now time.Time, node string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: preflight.ConfigMapNamespace, Name: preflight.ConfigMapName}, Data: map[string]string{
		preflight.ResultDataKey: `{"schema_version":1,"passed":true,"timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339Nano) + `","node_fingerprint":"` + node + `","runtime_fingerprint":"runtime"}`,
	}}
}
