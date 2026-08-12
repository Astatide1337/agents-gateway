package status

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestBoundedAgentRunStatusCapsProjectionWithoutMutatingInput(t *testing.T) {
	longMessage := strings.Repeat("x", MaxStatusMessageBytes*4)
	input := v1alpha1.AgentRunStatus{
		Phase:      v1alpha1.PhaseWorking,
		BaseSHA:    strings.Repeat("a", MaxBaseSHABytes*2),
		Failure:    &v1alpha1.FailureStatus{Code: strings.Repeat("c", MaxFailureCodeBytes*2), Message: longMessage},
		Conditions: make([]v1alpha1.Condition, MaxStatusConditions+4),
	}
	for index := range input.Conditions {
		input.Conditions[index] = v1alpha1.Condition{
			Type:    "condition-" + string(rune('a'+index)),
			Status:  v1alpha1.ConditionTrue,
			Reason:  strings.Repeat("r", MaxReasonBytes*2),
			Message: longMessage,
		}
	}

	bounded := BoundedAgentRunStatus(input)
	if len(bounded.Conditions) != MaxStatusConditions {
		t.Fatalf("bounded conditions = %d, want %d", len(bounded.Conditions), MaxStatusConditions)
	}
	if len(bounded.BaseSHA) > MaxBaseSHABytes || len(bounded.Failure.Message) > MaxStatusMessageBytes {
		t.Fatalf("bounded fields exceeded limits: %#v", bounded)
	}
	if len(input.Conditions) != MaxStatusConditions+4 || len(input.Failure.Message) != len(longMessage) {
		t.Fatal("bounding mutated the input status")
	}
	if err := ValidateAgentRunStatus(bounded); err != nil {
		t.Fatalf("bounded status is invalid: %v", err)
	}
	encoded, err := Encode(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= MaxStatusBytes {
		t.Fatalf("encoded status size = %d, want < %d", len(encoded), MaxStatusBytes)
	}
}

func TestSandboxChildPlanFingerprintIsRequiredBoundedAndDeepCopied(t *testing.T) {
	fingerprint := "sha256:" + strings.Repeat("f", 64)
	input := v1alpha1.AgentRunStatus{WorkSandboxRef: &v1alpha1.ChildRef{
		Name: "agw-work", Kind: "Sandbox", Role: "work", PlanFingerprint: fingerprint,
	}}
	bounded := BoundedAgentRunStatus(input)
	if err := ValidateAgentRunStatus(bounded); err != nil {
		t.Fatalf("valid child plan fingerprint was rejected: %v", err)
	}
	bounded.WorkSandboxRef.PlanFingerprint = "sha256:" + strings.Repeat("e", 64)
	if input.WorkSandboxRef.PlanFingerprint != fingerprint {
		t.Fatal("status projection mutated the input ChildRef")
	}

	missing := input
	missing.WorkSandboxRef = &v1alpha1.ChildRef{Name: "agw-work", Kind: "Sandbox", Role: "work"}
	if err := ValidateAgentRunStatus(missing); err == nil {
		t.Fatal("Sandbox ChildRef without planFingerprint was accepted")
	}

	oversized := input
	oversized.WorkSandboxRef = &v1alpha1.ChildRef{Name: "agw-work", Kind: "Sandbox", Role: "work", PlanFingerprint: strings.Repeat("f", MaxDigestBytes+10)}
	projected := BoundedAgentRunStatus(oversized)
	if len(projected.WorkSandboxRef.PlanFingerprint) != MaxDigestBytes {
		t.Fatalf("bounded plan fingerprint length=%d, want %d", len(projected.WorkSandboxRef.PlanFingerprint), MaxDigestBytes)
	}
	if err := ValidateAgentRunStatus(projected); err == nil {
		t.Fatal("truncated non-digest planFingerprint was accepted")
	}
}

func TestSetConditionReplacesProjectionInsteadOfAppendingHistory(t *testing.T) {
	status := v1alpha1.AgentRunStatus{Phase: v1alpha1.PhasePending}
	condition := v1alpha1.Condition{Type: string(v1alpha1.ConditionAdmitted), Status: v1alpha1.ConditionTrue, Reason: "accepted", Message: "first"}
	if err := SetCondition(&status, condition); err != nil {
		t.Fatal(err)
	}
	condition.Message = "current"
	if err := SetCondition(&status, condition); err != nil {
		t.Fatal(err)
	}
	if len(status.Conditions) != 1 || status.Conditions[0].Message != "current" {
		t.Fatalf("condition projection = %#v, want one current condition", status.Conditions)
	}
}

func TestSetPhaseAndFailureUseTheLifecycleFSM(t *testing.T) {
	status := v1alpha1.AgentRunStatus{}
	if err := SetPhaseAt(&status, v1alpha1.PhasePending, time.Date(2026, 8, 11, 12, 0, 0, 0, time.FixedZone("test", -4*60*60))); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 11, 12, 1, 0, 0, time.FixedZone("test", -4*60*60))
	if err := SetPhaseAt(&status, v1alpha1.PhaseCloning, started); err != nil {
		t.Fatal(err)
	}
	if status.StartedAt == nil || !status.StartedAt.Time.Equal(started.UTC()) {
		t.Fatalf("startedAt = %#v, want %s", status.StartedAt, started.UTC())
	}
	if err := SetFailure(&status, "capture_failed", "capture did not finish"); err != nil {
		t.Fatal(err)
	}
	if status.Phase != v1alpha1.PhaseFailed || status.Failure == nil || status.Failure.Code != "capture_failed" {
		t.Fatalf("failure status = %#v", status)
	}
	if status.CompletedAt != nil {
		t.Fatal("SetFailure without a clock unexpectedly set CompletedAt")
	}
	if err := SetPhase(&status, v1alpha1.PhaseWorking); err == nil {
		t.Fatal("terminal status accepted a new phase")
	}
}

