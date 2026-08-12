package findingcorroboration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func testFinding(id, path, rule string) CriticFinding {
	return CriticFinding{
		ID:       id,
		Path:     path,
		Location: Location{StartLine: 10, StartColumn: 2, EndLine: 10, EndColumn: 8},
		RuleID:   rule,
		Message:  "the checked condition is violated",
	}
}

func testInput(findings []CriticFinding, evidence []Evidence) CorroborationInput {
	return CorroborationInput{
		SchemaVersion: SchemaVersion,
		Findings:      findings,
		Evidence:      evidence,
	}
}

func testArtifactDigest(ch byte) string {
	return "sha256:" + strings.Repeat(string(ch), 64)
}

func testEvidence(t *testing.T, id string, finding CriticFinding, class EvidenceClass, artifact byte) Evidence {
	t.Helper()
	evidence := Evidence{
		ID:             id,
		Class:          class,
		Binding:        EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID},
		ArtifactDigest: testArtifactDigest(artifact),
		Validation: EvidenceValidation{
			Validator:     "verifier-v1",
			Deterministic: true,
			Independent:   true,
		},
	}
	switch class {
	case EvidenceClassReproduction:
		evidence.Reproduction = &ReproductionEvidence{Command: "go test ./...", Base: OutcomeFailed, Candidate: OutcomePassed}
	case EvidenceClassStatic:
		evidence.Static = &StaticEvidence{Tool: "ast-grep", Matched: true, MatchCount: 1}
	case EvidenceClassSymbolGraph:
		evidence.SymbolGraph = &SymbolGraphEvidence{Relation: "caller", Matched: true, MatchCount: 1}
	case EvidenceClassPolicy:
		evidence.Policy = &PolicyEvidence{PolicyID: finding.RuleID, Violated: true}
	case EvidenceClassModelAssertion:
		evidence.ModelAssertion = &ModelAssertionEvidence{Model: "critic-model", Claim: "this appears unsafe"}
	default:
		t.Fatalf("test helper does not support class %q", class)
	}
	sealed, err := SealEvidence(evidence)
	if err != nil {
		t.Fatalf("SealEvidence(%s): %v", id, err)
	}
	return sealed
}

func cloneEvidence(evidence Evidence) Evidence {
	cloned := evidence
	if evidence.Reproduction != nil {
		payload := *evidence.Reproduction
		cloned.Reproduction = &payload
	}
	if evidence.Static != nil {
		payload := *evidence.Static
		cloned.Static = &payload
	}
	if evidence.SymbolGraph != nil {
		payload := *evidence.SymbolGraph
		cloned.SymbolGraph = &payload
	}
	if evidence.Policy != nil {
		payload := *evidence.Policy
		cloned.Policy = &payload
	}
	if evidence.ModelAssertion != nil {
		payload := *evidence.ModelAssertion
		cloned.ModelAssertion = &payload
	}
	return cloned
}

func TestCorroborateRoutesEvidenceByADR020(t *testing.T) {
	reproductionFinding := testFinding("finding-reproduction", "internal/repro.go", "bug.reproduction")
	staticFinding := testFinding("finding-static", "internal/static.go", "bug.static")
	symbolFinding := testFinding("finding-symbol", "internal/symbol.go", "bug.symbol")
	policyFinding := testFinding("finding-policy", "internal/policy.go", "bug.policy")
	modelFinding := testFinding("finding-model", "internal/model.go", "bug.model")

	reproduction := testEvidence(t, "e-reproduction", reproductionFinding, EvidenceClassReproduction, 'a')
	static := testEvidence(t, "e-static", staticFinding, EvidenceClassStatic, 'b')
	static.Validation.Independent = false
	static = resealEvidence(t, static)
	symbol := testEvidence(t, "e-symbol", symbolFinding, EvidenceClassSymbolGraph, 'c')
	symbol.SymbolGraph.Matched = false
	symbol.SymbolGraph.MatchCount = 0
	symbol = resealEvidence(t, symbol)
	policy := testEvidence(t, "e-policy", policyFinding, EvidenceClassPolicy, 'd')
	model := testEvidence(t, "e-model", modelFinding, EvidenceClassModelAssertion, 'e')

	result, err := Corroborate(testInput(
		[]CriticFinding{modelFinding, policyFinding, symbolFinding, staticFinding, reproductionFinding},
		[]Evidence{model, policy, symbol, static, reproduction},
	))
	if err != nil {
		t.Fatalf("Corroborate: %v", err)
	}
	if got, want := result.Counts, (Counts{
		TotalFindings:         5,
		BlockingFindings:      2,
		AdvisoryFindings:      3,
		DeterministicEvidence: 3,
		CorroboratingEvidence: 2,
		ModelAssertions:       1,
	}); got != want {
		t.Fatalf("counts = %#v, want %#v", got, want)
	}
	if got, want := result.Findings[0].Finding.ID, "finding-model"; got != want {
		t.Fatalf("findings are not sorted: first ID %q, want %q", got, want)
	}
	for _, decision := range result.Findings {
		if decision.Finding.ID == modelFinding.ID && decision.Route != RouteAdvisory {
			t.Fatalf("model assertion route = %q, want advisory", decision.Route)
		}
	}
	for _, decision := range result.Findings {
		switch decision.Finding.ID {
		case reproductionFinding.ID, policyFinding.ID:
			if decision.Route != RouteBlocking {
				t.Errorf("%s route = %q, want blocking", decision.Finding.ID, decision.Route)
			}
		case staticFinding.ID, symbolFinding.ID, modelFinding.ID:
			if decision.Route != RouteAdvisory {
				t.Errorf("%s route = %q, want advisory", decision.Finding.ID, decision.Route)
			}
		}
	}
}

