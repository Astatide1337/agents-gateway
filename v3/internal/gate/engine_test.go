package gate

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestEvaluateAcceptsCompleteIndependentEvidence(t *testing.T) {
	decision := Evaluate(testInput())
	if decision.Verdict != Accepted || !decision.Accepted() {
		t.Fatalf("expected Accepted, got %#v", decision)
	}
	if len(decision.Checks) != 7 {
		t.Fatalf("expected seven deterministic checks, got %d: %#v", len(decision.Checks), decision.Checks)
	}
	for _, check := range decision.Checks {
		if !check.Passed {
			t.Fatalf("unexpected failed check: %#v", check)
		}
	}
}

func TestEvaluateRunUsesAgentRunScopeAndGatePolicy(t *testing.T) {
	input := testInput()
	run := v1alpha1.AgentRun{}
	run.Spec.Scope = input.Scope
	policy := v1alpha1.Gate{}
	policy.Spec.Require = input.Requirements
	policy.Spec.Verify.Commands = input.Commands
	decision := EvaluateRun(run, policy, input.Observations, nil)
	if !decision.Accepted() {
		t.Fatalf("EvaluateRun rejected valid input: %#v", decision)
	}
	run.Spec.Scope.Paths = []string{"other/**"}
	decision = EvaluateRun(run, policy, input.Observations, nil)
	if decision.Accepted() {
		t.Fatal("EvaluateRun ignored AgentRun scope")
	}
}

func TestEvaluatePolicyChecksFailClosedAndKeepAdvisoryNonBlocking(t *testing.T) {
	input := testInput()
	input.PolicyRules = []PolicyRule{
		{RuleID: "blocking-rule", ContractDigest: testDigest("b"), ScriptDigest: testDigest("c"), Blocking: true, HasScript: true},
		{RuleID: "advisory-rule", ContractDigest: testDigest("b"), ScriptDigest: testDigest("d"), Blocking: false, HasScript: true},
	}
	input.Observations.PolicyChecks = []PolicyObservation{
		{RuleID: "blocking-rule", ContractDigest: testDigest("b"), ScriptDigest: testDigest("c"), ExitCode: int32Ptr(0)},
		{RuleID: "advisory-rule", ContractDigest: testDigest("b"), ScriptDigest: testDigest("d"), ExitCode: int32Ptr(7)},
	}
	decision := Evaluate(input)
	if !decision.Accepted() {
		t.Fatalf("advisory policy failure rejected the run: %#v", decision.Checks)
	}
	if check := findCheck(decision, "policy.blocking"); check == nil || !check.Passed {
		t.Fatalf("blocking policy did not pass: %#v", check)
	}
	if check := findCheck(decision, "policy.advisory"); check == nil || !check.Passed || !strings.Contains(check.Message, "1 advisory") {
		t.Fatalf("advisory policy was not recorded as non-blocking: %#v", check)
	}

	input.Observations.PolicyChecks[0].ExitCode = int32Ptr(7)
	decision = Evaluate(input)
	if decision.Accepted() {
		t.Fatal("blocking policy failure was accepted")
	}
	if check := findCheck(decision, "policy.blocking"); check == nil || check.Passed {
		t.Fatalf("blocking policy failure was not projected: %#v", check)
	}

	input.Observations.PolicyChecks[0].ExitCode = int32Ptr(0)
	input.Observations.PolicyChecks[0].ContractDigest = testDigest("e")
	decision = Evaluate(input)
	if decision.Accepted() {
		t.Fatal("policy contract mismatch was accepted")
	}
	if check := findCheck(decision, "policy.evidence"); check == nil || check.Passed {
		t.Fatalf("policy contract mismatch was not rejected: %#v", check)
	}
}

func TestEvaluateMissingEvidenceFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
		check  string
	}{
		{"files", func(input *Input) { input.Observations.FilesChanged = nil }, "files.max"},
		{"lines", func(input *Input) { input.Observations.LinesChanged = nil }, "diff.lines.max"},
		{"binary", func(input *Input) { input.Observations.HasBinaryFiles = nil }, "binary.none"},
		{"coverage", func(input *Input) { input.Observations.CoverageDelta = nil }, "coverage.delta"},
		{"new tests", func(input *Input) { input.Observations.NewTestsFailOnBase = nil }, "tests.new-fail-on-base"},
		{"command exit", func(input *Input) { input.Observations.Commands[0].ExitCode = nil }, "verification.command.000"},
		{"command digest", func(input *Input) { input.Observations.Commands[0].EvidenceDigest = "" }, "verification.command.000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.mutate(&input)
			decision := Evaluate(input)
			if decision.Accepted() {
				t.Fatal("missing evidence was accepted")
			}
			if check := findCheck(decision, test.check); check == nil || check.Passed {
				t.Fatalf("expected failed %s check, got %#v", test.check, check)
			}
		})
	}
}

func TestEvaluateRequirementBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		files      int64
		lines      int64
		coverage   string
		binary     bool
		wantAccept bool
	}{
		{"exact limits", 2, 100, "0", false, true},
		{"too many files", 3, 100, "0", false, false},
		{"too many lines", 2, 101, "0", false, false},
		{"coverage below", 2, 100, "-0.01", false, false},
		{"binary", 2, 100, "0", true, false},
		{"negative files", -1, 100, "0", false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			input.Observations.FilesChanged = int64Ptr(test.files)
			input.Observations.LinesChanged = int64Ptr(test.lines)
			input.Observations.CoverageDelta = stringPtr(test.coverage)
			input.Observations.HasBinaryFiles = boolPtr(test.binary)
			if got := Evaluate(input).Accepted(); got != test.wantAccept {
				t.Fatalf("accepted=%v, want %v", got, test.wantAccept)
			}
		})
	}
}

func TestEvaluateTestStrengthModes(t *testing.T) {
	input := testInput()
	input.Requirements.TestStrength = v1alpha1.TestStrengthNone
	input.Observations.NewTestsFailOnBase = nil
	if !Evaluate(input).Accepted() {
		t.Fatal("disabled test-strength rule should not require evidence")
	}

	input.Requirements.TestStrength = v1alpha1.TestStrengthNewTestsFailOnBase
	input.Observations.NewTestsFailOnBase = boolPtr(false)
	if Evaluate(input).Accepted() {
		t.Fatal("test-strength failure was accepted")
	}
	input.Observations.NewTestsFailOnBase = boolPtr(true)
	if !Evaluate(input).Accepted() {
		t.Fatal("test-strength evidence was rejected")
	}
}

func TestEvaluateScopeStillRequiresFileCountWhenNoFileLimitIsSet(t *testing.T) {
	input := testInput()
	input.Requirements.MaxFilesChanged = 0
	input.Observations.FilesChanged = nil
	decision := Evaluate(input)
	if decision.Accepted() {
		t.Fatal("scope evaluation accepted without file-count evidence")
	}
	if check := findCheck(decision, "files.evidence"); check == nil || check.Passed {
		t.Fatalf("expected failed files.evidence check, got %#v", check)
	}
}

func TestEvaluateScopeRejectsForbiddenAndUnlistedPaths(t *testing.T) {
	input := testInput()
	input.Observations.ChangedPaths = append(input.Observations.ChangedPaths, "src/config.secret")
	input.Observations.FilesChanged = int64Ptr(3)
	input.Requirements.MaxFilesChanged = 3
	if Evaluate(input).Accepted() {
		t.Fatal("forbidden path was accepted")
	}
	input = testInput()
	input.Scope.Paths = nil
	if Evaluate(input).Accepted() {
		t.Fatal("unlisted path was accepted")
	}
}

