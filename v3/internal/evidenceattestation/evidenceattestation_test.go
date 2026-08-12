package evidenceattestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

func TestTrustedDualFormatRoundTripPreservesGateEvidence(t *testing.T) {
	verified, publicKey := testVerifiedReport(t, true)
	report := verified.Report()
	severities := make([]CheckSeverityBinding, 0, len(report.Checks))
	for index := len(report.Checks) - 1; index >= 0; index-- {
		severity := SeverityBlocking
		if index == 0 {
			severity = SeverityAdvisory
		}
		severities = append(severities, CheckSeverityBinding{Name: report.Checks[index].Name, Severity: severity})
	}
	evidence := []string{
		"sha256:" + strings.Repeat("9", 64),
		"sha256:" + strings.Repeat("8", 64),
	}
	options := Options{EvidenceArtifactDigests: evidence, CheckSeverities: severities}
	statement, err := StatementFromVerifiedReport(verified, options)
	if err != nil {
		t.Fatal(err)
	}
	body, err := StatementBytes(statement)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseStatement(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(MediaType, body, verified, options); err != nil {
		t.Fatalf("dual-format verification failed: %v", err)
	}
	if parsed.Type != StatementType || parsed.PredicateType != PredicateType || len(parsed.Subject) != 1 {
		t.Fatalf("unexpected statement header: %#v", parsed)
	}
	if got, want := parsed.Subject[0].Digest["sha256"], strings.TrimPrefix(report.PatchDigest, "sha256:"); got != want {
		t.Fatalf("subject digest=%q, want %q", got, want)
	}
	if parsed.Predicate.GateGeneration != 9_007_199_254_740_993 {
		t.Fatalf("Gate generation lost 64-bit precision: %d", parsed.Predicate.GateGeneration)
	}
	if parsed.Predicate.SignedReportDigest != verified.SignedReportDigest() || parsed.Predicate.ReportDigest != verified.ReportDigest() {
		t.Fatalf("report identities were not preserved: %#v", parsed.Predicate)
	}
	if parsed.Predicate.Scoring.ResultDigest != report.Scoring.ResultDigest || parsed.Predicate.Scoring.ExecutionScoreBasisPoints != report.Scoring.ExecutionScoreBasisPoints || parsed.Predicate.Scoring.WeightedScoreBasisPoints != report.Scoring.WeightedScoreBasisPoints {
		t.Fatalf("scoring projection was not preserved: %#v", parsed.Predicate.Scoring)
	}
	if len(parsed.Predicate.EvidenceArtifactDigests) != 2 || parsed.Predicate.EvidenceArtifactDigests[0] != evidence[1] || parsed.Predicate.EvidenceArtifactDigests[1] != evidence[0] {
		t.Fatalf("evidence identities were not canonicalized: %#v", parsed.Predicate.EvidenceArtifactDigests)
	}
	if parsed.Predicate.Checks[0].Severity != SeverityAdvisory {
		t.Fatalf("advisory classification was not preserved: %#v", parsed.Predicate.Checks)
	}
	second, err := StatementBytes(statement)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, second) {
		t.Fatal("statement bytes are not deterministic")
	}
	firstDigest, err := StatementDigest(statement)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := StatementDigest(parsed)
	if err != nil || firstDigest != secondDigest {
		t.Fatalf("statement digest is not deterministic: %q/%q err=%v", firstDigest, secondDigest, err)
	}
	if err := VerifyArtifact("application/json", body, verified, options); !errors.Is(err, ErrMediaTypeMismatch) {
		t.Fatalf("wrong media type error=%v, want ErrMediaTypeMismatch", err)
	}
	_ = publicKey
}