func resealEvidence(t *testing.T, evidence Evidence) Evidence {
	t.Helper()
	evidence.Digest = ""
	sealed, err := SealEvidence(evidence)
	if err != nil {
		t.Fatalf("reseal evidence %s: %v", evidence.ID, err)
	}
	return sealed
}

func TestModelAssertionCanNeverBlock(t *testing.T) {
	finding := testFinding("model-only", "internal/unsafe.go", "security.rule")
	evidence := testEvidence(t, "model-evidence", finding, EvidenceClassModelAssertion, 'f')
	// Even an over-optimistic producer cannot turn model prose into machine
	// corroboration. Corroborate must retain it and route it advisory.
	evidence.Validation.Deterministic = true
	evidence.Validation.Independent = true
	evidence = resealEvidence(t, evidence)

	result, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{evidence}))
	if err != nil {
		t.Fatalf("Corroborate: %v", err)
	}
	decision := result.Findings[0]
	if decision.Route != RouteAdvisory || len(decision.Evidence) != 1 {
		t.Fatalf("model-only decision = %#v, want one advisory finding", decision)
	}
	if decision.Evidence[0].Route != RouteAdvisory || decision.Evidence[0].Corroborates {
		t.Fatalf("model-only evidence decision = %#v, want advisory/non-corroborating", decision.Evidence[0])
	}
}

func TestSealDigestAndBindingTamperAreDetected(t *testing.T) {
	finding := testFinding("tamper", "pkg/handler.go", "handler.context")
	evidence := testEvidence(t, "tamper-evidence", finding, EvidenceClassStatic, 'a')
	digest, err := EvidenceDigest(evidence)
	if err != nil {
		t.Fatalf("EvidenceDigest: %v", err)
	}
	if digest != evidence.Digest {
		t.Fatalf("digest = %q, sealed digest = %q", digest, evidence.Digest)
	}

	cases := map[string]func(*Evidence){
		"path binding":      func(value *Evidence) { value.Binding.Path = "pkg/other.go" },
		"location binding":  func(value *Evidence) { value.Binding.Location.StartLine++ },
		"rule binding":      func(value *Evidence) { value.Binding.RuleID = "other.rule" },
		"payload":           func(value *Evidence) { value.Static.MatchCount = 2; value.Static.Matched = true },
		"artifact identity": func(value *Evidence) { value.ArtifactDigest = testArtifactDigest('b') },
		"immutable flag":    func(value *Evidence) { value.Immutable = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := cloneEvidence(evidence)
			mutate(&mutated)
			if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{mutated})); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Corroborate mutated evidence error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestResealedEvidenceMustStillBindExactlyToFinding(t *testing.T) {
	finding := testFinding("exact-binding", "pkg/handler.go", "handler.context")
	base := testEvidence(t, "exact-evidence", finding, EvidenceClassStatic, 'a')
	cases := map[string]func(*Evidence){
		"path":     func(value *Evidence) { value.Binding.Path = "pkg/other.go" },
		"location": func(value *Evidence) { value.Binding.Location.StartColumn++ },
		"rule":     func(value *Evidence) { value.Binding.RuleID = "other.rule" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := cloneEvidence(base)
			mutate(&mutated)
			mutated = resealEvidence(t, mutated)
			if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{mutated})); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("resealed binding mismatch error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestRejectsDuplicateIDsAndReusedEvidence(t *testing.T) {
	first := testFinding("same", "pkg/one.go", "rule.one")
	second := first
	second.Message = "a different claim"
	if _, err := Corroborate(testInput([]CriticFinding{first, second}, []Evidence{})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate finding IDs error = %v", err)
	}

	finding := testFinding("duplicate-evidence", "pkg/one.go", "rule.one")
	left := testEvidence(t, "same-evidence", finding, EvidenceClassStatic, 'a')
	right := testEvidence(t, "same-evidence", finding, EvidenceClassPolicy, 'b')
	if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{left, right})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate evidence IDs error = %v", err)
	}

	otherFinding := testFinding("other-evidence", "pkg/two.go", "rule.two")
	reused := testEvidence(t, "e-other", otherFinding, EvidenceClassStatic, 'c')
	reused.ArtifactDigest = left.ArtifactDigest
	reused = resealEvidence(t, reused)
	if _, err := Corroborate(testInput([]CriticFinding{finding, otherFinding}, []Evidence{left, reused})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reused artifact error = %v", err)
	}
}

