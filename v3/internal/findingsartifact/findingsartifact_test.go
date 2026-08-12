package findingsartifact

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
)

func TestCanonicalBytesAndDigestAreStable(t *testing.T) {
	input := testInput(t)
	result, err := findingcorroboration.Corroborate(input)
	if err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{
		Version: Version, RunUID: "run-uid", SpecDigest: digest('a'),
		BaseSHA: strings.Repeat("b", 40), PatchDigest: digest('c'),
		GateReportDigest: digest('d'), Input: input, Result: result,
	}
	first, err := CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical artifact bytes changed across encodes")
	}
	parsed, derived, err := Verify(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, mustCanonical(t, parsed)) {
		t.Fatal("parse/verify changed canonical artifact bytes")
	}
	if got, want := derived.Counts, result.Counts; got != want {
		t.Fatalf("derived counts=%#v, want %#v", got, want)
	}
}

func TestVerifyRejectsForgedDerivedResult(t *testing.T) {
	input := testInput(t)
	result, err := findingcorroboration.Corroborate(input)
	if err != nil {
		t.Fatal(err)
	}
	result.Findings[0].Reason = "forged runner verdict"
	artifact := Artifact{
		Version: Version, RunUID: "run-uid", SpecDigest: digest('a'),
		BaseSHA: strings.Repeat("b", 40), PatchDigest: digest('c'),
		GateReportDigest: digest('d'), Input: input, Result: result,
	}
	body, err := CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(body); err != ErrResultMismatch {
		t.Fatalf("Verify() error=%v, want %v", err, ErrResultMismatch)
	}
}

func TestParseCanonicalRejectsNoncanonicalAndUnknownFields(t *testing.T) {
	input := testInput(t)
	result, err := findingcorroboration.Corroborate(input)
	if err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{
		Version: Version, RunUID: "run-uid", SpecDigest: digest('a'),
		BaseSHA: strings.Repeat("b", 40), PatchDigest: digest('c'),
		GateReportDigest: digest('d'), Input: input, Result: result,
	}
	body, err := CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCanonical(append(append([]byte(nil), body...), '\n')); err == nil {
		t.Fatal("ParseCanonical accepted trailing whitespace")
	}
	unknown := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"unexpected":true}`)...)
	if _, err := ParseCanonical(unknown); err == nil {
		t.Fatal("ParseCanonical accepted an unknown field")
	}
}

func testInput(t *testing.T) findingcorroboration.CorroborationInput {
	t.Helper()
	finding := findingcorroboration.CriticFinding{
		ID: "finding-1", Path: "src/main.go",
		Location: findingcorroboration.Location{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2},
		RuleID:   "rule-1", Message: "bounded finding",
	}
	evidence, err := findingcorroboration.SealEvidence(findingcorroboration.Evidence{
		ID: "evidence-1", Class: findingcorroboration.ClassStatic,
		Binding:   findingcorroboration.EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID},
		Immutable: true, ArtifactDigest: digest('e'),
		Validation: findingcorroboration.EvidenceValidation{Validator: "ast-grep", Deterministic: true, Independent: true},
		Static:     &findingcorroboration.StaticEvidence{Tool: "ast-grep", Matched: true, MatchCount: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return findingcorroboration.CorroborationInput{
		SchemaVersion: findingcorroboration.SchemaVersion,
		Findings:      []findingcorroboration.CriticFinding{finding},
		Evidence:      []findingcorroboration.Evidence{evidence},
	}
}

func digest(value byte) string {
	return "sha256:" + strings.Repeat(string(value), 64)
}

func mustCanonical(t *testing.T, artifact Artifact) []byte {
	t.Helper()
	body, err := CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
