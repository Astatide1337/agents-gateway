// Package gate contains the pure, independent Gate decision engine.
//
// It deliberately has no Kubernetes, filesystem, process, or network
// dependencies.  A verifier supplies bounded observations; this package turns
// those observations and the declared API policy into a deterministic
// decision and a signed evidence report.
package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
)

const (
	// MaxChangedPaths is the maximum number of changed paths accepted as one
	// bounded observation. It is intentionally larger than the API scope limit
	// because a patch may contain more files than its allowlist.
	MaxChangedPaths = 4096
	// MaxPathBytes is the maximum UTF-8 byte length of a repository path.
	MaxPathBytes = 512
	// MaxScopePatterns is the maximum combined number of allow and deny glob
	// patterns examined by one evaluation.
	MaxScopePatterns = 256
	// MaxPatternBytes is the maximum UTF-8 byte length of a scope glob.
	MaxPatternBytes = 512
	// MaxCommands is the maximum number of configured verification commands.
	MaxCommands = 32
	// MaxCommandEvidence is the maximum number of command observations.
	MaxCommandEvidence = 32
	// MaxChecks bounds the decision and report check list. It leaves room for
	// every API rule plus every configured command and an input-integrity check.
	MaxChecks = 64
	// MaxObservedFiles and MaxObservedLines prevent nonsensical counter
	// values from entering a report while retaining int64 counters at the API.
	MaxObservedFiles = int64(MaxChangedPaths)
	MaxObservedLines = int64(1_000_000_000)
	// MaxDurationMillis bounds command duration evidence.
	MaxDurationMillis = int64(24 * 60 * 60 * 1000)
)

// Verdict is the only terminal result emitted by the decision engine.
type Verdict string

const (
	Accepted Verdict = "Accepted"
	Rejected Verdict = "Rejected"
)

// CommandObservation is evidence for one configured Gate verification
// command. A missing slice entry, nil ExitCode, or empty/invalid digest is
// missing evidence and fails that command check.
type CommandObservation struct {
	Index          int
	ExitCode       *int32
	EvidenceDigest string
	DurationMillis *int64
}

// PolicyRule is the verifier-facing identity of one deterministic Policy
// rule. It is intentionally smaller than policycontract.GateCheckDescriptor
// so the pure Gate engine does not depend on a filesystem or compiler.
type PolicyRule struct {
	RuleID         string
	ContractDigest string
	ScriptDigest   string
	Blocking       bool
	HasScript      bool
}

// PolicyObservation is emitted by the independent verifier after it has
// executed the exact script selected by PolicyRule. A missing ExitCode or a
// digest mismatch is missing evidence, never a pass.
type PolicyObservation struct {
	RuleID         string
	ContractDigest string
	ScriptDigest   string
	ExitCode       *int32
	Skipped        bool
}

// Observations are the only facts consumed by Evaluate. Pointer fields make
// absence distinguishable from a measured zero, which is essential for
// fail-closed evaluation.
type Observations struct {
	ChangedPaths       []string
	FilesChanged       *int64
	LinesChanged       *int64
	HasBinaryFiles     *bool
	CoverageDelta      *string
	NewTestsFailOnBase *bool
	Commands           []CommandObservation
	PolicyChecks       []PolicyObservation
}

// Input is the pure policy/evidence boundary. Commands are copied from
// Gate.Spec.Verify.Commands; Scope is copied from AgentRun.Spec.Scope.
type Input struct {
	Scope        v1alpha1.ScopeSpec
	Requirements v1alpha1.GateRequirements
	Commands     []v1alpha1.VerifyCommand
	PolicyRules  []PolicyRule
	Observations Observations
}

// EvaluateRun is a convenience adapter for callers holding the API objects.
// It still performs no Kubernetes work and uses only the run scope and Gate
// policy.
func EvaluateRun(run v1alpha1.AgentRun, gate v1alpha1.Gate, observations Observations, policyRules []PolicyRule) Decision {
	return Evaluate(Input{
		Scope:        run.Spec.Scope,
		Requirements: gate.Spec.Require,
		Commands:     gate.Spec.Verify.Commands,
		PolicyRules:  policyRules,
		Observations: observations,
	})
}

// Decision is the bounded result of an evaluation. The exported fields are
// suitable for projection into AgentRun.status. The private fields preserve
// the exact normalized evidence needed to build a report from this decision;
// callers cannot manufacture an accepted report by constructing Decision by
// hand.
type Decision struct {
	Verdict Verdict
	Checks  []v1alpha1.GateCheck

	evaluated      bool
	commandRecords []ReportCommand
	evidence       EvidenceSummary
}

