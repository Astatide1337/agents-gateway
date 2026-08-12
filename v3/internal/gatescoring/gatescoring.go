// Package gatescoring combines bounded deterministic execution evidence with
// an ADR-020-routed critic result.
//
// This package is deliberately pure. It does not call a model, execute a
// verifier, or decide whether an evidence producer is trustworthy. Those
// responsibilities belong to the verify/controller boundary. The package
// validates the shape and internal consistency of the supplied observations,
// performs fixed-point arithmetic, and emits a canonical result suitable for
// content-addressed storage.
package gatescoring

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// SchemaVersion identifies this canonical result contract independently of
	// the Kubernetes API version.
	SchemaVersion = "agents.astatide.com/gate-scoring/v1alpha1"

	// MaxExecutionChecks bounds the deterministic evidence copied into one
	// scoring result. It is intentionally no larger than the existing Gate
	// report check budget.
	MaxExecutionChecks = 64
	// MaxReasonBytes bounds one derived blocking reason.
	MaxReasonBytes = 256
	// MaxBlockingReasons bounds the result projection before it is encoded.
	MaxBlockingReasons = MaxExecutionChecks + findingcorroboration.MaxFindings
	// MaxResultBytes prevents a malformed critic result from becoming an
	// unbounded persistence or hashing operation.
	MaxResultBytes = 1 << 20
)

var (
	ErrInvalidInput  = errors.New("invalid Gate scoring input")
	ErrNonCanonical  = errors.New("Gate scoring result is not canonical")
	checkNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
)

// Verdict is the terminal scoring decision. A score never overrides a
// blocking deterministic check or a corroborated critic finding.
type Verdict string

const (
	Accepted Verdict = "Accepted"
	Rejected Verdict = "Rejected"
)

// ExecutionCheck is one verifier-produced deterministic observation. A
// failed check with Blocking=true is an unconditional rejection, even when
// the weighted score is above the configured minimum. Non-blocking checks may
// lower the execution signal without becoming an independent veto.
type ExecutionCheck struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Blocking bool   `json:"blocking"`
}

// ExecutionSignal is the bounded deterministic signal supplied by the
// independent verifier. The score is an integer in [0, 10000] basis points;
// it is not a floating-point model confidence value.
type ExecutionSignal struct {
	ScoreBasisPoints int32            `json:"scoreBasisPoints"`
	Checks           []ExecutionCheck `json:"checks"`
}

// CriticSignal is the only critic input accepted by the scoring core. The
// result must have already passed findingcorroboration's strict routing
// contract. In particular, model-only and otherwise uncorroborated findings
// remain advisory and cannot create a blocking reason.
type CriticSignal struct {
	Result findingcorroboration.CorroborationResult
}

// Input is the pure scoring boundary. A nil Signals pointer selects the
// explicit execution-only default. A critic signal is required exactly when
// the declared Gate signal block enables a critic weight.
type Input struct {
	Signals   *v1alpha1.GateSignalsSpec
	Execution ExecutionSignal
	Critic    *CriticSignal
}

// Result is the canonical, derived scoring output. Signals are materialized
// even when the caller used the execution-only default, so the digest binds
// the exact effective policy rather than the spelling of an omitted field.
type Result struct {
	SchemaVersion string                   `json:"schemaVersion"`
	Signals       v1alpha1.GateSignalsSpec `json:"signals"`

	ExecutionScoreBasisPoints int32 `json:"executionScoreBasisPoints"`
	CriticScoreBasisPoints    int32 `json:"criticScoreBasisPoints"`
	WeightedScoreBasisPoints  int32 `json:"weightedScoreBasisPoints"`

	ExecutionChecks []ExecutionCheck                          `json:"executionChecks"`
	CriticResult    *findingcorroboration.CorroborationResult `json:"criticResult,omitempty"`
	BlockingReasons []string                                  `json:"blockingReasons"`
	Verdict         Verdict                                   `json:"verdict"`
}