func TestRejectsUnknownClassesPayloadAmbiguityAndExactBinding(t *testing.T) {
	finding := testFinding("shape", "pkg/shape.go", "shape.rule")
	base := testEvidence(t, "shape-evidence", finding, EvidenceClassStatic, 'a')

	cases := map[string]func(*Evidence){
		"unknown class": func(value *Evidence) { value.Class = EvidenceClass("future") },
		"two payloads": func(value *Evidence) {
			value.ModelAssertion = &ModelAssertionEvidence{Model: "model", Claim: "claim"}
		},
		"class payload mismatch": func(value *Evidence) {
			value.Class = EvidenceClassPolicy
			value.Static = nil
			value.Policy = &PolicyEvidence{PolicyID: finding.RuleID, Violated: true}
		},
		"unknown finding": func(value *Evidence) { value.Binding.FindingID = "missing" },
		"policy rule mismatch": func(value *Evidence) {
			value.Class = EvidenceClassPolicy
			value.Static = nil
			value.Policy = &PolicyEvidence{PolicyID: "other.rule", Violated: true}
		},
		"invalid digest": func(value *Evidence) { value.ArtifactDigest = "sha256:ABC" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := cloneEvidence(base)
			mutate(&mutated)
			if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{mutated})); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestRejectsTraversalAmbiguityAndBounds(t *testing.T) {
	badPaths := []string{"", "/absolute.go", "../escape.go", "pkg/../escape.go", "pkg//file.go", "pkg/./file.go", `pkg\\file.go`, "pkg/\nfile.go", "pkg/\u202e.go"}
	for _, path := range badPaths {
		t.Run(fmt.Sprintf("path-%q", path), func(t *testing.T) {
			finding := testFinding("path", path, "path.rule")
			if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{})); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("path %q error = %v, want ErrInvalidInput", path, err)
			}
		})
	}

	badLocations := []Location{
		{StartLine: 0, StartColumn: 1, EndLine: 1, EndColumn: 1},
		{StartLine: 2, StartColumn: 1, EndLine: 1, EndColumn: 1},
		{StartLine: 1, StartColumn: 3, EndLine: 1, EndColumn: 2},
		{StartLine: 1, StartColumn: 1, EndLine: 1_000_000_001, EndColumn: 1},
	}
	for i, location := range badLocations {
		t.Run(fmt.Sprintf("location-%d", i), func(t *testing.T) {
			finding := testFinding("location", "pkg/file.go", "location.rule")
			finding.Location = location
			if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{})); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("location %#v error = %v, want ErrInvalidInput", location, err)
			}
		})
	}

	tooManyFindings := make([]CriticFinding, MaxFindings+1)
	for i := range tooManyFindings {
		tooManyFindings[i] = testFinding(fmt.Sprintf("finding-%03d", i), "pkg/file.go", "bound.rule")
	}
	if _, err := Corroborate(testInput(tooManyFindings, []Evidence{})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("too many findings error = %v, want ErrInvalidInput", err)
	}

	finding := testFinding("too-many-evidence", "pkg/file.go", "bound.rule")
	tooManyEvidence := make([]Evidence, MaxEvidence+1)
	for i := range tooManyEvidence {
		tooManyEvidence[i] = testEvidence(t, fmt.Sprintf("evidence-%04d", i), finding, EvidenceClassStatic, byte('a'+i%6))
		tooManyEvidence[i].ArtifactDigest = fmt.Sprintf("sha256:%064x", i+1)
		tooManyEvidence[i] = resealEvidence(t, tooManyEvidence[i])
	}
	if _, err := Corroborate(testInput([]CriticFinding{finding}, tooManyEvidence)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("too much evidence error = %v, want ErrInvalidInput", err)
	}

	invalidStatic := testEvidence(t, "invalid-static", finding, EvidenceClassStatic, 'f')
	invalidStatic.Static.MatchCount = 0
	if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{invalidStatic})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("tampered static evidence error = %v, want ErrInvalidInput", err)
	}
	invalidStatic = testEvidence(t, "invalid-static-count", finding, EvidenceClassStatic, 'e')
	invalidStatic.Static.MatchCount = MaxEvidenceMatches + 1
	invalidStatic.Static.Matched = true
	if _, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{invalidStatic})); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized match count error = %v, want ErrInvalidInput", err)
	}
}