// Accepted reports whether every emitted check passed.
func (d Decision) Accepted() bool { return d.Verdict == Accepted }

// Evaluate applies every enabled requirement and every configured command.
// It never returns an error: malformed policy, malformed observations, and
// missing evidence are represented as bounded failed checks. This makes it
// impossible for a caller to accidentally interpret an error path as an
// accepted Gate.
func Evaluate(input Input) Decision {
	d := Decision{Verdict: Rejected}
	checks := make([]v1alpha1.GateCheck, 0, 16)
	checkNames := make(map[string]struct{}, 16)
	inputInvalid := false

	add := func(name string, passed bool, message string) {
		if len(checks) >= MaxChecks {
			inputInvalid = true
			return
		}
		if len(name) == 0 || len(name) > 128 || !utf8.ValidString(name) {
			inputInvalid = true
			return
		}
		if _, exists := checkNames[name]; exists {
			inputInvalid = true
			return
		}
		checkNames[name] = struct{}{}
		if len(message) > v1alpha1.MaxStatusMessage {
			message = message[:v1alpha1.MaxStatusMessage]
		}
		checks = append(checks, v1alpha1.GateCheck{Name: name, Passed: passed, Message: message})
	}
	invalid := func(name, message string) {
		inputInvalid = true
		add(name, false, message)
	}

	obs := normalizeObservations(input.Observations)
	if len(input.Observations.ChangedPaths) > MaxChangedPaths || len(input.Observations.Commands) > MaxCommandEvidence {
		invalid("input.bounds", "bounded observation limit exceeded")
	}
	if len(input.Scope.Paths)+len(input.Scope.Forbidden) > MaxScopePatterns {
		invalid("input.scope.bounds", "bounded scope pattern limit exceeded")
	}
	if len(input.Commands) > MaxCommands {
		invalid("configuration.commands", "verification command limit exceeded")
	}
	if len(input.PolicyRules) > 512 || len(input.Observations.PolicyChecks) > 512 {
		invalid("policy.bounds", "policy check limit exceeded")
	}

	pathValid, duplicatePath := validateChangedPaths(obs.ChangedPaths)
	if !pathValid || duplicatePath {
		invalid("observations.paths", "changed path evidence is not normalized")
	}

	allowValid := validatePatterns(input.Scope.Paths)
	forbidValid := validatePatterns(input.Scope.Forbidden)
	if !allowValid || !forbidValid {
		invalid("configuration.scope", "scope contains an invalid glob")
	}

	needFiles := input.Requirements.ScopeRespected || input.Requirements.MaxFilesChanged > 0
	needLines := input.Requirements.MaxDiffLines > 0
	needBinary := input.Requirements.NoBinaryFiles
	needCoverage := strings.TrimSpace(input.Requirements.CoverageDelta) != ""
	needTests := input.Requirements.TestStrength == v1alpha1.TestStrengthNewTestsFailOnBase

	if needFiles {
		passed := obs.FilesChanged != nil && validCounter(obs.FilesChanged, MaxObservedFiles)
		if passed {
			passed = *obs.FilesChanged == int64(len(obs.ChangedPaths))
		}
		if input.Requirements.MaxFilesChanged > 0 && passed {
			passed = *obs.FilesChanged <= int64(input.Requirements.MaxFilesChanged)
		}
		message := "file count is within the configured bound"
		if !passed {
			message = "file count evidence is missing, inconsistent, or over the configured bound"
		}
		if input.Requirements.MaxFilesChanged > 0 {
			add("files.max", passed, message)
		} else {
			// Scope checking still requires an independently recorded file
			// count even when no numeric maximum is configured.
			add("files.evidence", passed, message)
		}
	}

	if needLines {
		passed := obs.LinesChanged != nil && validCounter(obs.LinesChanged, MaxObservedLines)
		if passed {
			passed = *obs.LinesChanged <= int64(input.Requirements.MaxDiffLines)
		}
		message := "diff line count is within the configured bound"
		if !passed {
			message = "diff line count evidence is missing, invalid, or over the configured bound"
		}
		add("diff.lines.max", passed, message)
	}

	if needBinary {
		passed := obs.HasBinaryFiles != nil && !*obs.HasBinaryFiles
		message := "patch contains no binary files"
		if !passed {
			message = "binary-file evidence is missing or the patch contains a binary file"
		}
		add("binary.none", passed, message)
	}

	if input.Requirements.ScopeRespected {
		passed := pathValid && duplicatePath == false && allowValid && forbidValid && pathsRespectScope(obs.ChangedPaths, input.Scope)
		message := "all changed paths are allowed and not forbidden"
		if !passed {
			message = "a changed path is outside the allowlist, forbidden, or path evidence is invalid"
		}
		add("scope.respected", passed, message)
	}

	if needCoverage {
		passed := false
		if obs.CoverageDelta != nil {
			passed = coverageSatisfies(input.Requirements.CoverageDelta, *obs.CoverageDelta)
		}
		message := "coverage delta satisfies the configured requirement"
		if !passed {
			message = "coverage delta evidence is missing, malformed, or below the configured requirement"
		}
		add("coverage.delta", passed, message)
	}

	if needTests {
		passed := obs.NewTestsFailOnBase != nil && *obs.NewTestsFailOnBase
		message := "new tests fail on the pristine base revision"
		if !passed {
			message = "new-tests-on-base evidence is missing or the tests pass on the base revision"
		}
		add("tests.new-fail-on-base", passed, message)
	}

	commandRecords, commandEvidenceInvalid := evaluateCommands(input.Commands, obs.Commands, add)
	if commandEvidenceInvalid {
		invalid("verification.evidence", "verification command evidence is malformed or contains an unknown command")
	}
	if len(input.Commands) == 0 {
		invalid("configuration.commands", "at least one verification command is required")
	}
	evaluatePolicyChecks(input.PolicyRules, obs.PolicyChecks, add)

	if input.Requirements.TestStrength != v1alpha1.TestStrengthNone && !needTests {
		invalid("configuration.test-strength", "unsupported test-strength policy")
	}
	if input.Requirements.TestStrength == "" {
		invalid("configuration.test-strength", "test-strength policy is missing")
	}
	if input.Requirements.CoverageDelta != "" && !validCoverageRequirement(input.Requirements.CoverageDelta) {
		invalid("configuration.coverage", "coverage requirement is malformed")
	}
	if input.Requirements.MaxFilesChanged < 0 || input.Requirements.MaxDiffLines < 0 {
		invalid("configuration.bounds", "configured file or line bound is negative")
	}

	if inputInvalid && len(checks) == 0 {
		// This branch is defensive for a future change to MaxChecks or check
		// name validation. It keeps the fail-closed invariant explicit.
		checks = append(checks, v1alpha1.GateCheck{Name: "input.valid", Passed: false, Message: "gate input is invalid"})
	}
	for _, check := range checks {
		if !check.Passed {
			inputInvalid = true
			break
		}
	}
	if !inputInvalid {
		d.Verdict = Accepted
	}
	d.Checks = checks
	d.commandRecords = commandRecords
	d.evidence = obs.summary()
	d.evaluated = true
	return d
}

