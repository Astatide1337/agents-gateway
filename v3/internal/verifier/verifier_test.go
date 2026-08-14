package verifier

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

func TestCleanEnvironmentTrustsOnlyFixedVerifierCheckout(t *testing.T) {
	environment := cleanEnvironment(map[string]string{
		"GIT_CONFIG_COUNT":   "99",
		"GIT_CONFIG_KEY_0":   "safe.directory",
		"GIT_CONFIG_VALUE_0": "/tmp/untrusted",
		"AGW_TEST_EXTRA":     "retained",
	})
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = value
		}
	}
	if values["GIT_CONFIG_COUNT"] != "1" || values["GIT_CONFIG_KEY_0"] != "safe.directory" || values["GIT_CONFIG_VALUE_0"] != "/verify/workspace/repo" {
		t.Fatalf("verifier Git trust was not fixed to the verification checkout: %#v", values)
	}
	if values["AGW_TEST_EXTRA"] != "retained" {
		t.Fatalf("extra environment was not retained: %#v", values)
	}
}

func TestRunEmitsOneIdentityBoundStrictFrame(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"printf verifier-ok"}]}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.Count(output.String(), EvidenceFramePrefix) != 1 || strings.Contains(output.String(), "=") {
		t.Fatalf("stdout is not exactly one unpadded frame: %q", output.String())
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.RunUID != fixture.env["AGW_RUN_UID"] || evidence.SpecDigest != fixture.env["AGW_SPEC_DIGEST"] || evidence.BaseSHA != fixture.env["AGW_BASE_SHA"] {
		t.Fatalf("identity was not preserved: %+v", evidence)
	}
	if evidence.PatchDigest != digestBytes([]byte(fixture.patch)) || len(evidence.ChangedPaths) != 1 || evidence.ChangedPaths[0] != "main.go" || evidence.FilesChanged == nil || *evidence.FilesChanged != 1 || evidence.LinesChanged == nil || *evidence.LinesChanged != 2 || evidence.HasBinaryFiles == nil || *evidence.HasBinaryFiles {
		t.Fatalf("unexpected patch evidence: %+v", evidence)
	}
	if len(evidence.Commands) != 1 || evidence.Commands[0].ExitCode == nil || *evidence.Commands[0].ExitCode != 0 || evidence.Commands[0].EvidenceDigest == "" || evidence.Commands[0].DurationMillis == nil {
		t.Fatalf("command evidence is incomplete: %+v", evidence.Commands)
	}
	body, err := verifycontroller.DecodeEvidenceFrame(output.Bytes(), verifycontroller.DefaultMaxEvidenceBytes)
	if err != nil {
		t.Fatalf("controller rejected verifier frame: %v", err)
	}
	var controllerEvidence verifycontroller.MachineEvidence
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&controllerEvidence); err != nil {
		t.Fatalf("controller evidence decode failed: %v", err)
	}
	decision := gate.Evaluate(gate.Input{
		Scope:        v1alpha1.ScopeSpec{Paths: []string{"**"}},
		Requirements: v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 5, MaxDiffLines: 10, NoBinaryFiles: true},
		Commands:     []v1alpha1.VerifyCommand{{Shell: stringPtr("printf verifier-ok")}},
		Observations: gate.Observations{ChangedPaths: controllerEvidence.ChangedPaths, FilesChanged: controllerEvidence.FilesChanged, LinesChanged: controllerEvidence.LinesChanged, HasBinaryFiles: controllerEvidence.HasBinaryFiles, Commands: []gate.CommandObservation{{Index: controllerEvidence.Commands[0].Index, ExitCode: controllerEvidence.Commands[0].ExitCode, EvidenceDigest: controllerEvidence.Commands[0].EvidenceDigest, DurationMillis: controllerEvidence.Commands[0].DurationMillis}}},
	})
	if !decision.Accepted() {
		t.Fatalf("controller Gate rejected complete verifier evidence: %+v", decision.Checks)
	}
}

