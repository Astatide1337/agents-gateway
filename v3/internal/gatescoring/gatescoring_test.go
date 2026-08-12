package gatescoring

import (
	"bytes"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
)

func TestEvaluateUsesFixedPointAndBlockingChecksOverrideScore(t *testing.T) {
	result, err := Evaluate(Input{
		Signals: &v1alpha1.GateSignalsSpec{
			ExecutionWeightBasisPoints: 7000,
			Critic:                     &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 3000, ModelRouteRef: "critic-route", MaxFindings: 8},
			MinScoreBasisPoints:        8500,
		},
		Execution: ExecutionSignal{ScoreBasisPoints: 9999, Checks: []ExecutionCheck{{Name: "tests", Passed: false, Blocking: true}}},
		Critic:    &CriticSignal{Result: emptyCorroboration()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != Rejected || result.WeightedScoreBasisPoints != 9999 {
		t.Fatalf("result=%#v, want rejected with preserved high score", result)
	}
	if !contains(result.BlockingReasons, "execution.tests") {
		t.Fatalf("blocking reasons=%v", result.BlockingReasons)
	}
}

func TestEvaluateRoutesModelOnlyAndUncorroboratedFindingsAsAdvisory(t *testing.T) {
	modelFinding := findingcorroboration.CriticFinding{ID: "model-only", Path: "pkg/a.go", Location: findingcorroboration.Location{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2}, RuleID: "quality", Message: "looks risky"}
	modelEvidence := sealedModelEvidence(t, modelFinding)
	corroboration, err := findingcorroboration.Corroborate(findingcorroboration.CorroborationInput{
		SchemaVersion: findingcorroboration.SchemaVersion,
		Findings:      []findingcorroboration.CriticFinding{modelFinding},
		Evidence:      []findingcorroboration.Evidence{modelEvidence},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Evaluate(Input{
		Signals: &v1alpha1.GateSignalsSpec{
			ExecutionWeightBasisPoints: 6000,
			Critic:                     &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 4000, ModelRouteRef: "critic-route", MaxFindings: 8},
			MinScoreBasisPoints:        9000,
		},
		Execution: ExecutionSignal{ScoreBasisPoints: 10000, Checks: []ExecutionCheck{{Name: "tests", Passed: true, Blocking: true}}},
		Critic:    &CriticSignal{Result: corroboration},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != Accepted || result.CriticScoreBasisPoints != 10000 || result.WeightedScoreBasisPoints != 10000 {
		t.Fatalf("model-only critic blocked or changed score: %#v", result)
	}
}

func TestEvaluateRejectsCorroboratedCriticFindingAfterRouting(t *testing.T) {
	finding := findingcorroboration.CriticFinding{ID: "static-bug", Path: "pkg/a.go", Location: findingcorroboration.Location{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2}, RuleID: "quality", Message: "static issue"}
	evidence := findingcorroboration.Evidence{
		ID:             "static-evidence",
		Class:          findingcorroboration.EvidenceClassStatic,
		Binding:        findingcorroboration.EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID},
		ArtifactDigest: digest('a'),
		Validation:     findingcorroboration.EvidenceValidation{Validator: "ast-grep", Deterministic: true, Independent: true},
		Static:         &findingcorroboration.StaticEvidence{Tool: "ast-grep", Matched: true, MatchCount: 1},
	}
	evidence, err := findingcorroboration.SealEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	corroboration, err := findingcorroboration.Corroborate(findingcorroboration.CorroborationInput{SchemaVersion: findingcorroboration.SchemaVersion, Findings: []findingcorroboration.CriticFinding{finding}, Evidence: []findingcorroboration.Evidence{evidence}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findingcorroboration.CanonicalResultBytes(corroboration); err != nil {
		t.Fatalf("direct canonical corroboration: %v", err)
	}
	normalized, err := normalizeCriticResult(corroboration, 8)
	if err != nil {
		t.Fatalf("first normalize corroboration: %v", err)
	}
	if _, err := normalizeCriticResult(normalized, 8); err != nil {
		t.Fatalf("second normalize corroboration: %v", err)
	}
	result, err := Evaluate(Input{
		Signals:   &v1alpha1.GateSignalsSpec{ExecutionWeightBasisPoints: 6000, Critic: &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 4000, ModelRouteRef: "critic-route", MaxFindings: 8}, MinScoreBasisPoints: 9000},
		Execution: ExecutionSignal{ScoreBasisPoints: 10000, Checks: []ExecutionCheck{{Name: "tests", Passed: true, Blocking: true}}},
		Critic:    &CriticSignal{Result: corroboration},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != Rejected || result.CriticScoreBasisPoints != 0 || !contains(result.BlockingReasons, "critic.static-bug") {
		t.Fatalf("result=%#v, want corroborated critic veto", result)
	}
}

func TestCanonicalResultIsOrderIndependentAndDigestBound(t *testing.T) {
	input := Input{Execution: ExecutionSignal{ScoreBasisPoints: 8750, Checks: []ExecutionCheck{{Name: "zeta", Passed: true, Blocking: false}, {Name: "alpha", Passed: true, Blocking: true}}}}
	left, err := Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Execution.Checks = []ExecutionCheck{{Name: "alpha", Passed: true, Blocking: true}, {Name: "zeta", Passed: true, Blocking: false}}
	right, err := Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	leftBytes, err := CanonicalResultBytes(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalResultBytes(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", leftBytes, rightBytes)
	}
	leftDigest, err := ResultDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonicalResult(leftBytes, leftDigest); err != nil {
		t.Fatalf("ParseCanonicalResult: %v", err)
	}
	if _, err := ParseCanonicalResult(leftBytes, digest('f')); err == nil {
		t.Fatal("wrong digest was accepted")
	}
}

func TestCanonicalResultRejectsTamperedDerivedFieldsAndBounds(t *testing.T) {
	result, err := Evaluate(Input{Execution: ExecutionSignal{ScoreBasisPoints: 10000, Checks: []ExecutionCheck{{Name: "tests", Passed: true, Blocking: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	result.WeightedScoreBasisPoints = 1
	if _, err := CanonicalResultBytes(result); err == nil {
		t.Fatal("tampered weighted score was accepted")
	}
	_, err = Evaluate(Input{Execution: ExecutionSignal{ScoreBasisPoints: 10001, Checks: []ExecutionCheck{{Name: "tests", Passed: true, Blocking: true}}}})
	if err == nil {
		t.Fatal("out-of-range score was accepted")
	}
	_, err = Evaluate(Input{Signals: &v1alpha1.GateSignalsSpec{ExecutionWeightBasisPoints: 5000, Critic: &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 5000, ModelRouteRef: "critic-route", MaxFindings: 8}, MinScoreBasisPoints: 9000}})
	if err == nil {
		t.Fatal("missing execution checks was accepted")
	}
}

func emptyCorroboration() findingcorroboration.CorroborationResult {
	return findingcorroboration.CorroborationResult{SchemaVersion: findingcorroboration.SchemaVersion, Findings: []findingcorroboration.FindingDecision{}, Counts: findingcorroboration.Counts{}}
}

func sealedModelEvidence(t *testing.T, finding findingcorroboration.CriticFinding) findingcorroboration.Evidence {
	t.Helper()
	evidence, err := findingcorroboration.SealEvidence(findingcorroboration.Evidence{
		ID:             "model-evidence",
		Class:          findingcorroboration.EvidenceClassModelAssertion,
		Binding:        findingcorroboration.EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID},
		ArtifactDigest: digest('b'),
		Validation:     findingcorroboration.EvidenceValidation{Validator: "critic", Deterministic: false, Independent: false},
		ModelAssertion: &findingcorroboration.ModelAssertionEvidence{Model: "critic", Claim: "model claim"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func digest(ch byte) string { return "sha256:" + strings.Repeat(string(ch), 64) }

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