func evaluatePolicyChecks(rules []PolicyRule, observations []PolicyObservation, add func(string, bool, string)) {
	if len(rules) == 0 && len(observations) == 0 {
		return
	}
	if len(rules) == 0 || len(observations) == 0 {
		add("policy.evidence", false, "policy contract or policy evidence is missing")
		return
	}
	expected := make(map[string]PolicyRule, len(rules))
	contractDigest := ""
	configValid := true
	for _, rule := range rules {
		if rule.RuleID == "" || !canonical.ValidDigest(rule.ContractDigest) || (rule.HasScript && !canonical.ValidDigest(rule.ScriptDigest)) || (!rule.HasScript && rule.ScriptDigest != "") {
			configValid = false
		}
		if contractDigest == "" {
			contractDigest = rule.ContractDigest
		} else if contractDigest != rule.ContractDigest {
			configValid = false
		}
		if _, exists := expected[rule.RuleID]; exists {
			configValid = false
		}
		expected[rule.RuleID] = rule
	}
	seen := make(map[string]struct{}, len(observations))
	blockingFailures := 0
	advisoryFailures := 0
	for _, observation := range observations {
		if observation.RuleID == "" || observation.ContractDigest != contractDigest {
			configValid = false
		}
		if _, exists := seen[observation.RuleID]; exists {
			configValid = false
		}
		seen[observation.RuleID] = struct{}{}
		rule, ok := expected[observation.RuleID]
		if !ok {
			configValid = false
			continue
		}
		passed := observation.ScriptDigest == rule.ScriptDigest && observation.ExitCode != nil && *observation.ExitCode == 0
		if !rule.HasScript {
			passed = observation.Skipped && observation.ScriptDigest == "" && observation.ExitCode == nil
		}
		if !passed {
			if rule.Blocking {
				blockingFailures++
			} else {
				advisoryFailures++
			}
		}
	}
	if len(seen) != len(expected) {
		configValid = false
	}
	add("policy.evidence", configValid, "policy observations match the immutable contract")
	add("policy.blocking", configValid && blockingFailures == 0, policyFailureMessage(blockingFailures, "blocking"))
	// Advisory findings are intentionally visible in the report but cannot
	// reject a run. Keep this check passing while retaining the count in its
	// message so the bounded status projection does not turn advisory policy
	// into an enforcement rule.
	add("policy.advisory", true, policyFailureMessage(advisoryFailures, "advisory"))
}