func TestInputByteAndJSONDepthBounds(t *testing.T) {
	findings := make([]CriticFinding, MaxFindings)
	for i := range findings {
		findings[i] = testFinding(fmt.Sprintf("large-%03d", i), "pkg/file.go", "large.rule")
		findings[i].Message = strings.Repeat("x", MaxMessageBytes)
	}
	input := testInput(findings, []Evidence{})
	if _, err := CanonicalInputBytes(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized canonical input error = %v, want ErrInvalidInput", err)
	}
	if _, err := Corroborate(input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized typed input error = %v, want ErrInvalidInput", err)
	}

	deep := strings.Repeat("[", MaxJSONDepth+1) + strings.Repeat("]", MaxJSONDepth+1)
	if _, err := ParseCanonicalInput([]byte(deep)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("deep JSON error = %v, want ErrInvalidInput", err)
	}
}

func TestCanonicalInputIsStrictAndOrderIndependent(t *testing.T) {
	first := testFinding("a-finding", "pkg/a.go", "rule.a")
	second := testFinding("b-finding", "pkg/b.go", "rule.b")
	firstEvidence := testEvidence(t, "b-evidence", first, EvidenceClassPolicy, 'a')
	secondEvidence := testEvidence(t, "a-evidence", second, EvidenceClassStatic, 'b')
	left := testInput([]CriticFinding{second, first}, []Evidence{firstEvidence, secondEvidence})
	right := testInput([]CriticFinding{first, second}, []Evidence{secondEvidence, firstEvidence})
	leftBefore := append([]CriticFinding(nil), left.Findings...)
	leftBytes, err := CanonicalInputBytes(left)
	if err != nil {
		t.Fatalf("CanonicalInputBytes(left): %v", err)
	}
	rightBytes, err := CanonicalInputBytes(right)
	if err != nil {
		t.Fatalf("CanonicalInputBytes(right): %v", err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("order-dependent canonical input:\nleft=%s\nright=%s", leftBytes, rightBytes)
	}
	if !reflect.DeepEqual(left.Findings, leftBefore) {
		t.Fatalf("CanonicalInputBytes mutated caller-owned findings")
	}
	parsed, err := ParseCanonicalInput(leftBytes)
	if err != nil {
		t.Fatalf("ParseCanonicalInput(canonical): %v", err)
	}
	parsedBytes, err := CanonicalInputBytes(parsed)
	if err != nil || !bytes.Equal(parsedBytes, leftBytes) {
		t.Fatalf("canonical parse round trip: err=%v bytesEqual=%v", err, bytes.Equal(parsedBytes, leftBytes))
	}

	mutations := map[string][]byte{
		"whitespace":                append([]byte(" \n"), leftBytes...),
		"trailing value":            append(append([]byte(nil), leftBytes...), []byte("{}")...),
		"unknown top-level field":   []byte(strings.Replace(string(leftBytes), "{", `{"unknown":1,`, 1)),
		"duplicate top-level field": []byte(strings.Replace(string(leftBytes), `"schemaVersion":`, `"schemaVersion":`+fmt.Sprintf("%q,\"schemaVersion\":", SchemaVersion), 1)),
	}
	for name, encoded := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCanonicalInput(encoded); err == nil {
				t.Fatalf("ParseCanonicalInput unexpectedly accepted %s", name)
			}
			if name == "whitespace" || name == "trailing value" {
				if _, err := ParseCanonicalInput(encoded); !errors.Is(err, ErrNonCanonical) && !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("%s error = %v, want canonical/input error", name, err)
				}
			}
		})
	}
}