func TestRunExecutesImmutablePolicyProjectionAndEmitsEvidence(t *testing.T) {
	policyScript := []byte("#!/bin/sh\nexit 0\n")
	fixture := newFixture(t,
		map[string][]byte{"main.go": []byte("one\n"), "policies/check.sh": policyScript},
		map[string][]byte{"main.go": []byte("two\n"), "policies/check.sh": policyScript},
	)
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	contractDigest := "sha256:" + strings.Repeat("c", 64)
	policyCheck := policycontract.GateCheckDescriptor{
		CheckDescriptor: policycontract.CheckDescriptor{
			ContractDigest:        contractDigest,
			RuleID:                "blocking-policy",
			Severity:              policycontract.SeverityBlocking,
			Blocking:              true,
			HasScript:             true,
			Argv:                  []string{policycontract.ScriptRunner, ".agents/policies/blocking-policy/check.sh"},
			ScriptSourcePath:      "policies/check.sh",
			ContextPackOutputPath: ".agents/policies/blocking-policy/check.sh",
			ScriptDigest:          digestBytes(policyScript),
			Kind:                  policycontract.CheckKindScript,
			Expect:                policycontract.ExpectExit0,
			ExpectedExitCode:      policycontract.ExpectedExitCode,
			FailureMode:           policycontract.FailureReject,
		},
		RejectsOnFailure: true,
	}
	if err := policycontract.ValidateGateChecks([]policycontract.GateCheckDescriptor{policyCheck}); err != nil {
		t.Fatalf("test policy descriptor is invalid: %v", err)
	}
	encoded, err := json.Marshal([]policycontract.GateCheckDescriptor{policyCheck})
	if err != nil {
		t.Fatal(err)
	}
	fixture.env["AGW_VERIFY_POLICY_CHECKS_JSON"] = string(mustCanonicalJSON(t, encoded))

	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err != nil {
		t.Fatalf("Run() policy error = %v", err)
	}
	evidence := decodeFrame(t, output.Bytes())
	if len(evidence.PolicyChecks) != 1 {
		t.Fatalf("policy evidence count = %d, want one: %+v", len(evidence.PolicyChecks), evidence.PolicyChecks)
	}
	observation := evidence.PolicyChecks[0]
	if observation.RuleID != policyCheck.RuleID || observation.ContractDigest != contractDigest || observation.ScriptDigest != policyCheck.ScriptDigest || observation.ExitCode == nil || *observation.ExitCode != 0 || observation.Skipped {
		t.Fatalf("policy evidence did not bind the executed script: %+v", observation)
	}
	decision := gate.Evaluate(gate.Input{
		Scope:        v1alpha1.ScopeSpec{Paths: []string{"**"}},
		Requirements: v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 5, MaxDiffLines: 10, NoBinaryFiles: true},
		Commands:     []v1alpha1.VerifyCommand{{Argv: []string{"/bin/true"}}},
		PolicyRules: []gate.PolicyRule{{
			RuleID: policyCheck.RuleID, ContractDigest: policyCheck.ContractDigest, ScriptDigest: policyCheck.ScriptDigest, Blocking: true, HasScript: true,
		}},
		Observations: gate.Observations{
			ChangedPaths: evidence.ChangedPaths, FilesChanged: evidence.FilesChanged, LinesChanged: evidence.LinesChanged, HasBinaryFiles: evidence.HasBinaryFiles,
			Commands:     []gate.CommandObservation{{Index: 0, ExitCode: testInt32Ptr(0), EvidenceDigest: "sha256:" + strings.Repeat("a", 64), DurationMillis: int64Ptr(1)}},
			PolicyChecks: []gate.PolicyObservation{{RuleID: observation.RuleID, ContractDigest: observation.ContractDigest, ScriptDigest: observation.ScriptDigest, ExitCode: observation.ExitCode}},
		},
	})
	if !decision.Accepted() {
		t.Fatalf("Gate rejected verifier policy evidence: %#v", decision.Checks)
	}
}

func mustCanonicalJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	normalized, err := strictjson.Normalize(body)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func testInt32Ptr(value int32) *int32 { return &value }

func TestRunCommandFailureIsEvidenceNotAcceptance(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"argv":["/bin/sh","-c","exit 7"]}]}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err != nil {
		t.Fatalf("a measured non-zero Gate command should still emit evidence: %v", err)
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.Commands[0].ExitCode == nil || *evidence.Commands[0].ExitCode != 7 || evidence.Commands[0].EvidenceDigest == "" {
		t.Fatalf("non-zero command was not recorded: %+v", evidence.Commands[0])
	}
}

