package gate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestBuildReportIsCanonicalAndBindsEvidence(t *testing.T) {
	decision := Evaluate(testInput())
	context := ReportContext{
		RunUID:              "run-uid-1",
		SpecDigest:          "sha256:" + strings.Repeat("b", 64),
		BaseSHA:             strings.Repeat("c", 40),
		PatchDigest:         "sha256:" + strings.Repeat("d", 64),
		GateUID:             "gate-uid-1",
		GateGeneration:      4,
		RuntimeImageDigest:  "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64),
		VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
		HelperImageDigests: []ImageEvidence{
			{Name: "lockdown", Digest: "ghcr.io/example/lockdown@sha256:" + strings.Repeat("1", 64)},
			{Name: "clone", Digest: "ghcr.io/example/clone@sha256:" + strings.Repeat("2", 64)},
			{Name: "broker", Digest: "ghcr.io/example/broker@sha256:" + strings.Repeat("3", 64)},
		},
		SkillDigests: []string{"sha256:" + strings.Repeat("f", 64), "sha256:" + strings.Repeat("0", 64)},
	}
	report, err := BuildReport(context, decision)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != Accepted || report.SchemaVersion != ReportSchemaVersion {
		t.Fatalf("unexpected report header: %#v", report)
	}
	if report.Scoring.ExecutionWeightBasisPoints != v1alpha1.GateScoreScale || report.Scoring.CriticWeightBasisPoints != 0 || report.Scoring.ExecutionScoreBasisPoints != v1alpha1.GateScoreScale || report.Scoring.WeightedScoreBasisPoints != v1alpha1.GateScoreScale || report.Scoring.ResultDigest == "" {
		t.Fatalf("execution-only scoring projection=%#v", report.Scoring)
	}
	if len(report.Commands) != 1 || !report.Commands[0].Observed || report.Commands[0].EvidenceDigest == "" {
		t.Fatalf("command evidence was not bound: %#v", report.Commands)
	}
	if !sortStrings(report.SkillDigests) {
		t.Fatalf("skill digests are not canonical: %#v", report.SkillDigests)
	}
	first, err := CanonicalReportBytes(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalReportBytes(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("canonical report bytes changed between serializations")
	}
	digest, err := ReportDigest(report)
	if err != nil || !validSHA256Digest(digest) {
		t.Fatalf("invalid report digest %q: %v", digest, err)
	}
}

func TestLegacyReportSchemaRequiresExplicitMigration(t *testing.T) {
	report, err := BuildReport(testReportContext(), Evaluate(testInput()))
	if err != nil {
		t.Fatal(err)
	}
	report.SchemaVersion = LegacyReportSchemaVersion
	if _, err := CanonicalReportBytes(report); err == nil || !strings.Contains(err.Error(), "unsupported verification report schema") {
		t.Fatalf("legacy report was silently accepted: %v", err)
	}
	if ReportSchemaVersion == LegacyReportSchemaVersion {
		t.Fatal("report schema did not bump after adding scoring provenance")
	}
}

func TestSignedReportRoundTripAndTamperDetection(t *testing.T) {
	decision := Evaluate(testInput())
	report, err := BuildReport(testReportContext(), decision)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignedReport(signed, publicKey); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	encoded, err := SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSignedReport(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignedReport(parsed, publicKey); err != nil {
		t.Fatalf("parsed signature rejected: %v", err)
	}
	encodedAgain, err := SignedReportBytes(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, encodedAgain) {
		t.Fatal("signed report bytes are not deterministic")
	}

	tamperedReport := signed
	tamperedReport.Report.PatchDigest = "sha256:" + strings.Repeat("1", 64)
	if err := VerifySignedReport(tamperedReport, publicKey); err == nil {
		t.Fatal("tampered report verified")
	}
	tamperedSignature := signed
	tamperedSignature.Signature = base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if err := VerifySignedReport(tamperedSignature, publicKey); err == nil {
		t.Fatal("tampered signature verified")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignedReport(signed, otherPublic); err == nil {
		t.Fatal("untrusted signer was accepted")
	}
	if bytes.Contains(encoded, privateKey.Seed()) {
		t.Fatal("signed report contains private signing material")
	}
}

func TestReportRejectsInvalidBoundsAndDecisionForgery(t *testing.T) {
	decision := Evaluate(testInput())
	context := testReportContext()
	cases := []struct {
		name   string
		mutate func(*VerificationReport)
	}{
		{"run UID", func(report *VerificationReport) { report.RunUID = "" }},
		{"spec digest", func(report *VerificationReport) { report.SpecDigest = "sha256:bad" }},
		{"image digest", func(report *VerificationReport) { report.VerifierImageDigest = "latest" }},
		{"runtime image digest", func(report *VerificationReport) { report.RuntimeImageDigest = "latest" }},
		{"gate identity", func(report *VerificationReport) { report.GateUID = "" }},
		{"helper image", func(report *VerificationReport) { report.HelperImageDigests[0].Digest = "latest" }},
		{"path escape", func(report *VerificationReport) { report.Evidence.ChangedPaths[0] = "../escape" }},
		{"check message", func(report *VerificationReport) {
			report.Checks[0].Message = strings.Repeat("x", MaxReportMessageBytes+1)
		}},
		{"missing observed evidence", func(report *VerificationReport) { report.Commands[0].Observed = false }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			report, err := BuildReport(context, decision)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&report)
			if _, err := CanonicalReportBytes(report); err == nil {
				t.Fatal("invalid report was accepted")
			}
		})
	}
	forged := Decision{Verdict: Accepted, Checks: []v1alpha1.GateCheck{{Name: "forged", Passed: true}}}
	if _, err := BuildReport(context, forged); err == nil {
		t.Fatal("hand-built accepted decision produced a report")
	}
}

func TestRejectedDecisionWithUnsafeEvidenceStillProducesSafeReport(t *testing.T) {
	input := testInput()
	input.Observations.ChangedPaths[0] = "../escape"
	input.Observations.Commands[0].DurationMillis = int64Ptr(-1)
	decision := Evaluate(input)
	if decision.Accepted() {
		t.Fatal("unsafe evidence was accepted")
	}
	report, err := BuildReport(testReportContext(), decision)
	if err != nil {
		t.Fatalf("rejected evidence could not be reported: %v", err)
	}
	if report.Verdict != Rejected || len(report.Evidence.ChangedPaths) != 1 {
		t.Fatalf("unsafe evidence was not safely reduced: %#v", report)
	}
	if report.Commands[0].Observed {
		t.Fatal("invalid duration was retained as observed command evidence")
	}
	if _, err := CanonicalReportBytes(report); err != nil {
		t.Fatal(err)
	}
}

func TestReportCanonicalizesOrderIndependentCollections(t *testing.T) {
	decision := Evaluate(testInput())
	report, err := BuildReport(testReportContext(), decision)
	if err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalReportBytes(report)
	if err != nil {
		t.Fatal(err)
	}
	reordered := report
	reordered.SkillDigests = append([]string(nil), report.SkillDigests...)
	if len(reordered.SkillDigests) > 1 {
		reordered.SkillDigests[0], reordered.SkillDigests[1] = reordered.SkillDigests[1], reordered.SkillDigests[0]
	}
	reordered.HelperImageDigests = append([]ImageEvidence(nil), report.HelperImageDigests...)
	for left, right := 0, len(reordered.HelperImageDigests)-1; left < right; left, right = left+1, right-1 {
		reordered.HelperImageDigests[left], reordered.HelperImageDigests[right] = reordered.HelperImageDigests[right], reordered.HelperImageDigests[left]
	}
	reordered.Checks = append([]v1alpha1.GateCheck(nil), report.Checks...)
	for left, right := 0, len(reordered.Checks)-1; left < right; left, right = left+1, right-1 {
		reordered.Checks[left], reordered.Checks[right] = reordered.Checks[right], reordered.Checks[left]
	}
	second, err := CanonicalReportBytes(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("order-independent report fields changed canonical bytes")
	}
}

func TestParseSignedReportRejectsUnknownFields(t *testing.T) {
	decision := Evaluate(testInput())
	report, err := BuildReport(testReportContext(), decision)
	if err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		SignedReport
		Extra string `json:"extra"`
	}{SignedReport: signed, Extra: "unexpected"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSignedReport(encoded); err == nil {
		t.Fatal("unknown signed report field was accepted")
	}
}

func testReportContext() ReportContext {
	return ReportContext{
		RunUID:              "run-uid-1",
		SpecDigest:          "sha256:" + strings.Repeat("b", 64),
		BaseSHA:             strings.Repeat("c", 40),
		PatchDigest:         "sha256:" + strings.Repeat("d", 64),
		GateUID:             "gate-uid-1",
		GateGeneration:      4,
		RuntimeImageDigest:  "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64),
		VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
		HelperImageDigests: []ImageEvidence{
			{Name: "clone", Digest: "ghcr.io/example/clone@sha256:" + strings.Repeat("2", 64)},
			{Name: "lockdown", Digest: "ghcr.io/example/lockdown@sha256:" + strings.Repeat("1", 64)},
			{Name: "broker", Digest: "ghcr.io/example/broker@sha256:" + strings.Repeat("3", 64)},
		},
		SkillDigests: []string{"sha256:" + strings.Repeat("f", 64)},
	}
}

func sortStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] > values[index] {
			return false
		}
	}
	return true
}

func FuzzReportTamperingNeverPanics(f *testing.F) {
	decision := Evaluate(testInput())
	report, err := BuildReport(testReportContext(), decision)
	if err != nil {
		f.Fatalf("build report: %v", err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.Fatalf("generate key: %v", err)
	}
	signed, err := SignReport(report, privateKey)
	if err != nil {
		f.Fatalf("sign report: %v", err)
	}
	for _, seed := range []string{"", "x", "sha256:bad", strings.Repeat("z", 300)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		tampered := signed
		tampered.PublicKey = value
		tampered.Signature = value
		tampered.Report.RunUID = value
		_, _ = SignedReportBytes(tampered)
		_ = VerifySignedReport(tampered, nil)
	})
}