func TestCanonicalResultIsOrderIndependentAndInternallyConsistent(t *testing.T) {
	first := testFinding("first", "pkg/first.go", "rule.first")
	second := testFinding("second", "pkg/second.go", "rule.second")
	firstEvidence := testEvidence(t, "z-evidence", first, EvidenceClassPolicy, 'a')
	secondEvidence := testEvidence(t, "a-evidence", second, EvidenceClassReproduction, 'b')
	result, err := Corroborate(testInput([]CriticFinding{second, first}, []Evidence{secondEvidence, firstEvidence}))
	if err != nil {
		t.Fatalf("Corroborate: %v", err)
	}
	want, err := CanonicalResultBytes(result)
	if err != nil {
		t.Fatalf("CanonicalResultBytes(result): %v", err)
	}

	permuted := result
	permuted.Findings = append([]FindingDecision(nil), result.Findings...)
	permuted.Findings[0], permuted.Findings[1] = permuted.Findings[1], permuted.Findings[0]
	permuted.Findings[0].Evidence = append([]EvidenceDecision(nil), permuted.Findings[0].Evidence...)
	permuted.Findings[1].Evidence = append([]EvidenceDecision(nil), permuted.Findings[1].Evidence...)
	got, err := CanonicalResultBytes(permuted)
	if err != nil {
		t.Fatalf("CanonicalResultBytes(permuted): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("order-dependent canonical result:\nwant=%s\ngot=%s", want, got)
	}

	badCounts := result
	badCounts.Counts.AdvisoryFindings++
	if _, err := CanonicalResultBytes(badCounts); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inconsistent counts error = %v, want ErrInvalidInput", err)
	}
	badRoute := result
	badRoute.Findings = append([]FindingDecision(nil), result.Findings...)
	badRoute.Findings[0].Route = RouteAdvisory
	if _, err := CanonicalResultBytes(badRoute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inconsistent finding route error = %v, want ErrInvalidInput", err)
	}
	badEvidenceRoute := result
	badEvidenceRoute.Findings = append([]FindingDecision(nil), result.Findings...)
	badEvidenceRoute.Findings[0].Evidence = append([]EvidenceDecision(nil), result.Findings[0].Evidence...)
	badEvidenceRoute.Findings[0].Evidence[0].Route = RouteAdvisory
	if _, err := CanonicalResultBytes(badEvidenceRoute); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inconsistent evidence route error = %v, want ErrInvalidInput", err)
	}
}

