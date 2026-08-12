package gate

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/Astatide1337/agents-gateway/v3/internal/gatescoring"
)

// ReportSchemaVersion is the versioned, canonical report contract. It is
// independent of Kubernetes object versioning so stored evidence remains
// verifiable after the CRD evolves.
//
// v1beta1 is intentional: scoring and critic provenance are required fields
// in the signed payload. The old v1alpha1 shape is not silently interpreted as
// a scored report; callers must explicitly re-run/sign it under this schema.
const (
	ReportSchemaVersion       = "agents.astatide.com/verification/v1beta1"
	LegacyReportSchemaVersion = "agents.astatide.com/verification/v1alpha1"
)

const (
	MaxRunUIDBytes             = 128
	MaxReportMessageBytes      = 1024
	MaxReportBytes             = 8 << 20
	MaxSkillDigests            = 32
	MaxHelperImages            = 16
	MaxVerifierImageBytes      = 512
	MaxReportCheckNameBytes    = 128
	MaxReportCommandRecords    = MaxCommands
	MaxReportChangedPathBytes  = MaxChangedPaths * MaxPathBytes
	maxReportSignatureEncoding = 256
)

var (
	sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	baseSHAPattern      = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	imageDigestPattern  = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)
	imageRolePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// ImageEvidence binds a trusted helper role to the immutable image that ran
// it. Names are logical roles (for example clone, lockdown, or broker), never
// user-controlled container names.
type ImageEvidence struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// ReportContext identifies the immutable inputs whose evidence is being
// judged. Credentials and private signing material are intentionally absent.
type ReportContext struct {
	RunUID              string
	SpecDigest          string
	BaseSHA             string
	PatchDigest         string
	GateUID             string
	GateGeneration      int64
	RuntimeImageDigest  string
	VerifierImageDigest string
	HelperImageDigests  []ImageEvidence
	SkillDigests        []string
	// Scoring is optional only for source compatibility with existing
	// execution-only callers. A nil value is materialized as an explicit
	// execution-only 10000-point result by BuildReport.
	Scoring *ScoringContext
}

// EvidenceSummary is a bounded copy of the observations used by the Gate.
// Nil pointers preserve missing-evidence semantics in the report.
type EvidenceSummary struct {
	ChangedPaths       []string `json:"changedPaths"`
	FilesChanged       *int64   `json:"filesChanged,omitempty"`
	LinesChanged       *int64   `json:"linesChanged,omitempty"`
	HasBinaryFiles     *bool    `json:"hasBinaryFiles,omitempty"`
	CoverageDelta      *string  `json:"coverageDelta,omitempty"`
	NewTestsFailOnBase *bool    `json:"newTestsFailOnBase,omitempty"`
}

// ReportCommand binds a configured verification command to its observed
// result and content-addressed evidence. Missing evidence is represented
// explicitly with Observed=false.
type ReportCommand struct {
	Index          int    `json:"index"`
	ConfigDigest   string `json:"configDigest"`
	Observed       bool   `json:"observed"`
	ExitCode       *int32 `json:"exitCode,omitempty"`
	EvidenceDigest string `json:"evidenceDigest,omitempty"`
	DurationMillis *int64 `json:"durationMillis,omitempty"`
}

// RouteEvidence binds the resolved critic route revision and the effective
// provider family used by the external critic runner. The route's credential
// and model values never enter a Gate report.
type RouteEvidence struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	Provider   string `json:"provider"`
	Family     string `json:"family"`
}

// ScoringEvidence is the signed projection of gatescoring.Result. The full
// canonical result may live in a separate artifact; ResultDigest binds that
// artifact to these bounded integer values and reasons.
type ScoringEvidence struct {
	ResultDigest                string         `json:"resultDigest"`
	ExecutionWeightBasisPoints  int32          `json:"executionWeightBasisPoints"`
	CriticWeightBasisPoints     int32          `json:"criticWeightBasisPoints"`
	MinScoreBasisPoints         int32          `json:"minScoreBasisPoints"`
	ExecutionScoreBasisPoints   int32          `json:"executionScoreBasisPoints"`
	CriticScoreBasisPoints      int32          `json:"criticScoreBasisPoints"`
	WeightedScoreBasisPoints    int32          `json:"weightedScoreBasisPoints"`
	BlockingReasons             []string       `json:"blockingReasons"`
	CriticRoute                 *RouteEvidence `json:"criticRoute,omitempty"`
	CorroborationArtifactDigest string         `json:"corroborationArtifactDigest,omitempty"`
}