func TestSetPhaseRejectsNonPendingInitialAndTerminalUpdates(t *testing.T) {
	status := v1alpha1.AgentRunStatus{}
	if err := SetPhase(&status, v1alpha1.PhaseWorking); err == nil {
		t.Fatal("empty status accepted non-Pending initial phase")
	}
	status.Phase = v1alpha1.PhasePublishing
	if err := SetPhaseAt(&status, v1alpha1.PhaseSucceeded, time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if status.CompletedAt == nil {
		t.Fatal("terminal transition did not set CompletedAt")
	}
	if err := SetPhase(&status, v1alpha1.PhasePublishing); err == nil {
		t.Fatal("terminal status accepted outgoing transition")
	}
}

func TestStatusReferencesRequireDigestAndDoNotCarryURICredentials(t *testing.T) {
	status := v1alpha1.AgentRunStatus{}
	ref := v1alpha1.ArtifactRef{
		URI: "s3://bucket/runs/run-1/resolved.json", Digest: "sha256:" + strings.Repeat("a", 64),
		Kind: "resolved-spec", Name: "resolved.json", MediaType: "application/json", SizeBytes: 128,
	}
	if err := SetResolvedSpecRef(&status, ref); err != nil {
		t.Fatal(err)
	}
	if status.ResolvedSpecRef == nil || status.ResolvedSpecRef.Digest != ref.Digest {
		t.Fatalf("resolved ref = %#v", status.ResolvedSpecRef)
	}
	if *status.ResolvedSpecRef != ref {
		t.Fatalf("artifact descriptor was truncated: got %#v, want %#v", *status.ResolvedSpecRef, ref)
	}
	unsafe := v1alpha1.ArtifactRef{URI: "https://user:password@example.test/report", Digest: ref.Digest}
	if err := SetEventStreamRef(&status, unsafe); err == nil {
		t.Fatal("URI with userinfo was accepted")
	}
	invalid := status
	invalid.ResolvedSpecRef = &v1alpha1.ArtifactRef{URI: ref.URI, Digest: "md5:bad"}
	if err := Validate(invalid); err == nil {
		t.Fatal("non-SHA-256 artifact digest was accepted")
	}
}

func TestStatusAcceptsUnknownEffectLedgerOutcome(t *testing.T) {
	status := v1alpha1.AgentRunStatus{
		Phase:  v1alpha1.PhaseUnknownEffect,
		Effect: &v1alpha1.EffectSummary{State: v1alpha1.EffectUnknown},
	}
	if err := Validate(status); err != nil {
		t.Fatalf("unknown effect outcome was rejected: %v", err)
	}
}

func TestStatusDistinguishesDefinitiveFailedEffectFromUnknown(t *testing.T) {
	status := v1alpha1.AgentRunStatus{
		Phase:  v1alpha1.PhaseRejected,
		Effect: &v1alpha1.EffectSummary{State: v1alpha1.EffectFailed},
	}
	if err := Validate(status); err != nil {
		t.Fatalf("definitive failed effect was rejected: %v", err)
	}
}

func TestStatusSizeHelperUsesTheBoundedProjection(t *testing.T) {
	status := v1alpha1.AgentRunStatus{
		Phase:   v1alpha1.PhaseSucceeded,
		Effect:  &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: "https://github.com/example/repo/pull/1"},
		Failure: &v1alpha1.FailureStatus{Code: "", Message: strings.Repeat("x", MaxStatusMessageBytes*8)},
	}
	if _, err := Size(status); err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if !Fits(status) {
		t.Fatal("bounded status did not fit")
	}
	if _, err := MarshalAgentRunStatus(status); err != nil {
		t.Fatal(err)
	}
}