func TestRunTimeoutProducesMissingCommandEvidence(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_TIMEOUT"] = "40ms"
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"sleep 2"}]}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("timeout unexpectedly succeeded")
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.Commands[0].ExitCode != nil || evidence.Commands[0].EvidenceDigest != "" || evidence.Commands[0].DurationMillis != nil {
		t.Fatalf("timeout was represented as proof: %+v", evidence.Commands[0])
	}
}

func TestRunOutputOverflowProducesMissingCommandEvidence(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_MAX_OUTPUT_BYTES"] = "32"
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"head -c 4096 /dev/zero"}]}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("output overflow unexpectedly succeeded")
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.Commands[0].ExitCode != nil || evidence.Commands[0].EvidenceDigest != "" {
		t.Fatalf("oversized output was represented as proof: %+v", evidence.Commands[0])
	}
}

func TestRunRejectsMalformedOrUnsupportedConfiguration(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":2,"commands":[{"shell":"true"}]}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("unsupported command envelope unexpectedly succeeded")
	}
	if output.Len() != 0 {
		t.Fatalf("malformed configuration emitted a success-shaped frame: %q", output.String())
	}
}

func TestAnalyzeRejectsSymlinkAndUnsafePatchPath(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	if err := os.Symlink("/etc", filepath.Join(fixture.repo, "escape")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	if _, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, DefaultMaxPatchBytes); !errors.Is(err, ErrAnalysis) || !strings.Contains(err.Error(), ErrSymlink.Error()) {
		t.Fatalf("symlink was not rejected: %v", err)
	}

	fixture = newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/../escape b/../escape\n--- a/../escape\n+++ b/../escape\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	if _, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, DefaultMaxPatchBytes); err == nil {
		t.Fatal("unsafe patch path unexpectedly accepted")
	}
}

func TestAnalyzeDetectsBinaryAndContentChangesWithSameSize(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"data.bin": {0, 1, 2}}, map[string][]byte{"data.bin": {0, 1, 3}})
	fixture.patch = "diff --git a/data.bin b/data.bin\nindex 1111111..2222222 100644\nBinary files a/data.bin and b/data.bin differ\n"
	fixture.writePatch(t)
	analysis, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, DefaultMaxPatchBytes)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(analysis.ChangedPaths) != 1 || analysis.ChangedPaths[0] != "data.bin" || !analysis.HasBinaryFiles || analysis.LinesChanged != 0 {
		t.Fatalf("unexpected binary analysis: %+v", analysis)
	}
}

func TestAnalyzeIgnoresReadOnlyWritableHandoffPermissions(t *testing.T) {
	fixture := newFixture(t,
		map[string][]byte{"unchanged.txt": []byte("same\n")},
		map[string][]byte{"unchanged.txt": []byte("same\n"), "new.txt": []byte("new\n")},
	)
	if err := os.Chmod(filepath.Join(fixture.base, "unchanged.txt"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(fixture.repo, "unchanged.txt"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(fixture.repo, "new.txt"), 0666); err != nil {
		t.Fatal(err)
	}
	fixture.patch = "diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+new\n"
	fixture.writePatch(t)
	analysis, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, DefaultMaxPatchBytes)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(analysis.ChangedPaths) != 1 || analysis.ChangedPaths[0] != "new.txt" {
		t.Fatalf("handoff permissions changed the observed patch: %+v", analysis)
	}
}

func TestAnalyzeDoesNotMisclassifyUTF8SplitAcrossReadChunks(t *testing.T) {
	baseText := append(bytes.Repeat([]byte("a"), 32767), []byte("é\n")...)
	repoText := append(bytes.Repeat([]byte("a"), 32767), []byte("ö\n")...)
	fixture := newFixture(t, map[string][]byte{"unicode.txt": baseText}, map[string][]byte{"unicode.txt": repoText})
	fixture.patch = "diff --git a/unicode.txt b/unicode.txt\n--- a/unicode.txt\n+++ b/unicode.txt\n@@ -1 +1 @@\n-" + string(baseText) + "+" + string(repoText)
	fixture.writePatch(t)
	analysis, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, DefaultMaxPatchBytes)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if analysis.HasBinaryFiles {
		t.Fatal("valid UTF-8 split at the read boundary was marked binary")
	}
}