// ScoringContext is supplied by the independent verification controller. The
// scorer remains pure: this package validates the canonical result and binds
// its digest/projection into the signed report, but never runs a model.
type ScoringContext struct {
	Result                      gatescoring.Result
	CriticRoute                 *RouteEvidence
	CorroborationArtifactDigest string
}

// VerificationReport is the unsigned canonical payload. Signatures cover
// the canonical JSON bytes of this value, never a private key or seed.
type VerificationReport struct {
	SchemaVersion       string               `json:"schemaVersion"`
	RunUID              string               `json:"runUID"`
	SpecDigest          string               `json:"specDigest"`
	BaseSHA             string               `json:"baseSHA"`
	PatchDigest         string               `json:"patchDigest"`
	GateUID             string               `json:"gateUID"`
	GateGeneration      int64                `json:"gateGeneration"`
	RuntimeImageDigest  string               `json:"runtimeImageDigest"`
	VerifierImageDigest string               `json:"verifierImageDigest"`
	HelperImageDigests  []ImageEvidence      `json:"helperImageDigests"`
	SkillDigests        []string             `json:"skillDigests"`
	Evidence            EvidenceSummary      `json:"evidence"`
	Commands            []ReportCommand      `json:"commands"`
	Checks              []v1alpha1.GateCheck `json:"checks"`
	Scoring             ScoringEvidence      `json:"scoring"`
	Verdict             Verdict              `json:"verdict"`

	builtByGate bool
}

// SignedReport is an Ed25519 envelope. PublicKey is public verification
// material only; authenticating callers should compare it with a trusted key
// using VerifySignedReport.
type SignedReport struct {
	Report    VerificationReport `json:"report"`
	Algorithm string             `json:"algorithm"`
	PublicKey string             `json:"publicKey"`
	Signature string             `json:"signature"`
}

// BuildReport converts a decision into a canonical report. The decision must
// have originated from Evaluate; a hand-built accepted Decision is rejected.
func BuildReport(context ReportContext, decision Decision) (VerificationReport, error) {
	if !decision.evaluated {
		return VerificationReport{}, errors.New("gate decision was not produced by Evaluate")
	}
	if err := validateDecision(decision); err != nil {
		return VerificationReport{}, err
	}
	scoring, scoringVerdict, err := scoringForReport(context, decision)
	if err != nil {
		return VerificationReport{}, err
	}
	report := VerificationReport{
		SchemaVersion:       ReportSchemaVersion,
		RunUID:              context.RunUID,
		SpecDigest:          context.SpecDigest,
		BaseSHA:             context.BaseSHA,
		PatchDigest:         context.PatchDigest,
		GateUID:             context.GateUID,
		GateGeneration:      context.GateGeneration,
		RuntimeImageDigest:  context.RuntimeImageDigest,
		VerifierImageDigest: context.VerifierImageDigest,
		HelperImageDigests:  append([]ImageEvidence(nil), context.HelperImageDigests...),
		SkillDigests:        append([]string(nil), context.SkillDigests...),
		Evidence:            cloneEvidenceSummary(decision.evidence),
		Commands:            cloneReportCommands(decision.commandRecords),
		Checks:              append([]v1alpha1.GateCheck(nil), decision.Checks...),
		Scoring:             scoring,
		Verdict:             Verdict(scoringVerdict),
		builtByGate:         true,
	}
	return canonicalizeReport(report)
}

// CanonicalReportBytes returns deterministic compact JSON for a report. It
// sorts order-insensitive collections, validates all bounds, and recomputes
// the verdict from checks before encoding.
func CanonicalReportBytes(report VerificationReport) ([]byte, error) {
	normalized, err := canonicalizeReport(report)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal verification report: %w", err)
	}
	if len(encoded) > MaxReportBytes {
		return nil, errors.New("verification report exceeds bounded size")
	}
	return encoded, nil
}