// Evaluate validates, normalizes, and scores one bounded input.
func Evaluate(input Input) (Result, error) {
	signals, err := normalizeSignals(input.Signals)
	if err != nil {
		return Result{}, err
	}

	execution, executionReasons, err := normalizeExecution(input.Execution)
	if err != nil {
		return Result{}, err
	}

	var criticResult *findingcorroboration.CorroborationResult
	criticScore := int32(v1alpha1.GateScoreScale)
	criticReasons := []string(nil)
	if signals.Critic != nil {
		if input.Critic == nil {
			return Result{}, invalid("critic", "enabled critic signal has no corroboration result")
		}
		normalized, err := normalizeCriticResult(input.Critic.Result, signals.Critic.MaxFindings)
		if err != nil {
			return Result{}, err
		}
		criticResult = &normalized
		criticScore = criticScoreFor(normalized)
		criticReasons = criticBlockingReasons(normalized)
	} else if input.Critic != nil {
		return Result{}, invalid("critic", "critic evidence supplied while the critic signal is disabled")
	}

	weighted, err := weightedScore(execution.ScoreBasisPoints, signals, criticScore)
	if err != nil {
		return Result{}, err
	}
	reasons := append(executionReasons, criticReasons...)
	if weighted < signals.MinScoreBasisPoints {
		reasons = append(reasons, "score.minimum")
	}
	reasons = normalizeReasons(reasons)
	verdict := Accepted
	if len(reasons) > 0 {
		verdict = Rejected
	}

	result := Result{
		SchemaVersion:             SchemaVersion,
		Signals:                   signals,
		ExecutionScoreBasisPoints: execution.ScoreBasisPoints,
		CriticScoreBasisPoints:    criticScore,
		WeightedScoreBasisPoints:  weighted,
		ExecutionChecks:           execution.Checks,
		CriticResult:              criticResult,
		BlockingReasons:           reasons,
		Verdict:                   verdict,
	}
	return canonicalizeResult(result)
}

// CanonicalResultBytes returns deterministic compact JSON for a result. It
// recomputes all derived scores, routes, reasons, and verdicts before
// encoding, so caller-controlled derived fields cannot become authority.
func CanonicalResultBytes(result Result) ([]byte, error) {
	normalized, err := canonicalizeResult(result)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal Gate scoring result: %w", err)
	}
	if len(encoded) > MaxResultBytes {
		return nil, invalid("result", "exceeds %d bytes", MaxResultBytes)
	}
	return encoded, nil
}