func TestRunTestStrengthCopiesOnlyChangedTests(t *testing.T) {
	fixture := newFixture(t,
		map[string][]byte{
			"go.mod":      []byte("module example.com/agwtest\n\ngo 1.22\n"),
			"main.go":     []byte("package agwtest\n\nfunc Value() int { return 1 }\n"),
			"foo_test.go": []byte("package agwtest\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fatalf(\"want 2, got %d\", Value()) } }\n"),
		},
		map[string][]byte{
			"go.mod":      []byte("module example.com/agwtest\n\ngo 1.22\n"),
			"main.go":     []byte("package agwtest\n\nfunc Value() int { return 2 }\n"),
			"foo_test.go": []byte("package agwtest\n\nimport \"testing\"\n\n// changed test file\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fatalf(\"want 2, got %d\", Value()) } }\n"),
		},
	)
	primeGoBuildCache(t, fixture.base)
	mirrorFixtureTree(t, filepath.Join(fixture.base, ".agw-go-cache"), filepath.Join(fixture.repo, ".agw-go-cache"))
	mirrorFixtureTree(t, filepath.Join(fixture.base, ".agw-go-modcache"), filepath.Join(fixture.repo, ".agw-go-modcache"))
	fixture.patch = "diff --git a/foo_test.go b/foo_test.go\n--- a/foo_test.go\n+++ b/foo_test.go\n@@ -1 +1 @@\n-old-test\n+new-test\n\n" +
		"diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_TIMEOUT"] = "20s"
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"argv":["/bin/true"]}]}`
	fixture.env["AGW_VERIFY_REQUIREMENTS_JSON"] = `{"version":2,"adapter":"go","testStrength":"newTestsMustFailOnBase","baseTestCommand":{"argv":["go","test","-vet=off","-count=1","./..."]}}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err != nil {
		t.Fatalf("test-strength verification error = %v", err)
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.NewTestsFailOnBase == nil || !*evidence.NewTestsFailOnBase {
		t.Fatalf("new test evidence was not earned: %+v", evidence)
	}
}

