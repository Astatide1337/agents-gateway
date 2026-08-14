package admission

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/preflight"
)

func validAgentRun() *v1alpha1.AgentRun {
	task := "repair the issue"
	return &v1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "agw-runs"},
		Spec: v1alpha1.AgentRunSpec{
			AgentRef: "agent-codex",
			GateRef:  "gate-default",
			Task:     v1alpha1.TaskSpec{Inline: &task},
		},
	}
}

func TestValidateAgentRunStaticRejectsInvalidTaskContent(t *testing.T) {
	tests := []struct {
		name string
		edit func(*v1alpha1.AgentRun)
		code Code
	}{
		{
			name: "missing both sources",
			edit: func(run *v1alpha1.AgentRun) { run.Spec.Task = v1alpha1.TaskSpec{} },
			code: CodeTaskSourceInvalid,
		},
		{
			name: "both sources",
			edit: func(run *v1alpha1.AgentRun) {
				ref := "task-input"
				run.Spec.Task.ConfigMapRef = &ref
			},
			code: CodeTaskSourceInvalid,
		},
		{
			name: "empty inline",
			edit: func(run *v1alpha1.AgentRun) { run.Spec.Task.Inline = admissionStringPtr("\t\n  ") },
			code: CodeTaskContentEmpty,
		},
		{
			name: "too large inline",
			edit: func(run *v1alpha1.AgentRun) {
				run.Spec.Task.Inline = admissionStringPtr(strings.Repeat("x", v1alpha1.MaxTaskLength+1))
			},
			code: CodeTaskContentTooLong,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			run := validAgentRun()
			test.edit(run)
			if violations := ValidateAgentRunStaticErrors(run, nil); !hasCode(violations, test.code) {
				t.Fatalf("violations=%#v, want %q", violations, test.code)
			}
		})
	}
}

func admissionStringPtr(value string) *string { return &value }

func TestValidateAgentRunStaticReferences(t *testing.T) {
	run := validAgentRun()
	if err := ValidateAgentRunStatic(run, nil); err != nil {
		t.Fatalf("valid AgentRun rejected: %v", err)
	}

	bad := validAgentRun()
	bad.Spec.AgentRef = ""
	bad.Spec.GateRef = "not a name"
	violations := ValidateAgentRunStaticErrors(bad, nil)
	if !hasCode(violations, CodeReferenceRequired) || !hasCode(violations, CodeReferenceInvalid) {
		t.Fatalf("reference violations=%#v", violations)
	}
}

func TestValidateAgentRunStaticFreezesSpecExceptCancellation(t *testing.T) {
	previous := validAgentRun()
	current := validAgentRun()
	current.Spec.AgentRef = "different-agent"
	current.Spec.CancelRequested = true
	violations := ValidateAgentRunStaticErrors(current, previous)
	if !hasCode(violations, CodeSpecImmutable) {
		t.Fatalf("cancellation plus spec mutation was accepted: %#v", violations)
	}

	current = validAgentRun()
	current.Spec.CancelRequested = true
	if violations := ValidateAgentRunStaticErrors(current, previous); len(violations) != 0 {
		t.Fatalf("pure cancellation was rejected: %#v", violations)
	}
}

func TestValidateAgentRunStaticProtectsCleanupFinalizer(t *testing.T) {
	previous := validRun()
	previous.Finalizers = []string{"agw.astatide.com/cleanup"}
	current := previous.DeepCopy()
	current.Finalizers = nil
	violations := ValidateAgentRunStaticErrors(current, previous)
	if !hasCode(violations, CodeFinalizerProtected) {
		t.Fatalf("finalizer removal was accepted: %#v", violations)
	}
}

func TestValidateAgentRunStaticAllowsControllerFinalizerRemovalDuringDeletion(t *testing.T) {
	previous := validRun()
	previous.Finalizers = []string{"agw.astatide.com/cleanup"}
	current := previous.DeepCopy()
	current.Finalizers = nil
	deletionTime := metav1.NewTime(time.Date(2026, 8, 12, 21, 0, 0, 0, time.UTC))
	current.DeletionTimestamp = &deletionTime
	if violations := ValidateAgentRunStaticErrors(current, previous); len(violations) != 0 {
		t.Fatalf("controller finalizer removal during deletion was rejected: %#v", violations)
	}
}