func TestTamperingEveryGateBindingFailsVerification(t *testing.T) {
	verified, _ := testVerifiedReport(t, true)
	options := testOptions(verified.Report())
	original, err := StatementFromVerifiedReport(verified, options)
	if err != nil {
		t.Fatal(err)
	}
	originalBytes, err := StatementBytes(original)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Statement)
	}{
		{"subject patch", func(statement *Statement) { statement.Subject[0].Digest["sha256"] = strings.Repeat("0", 64) }},
		{"predicate type", func(statement *Statement) { statement.PredicateType = "https://example.invalid/wrong" }},
		{"run UID", func(statement *Statement) { statement.Predicate.RunUID = "other-run" }},
		{"spec digest", func(statement *Statement) { statement.Predicate.SpecDigest = "sha256:" + strings.Repeat("1", 64) }},
		{"base SHA", func(statement *Statement) { statement.Predicate.BaseSHA = strings.Repeat("d", 40) }},
		{"patch digest", func(statement *Statement) { statement.Predicate.PatchDigest = "sha256:" + strings.Repeat("2", 64) }},
		{"Gate UID", func(statement *Statement) { statement.Predicate.GateUID = "other-gate" }},
		{"Gate generation", func(statement *Statement) { statement.Predicate.GateGeneration++ }},
		{"report digest", func(statement *Statement) { statement.Predicate.ReportDigest = "sha256:" + strings.Repeat("3", 64) }},
		{"scoring digest", func(statement *Statement) {
			statement.Predicate.Scoring.ResultDigest = "sha256:" + strings.Repeat("3", 64)
		}},
		{"signed report digest", func(statement *Statement) {
			statement.Predicate.SignedReportDigest = "sha256:" + strings.Repeat("4", 64)
		}},
		{"raw evidence identity", func(statement *Statement) {
			statement.Predicate.EvidenceArtifactDigests[0] = "sha256:" + strings.Repeat("7", 64)
		}},
		{"runtime image", func(statement *Statement) {
			statement.Predicate.RuntimeImageDigest = "ghcr.io/example/runtime@sha256:" + strings.Repeat("5", 64)
		}},
		{"helper image", func(statement *Statement) {
			statement.Predicate.HelperImageDigests[0].Digest = "ghcr.io/example/helper@sha256:" + strings.Repeat("6", 64)
		}},
		{"skill identity", func(statement *Statement) { statement.Predicate.SkillDigests[0] = "sha256:" + strings.Repeat("a", 64) }},
		{"changed evidence", func(statement *Statement) { statement.Predicate.Evidence.ChangedPaths[0] = "src/other.go" }},
		{"command evidence", func(statement *Statement) {
			statement.Predicate.Commands[0].EvidenceDigest = "sha256:" + strings.Repeat("b", 64)
		}},
		{"check result", func(statement *Statement) {
			statement.Predicate.Checks[0].Passed = !statement.Predicate.Checks[0].Passed
		}},
		{"check message", func(statement *Statement) { statement.Predicate.Checks[0].Message = "tampered" }},
		{"check severity", func(statement *Statement) { statement.Predicate.Checks[0].Severity = SeverityAdvisory }},
		{"verdict", func(statement *Statement) { statement.Predicate.Verdict = gate.Rejected }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mutated := cloneStatement(original)
			test.mutate(&mutated)
			body, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyStatement(body, verified, options); err == nil {
				t.Fatal("tampered Gate binding was accepted")
			}
		})
	}
	if err := VerifyStatement(originalBytes, verified, Options{EvidenceArtifactDigests: []string{"sha256:" + strings.Repeat("e", 64)}, CheckSeverities: options.CheckSeverities}); err == nil {
		t.Fatal("changed expected evidence identity was accepted")
	}
}