func TestRunTestStrengthFailsWithoutChangedTests(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("old\n")}, map[string][]byte{"main.go": []byte("new\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"argv":["/bin/true"]}]}`
	fixture.env["AGW_VERIFY_REQUIREMENTS_JSON"] = `{"version":2,"adapter":"go","testStrength":"newTestsMustFailOnBase","baseTestCommand":{"argv":["go","test","./..."]}}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("missing changed test files unexpectedly succeeded")
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.NewTestsFailOnBase == nil || *evidence.NewTestsFailOnBase {
		t.Fatalf("missing test files were represented as success: %+v", evidence)
	}
}

func TestRunCoverageMarkerCannotForgeCoverage(t *testing.T) {
	fixture := newFixture(t,
		map[string][]byte{"go.mod": []byte("module example.com/agwcoverage\n\ngo 1.26\n"), "main.go": []byte("package agwcoverage\n\nfunc Value() int { return 1 }\n")},
		map[string][]byte{"go.mod": []byte("module example.com/agwcoverage\n\ngo 1.26\n"), "main.go": []byte("package agwcoverage\n\nfunc Value() int { return 2 }\n")},
	)
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_TIMEOUT"] = "60s"
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"printf 'AGW_COVERAGE_DELTA_V1 0.4200\n'"}]}`
	fixture.env["AGW_VERIFY_REQUIREMENTS_JSON"] = `{"version":2,"adapter":"go","coverageDelta":">= 0"}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err != nil {
		t.Fatalf("coverage verification error = %v", err)
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.CoverageDelta == nil || *evidence.CoverageDelta == "0.42" {
		t.Fatalf("repository stdout forged coverage evidence: %+v", evidence)
	}
}

func TestRunTestStrengthIgnoresUnrelatedBuildFailure(t *testing.T) {
	fixture := newFixture(t,
		map[string][]byte{
			"go.mod":      []byte("module example.com/agwstrength\n\ngo 1.22\n"),
			"main.go":     []byte("package agwstrength\n\nfunc Value() int { return 1 }\n"),
			"foo_test.go": []byte("package agwstrength\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatalf(\"unexpected\") } }\n"),
		},
		map[string][]byte{
			"go.mod":      []byte("module example.com/agwstrength\n\ngo 1.22\n"),
			"main.go":     []byte("package agwstrength\n\nfunc Value() int { return 2 }\n"),
			"foo_test.go": []byte("package agwstrength\n\nimport \"testing\"\n\n// changed test file\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatalf(\"unexpected\") } }\n"),
		},
	)
	fixture.patch = "diff --git a/foo_test.go b/foo_test.go\n--- a/foo_test.go\n+++ b/foo_test.go\n@@ -1 +1 @@\n-old\n+new\n\n" +
		"diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_TIMEOUT"] = "20s"
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"argv":["go","build","./does-not-exist"]}]}`
	fixture.env["AGW_VERIFY_REQUIREMENTS_JSON"] = `{"version":2,"adapter":"go","testStrength":"newTestsMustFailOnBase","baseTestCommand":{"argv":["go","test","-count=1","./..."]}}`
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("test-strength unexpectedly accepted a passing explicit base test command")
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.NewTestsFailOnBase == nil || *evidence.NewTestsFailOnBase {
		t.Fatalf("unrelated build failure forged test-strength evidence: %+v", evidence)
	}
}

func TestRunExpectedPatchDigestMismatchCannotForgeSuccess(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_PATCH_DIGEST"] = "sha256:" + strings.Repeat("0", 64)
	var output bytes.Buffer
	if err := Run(context.Background(), fixture.lookup, &output); err == nil {
		t.Fatal("patch identity mismatch unexpectedly succeeded")
	}
	evidence := decodeFrame(t, output.Bytes())
	if evidence.Commands[0].ExitCode != nil || evidence.PatchDigest == fixture.env["AGW_PATCH_DIGEST"] {
		t.Fatalf("identity mismatch was represented as successful evidence: %+v", evidence)
	}
}

func TestEncodeEvidenceRejectsNondeterministicPathsAndPartialCommands(t *testing.T) {
	evidence := MachineEvidence{
		SchemaVersion: EvidenceSchemaVersion,
		RunUID:        "run-1",
		SpecDigest:    "sha256:" + strings.Repeat("a", 64),
		BaseSHA:       strings.Repeat("b", 40),
		PatchDigest:   "sha256:" + strings.Repeat("c", 64),
		ChangedPaths:  []string{"z.go", "a.go"},
		FilesChanged:  int64Ptr(2),
		Commands:      []CommandEvidence{{Index: 0, DurationMillis: int64Ptr(1)}},
	}
	if _, err := EncodeEvidenceFrame(evidence); err == nil {
		t.Fatal("invalid evidence unexpectedly encoded")
	}
}

type fixture struct {
	root, workspace, repo, base, patchPath string
	patch                                  string
	env                                    map[string]string
}

func newFixture(t *testing.T, baseFiles, repoFiles map[string][]byte) *fixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	repo := filepath.Join(workspace, "repo")
	base := filepath.Join(workspace, "base")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	fixture := &fixture{
		root: root, workspace: workspace, repo: repo, base: base,
		patchPath: filepath.Join(workspace, "patch.diff"), env: map[string]string{
			"AGW_WORKSPACE": workspace, "AGW_REPO_PATH": repo, "AGW_BASE_PATH": base,
			"AGW_PATCH_PATH": filepath.Join(workspace, "patch.diff"), "AGW_RUN_UID": "run-123",
			"AGW_SPEC_DIGEST": "sha256:" + strings.Repeat("a", 64), "AGW_BASE_SHA": strings.Repeat("b", 40),
			"AGW_VERIFY_TIMEOUT": "2s", "AGW_VERIFY_NETWORK": "disabled",
			"AGW_VERIFY_COMMANDS_JSON": `{"version":1,"commands":[{"argv":["/bin/true"]}]}`,
		},
	}
	writeFiles(t, base, baseFiles)
	writeFiles(t, repo, repoFiles)
	return fixture
}

func (f *fixture) writePatch(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.patchPath, []byte(f.patch), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) lookup(name string) (string, bool) {
	value, ok := f.env[name]
	return value, ok
}

