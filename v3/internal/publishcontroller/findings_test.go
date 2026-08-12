package publishcontroller

import (
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
)

func TestFindingsPublicationParserRecomputesResult(t *testing.T) {
	input := Input{
		RunUID: "run-uid", SpecDigest: digest("a"), BaseSHA: strings.Repeat("b", 40),
		Patch:      artifactRef("patch", digest("c")),
		GateReport: artifactRef("report", digest("d")),
	}
	corroborationInput := findingsInput(t)
	derived, err := findingcorroboration.Corroborate(corroborationInput)
	if err != nil {
		t.Fatal(err)
	}
	artifact := findingsartifact.Artifact{
		Version: findingsartifact.Version, RunUID: input.RunUID, SpecDigest: input.SpecDigest,
		BaseSHA: input.BaseSHA, PatchDigest: input.Patch.Digest, GateReportDigest: input.GateReport.Digest,
		Input: corroborationInput, Result: derived,
	}
	body, err := findingsartifact.CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseFindingsArtifact(body, input, input.Patch.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Counts != derived.Counts {
		t.Fatalf("derived counts=%#v, want %#v", got.Counts, derived.Counts)
	}

	artifact.Result.Findings[0].Reason = "forged publication result"
	forged, err := findingsartifact.CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFindingsArtifact(forged, input, input.Patch.Digest); !errors.Is(err, ErrArtifactIdentity) {
		t.Fatalf("forged result error=%v, want identity mismatch", err)
	}
}

func TestFindingsPublicationCanonicalDigestIsStable(t *testing.T) {
	corroborationInput := findingsInput(t)
	result, err := findingcorroboration.Corroborate(corroborationInput)
	if err != nil {
		t.Fatal(err)
	}
	artifact := findingsartifact.Artifact{
		Version: findingsartifact.Version, RunUID: "run-uid", SpecDigest: digest("a"),
		BaseSHA: strings.Repeat("b", 40), PatchDigest: digest("c"), GateReportDigest: digest("d"),
		Input: corroborationInput, Result: result,
	}
	first, err := findingsartifact.CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	second, err := findingsartifact.CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if digestBytes(first) != digestBytes(second) {
		t.Fatalf("artifact digest changed: first=%s second=%s", digestBytes(first), digestBytes(second))
	}
}

func findingsInput(t *testing.T) findingcorroboration.CorroborationInput {
	t.Helper()
	finding := findingcorroboration.CriticFinding{
		ID: "finding-1", Path: "src/main.go",
		Location: findingcorroboration.Location{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2},
		RuleID:   "rule-1", Message: "bounded finding",
	}
	evidence, err := findingcorroboration.SealEvidence(findingcorroboration.Evidence{
		ID: "evidence-1", Class: findingcorroboration.ClassStatic,
		Binding:   findingcorroboration.EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID},
		Immutable: true, ArtifactDigest: digest("e"),
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

func artifactRef(kind, value string) v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{Kind: kind, Digest: value, SizeBytes: 1}
}

func digest(value string) string {
	return "sha256:" + strings.Repeat(value, 64)
}