func TestValidateCancelTransitionIsOneWayAndTerminalAware(t *testing.T) {
	previous := validAgentRun()
	previous.Spec.CancelRequested = true
	current := validAgentRun()
	if violations := ValidateCancelTransition(previous, current); !hasCode(violations, CodeCancelReversal) {
		t.Fatalf("clearing cancelRequested was accepted: %#v", violations)
	}

	current = validAgentRun()
	current.Spec.CancelRequested = true
	if violations := ValidateCancelTransition(previous, current); len(violations) != 0 {
		t.Fatalf("replaying cancelRequested was rejected: %#v", violations)
	}

	current = validAgentRun()
	current.Spec.CancelRequested = true
	previous = validAgentRun()
	previous.Status.Phase = v1alpha1.PhaseSucceeded
	if violations := ValidateCancelTransition(previous, current); !hasCode(violations, CodeCancelAfterTerminal) {
		t.Fatalf("cancel after terminal phase was accepted: %#v", violations)
	}

	current = validAgentRun()
	current.Spec.CancelRequested = true
	current.Status.Phase = v1alpha1.PhaseFailed
	if violations := ValidateCancelTransition(nil, current); !hasCode(violations, CodeCancelAfterTerminal) {
		t.Fatalf("cancel on a terminal object was accepted: %#v", violations)
	}
}

func TestValidateDynamicStateRequiresReferencesAndPreflight(t *testing.T) {
	state := DynamicState{
		Agent:              ReferenceFound,
		Gate:               ReferenceFound,
		ToolSet:            ReferenceFound,
		ModelRoute:         ReferenceFound,
		ContextStrategy:    ReferenceFound,
		ToolSetRef:         "tools-default",
		ModelRouteRef:      "models-default",
		ContextStrategyRef: "context-default",
		PolicyRefsMatch:    true,
		Preflight:          preflight.Decision{Allowed: true, Reason: preflight.ReasonAllowed},
	}
	if violations := ValidateDynamicState(state); len(violations) != 0 {
		t.Fatalf("complete dynamic state rejected: %#v", violations)
	}

	state.Preflight = preflight.Decision{Reason: preflight.ReasonMissing}
	state.Agent = ReferenceMissing
	state.Gate = ReferenceUnavailable
	violations := ValidateDynamicState(state)
	if !hasCode(violations, CodePreflightMissing) || !hasCode(violations, CodeReferenceMissing) || !hasCode(violations, CodeReferenceUnavailable) {
		t.Fatalf("dynamic denials=%#v", violations)
	}

	state.Preflight = preflight.Decision{Reason: preflight.ReasonFingerprintMismatch}
	state.Agent = ReferenceFound
	state.ToolSet = ReferenceUnknown
	state.ModelRoute = ReferenceUnknown
	state.ToolSetRef = "tools-default"
	state.ModelRouteRef = "models-default"
	violations = ValidateDynamicState(state)
	if !hasCode(violations, CodePreflightFingerprintMismatch) || !hasCode(violations, CodeReferenceUnavailable) {
		t.Fatalf("unknown references or fingerprint mismatch were accepted: %#v", violations)
	}
}

func TestValidateAgentRunDynamicFailsClosedWithoutReader(t *testing.T) {
	err := ValidateAgentRunDynamic(context.Background(), nil, validAgentRun(), DynamicOptions{
		Now:                 time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC),
		PreflightTTL:        time.Minute,
		ExpectedFingerprint: preflight.Fingerprint{Node: "node", Runtime: "runtime"},
	})
	violations := requireViolations(t, err)
	if !hasCode(violations, CodePreflightUnavailable) || !hasCode(violations, CodeReferenceUnavailable) {
		t.Fatalf("nil reader did not fail closed: %#v", violations)
	}
}

func TestValidationErrorsAreBounded(t *testing.T) {
	long := strings.Repeat("x", MaxValidationErrorBytes*4)
	if got := (ValidationError{Code: CodeInvalidObject, Field: long, Message: long}).Error(); len(got) > MaxValidationErrorBytes {
		t.Fatalf("single validation error exceeded bound: %d", len(got))
	}
	violations := make(ValidationErrors, MaxValidationErrors+3)
	for index := range violations {
		violations[index] = ValidationError{Code: CodeInvalidObject, Field: "field", Message: "message"}
	}
	if got := violations.Error(); len(got) > MaxValidationErrorBytes {
		t.Fatalf("validation error collection exceeded bound: %d", len(got))
	}
}

func requireViolations(t *testing.T, err error) ValidationErrors {
	t.Helper()
	if err == nil {
		t.Fatal("expected validation error")
	}
	violations, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("error type=%T, want ValidationErrors", err)
	}
	return violations
}

func hasCode(violations ValidationErrors, want Code) bool {
	for _, violation := range violations {
		if violation.Code == want {
			return true
		}
	}
	return false
}