func TestStrictParsingRejectsUnknownDuplicateAndNonCanonicalFields(t *testing.T) {
	verified, _ := testVerifiedReport(t, true)
	statement, err := StatementFromVerifiedReport(verified, testOptions(verified.Report()))
	if err != nil {
		t.Fatal(err)
	}
	body, err := StatementBytes(statement)
	if err != nil {
		t.Fatal(err)
	}
	unknown := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"unexpected":true}`)...)
	if _, err := ParseStatement(unknown); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
	duplicate := append(append([]byte(nil), body[:len(body)-1]...), []byte(`,"_type":"https://in-toto.io/Statement/v1"}`)...)
	if _, err := ParseStatement(duplicate); err == nil {
		t.Fatal("duplicate JSON field was accepted")
	}
	if _, err := ParseStatement(append([]byte("\n"), body...)); !errors.Is(err, ErrStatementNonCanonical) {
		t.Fatalf("noncanonical whitespace error=%v, want ErrStatementNonCanonical", err)
	}
	if _, err := ParseStatement(append(body, []byte(" ")...)); !errors.Is(err, ErrStatementNonCanonical) {
		t.Fatalf("noncanonical trailing whitespace error=%v, want ErrStatementNonCanonical", err)
	}
	if _, err := ParseStatement(append(body, []byte("{}")...)); err == nil {
		t.Fatal("trailing JSON document was accepted")
	}
}

func TestTrustedSourceAndDefensiveCopies(t *testing.T) {
	verified, publicKey := testVerifiedReport(t, true)
	if _, err := VerifySignedReportBytes(verified.SignedReportBytes(), nil); !errors.Is(err, ErrUntrustedReport) {
		t.Fatalf("nil trusted key error=%v, want ErrUntrustedReport", err)
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySignedReportBytes(verified.SignedReportBytes(), otherPublic); !errors.Is(err, ErrUntrustedReport) {
		t.Fatalf("wrong trusted key error=%v, want ErrUntrustedReport", err)
	}
	if _, err := StatementFromVerifiedReport(VerifiedReport{}, Options{}); !errors.Is(err, ErrUntrustedReport) {
		t.Fatalf("forged proof error=%v, want ErrUntrustedReport", err)
	}

	options := testOptions(verified.Report())
	statement, err := StatementFromVerifiedReport(verified, options)
	if err != nil {
		t.Fatal(err)
	}
	reportCopy := verified.Report()
	originalSkill := reportCopy.SkillDigests[0]
	originalFiles := *reportCopy.Evidence.FilesChanged
	originalCommandEvidence := reportCopy.Commands[0].EvidenceDigest
	reportCopy.HelperImageDigests[0].Name = "mutated"
	reportCopy.SkillDigests[0] = "sha256:" + strings.Repeat("0", 64)
	*reportCopy.Evidence.FilesChanged = 99
	reportCopy.Commands[0].EvidenceDigest = "sha256:" + strings.Repeat("0", 64)
	current := verified.Report()
	if current.HelperImageDigests[0].Name == "mutated" || current.SkillDigests[0] != originalSkill || *current.Evidence.FilesChanged != originalFiles || current.Commands[0].EvidenceDigest != originalCommandEvidence {
		t.Fatal("verified report accessor leaked mutable state")
	}
	canonical := verified.CanonicalReportBytes()
	canonical[0] ^= 0xff
	signed := verified.SignedReportBytes()
	signed[0] ^= 0xff
	if bytes.Equal(canonical, verified.CanonicalReportBytes()) || bytes.Equal(signed, verified.SignedReportBytes()) {
		t.Fatal("verified byte accessor leaked mutable state")
	}
	statement.Predicate.SkillDigests[1] = "sha256:" + strings.Repeat("a", 64)
	again, err := StatementFromVerifiedReport(verified, options)
	if err != nil {
		t.Fatal(err)
	}
	if again.Predicate.SkillDigests[1] == statement.Predicate.SkillDigests[1] {
		t.Fatal("statement construction retained caller-owned slices")
	}
	_ = publicKey
}

func TestBoundsAndAdvisoryOptionValidation(t *testing.T) {
	verified, _ := testVerifiedReport(t, true)
	report := verified.Report()
	tooMany := make([]string, MaxEvidenceArtifactDigests+1)
	for index := range tooMany {
		tooMany[index] = "sha256:" + strings.Repeat(string(rune('a'+index%6)), 64)
	}
	if _, err := StatementFromVerifiedReport(verified, Options{EvidenceArtifactDigests: tooMany}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized evidence options error=%v, want ErrInvalidInput", err)
	}
	if _, err := StatementFromVerifiedReport(verified, Options{CheckSeverities: []CheckSeverityBinding{{Name: report.Checks[0].Name, Severity: SeverityAdvisory}}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("partial severity options error=%v, want ErrInvalidInput", err)
	}
	duplicate := testOptions(report)
	duplicate.EvidenceArtifactDigests = append(duplicate.EvidenceArtifactDigests, duplicate.EvidenceArtifactDigests[0])
	if _, err := StatementFromVerifiedReport(verified, duplicate); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate evidence options error=%v, want ErrInvalidInput", err)
	}
	statement, err := StatementFromVerifiedReport(verified, testOptions(report))
	if err != nil {
		t.Fatal(err)
	}
	body, err := StatementBytes(statement)
	if err != nil {
		t.Fatal(err)
	}
	oversized := append(append([]byte(nil), body...), bytes.Repeat([]byte(" "), MaxStatementBytes-len(body)+1)...)
	if _, err := ParseStatement(oversized); err == nil {
		t.Fatal("oversized statement was accepted")
	}
}

func TestRejectedReportIsStillLosslesslyAttested(t *testing.T) {
	verified, _ := testVerifiedReport(t, false)
	statement, err := StatementFromVerifiedReport(verified, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if statement.Predicate.Verdict != gate.Rejected {
		t.Fatalf("verdict=%q, want Rejected", statement.Predicate.Verdict)
	}
	body, err := StatementBytes(statement)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyStatement(body, verified, Options{}); err != nil {
		t.Fatalf("rejected statement did not verify: %v", err)
	}
}

func testVerifiedReport(t *testing.T, accepted bool) (VerifiedReport, ed25519.PublicKey) {
	t.Helper()
	files := int64(1)
	lines := int64(17)
	hasBinary := false
	duration := int64(25)
	path := "src/main.go"
	if !accepted {
		path = "../escape"
	}
	decision := gate.Evaluate(gate.Input{
		Scope: v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
		Requirements: v1alpha1.GateRequirements{
			ScopeRespected: true,
			TestStrength:   v1alpha1.TestStrengthNone,
			NoBinaryFiles:  true,
		},
		Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}},
		Observations: gate.Observations{
			ChangedPaths: pathList(path), FilesChanged: &files, LinesChanged: &lines,
			HasBinaryFiles: &hasBinary,
			Commands:       []gate.CommandObservation{{Index: 0, ExitCode: int32Ptr(0), EvidenceDigest: "sha256:" + strings.Repeat("f", 64), DurationMillis: &duration}},
		},
	})
	if accepted && !decision.Accepted() {
		t.Fatalf("test Gate decision unexpectedly rejected: %#v", decision.Checks)
	}
	if !accepted && decision.Accepted() {
		t.Fatal("test Gate decision unexpectedly accepted")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID:              "run-uid-1",
		SpecDigest:          "sha256:" + strings.Repeat("b", 64),
		BaseSHA:             strings.Repeat("c", 40),
		PatchDigest:         "sha256:" + strings.Repeat("d", 64),
		GateUID:             "gate-uid-1",
		GateGeneration:      9_007_199_254_740_993,
		RuntimeImageDigest:  "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64),
		VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
		HelperImageDigests: []gate.ImageEvidence{
			{Name: "lockdown", Digest: "ghcr.io/example/lockdown@sha256:" + strings.Repeat("1", 64)},
			{Name: "clone", Digest: "ghcr.io/example/clone@sha256:" + strings.Repeat("2", 64)},
		},
		SkillDigests: []string{"sha256:" + strings.Repeat("f", 64), "sha256:" + strings.Repeat("0", 64)},
	}, decision)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := gate.SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := gate.SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifySignedReportBytes(encoded, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return verified, publicKey
}

func testOptions(report gate.VerificationReport) Options {
	severities := make([]CheckSeverityBinding, 0, len(report.Checks))
	for _, check := range report.Checks {
		severities = append(severities, CheckSeverityBinding{Name: check.Name, Severity: SeverityBlocking})
	}
	return Options{
		EvidenceArtifactDigests: []string{
			"sha256:" + strings.Repeat("8", 64),
			"sha256:" + strings.Repeat("9", 64),
		},
		CheckSeverities: severities,
	}
}

func cloneStatement(input Statement) Statement {
	output := input
	output.Subject = append([]Subject(nil), input.Subject...)
	for index := range output.Subject {
		output.Subject[index].Digest = make(map[string]string, len(input.Subject[index].Digest))
		for key, value := range input.Subject[index].Digest {
			output.Subject[index].Digest[key] = value
		}
	}
	output.Predicate.EvidenceArtifactDigests = append([]string(nil), input.Predicate.EvidenceArtifactDigests...)
	output.Predicate.HelperImageDigests = append([]gate.ImageEvidence(nil), input.Predicate.HelperImageDigests...)
	output.Predicate.SkillDigests = append([]string(nil), input.Predicate.SkillDigests...)
	output.Predicate.Evidence = cloneEvidence(input.Predicate.Evidence)
	output.Predicate.Commands = cloneCommands(input.Predicate.Commands)
	output.Predicate.Checks = append([]CheckBinding(nil), input.Predicate.Checks...)
	return output
}

func pathList(path string) []string { return []string{path} }

func int32Ptr(value int32) *int32 { return &value }