func policyFailureMessage(count int, kind string) string {
	if count == 0 {
		return "no " + kind + " policy checks failed"
	}
	return fmt.Sprintf("%d %s policy check(s) failed", count, kind)
}

func evaluateCommands(configured []v1alpha1.VerifyCommand, observed []CommandObservation, add func(string, bool, string)) ([]ReportCommand, bool) {
	if len(configured) > MaxCommands {
		configured = configured[:MaxCommands]
	}
	byIndex := make(map[int][]CommandObservation, len(observed))
	evidenceInvalid := false
	for _, item := range observed {
		if item.Index < 0 || item.Index >= len(configured) || len(byIndex[item.Index]) > 0 {
			evidenceInvalid = true
		}
		byIndex[item.Index] = append(byIndex[item.Index], item)
	}
	records := make([]ReportCommand, 0, len(configured))
	for index, command := range configured {
		configDigest := digestVerifyCommand(command)
		items := byIndex[index]
		passed := false
		message := "verification command passed with recorded evidence"
		record := ReportCommand{Index: index, ConfigDigest: configDigest}
		if len(items) == 1 {
			item := items[0]
			validEvidence := item.ExitCode != nil && validEvidenceDigest(item.EvidenceDigest) && validDuration(item.DurationMillis)
			if validEvidence {
				record.Observed = true
				record.ExitCode = cloneInt32(item.ExitCode)
				record.EvidenceDigest = item.EvidenceDigest
				record.DurationMillis = cloneInt64(item.DurationMillis)
			}
			passed = validVerifyCommand(command) && validEvidence && *item.ExitCode == 0
			if !passed {
				message = "verification command failed or its evidence is missing/invalid"
			}
		} else {
			message = "verification command evidence is missing or duplicated"
			evidenceInvalid = true
		}
		add(fmt.Sprintf("verification.command.%03d", index), passed, message)
		records = append(records, record)
	}
	return records, evidenceInvalid
}

func validVerifyCommand(command v1alpha1.VerifyCommand) bool {
	argv := len(command.Argv) > 0
	shell := command.Shell != nil && *command.Shell != ""
	if argv == shell || len(command.Argv) > 64 {
		return false
	}
	if shell && (len(*command.Shell) > 4096 || !utf8.ValidString(*command.Shell)) {
		return false
	}
	for _, arg := range command.Argv {
		if len(arg) == 0 || len(arg) > 4096 || !utf8.ValidString(arg) {
			return false
		}
	}
	return true
}

func normalizeObservations(input Observations) Observations {
	output := input
	if len(input.ChangedPaths) <= MaxChangedPaths {
		output.ChangedPaths = append([]string(nil), input.ChangedPaths...)
		for index, path := range output.ChangedPaths {
			if !validRepoPath(path) {
				// Do not retain or sort unbounded/unportable attacker input.
				// The marker is intentionally invalid and guarantees rejection.
				output.ChangedPaths[index] = "\x00"
			}
		}
		sort.Strings(output.ChangedPaths)
	} else {
		// Do not copy or sort attacker-controlled over-limit input. The
		// caller receives the bounded input.bounds failure instead.
		output.ChangedPaths = nil
	}
	if len(input.Commands) <= MaxCommandEvidence {
		output.Commands = append([]CommandObservation(nil), input.Commands...)
		sort.Slice(output.Commands, func(i, j int) bool {
			return commandObservationLess(output.Commands[i], output.Commands[j])
		})
	} else {
		output.Commands = nil
	}
	if len(input.PolicyChecks) <= 512 {
		output.PolicyChecks = append([]PolicyObservation(nil), input.PolicyChecks...)
	} else {
		output.PolicyChecks = nil
	}
	output.FilesChanged = cloneInt64(input.FilesChanged)
	output.LinesChanged = cloneInt64(input.LinesChanged)
	output.HasBinaryFiles = cloneBool(input.HasBinaryFiles)
	output.CoverageDelta = cloneString(input.CoverageDelta)
	output.NewTestsFailOnBase = cloneBool(input.NewTestsFailOnBase)
	return output
}