// ReportDigest returns the content digest of the canonical unsigned report.
func ReportDigest(report VerificationReport) (string, error) {
	bytes, err := CanonicalReportBytes(report)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SignReport signs the canonical report with an Ed25519 private key. The
// returned object contains only the corresponding public key and signature.
func SignReport(report VerificationReport, privateKey ed25519.PrivateKey) (SignedReport, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return SignedReport{}, errors.New("invalid Ed25519 private key length")
	}
	if !report.builtByGate {
		return SignedReport{}, errors.New("report was not produced by BuildReport")
	}
	normalized, err := canonicalizeReport(report)
	if err != nil {
		return SignedReport{}, err
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return SignedReport{}, fmt.Errorf("marshal report for signing: %w", err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	signature := ed25519.Sign(privateKey, payload)
	return SignedReport{
		Report:    normalized,
		Algorithm: "Ed25519",
		PublicKey: base64.RawStdEncoding.EncodeToString(publicKey),
		Signature: base64.RawStdEncoding.EncodeToString(signature),
	}, nil
}

// VerifySignedReport verifies integrity and, when supplied, signer identity.
// Passing nil for trustedPublicKey verifies the embedded public key only; that
// detects tampering but does not authenticate who signed the report.
func VerifySignedReport(signed SignedReport, trustedPublicKey ed25519.PublicKey) error {
	if signed.Algorithm != "Ed25519" {
		return errors.New("unsupported report signature algorithm")
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(signed.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid report public key")
	}
	signature, err := base64.RawStdEncoding.DecodeString(signed.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || len(signed.Signature) > maxReportSignatureEncoding {
		return errors.New("invalid report signature")
	}
	if trustedPublicKey != nil {
		if len(trustedPublicKey) != ed25519.PublicKeySize || !bytes.Equal(publicKey, trustedPublicKey) {
			return errors.New("report signer is not trusted")
		}
	}
	payload, err := CanonicalReportBytes(signed.Report)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return errors.New("verification report signature is invalid")
	}
	return nil
}

// SignedReportBytes serializes a signed report in deterministic compact JSON.
func SignedReportBytes(signed SignedReport) ([]byte, error) {
	if err := VerifySignedReport(signed, nil); err != nil {
		return nil, err
	}
	normalized, err := canonicalizeReport(signed.Report)
	if err != nil {
		return nil, err
	}
	signed.Report = normalized
	encoded, err := json.Marshal(signed)
	if err != nil {
		return nil, fmt.Errorf("marshal signed report: %w", err)
	}
	if len(encoded) > MaxReportBytes+1024 {
		return nil, errors.New("signed verification report exceeds bounded size")
	}
	return encoded, nil
}

// ParseSignedReport parses a canonical envelope without silently accepting
// unknown fields. Call VerifySignedReport afterward to authenticate it.
func ParseSignedReport(encoded []byte) (SignedReport, error) {
	if len(encoded) == 0 || len(encoded) > MaxReportBytes+1024 {
		return SignedReport{}, errors.New("signed report size is out of bounds")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var signed SignedReport
	if err := decoder.Decode(&signed); err != nil {
		return SignedReport{}, fmt.Errorf("decode signed report: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SignedReport{}, errors.New("signed report has trailing JSON")
	}
	return signed, nil
}

func validateDecision(decision Decision) error {
	if len(decision.Checks) == 0 || len(decision.Checks) > MaxChecks {
		return errors.New("decision check count is out of bounds")
	}
	computed := Accepted
	seen := make(map[string]struct{}, len(decision.Checks))
	for _, check := range decision.Checks {
		if err := validateCheck(check); err != nil {
			return err
		}
		if _, exists := seen[check.Name]; exists {
			return errors.New("decision contains duplicate check names")
		}
		seen[check.Name] = struct{}{}
		if !check.Passed {
			computed = Rejected
		}
	}
	if decision.Verdict != computed {
		return errors.New("decision verdict does not match checks")
	}
	if len(decision.commandRecords) > MaxReportCommandRecords {
		return errors.New("decision command record count is out of bounds")
	}
	return nil
}

func scoringForReport(context ReportContext, decision Decision) (ScoringEvidence, gatescoring.Verdict, error) {
	var result gatescoring.Result
	var route *RouteEvidence
	corroborationDigest := ""
	if context.Scoring == nil {
		checks := make([]gatescoring.ExecutionCheck, 0, len(decision.Checks))
		for _, check := range decision.Checks {
			checks = append(checks, gatescoring.ExecutionCheck{Name: check.Name, Passed: check.Passed, Blocking: true})
		}
		executionScore := int32(0)
		if decision.Accepted() {
			executionScore = v1alpha1.GateScoreScale
		}
		var err error
		result, err = gatescoring.Evaluate(gatescoring.Input{
			Execution: gatescoring.ExecutionSignal{ScoreBasisPoints: executionScore, Checks: checks},
		})
		if err != nil {
			return ScoringEvidence{}, "", fmt.Errorf("materialize execution-only scoring: %w", err)
		}
	} else {
		result = context.Scoring.Result
		route = cloneRouteEvidence(context.Scoring.CriticRoute)
		corroborationDigest = context.Scoring.CorroborationArtifactDigest
	}

	if _, err := gatescoring.CanonicalResultBytes(result); err != nil {
		return ScoringEvidence{}, "", fmt.Errorf("canonicalize Gate scoring result: %w", err)
	}
	resultDigest, err := gatescoring.ResultDigest(result)
	if err != nil {
		return ScoringEvidence{}, "", fmt.Errorf("digest Gate scoring result: %w", err)
	}
	if err := validateScoringBinding(result, decision, route, corroborationDigest); err != nil {
		return ScoringEvidence{}, "", err
	}
	criticWeight := int32(0)
	if result.Signals.Critic != nil {
		criticWeight = result.Signals.Critic.WeightBasisPoints
	}
	projection := ScoringEvidence{
		ResultDigest:                resultDigest,
		ExecutionWeightBasisPoints:  result.Signals.ExecutionWeightBasisPoints,
		CriticWeightBasisPoints:     criticWeight,
		MinScoreBasisPoints:         result.Signals.MinScoreBasisPoints,
		ExecutionScoreBasisPoints:   result.ExecutionScoreBasisPoints,
		CriticScoreBasisPoints:      result.CriticScoreBasisPoints,
		WeightedScoreBasisPoints:    result.WeightedScoreBasisPoints,
		BlockingReasons:             append([]string(nil), result.BlockingReasons...),
		CriticRoute:                 route,
		CorroborationArtifactDigest: corroborationDigest,
	}
	return projection, result.Verdict, nil
}

func validateScoringBinding(result gatescoring.Result, decision Decision, route *RouteEvidence, corroborationDigest string) error {
	if len(result.ExecutionChecks) != len(decision.Checks) {
		return errors.New("scoring result does not cover every Gate check")
	}
	expected := append([]v1alpha1.GateCheck(nil), decision.Checks...)
	sort.Slice(expected, func(left, right int) bool { return expected[left].Name < expected[right].Name })
	for index, check := range result.ExecutionChecks {
		if check.Name != expected[index].Name || check.Passed != expected[index].Passed || !check.Blocking {
			return errors.New("scoring result is not bound to the deterministic Gate checks")
		}
	}
	if result.Signals.Critic == nil {
		if route != nil || corroborationDigest != "" || result.CriticResult != nil || result.CriticScoreBasisPoints != v1alpha1.GateScoreScale || result.Signals.ExecutionWeightBasisPoints != v1alpha1.GateScoreScale || result.Signals.MinScoreBasisPoints != v1alpha1.GateScoreScale {
			return errors.New("execution-only scoring contains critic provenance")
		}
		return nil
	}
	if route == nil || !validRouteEvidence(*route) {
		return errors.New("critic scoring is missing a valid route identity")
	}
	if route.Name != result.Signals.Critic.ModelRouteRef {
		return errors.New("critic route identity does not match the resolved scoring route")
	}
	if !validSHA256Digest(corroborationDigest) {
		return errors.New("critic scoring is missing a corroboration artifact digest")
	}
	if result.CriticResult == nil {
		return errors.New("critic scoring is missing its corroboration result")
	}
	return nil
}

func validRouteEvidence(route RouteEvidence) bool {
	return validRunUID(route.Name) && validRunUID(route.UID) && route.Generation > 0 && validRunUID(route.Provider) && validRunUID(route.Family)
}

func cloneRouteEvidence(input *RouteEvidence) *RouteEvidence {
	if input == nil {
		return nil
	}
	output := *input
	return &output
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func canonicalizeReport(input VerificationReport) (VerificationReport, error) {
	if input.SchemaVersion != ReportSchemaVersion {
		return VerificationReport{}, errors.New("unsupported verification report schema")
	}
	if !validRunUID(input.RunUID) {
		return VerificationReport{}, errors.New("invalid report run UID")
	}
	if !validSHA256Digest(input.SpecDigest) || !validSHA256Digest(input.PatchDigest) {
		return VerificationReport{}, errors.New("invalid report content digest")
	}
	if !baseSHAPattern.MatchString(input.BaseSHA) || len(input.BaseSHA) > 64 {
		return VerificationReport{}, errors.New("invalid report base SHA")
	}
	if !validRunUID(input.GateUID) || input.GateGeneration <= 0 {
		return VerificationReport{}, errors.New("invalid report Gate identity")
	}
	for name, digest := range map[string]string{"runtime": input.RuntimeImageDigest, "verifier": input.VerifierImageDigest} {
		if len(digest) == 0 || len(digest) > MaxVerifierImageBytes || !utf8.ValidString(digest) || !imageDigestPattern.MatchString(digest) || strings.ContainsAny(digest, "\x00\r\n\t ") {
			return VerificationReport{}, fmt.Errorf("invalid %s image digest", name)
		}
	}

	output := input
	output.HelperImageDigests = append([]ImageEvidence(nil), input.HelperImageDigests...)
	if len(output.HelperImageDigests) == 0 || len(output.HelperImageDigests) > MaxHelperImages {
		return VerificationReport{}, errors.New("helper image evidence count is out of bounds")
	}
	sort.Slice(output.HelperImageDigests, func(i, j int) bool { return output.HelperImageDigests[i].Name < output.HelperImageDigests[j].Name })
	for index, image := range output.HelperImageDigests {
		if !imageRolePattern.MatchString(image.Name) || len(image.Digest) > MaxVerifierImageBytes || !imageDigestPattern.MatchString(image.Digest) || strings.ContainsAny(image.Digest, "\x00\r\n\t ") {
			return VerificationReport{}, errors.New("helper image evidence is invalid")
		}
		if index > 0 && output.HelperImageDigests[index-1].Name == image.Name {
			return VerificationReport{}, errors.New("helper image evidence contains duplicate roles")
		}
	}
	output.SkillDigests = append([]string(nil), input.SkillDigests...)
	if len(output.SkillDigests) > MaxSkillDigests {
		return VerificationReport{}, errors.New("skill digest count is out of bounds")
	}
	for _, digest := range output.SkillDigests {
		if !validSHA256Digest(digest) {
			return VerificationReport{}, errors.New("skill digest list is invalid")
		}
	}
	sort.Strings(output.SkillDigests)
	for index, digest := range output.SkillDigests {
		if !validSHA256Digest(digest) || (index > 0 && output.SkillDigests[index-1] == digest) {
			return VerificationReport{}, errors.New("skill digest list is invalid")
		}
	}

	var err error
	output.Evidence, err = canonicalizeEvidence(input.Evidence)
	if err != nil {
		return VerificationReport{}, err
	}
	output.Commands, err = canonicalizeCommands(input.Commands)
	if err != nil {
		return VerificationReport{}, err
	}
	output.Checks, err = canonicalizeChecks(input.Checks)
	if err != nil {
		return VerificationReport{}, err
	}
	output.Scoring, err = canonicalizeScoring(input.Scoring, output.Checks)
	if err != nil {
		return VerificationReport{}, err
	}
	computed := Verdict(scoringVerdictFromProjection(output.Scoring))
	if len(output.Checks) == 0 || input.Verdict != computed {
		return VerificationReport{}, errors.New("report verdict does not match checks")
	}
	output.Verdict = computed
	return output, nil
}

func canonicalizeScoring(input ScoringEvidence, checks []v1alpha1.GateCheck) (ScoringEvidence, error) {
	output := input
	if !validSHA256Digest(output.ResultDigest) {
		return ScoringEvidence{}, errors.New("scoring result digest is invalid")
	}
	if output.ExecutionWeightBasisPoints < 0 || output.ExecutionWeightBasisPoints > v1alpha1.GateScoreScale || output.CriticWeightBasisPoints < 0 || output.CriticWeightBasisPoints > v1alpha1.GateScoreScale || output.MinScoreBasisPoints < 0 || output.MinScoreBasisPoints > v1alpha1.GateScoreScale || output.ExecutionScoreBasisPoints < 0 || output.ExecutionScoreBasisPoints > v1alpha1.GateScoreScale || output.CriticScoreBasisPoints < 0 || output.CriticScoreBasisPoints > v1alpha1.GateScoreScale || output.WeightedScoreBasisPoints < 0 || output.WeightedScoreBasisPoints > v1alpha1.GateScoreScale {
		return ScoringEvidence{}, errors.New("scoring integer value is outside 0..10000")
	}
	if int64(output.ExecutionWeightBasisPoints)+int64(output.CriticWeightBasisPoints) != int64(v1alpha1.GateScoreScale) {
		return ScoringEvidence{}, errors.New("scoring weights do not sum to 10000")
	}
	weighted := int32((int64(output.ExecutionScoreBasisPoints)*int64(output.ExecutionWeightBasisPoints) + int64(output.CriticScoreBasisPoints)*int64(output.CriticWeightBasisPoints)) / int64(v1alpha1.GateScoreScale))
	if output.WeightedScoreBasisPoints != weighted {
		return ScoringEvidence{}, errors.New("scoring weighted value is inconsistent")
	}
	output.BlockingReasons = append([]string(nil), input.BlockingReasons...)
	if len(output.BlockingReasons) > gatescoring.MaxBlockingReasons {
		return ScoringEvidence{}, errors.New("scoring blocking reason count is out of bounds")
	}
	for _, reason := range output.BlockingReasons {
		if len(reason) == 0 || len(reason) > gatescoring.MaxReasonBytes || !utf8.ValidString(reason) || strings.ContainsAny(reason, "\x00\r\n\t ") {
			return ScoringEvidence{}, errors.New("scoring blocking reason is invalid")
		}
	}
	sort.Strings(output.BlockingReasons)
	for index := 1; index < len(output.BlockingReasons); index++ {
		if output.BlockingReasons[index-1] == output.BlockingReasons[index] {
			return ScoringEvidence{}, errors.New("scoring blocking reasons contain duplicates")
		}
	}
	for _, check := range checks {
		if !check.Passed && !containsString(output.BlockingReasons, "execution."+check.Name) {
			return ScoringEvidence{}, errors.New("scoring does not preserve a failed Gate check as a blocking reason")
		}
	}
	if output.CriticWeightBasisPoints == 0 {
		if output.ExecutionWeightBasisPoints != v1alpha1.GateScoreScale || output.MinScoreBasisPoints != v1alpha1.GateScoreScale || output.CriticScoreBasisPoints != v1alpha1.GateScoreScale || output.WeightedScoreBasisPoints != output.ExecutionScoreBasisPoints || output.CriticRoute != nil || output.CorroborationArtifactDigest != "" {
			return ScoringEvidence{}, errors.New("execution-only scoring is not the explicit 10000-point form")
		}
	} else {
		if output.CriticWeightBasisPoints < 1 || output.ExecutionWeightBasisPoints < 1 || output.CriticRoute == nil || !validRouteEvidence(*output.CriticRoute) || !validSHA256Digest(output.CorroborationArtifactDigest) {
			return ScoringEvidence{}, errors.New("critic scoring is missing route or corroboration provenance")
		}
	}
	return output, nil
}

func scoringVerdictFromProjection(scoring ScoringEvidence) gatescoring.Verdict {
	if len(scoring.BlockingReasons) != 0 || scoring.WeightedScoreBasisPoints < scoring.MinScoreBasisPoints {
		return gatescoring.Rejected
	}
	return gatescoring.Accepted
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func canonicalizeEvidence(input EvidenceSummary) (EvidenceSummary, error) {
	output := input
	output.ChangedPaths = append([]string(nil), input.ChangedPaths...)
	if len(output.ChangedPaths) > MaxChangedPaths {
		return EvidenceSummary{}, errors.New("report changed path count is out of bounds")
	}
	totalPathBytes := 0
	for _, path := range output.ChangedPaths {
		if !validRepoPath(path) {
			return EvidenceSummary{}, errors.New("report changed paths are invalid")
		}
		totalPathBytes += len(path)
		if totalPathBytes > MaxReportChangedPathBytes {
			return EvidenceSummary{}, errors.New("report changed path bytes are out of bounds")
		}
	}
	sort.Strings(output.ChangedPaths)
	for index, path := range output.ChangedPaths {
		if !validRepoPath(path) || (index > 0 && output.ChangedPaths[index-1] == path) {
			return EvidenceSummary{}, errors.New("report changed paths are invalid")
		}
	}
	if output.FilesChanged != nil && !validCounter(output.FilesChanged, MaxObservedFiles) {
		return EvidenceSummary{}, errors.New("report file count is invalid")
	}
	if output.LinesChanged != nil && !validCounter(output.LinesChanged, MaxObservedLines) {
		return EvidenceSummary{}, errors.New("report line count is invalid")
	}
	output.CoverageDelta = cloneString(input.CoverageDelta)
	if output.CoverageDelta != nil {
		normalized, ok := normalizeDecimal(*output.CoverageDelta)
		if !ok {
			return EvidenceSummary{}, errors.New("report coverage delta is invalid")
		}
		output.CoverageDelta = &normalized
	}
	output.FilesChanged = cloneInt64(input.FilesChanged)
	output.LinesChanged = cloneInt64(input.LinesChanged)
	output.HasBinaryFiles = cloneBool(input.HasBinaryFiles)
	output.NewTestsFailOnBase = cloneBool(input.NewTestsFailOnBase)
	return output, nil
}

func canonicalizeCommands(input []ReportCommand) ([]ReportCommand, error) {
	output := cloneReportCommands(input)
	if len(output) > MaxReportCommandRecords {
		return nil, errors.New("report command count is out of bounds")
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Index < output[j].Index })
	for index := range output {
		item := &output[index]
		if item.Index < 0 || item.Index >= MaxCommands || !validSHA256Digest(item.ConfigDigest) {
			return nil, errors.New("report command record is invalid")
		}
		if index > 0 && output[index-1].Index == item.Index {
			return nil, errors.New("report command records contain duplicate indexes")
		}
		if item.Observed {
			if item.ExitCode == nil || !validEvidenceDigest(item.EvidenceDigest) || !validDuration(item.DurationMillis) {
				return nil, errors.New("observed report command is missing evidence")
			}
		} else if item.ExitCode != nil || item.EvidenceDigest != "" || item.DurationMillis != nil {
			return nil, errors.New("missing report command contains evidence")
		}
	}
	return output, nil
}

func canonicalizeChecks(input []v1alpha1.GateCheck) ([]v1alpha1.GateCheck, error) {
	output := append([]v1alpha1.GateCheck(nil), input...)
	if len(output) > MaxChecks {
		return nil, errors.New("report check count is out of bounds")
	}
	for _, check := range output {
		if err := validateCheck(check); err != nil {
			return nil, err
		}
	}
	sort.Slice(output, func(i, j int) bool { return output[i].Name < output[j].Name })
	for index := range output {
		if index > 0 && output[index-1].Name == output[index].Name {
			return nil, errors.New("report checks contain duplicate names")
		}
	}
	return output, nil
}

func validateCheck(check v1alpha1.GateCheck) error {
	if len(check.Name) == 0 || len(check.Name) > MaxReportCheckNameBytes || !utf8.ValidString(check.Name) || strings.IndexByte(check.Name, 0) >= 0 {
		return errors.New("report check name is invalid")
	}
	if len(check.Message) > MaxReportMessageBytes || !utf8.ValidString(check.Message) || strings.IndexByte(check.Message, 0) >= 0 {
		return errors.New("report check message is invalid")
	}
	return nil
}

func validRunUID(value string) bool {
	return len(value) > 0 && len(value) <= MaxRunUIDBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func validSHA256Digest(value string) bool {
	return len(value) == len("sha256:")+64 && sha256DigestPattern.MatchString(value)
}

func cloneEvidenceSummary(input EvidenceSummary) EvidenceSummary {
	output := input
	output.ChangedPaths = append([]string(nil), input.ChangedPaths...)
	output.FilesChanged = cloneInt64(input.FilesChanged)
	output.LinesChanged = cloneInt64(input.LinesChanged)
	output.HasBinaryFiles = cloneBool(input.HasBinaryFiles)
	output.CoverageDelta = cloneString(input.CoverageDelta)
	output.NewTestsFailOnBase = cloneBool(input.NewTestsFailOnBase)
	return output
}

func cloneReportCommands(input []ReportCommand) []ReportCommand {
	output := append([]ReportCommand(nil), input...)
	for index := range output {
		output[index].ExitCode = cloneInt32(output[index].ExitCode)
		output[index].DurationMillis = cloneInt64(output[index].DurationMillis)
	}
	return output
}