// ResultDigest returns the SHA-256 digest of the canonical result bytes.
func ResultDigest(result Result) (string, error) {
	encoded, err := CanonicalResultBytes(result)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// ParseCanonicalResult verifies a strict, canonical result and its expected
// digest. This is the persistence boundary used by future controller/verify
// integration; it does not authenticate who produced the result.
func ParseCanonicalResult(encoded []byte, digest string) (Result, error) {
	var zero Result
	if len(encoded) == 0 || len(encoded) > MaxResultBytes || !canonical.ValidDigest(digest) || strictjson.ValidateObject(encoded) != nil {
		return zero, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return zero, invalid("result", "decode: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return zero, ErrNonCanonical
	}
	canonicalBytes, err := CanonicalResultBytes(result)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBytes, encoded) {
		return zero, ErrNonCanonical
	}
	computed, err := ResultDigest(result)
	if err != nil || computed != digest {
		return zero, ErrInvalidInput
	}
	return result, nil
}

func normalizeSignals(input *v1alpha1.GateSignalsSpec) (v1alpha1.GateSignalsSpec, error) {
	if input == nil {
		return v1alpha1.GateSignalsSpec{
			ExecutionWeightBasisPoints: v1alpha1.GateScoreScale,
			MinScoreBasisPoints:        v1alpha1.GateScoreScale,
		}, nil
	}
	output := *input.DeepCopy()
	if output.ExecutionWeightBasisPoints < 0 || output.ExecutionWeightBasisPoints > v1alpha1.GateScoreScale {
		return v1alpha1.GateSignalsSpec{}, invalid("signals.executionWeightBasisPoints", "is outside 0..%d", v1alpha1.GateScoreScale)
	}
	if output.MinScoreBasisPoints < 0 || output.MinScoreBasisPoints > v1alpha1.GateScoreScale {
		return v1alpha1.GateSignalsSpec{}, invalid("signals.minScoreBasisPoints", "is outside 0..%d", v1alpha1.GateScoreScale)
	}
	if output.Critic == nil {
		if output.ExecutionWeightBasisPoints != v1alpha1.GateScoreScale || output.MinScoreBasisPoints != v1alpha1.GateScoreScale {
			return v1alpha1.GateSignalsSpec{}, invalid("signals", "execution-only signals must use execution=10000 and minimum=10000")
		}
		return output, nil
	}
	critic := output.Critic
	if critic.WeightBasisPoints < 1 || critic.WeightBasisPoints > v1alpha1.GateScoreScale {
		return v1alpha1.GateSignalsSpec{}, invalid("signals.critic.weightBasisPoints", "is outside 1..%d", v1alpha1.GateScoreScale)
	}
	if output.ExecutionWeightBasisPoints < 1 || int64(output.ExecutionWeightBasisPoints)+int64(critic.WeightBasisPoints) != int64(v1alpha1.GateScoreScale) {
		return v1alpha1.GateSignalsSpec{}, invalid("signals", "execution and critic weights must sum exactly to %d", v1alpha1.GateScoreScale)
	}
	if !validRouteRef(critic.ModelRouteRef) {
		return v1alpha1.GateSignalsSpec{}, invalid("signals.critic.modelRouteRef", "is invalid or missing")
	}
	if critic.MaxFindings < 1 || critic.MaxFindings > v1alpha1.MaxCriticFindings {
		return v1alpha1.GateSignalsSpec{}, invalid("signals.critic.maxFindings", "is outside 1..%d", v1alpha1.MaxCriticFindings)
	}
	return output, nil
}

func normalizeExecution(input ExecutionSignal) (ExecutionSignal, []string, error) {
	if input.ScoreBasisPoints < 0 || input.ScoreBasisPoints > v1alpha1.GateScoreScale {
		return ExecutionSignal{}, nil, invalid("execution.scoreBasisPoints", "is outside 0..%d", v1alpha1.GateScoreScale)
	}
	if input.Checks == nil || len(input.Checks) == 0 || len(input.Checks) > MaxExecutionChecks {
		return ExecutionSignal{}, nil, invalid("execution.checks", "must contain 1..%d checks", MaxExecutionChecks)
	}
	output := ExecutionSignal{ScoreBasisPoints: input.ScoreBasisPoints, Checks: append([]ExecutionCheck(nil), input.Checks...)}
	sort.Slice(output.Checks, func(i, j int) bool { return output.Checks[i].Name < output.Checks[j].Name })
	reasons := make([]string, 0, len(output.Checks))
	for index, check := range output.Checks {
		if !validCheckName(check.Name) {
			return ExecutionSignal{}, nil, invalid(fmt.Sprintf("execution.checks[%d].name", index), "is invalid")
		}
		if index > 0 && output.Checks[index-1].Name == check.Name {
			return ExecutionSignal{}, nil, invalid("execution.checks", "contains duplicate check names")
		}
		if check.Blocking && !check.Passed {
			reasons = append(reasons, "execution."+check.Name)
		}
	}
	return output, reasons, nil
}

func normalizeCriticResult(input findingcorroboration.CorroborationResult, maxFindings int32) (findingcorroboration.CorroborationResult, error) {
	if int64(len(input.Findings)) > int64(maxFindings) {
		return findingcorroboration.CorroborationResult{}, invalid("critic.result.findings", "exceeds configured maxFindings")
	}
	_, err := findingcorroboration.CanonicalResultBytes(input)
	if err != nil {
		return findingcorroboration.CorroborationResult{}, fmt.Errorf("%w: critic corroboration: %v", ErrInvalidInput, err)
	}
	// CanonicalResultBytes is the authoritative validator, but its current
	// projection intentionally does not round-trip derived Counts. Preserve
	// the validated counts from the source and normalize the order locally so
	// the Gate result remains digest-stable without changing the corroboration
	// package's contract.
	output := input
	output.Findings = make([]findingcorroboration.FindingDecision, len(input.Findings))
	for index, finding := range input.Findings {
		output.Findings[index] = finding
		output.Findings[index].Evidence = append([]findingcorroboration.EvidenceDecision(nil), finding.Evidence...)
	}
	sort.Slice(output.Findings, func(left, right int) bool {
		return output.Findings[left].Finding.ID < output.Findings[right].Finding.ID
	})
	for index := range output.Findings {
		sort.Slice(output.Findings[index].Evidence, func(left, right int) bool {
			return output.Findings[index].Evidence[left].ID < output.Findings[index].Evidence[right].ID
		})
	}
	return output, nil
}

// criticScoreFor intentionally ignores advisory findings. A model-only or
// uncorroborated finding is visible in the result, but it cannot lower the
// score enough to reject a candidate. Any corroborated blocking finding is an
// unconditional critic veto and therefore maps to zero.
func criticScoreFor(result findingcorroboration.CorroborationResult) int32 {
	if result.Counts.BlockingFindings > 0 {
		return 0
	}
	return v1alpha1.GateScoreScale
}

func criticBlockingReasons(result findingcorroboration.CorroborationResult) []string {
	reasons := make([]string, 0, result.Counts.BlockingFindings)
	for _, finding := range result.Findings {
		if finding.Route == findingcorroboration.RouteBlocking {
			reasons = append(reasons, "critic."+finding.Finding.ID)
		}
	}
	return reasons
}

func weightedScore(execution int32, signals v1alpha1.GateSignalsSpec, critic int32) (int32, error) {
	if execution < 0 || execution > v1alpha1.GateScoreScale || critic < 0 || critic > v1alpha1.GateScoreScale {
		return 0, invalid("score", "signal score is outside 0..%d", v1alpha1.GateScoreScale)
	}
	criticWeight := int32(0)
	if signals.Critic != nil {
		criticWeight = signals.Critic.WeightBasisPoints
	}
	if int64(signals.ExecutionWeightBasisPoints)+int64(criticWeight) != int64(v1alpha1.GateScoreScale) {
		return 0, invalid("signals", "weights do not sum exactly to %d", v1alpha1.GateScoreScale)
	}
	numerator := int64(execution)*int64(signals.ExecutionWeightBasisPoints) + int64(critic)*int64(criticWeight)
	return int32(numerator / int64(v1alpha1.GateScoreScale)), nil
}

func canonicalizeResult(input Result) (Result, error) {
	if input.SchemaVersion != SchemaVersion {
		return Result{}, invalid("result.schemaVersion", "must equal %q", SchemaVersion)
	}
	signals, err := normalizeSignals(&input.Signals)
	if err != nil {
		return Result{}, err
	}
	execution, executionReasons, err := normalizeExecution(ExecutionSignal{ScoreBasisPoints: input.ExecutionScoreBasisPoints, Checks: input.ExecutionChecks})
	if err != nil {
		return Result{}, err
	}
	var criticResult *findingcorroboration.CorroborationResult
	criticScore := int32(v1alpha1.GateScoreScale)
	criticReasons := []string(nil)
	if signals.Critic != nil {
		if input.CriticResult == nil {
			return Result{}, invalid("result.criticResult", "is required when the critic signal is enabled")
		}
		normalized, normalizeErr := normalizeCriticResult(*input.CriticResult, signals.Critic.MaxFindings)
		if normalizeErr != nil {
			return Result{}, normalizeErr
		}
		criticResult = &normalized
		criticScore = criticScoreFor(normalized)
		criticReasons = criticBlockingReasons(normalized)
	} else if input.CriticResult != nil {
		return Result{}, invalid("result.criticResult", "must be omitted when the critic signal is disabled")
	}
	weighted, err := weightedScore(execution.ScoreBasisPoints, signals, criticScore)
	if err != nil {
		return Result{}, err
	}
	reasons := normalizeReasons(append(executionReasons, criticReasons...))
	if weighted < signals.MinScoreBasisPoints {
		reasons = normalizeReasons(append(reasons, "score.minimum"))
	}
	verdict := Accepted
	if len(reasons) != 0 {
		verdict = Rejected
	}
	if input.CriticScoreBasisPoints != criticScore || input.WeightedScoreBasisPoints != weighted || input.BlockingReasons == nil || !equalStrings(input.BlockingReasons, reasons) || input.Verdict != verdict {
		return Result{}, ErrInvalidInput
	}
	return Result{
		SchemaVersion:             SchemaVersion,
		Signals:                   signals,
		ExecutionScoreBasisPoints: execution.ScoreBasisPoints,
		CriticScoreBasisPoints:    criticScore,
		WeightedScoreBasisPoints:  weighted,
		ExecutionChecks:           execution.Checks,
		CriticResult:              criticResult,
		BlockingReasons:           reasons,
		Verdict:                   verdict,
	}, nil
}

func normalizeReasons(input []string) []string {
	output := append(make([]string, 0, len(input)), input...)
	sort.Strings(output)
	unique := output[:0]
	for _, reason := range output {
		if len(reason) == 0 || len(reason) > MaxReasonBytes || !utf8.ValidString(reason) || strings.ContainsAny(reason, "\x00\r\n\t ") {
			continue
		}
		if len(unique) == 0 || unique[len(unique)-1] != reason {
			unique = append(unique, reason)
		}
	}
	if len(unique) > MaxBlockingReasons {
		return unique[:MaxBlockingReasons]
	}
	return unique
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validCheckName(value string) bool {
	return len(value) > 0 && len(value) <= 128 && utf8.ValidString(value) && checkNamePattern.MatchString(value)
}

func validRouteRef(value string) bool {
	if value == "" || len(value) > 253 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for index, part := range strings.Split(value, ".") {
		if index > 0 && part == "" {
			return false
		}
		for characterIndex, character := range part {
			if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || (character == '-' && characterIndex > 0 && characterIndex < len(part)-1) {
				continue
			}
			return false
		}
	}
	return true
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidInput, field, fmt.Sprintf(format, args...))
}