func TestCanonicalResultPreservesDerivedCounts(t *testing.T) {
	finding := testFinding("counted", "pkg/counted.go", "rule.counted")
	evidence := testEvidence(t, "counted-evidence", finding, EvidenceClassPolicy, 'c')
	result, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{evidence}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := CanonicalResultBytes(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CorroborationResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Counts != result.Counts {
		t.Fatalf("canonical counts=%#v, want %#v", decoded.Counts, result.Counts)
	}
}

func TestCanonicalResultRejectsReuseAndBounds(t *testing.T) {
	finding := testFinding("result", "pkg/result.go", "result.rule")
	evidence := testEvidence(t, "result-evidence", finding, EvidenceClassStatic, 'a')
	base, err := Corroborate(testInput([]CriticFinding{finding}, []Evidence{evidence}))
	if err != nil {
		t.Fatalf("Corroborate: %v", err)
	}

	duplicateID := base
	duplicateID.Findings = append([]FindingDecision(nil), base.Findings...)
	duplicateID.Findings = append(duplicateID.Findings, base.Findings[0])
	duplicateID.Counts.TotalFindings = 2
	duplicateID.Counts.BlockingFindings = 2
	if _, err := CanonicalResultBytes(duplicateID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate result finding ID error = %v, want ErrInvalidInput", err)
	}

	duplicateDigest := base
	duplicateDigest.Findings = append([]FindingDecision(nil), base.Findings...)
	duplicateDigest.Findings = append(duplicateDigest.Findings, FindingDecision{
		Finding:  testFinding("other-result", "pkg/other.go", "other.rule"),
		Route:    RouteBlocking,
		Reason:   "independently validated deterministic corroboration",
		Evidence: []EvidenceDecision{{ID: "other-evidence", Class: EvidenceClassStatic, Digest: evidence.Digest, Deterministic: true, Independent: true, Corroborates: true, Route: RouteBlocking}},
	})
	duplicateDigest.Counts = Counts{TotalFindings: 2, BlockingFindings: 2, DeterministicEvidence: 2, CorroboratingEvidence: 2}
	if _, err := CanonicalResultBytes(duplicateDigest); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate result evidence digest error = %v, want ErrInvalidInput", err)
	}

	tooLarge := CorroborationResult{SchemaVersion: SchemaVersion, Findings: make([]FindingDecision, MaxFindings)}
	tooLarge.Counts = Counts{TotalFindings: MaxFindings, AdvisoryFindings: MaxFindings}
	for i := range tooLarge.Findings {
		tooLarge.Findings[i] = FindingDecision{
			Finding:  testFinding(fmt.Sprintf("result-%03d", i), "pkg/file.go", "result.rule"),
			Route:    RouteAdvisory,
			Reason:   strings.Repeat("r", MaxEvidenceDescriptionBytes),
			Evidence: []EvidenceDecision{},
		}
	}
	if _, err := CanonicalResultBytes(tooLarge); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized result error = %v, want ErrInvalidInput", err)
	}
}

func TestConcurrentCorroborationHasStableOutput(t *testing.T) {
	finding := testFinding("concurrent", "pkg/concurrent.go", "concurrency.rule")
	evidence := testEvidence(t, "concurrent-evidence", finding, EvidenceClassPolicy, 'a')
	input := testInput([]CriticFinding{finding}, []Evidence{evidence})
	wantResult, err := Corroborate(input)
	if err != nil {
		t.Fatalf("initial Corroborate: %v", err)
	}
	wantBytes, err := CanonicalResultBytes(wantResult)
	if err != nil {
		t.Fatalf("initial result serialization: %v", err)
	}

	const workers = 32
	var wait sync.WaitGroup
	wait.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wait.Done()
			result, err := Corroborate(input)
			if err != nil {
				t.Errorf("concurrent Corroborate: %v", err)
				return
			}
			encoded, err := CanonicalResultBytes(result)
			if err != nil {
				t.Errorf("concurrent result serialization: %v", err)
				return
			}
			if !bytes.Equal(encoded, wantBytes) {
				t.Errorf("concurrent output differs")
			}
		}()
	}
	wait.Wait()
}

func TestCanonicalJSONRoundTripUsesOnlyKnownFields(t *testing.T) {
	finding := testFinding("json", "pkg/json.go", "json.rule")
	encoded, err := CanonicalInputBytes(testInput([]CriticFinding{finding}, []Evidence{}))
	if err != nil {
		t.Fatalf("CanonicalInputBytes: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal canonical input: %v", err)
	}
	if _, ok := decoded["unknown"]; ok {
		t.Fatal("canonical input contains an unknown field")
	}
	if _, err := ParseCanonicalInput(encoded); err != nil {
		t.Fatalf("ParseCanonicalInput: %v", err)
	}
}

func TestCanonicalJSONRejectsNestedUnknownAndDuplicateFields(t *testing.T) {
	finding := testFinding("json-nested", "pkg/json.go", "json.rule")
	evidence := testEvidence(t, "json-evidence", finding, EvidenceClassStatic, 'a')
	encoded, err := CanonicalInputBytes(testInput([]CriticFinding{finding}, []Evidence{evidence}))
	if err != nil {
		t.Fatalf("CanonicalInputBytes: %v", err)
	}
	needle := `"id":"json-evidence",`
	unknown := []byte(strings.Replace(string(encoded), needle, `"unknown":1,`+needle, 1))
	duplicate := []byte(strings.Replace(string(encoded), needle, `"id":"json-evidence","id":"json-evidence",`, 1))
	for name, mutated := range map[string][]byte{"nested unknown": unknown, "nested duplicate": duplicate} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCanonicalInput(mutated); err == nil {
				t.Fatalf("ParseCanonicalInput unexpectedly accepted %s", name)
			}
		})
	}
}