func TestEvaluateRejectsMalformedPolicyAndObservations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"absolute path", func(input *Input) { input.Observations.ChangedPaths[0] = "/etc/passwd" }},
		{"traversal path", func(input *Input) { input.Observations.ChangedPaths[0] = "src/../secret.go" }},
		{"backslash path", func(input *Input) { input.Observations.ChangedPaths[0] = `src\\main.go` }},
		{"nul path", func(input *Input) { input.Observations.ChangedPaths[0] = "src/\x00.go" }},
		{"duplicate path", func(input *Input) { input.Observations.ChangedPaths[1] = input.Observations.ChangedPaths[0] }},
		{"unknown command", func(input *Input) {
			input.Observations.Commands = append(input.Observations.Commands, CommandObservation{Index: 9})
		}},
		{"duplicate command", func(input *Input) {
			input.Observations.Commands = append(input.Observations.Commands, input.Observations.Commands[0])
		}},
		{"invalid glob", func(input *Input) { input.Scope.Paths = []string{"src/["} }},
		{"unsafe coverage", func(input *Input) { input.Requirements.CoverageDelta = ">= nope" }},
		{"missing test strength", func(input *Input) { input.Requirements.TestStrength = "" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.mutate(&input)
			if Evaluate(input).Accepted() {
				t.Fatal("malformed input was accepted")
			}
		})
	}
}

func TestEvaluateBoundsAreFinite(t *testing.T) {
	input := testInput()
	input.Observations.ChangedPaths = make([]string, MaxChangedPaths+1)
	if Evaluate(input).Accepted() {
		t.Fatal("oversized path evidence was accepted")
	}
	input = testInput()
	input.Commands = make([]v1alpha1.VerifyCommand, MaxCommands+1)
	if Evaluate(input).Accepted() {
		t.Fatal("oversized command configuration was accepted")
	}
	input = testInput()
	input.Observations.Commands = make([]CommandObservation, MaxCommandEvidence+1)
	if Evaluate(input).Accepted() {
		t.Fatal("oversized command evidence was accepted")
	}
}

func testInput() Input {
	exitCode := int32(0)
	duration := int64(125)
	files := int64(2)
	lines := int64(100)
	binary := false
	coverage := "0.25"
	newTestsFail := true
	return Input{
		Scope: v1alpha1.ScopeSpec{
			Paths:     []string{"src/**", "tests/**"},
			Forbidden: []string{"**/*.secret"},
		},
		Requirements: v1alpha1.GateRequirements{
			ScopeRespected:  true,
			TestStrength:    v1alpha1.TestStrengthNewTestsFailOnBase,
			CoverageDelta:   ">= 0",
			MaxFilesChanged: 2,
			MaxDiffLines:    100,
			NoBinaryFiles:   true,
		},
		Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}},
		Observations: Observations{
			ChangedPaths:       []string{"src/main.go", "tests/main_test.go"},
			FilesChanged:       &files,
			LinesChanged:       &lines,
			HasBinaryFiles:     &binary,
			CoverageDelta:      &coverage,
			NewTestsFailOnBase: &newTestsFail,
			Commands: []CommandObservation{{
				Index:          0,
				ExitCode:       &exitCode,
				EvidenceDigest: "sha256:" + strings.Repeat("a", 64),
				DurationMillis: &duration,
			}},
		},
	}
}

func findCheck(decision Decision, name string) *v1alpha1.GateCheck {
	for index := range decision.Checks {
		if decision.Checks[index].Name == name {
			return &decision.Checks[index]
		}
	}
	return nil
}

func int64Ptr(value int64) *int64    { return &value }
func int32Ptr(value int32) *int32    { return &value }
func boolPtr(value bool) *bool       { return &value }
func stringPtr(value string) *string { return &value }

func testDigest(value string) string { return "sha256:" + strings.Repeat(value, 64) }

func FuzzEvaluateNeverPanics(f *testing.F) {
	for _, seed := range []struct {
		path, pattern, coverage, digest string
	}{
		{"src/main.go", "src/**", "0", "sha256:" + strings.Repeat("a", 64)},
		{"../escape", "../**", "not-a-number", ""},
		{"\x00", "[", "-1.0", strings.Repeat("x", 10000)},
	} {
		f.Add(seed.path, seed.pattern, seed.coverage, seed.digest)
	}
	f.Fuzz(func(t *testing.T, path, pattern, coverage, digest string) {
		input := testInput()
		input.Observations.ChangedPaths = []string{path}
		input.Observations.FilesChanged = int64Ptr(1)
		input.Scope.Paths = []string{pattern}
		input.Observations.CoverageDelta = stringPtr(coverage)
		input.Observations.Commands[0].EvidenceDigest = digest
		_ = Evaluate(input)
	})
}