func (o Observations) summary() EvidenceSummary {
	paths := make([]string, 0, len(o.ChangedPaths))
	seen := make(map[string]struct{}, len(o.ChangedPaths))
	for _, path := range o.ChangedPaths {
		if validRepoPath(path) {
			if _, exists := seen[path]; !exists {
				seen[path] = struct{}{}
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	files := cloneInt64(o.FilesChanged)
	if !validCounter(files, MaxObservedFiles) {
		files = nil
	}
	lines := cloneInt64(o.LinesChanged)
	if !validCounter(lines, MaxObservedLines) {
		lines = nil
	}
	coverage := cloneString(o.CoverageDelta)
	if coverage != nil {
		normalized, ok := normalizeDecimal(*coverage)
		if ok {
			coverage = &normalized
		} else {
			coverage = nil
		}
	}
	return EvidenceSummary{
		ChangedPaths:       paths,
		FilesChanged:       files,
		LinesChanged:       lines,
		HasBinaryFiles:     cloneBool(o.HasBinaryFiles),
		CoverageDelta:      coverage,
		NewTestsFailOnBase: cloneBool(o.NewTestsFailOnBase),
	}
}

func validateChangedPaths(paths []string) (valid bool, duplicate bool) {
	seen := make(map[string]struct{}, len(paths))
	valid = true
	for _, item := range paths {
		if _, ok := seen[item]; ok {
			duplicate = true
		}
		seen[item] = struct{}{}
		if !validRepoPath(item) {
			valid = false
		}
	}
	return valid, duplicate
}

func validatePatterns(patterns []string) bool {
	if len(patterns) > MaxScopePatterns {
		return false
	}
	for _, pattern := range patterns {
		if _, err := compileGlob(pattern); err != nil {
			return false
		}
	}
	return true
}

func pathsRespectScope(paths []string, scope v1alpha1.ScopeSpec) bool {
	for _, item := range paths {
		allowed := false
		for _, pattern := range scope.Paths {
			if matchGlob(pattern, item) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
		for _, pattern := range scope.Forbidden {
			if matchGlob(pattern, item) {
				return false
			}
		}
	}
	return true
}

func validCounter(value *int64, maximum int64) bool {
	return value != nil && *value >= 0 && *value <= maximum
}

func validDuration(value *int64) bool {
	return value == nil || (*value >= 0 && *value <= MaxDurationMillis)
}

func validEvidenceDigest(value string) bool { return validSHA256Digest(value) }

func digestVerifyCommand(command v1alpha1.VerifyCommand) string {
	if !validVerifyCommand(command) {
		return "sha256:" + strings.Repeat("0", 64)
	}
	bytes, err := json.Marshal(command)
	if err != nil {
		return "sha256:" + strings.Repeat("0", 64)
	}
	sum := sha256.Sum256(bytes)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneInt32(value *int32) *int32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func commandObservationLess(left, right CommandObservation) bool {
	if left.Index != right.Index {
		return left.Index < right.Index
	}
	if left.ExitCode == nil && right.ExitCode != nil {
		return true
	}
	if left.ExitCode != nil && right.ExitCode == nil {
		return false
	}
	if left.ExitCode != nil && right.ExitCode != nil && *left.ExitCode != *right.ExitCode {
		return *left.ExitCode < *right.ExitCode
	}
	if left.EvidenceDigest != right.EvidenceDigest {
		return left.EvidenceDigest < right.EvidenceDigest
	}
	if left.DurationMillis == nil && right.DurationMillis != nil {
		return true
	}
	if left.DurationMillis != nil && right.DurationMillis == nil {
		return false
	}
	if left.DurationMillis != nil && right.DurationMillis != nil {
		return *left.DurationMillis < *right.DurationMillis
	}
	return false
}