func primeGoBuildCache(t *testing.T, root string) {
	t.Helper()
	cache := filepath.Join(root, ".agw-go-cache")
	modcache := filepath.Join(root, ".agw-go-modcache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(modcache, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-vet=off", "-run=^$", "./...")
	command.Dir = root
	command.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/nonexistent",
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"GOTOOLCHAIN=local",
		"GOCACHE=" + cache,
		"GOMODCACHE=" + modcache,
		"GOPROXY=off",
		"GOSUMDB=off",
		"GONOSUMDB=*",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("prime Go build cache: %v\n%s", err, output)
	}
}

func mirrorFixtureTree(t *testing.T, source, destination string) {
	t.Helper()
	const maxBytes = 512 << 20
	var copied int64
	if err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return os.MkdirAll(destination, 0700)
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("fixture cache contains a non-regular file")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || copied > maxBytes-info.Size() {
			return errors.New("fixture cache exceeds bounded setup")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != info.Size() {
			return io.ErrShortWrite
		}
		copied += written
		return nil
	}); err != nil {
		t.Fatalf("mirror fixture cache: %v", err)
	}
}

func writeFiles(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func decodeFrame(t *testing.T, frame []byte) MachineEvidence {
	t.Helper()
	if len(frame) < len(EvidenceFramePrefix)+2 || !bytes.HasPrefix(frame, []byte(EvidenceFramePrefix)) || frame[len(frame)-1] != '\n' {
		t.Fatalf("invalid frame: %q", frame)
	}
	line := frame[:len(frame)-1]
	encoded := line[len(EvidenceFramePrefix):]
	if strings.Contains(string(encoded), "=") {
		t.Fatal("frame contains base64 padding")
	}
	body, err := base64.RawStdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if err := strictjson.ValidateObject(body); err != nil {
		t.Fatalf("frame JSON is not strict: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var evidence MachineEvidence
	if err := decoder.Decode(&evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("evidence has trailing JSON: %v", err)
	}
	return evidence
}

func TestGoTestCommandRejectsAmbiguousOrNonTestCommands(t *testing.T) {
	cases := []v1alpha1.VerifyCommand{
		{Argv: []string{"go", "build", "./..."}},
		{Argv: []string{"/bin/false"}},
		{Shell: stringPtr("go test ./...")},
	}
	for _, command := range cases {
		if err := validateGoTestCommand(command); err == nil {
			t.Fatalf("ambiguous command accepted as a Go test command: %#v", command)
		}
	}
	if err := validateGoTestCommand(v1alpha1.VerifyCommand{Argv: []string{"go", "test", "./..."}}); err != nil {
		t.Fatalf("valid explicit Go test command rejected: %v", err)
	}
	if !gate.ValidateRepoPath("src/main.go") {
		t.Fatal("gate path contract unexpectedly rejected a normal path")
	}
}

func TestAnalyzePatchLimit(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "12345"
	fixture.writePatch(t)
	if _, err := Analyze(fixture.repo, fixture.base, fixture.patchPath, 4); !errors.Is(err, ErrPatchLimit) {
		t.Fatalf("expected patch limit, got %v", err)
	}
}

func TestRunHonorsParentCancellation(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"sleep 2"}]}`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var output bytes.Buffer
	if err := Run(ctx, fixture.lookup, &output); err == nil {
		t.Fatal("cancelled verifier unexpectedly succeeded")
	}
	if output.Len() == 0 {
		t.Fatal("cancelled verifier did not emit bounded failure evidence")
	}
}

func TestFallbackFrameIsNotAcceptedShape(t *testing.T) {
	fixture := newFixture(t, map[string][]byte{"main.go": []byte("one\n")}, map[string][]byte{"main.go": []byte("two\n")})
	fixture.patch = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-one\n+two\n"
	fixture.writePatch(t)
	fixture.env["AGW_VERIFY_COMMANDS_JSON"] = `{"version":1,"commands":[{"shell":"true"}]}`
	fixture.env["AGW_PATCH_DIGEST"] = "sha256:" + strings.Repeat("d", 64)
	var output bytes.Buffer
	_ = Run(context.Background(), fixture.lookup, &output)
	evidence := decodeFrame(t, output.Bytes())
	if evidence.HasBinaryFiles == nil || !*evidence.HasBinaryFiles || evidence.Commands[0].ExitCode != nil {
		t.Fatalf("fallback frame could be mistaken for acceptance: %+v", evidence)
	}
}
