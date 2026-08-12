// Package findingcorroboration routes critic findings according to ADR-020.
//
// The package is deliberately a pure boundary. It does not execute checks,
// call a model, read a repository, or fetch an artifact. A caller supplies the
// typed observations produced by an independent verifier. This package then
// validates their shape, binding, bounds, and content digests before deciding
// whether a finding is eligible for blocking.
//
// A content digest proves that the supplied evidence value has not changed
// since it was sealed. It is not an authentication mechanism by itself. The
// integration which obtains the value must authenticate the producer and make
// sure the digest refers to an immutable artifact. That integration boundary
// is intentionally outside this package.
package findingcorroboration

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
	"unicode"
	"unicode/utf8"
)

const (
	// SchemaVersion identifies the canonical wire contract.
	SchemaVersion = "agents.astatide.com/finding-corroboration/v1alpha1"

	// Bounds are intentionally conservative. They prevent a critic or an
	// artifact reader from turning this small decision boundary into an
	// unbounded memory sink.
	MaxInputBytes               = 1 << 20
	MaxResultBytes              = 512 << 10
	MaxFindings                 = 256
	MaxEvidence                 = 1024
	MaxFindingIDBytes           = 128
	MaxEvidenceIDBytes          = 128
	MaxRuleIDBytes              = 128
	MaxValidatorBytes           = 128
	MaxPathBytes                = 1024
	MaxMessageBytes             = 4096
	MaxCommandBytes             = 2048
	MaxToolBytes                = 128
	MaxRelationBytes            = 256
	MaxClaimBytes               = 4096
	MaxEvidenceDescriptionBytes = 2048
	MaxEvidenceMatches          = 1 << 20
	MaxJSONDepth                = 64
)

var (
	// ErrInvalidInput is returned for every fail-closed validation failure.
	ErrInvalidInput = errors.New("invalid finding corroboration input")
	// ErrNonCanonical is returned when valid JSON is not the unique canonical
	// representation of the supplied input.
	ErrNonCanonical = errors.New("finding corroboration JSON is not canonical")

	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$`)
)

// EvidenceClass identifies the source shape of a critic observation.
type EvidenceClass string

const (
	EvidenceClassReproduction   EvidenceClass = "reproduction"
	EvidenceClassStatic         EvidenceClass = "static"
	EvidenceClassSymbolGraph    EvidenceClass = "symbol-graph"
	EvidenceClassPolicy         EvidenceClass = "policy"
	EvidenceClassModelAssertion EvidenceClass = "model-assertion"

	// Short aliases make the class constants convenient without weakening the
	// wire values. They intentionally use Class* names so they do not collide
	// with the typed payload names below.
	ClassReproduction   = EvidenceClassReproduction
	ClassStatic         = EvidenceClassStatic
	ClassSymbolGraph    = EvidenceClassSymbolGraph
	ClassPolicy         = EvidenceClassPolicy
	ClassModelAssertion = EvidenceClassModelAssertion
)

// Route is the outcome assigned to a critic finding.
type Route string

const (
	RouteBlocking Route = "blocking"
	RouteAdvisory Route = "advisory"
)

// Outcome is the normalized result of a deterministic reproduction command.
type Outcome string

const (
	OutcomePassed Outcome = "passed"
	OutcomeFailed Outcome = "failed"
)

// Location identifies a non-empty source range. Lines and columns are
// one-based. A location is part of the binding and must match exactly.
type Location struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn"`
	EndLine     int `json:"endLine"`
	EndColumn   int `json:"endColumn"`
}

// CriticFinding is the bounded, typed claim emitted by a critic. The critic's
// prose is never sufficient for a blocking route.
type CriticFinding struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Location Location `json:"location"`
	RuleID   string   `json:"ruleID"`
	Message  string   `json:"message"`
}

// Finding is an alias for callers that use the shorter domain name.
type Finding = CriticFinding

// EvidenceBinding ties an observation to exactly one finding and source
// location. A digest that is valid for a different binding cannot corroborate
// this finding.
type EvidenceBinding struct {
	FindingID string   `json:"findingID"`
	Path      string   `json:"path"`
	Location  Location `json:"location"`
	RuleID    string   `json:"ruleID"`
}

// EvidenceValidation describes the producer's validation mode. The caller
// must obtain these values from a trusted independent verifier; this package
// validates the contract and routes conservatively, but cannot execute or
// authenticate the verifier.
type EvidenceValidation struct {
	Validator     string `json:"validator"`
	Deterministic bool   `json:"deterministic"`
	Independent   bool   `json:"independent"`
}

// ReproductionEvidence is positive when the issue reproduces on the pristine
// base and the candidate run passes the same bounded command.
type ReproductionEvidence struct {
	Command   string  `json:"command"`
	Base      Outcome `json:"base"`
	Candidate Outcome `json:"candidate"`
}

// StaticEvidence is positive when a deterministic static tool reports at least
// one match for the bound location and rule.
type StaticEvidence struct {
	Tool       string `json:"tool"`
	Matched    bool   `json:"matched"`
	MatchCount int    `json:"matchCount"`
}

// SymbolGraphEvidence is positive when a deterministic symbol query resolves
// at least one matching relationship for the bound location.
type SymbolGraphEvidence struct {
	Relation   string `json:"relation"`
	Matched    bool   `json:"matched"`
	MatchCount int    `json:"matchCount"`
}

// PolicyEvidence is positive when the named deterministic policy rule reports
// a violation at the bound location.
type PolicyEvidence struct {
	PolicyID string `json:"policyID"`
	Violated bool   `json:"violated"`
}

// ModelAssertionEvidence contains a critic/model claim. It is retained for
// visibility, but it is never eligible to block by itself.
type ModelAssertionEvidence struct {
	Model string `json:"model"`
	Claim string `json:"claim"`
}

// Evidence is a strict tagged union. Exactly one payload pointer must be
// present and it must agree with Class. Digest is the content digest of this
// object with Digest omitted. ArtifactDigest identifies the immutable source
// observation and is also checked for reuse across findings.
type Evidence struct {
	ID             string             `json:"id"`
	Class          EvidenceClass      `json:"class"`
	Binding        EvidenceBinding    `json:"binding"`
	Immutable      bool               `json:"immutable"`
	Digest         string             `json:"digest"`
	ArtifactDigest string             `json:"artifactDigest"`
	Validation     EvidenceValidation `json:"validation"`

	Reproduction   *ReproductionEvidence   `json:"reproduction,omitempty"`
	Static         *StaticEvidence         `json:"static,omitempty"`
	SymbolGraph    *SymbolGraphEvidence    `json:"symbolGraph,omitempty"`
	Policy         *PolicyEvidence         `json:"policy,omitempty"`
	ModelAssertion *ModelAssertionEvidence `json:"modelAssertion,omitempty"`
}

// CorroborationInput is the complete immutable decision input for one critic
// run. Findings and evidence are order-insensitive and are sorted by the
// canonical serializer.
type CorroborationInput struct {
	SchemaVersion string          `json:"schemaVersion"`
	Findings      []CriticFinding `json:"findings"`
	Evidence      []Evidence      `json:"evidence"`
}

// Input is a short alias for CorroborationInput.
type Input = CorroborationInput

// EvidenceDecision is the auditable routing result for one validated item.
type EvidenceDecision struct {
	ID            string        `json:"id"`
	Class         EvidenceClass `json:"class"`
	Digest        string        `json:"digest"`
	Deterministic bool          `json:"deterministic"`
	Independent   bool          `json:"independent"`
	Corroborates  bool          `json:"corroborates"`
	Route         Route         `json:"route"`
}

// FindingDecision is deterministic and ordered by Finding.ID in the result.
type FindingDecision struct {
	Finding  CriticFinding      `json:"finding"`
	Route    Route              `json:"route"`
	Reason   string             `json:"reason"`
	Evidence []EvidenceDecision `json:"evidence"`
}

// Counts records both finding routes and evidence classes. Counts are derived
// by Corroborate and are not accepted as caller-supplied authority.
type Counts struct {
	TotalFindings         int `json:"totalFindings"`
	BlockingFindings      int `json:"blockingFindings"`
	AdvisoryFindings      int `json:"advisoryFindings"`
	DeterministicEvidence int `json:"deterministicEvidence"`
	CorroboratingEvidence int `json:"corroboratingEvidence"`
	ModelAssertions       int `json:"modelAssertions"`
}

// CorroborationResult is the deterministic output of Corroborate.
type CorroborationResult struct {
	SchemaVersion string            `json:"schemaVersion"`
	Findings      []FindingDecision `json:"findings"`
	Counts        Counts            `json:"counts"`
}

// Result is a short alias for CorroborationResult.
type Result = CorroborationResult

// SealEvidence validates an evidence value without a digest and computes its
// canonical content digest. It sets Immutable=true because the returned value
// is now a sealed contract value. This does not upload or authenticate the
// referenced ArtifactDigest.
func SealEvidence(evidence Evidence) (Evidence, error) {
	evidence.Immutable = true
	evidence.Digest = ""
	if err := validateEvidence(evidence, false); err != nil {
		return Evidence{}, err
	}
	digest, err := digestEvidence(evidence)
	if err != nil {
		return Evidence{}, err
	}
	evidence.Digest = digest
	return evidence, nil
}

// EvidenceDigest returns the expected digest for an immutable evidence value.
// The supplied Digest field is ignored for the calculation, but all other
// fields must be valid.
func EvidenceDigest(evidence Evidence) (string, error) {
	if !evidence.Immutable {
		return "", invalid("evidence.immutable", "must be true")
	}
	evidence.Digest = ""
	if err := validateEvidence(evidence, false); err != nil {
		return "", err
	}
	return digestEvidence(evidence)
}

// Corroborate validates the complete input and deterministically routes every
// finding. Any malformed, ambiguous, out-of-bounds, reused, or tampered item
// aborts the whole decision with an error.
func Corroborate(input CorroborationInput) (CorroborationResult, error) {
	normalized, findingByID, err := normalizeInput(input)
	if err != nil {
		return CorroborationResult{}, err
	}

	evidenceByFinding := make(map[string][]Evidence, len(findingByID))
	for _, evidence := range normalized.Evidence {
		evidenceByFinding[evidence.Binding.FindingID] = append(evidenceByFinding[evidence.Binding.FindingID], evidence)
	}

	result := CorroborationResult{
		SchemaVersion: SchemaVersion,
		Findings:      make([]FindingDecision, 0, len(normalized.Findings)),
		Counts: Counts{
			TotalFindings: len(normalized.Findings),
		},
	}
	for _, finding := range normalized.Findings {
		evidence := evidenceByFinding[finding.ID]
		decision := FindingDecision{
			Finding:  finding,
			Route:    RouteAdvisory,
			Reason:   advisoryReason(evidence),
			Evidence: make([]EvidenceDecision, 0, len(evidence)),
		}
		blocking := false
		for _, item := range evidence {
			isModel := item.Class == EvidenceClassModelAssertion
			validatedDeterministic := !isModel && item.Validation.Deterministic && item.Validation.Independent
			corroborates := evidenceCorroborates(item)
			itemRoute := RouteAdvisory
			if validatedDeterministic && corroborates {
				blocking = true
				itemRoute = RouteBlocking
			}
			if validatedDeterministic {
				result.Counts.DeterministicEvidence++
			}
			if corroborates && validatedDeterministic {
				result.Counts.CorroboratingEvidence++
			}
			if isModel {
				result.Counts.ModelAssertions++
			}
			decision.Evidence = append(decision.Evidence, EvidenceDecision{
				ID:            item.ID,
				Class:         item.Class,
				Digest:        item.Digest,
				Deterministic: item.Validation.Deterministic,
				Independent:   item.Validation.Independent,
				Corroborates:  corroborates,
				Route:         itemRoute,
			})
		}
		if blocking {
			decision.Route = RouteBlocking
			decision.Reason = "independently validated deterministic corroboration"
			result.Counts.BlockingFindings++
		} else {
			result.Counts.AdvisoryFindings++
		}
		result.Findings = append(result.Findings, decision)
	}
	if _, err := CanonicalResultBytes(result); err != nil {
		return CorroborationResult{}, fmt.Errorf("%w: result: %v", ErrInvalidInput, err)
	}
	return result, nil
}

func advisoryReason(evidence []Evidence) string {
	if len(evidence) == 0 {
		return "no evidence supplied"
	}
	for _, item := range evidence {
		if item.Class == EvidenceClassModelAssertion {
			return "model assertion is advisory-only"
		}
	}
	return "no independently validated deterministic corroboration"
}

func evidenceCorroborates(evidence Evidence) bool {
	switch evidence.Class {
	case EvidenceClassReproduction:
		return evidence.Reproduction.Base == OutcomeFailed && evidence.Reproduction.Candidate == OutcomePassed
	case EvidenceClassStatic:
		return evidence.Static.Matched && evidence.Static.MatchCount > 0
	case EvidenceClassSymbolGraph:
		return evidence.SymbolGraph.Matched && evidence.SymbolGraph.MatchCount > 0
	case EvidenceClassPolicy:
		return evidence.Policy.Violated
	case EvidenceClassModelAssertion:
		return false
	default:
		return false
	}
}

// CanonicalInputBytes validates and serializes an input with stable field and
// collection order. It rejects oversized values before serialization.
func CanonicalInputBytes(input CorroborationInput) ([]byte, error) {
	normalized, _, err := normalizeInput(input)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal corroboration input: %w", err)
	}
	if len(encoded) > MaxInputBytes {
		return nil, invalid("input", "exceeds %d bytes", MaxInputBytes)
	}
	return encoded, nil
}

// ParseCanonicalInput rejects duplicate keys, unknown fields, trailing JSON,
// non-canonical ordering, and all semantic validation failures.
func ParseCanonicalInput(encoded []byte) (CorroborationInput, error) {
	var zero CorroborationInput
	if len(encoded) == 0 || len(encoded) > MaxInputBytes {
		return zero, invalid("input", "size is out of bounds")
	}
	if err := rejectDuplicateKeys(encoded); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var input CorroborationInput
	if err := decoder.Decode(&input); err != nil {
		return zero, invalid("input", "decode: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return zero, ErrNonCanonical
		}
		return zero, invalid("input", "trailing JSON: %v", err)
	}
	canonical, err := CanonicalInputBytes(input)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonical, encoded) {
		return zero, ErrNonCanonical
	}
	return input, nil
}

// CanonicalResultBytes serializes a generated result deterministically and
// enforces the output bound. Results are not accepted as authority by this
// package; this function is for stable evidence publication and tests.
func CanonicalResultBytes(result CorroborationResult) ([]byte, error) {
	copyResult, err := normalizeResult(result)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(copyResult)
	if err != nil {
		return nil, fmt.Errorf("marshal corroboration result: %w", err)
	}
	if len(encoded) > MaxResultBytes {
		return nil, invalid("result", "exceeds %d bytes", MaxResultBytes)
	}
	return encoded, nil
}

// normalizeResult validates a generated result, derives its counts and routes,
// and returns a copy whose collections are in canonical order. Result values
// are not accepted as authority by this package, but accepting malformed or
// internally inconsistent result bytes would make the published audit record
// ambiguous and unsafe to compare.
func normalizeResult(result CorroborationResult) (CorroborationResult, error) {
	if result.SchemaVersion != SchemaVersion {
		return CorroborationResult{}, invalid("result.schemaVersion", "must equal %q", SchemaVersion)
	}
	if result.Findings == nil || len(result.Findings) > MaxFindings {
		return CorroborationResult{}, invalid("result.findings", "must be a bounded non-nil array")
	}

	normalized := CorroborationResult{
		SchemaVersion: result.SchemaVersion,
		Findings:      make([]FindingDecision, len(result.Findings)),
	}
	derived := Counts{TotalFindings: len(result.Findings)}
	seenFindings := make(map[string]struct{}, len(result.Findings))
	seenEvidenceIDs := make(map[string]struct{})
	seenEvidenceDigests := make(map[string]struct{})
	totalEvidence := 0

	for i, source := range result.Findings {
		decision := source
		if err := validateFinding(decision.Finding); err != nil {
			return CorroborationResult{}, invalid(fmt.Sprintf("result.findings[%d]", i), "%v", err)
		}
		if _, exists := seenFindings[decision.Finding.ID]; exists {
			return CorroborationResult{}, invalid(fmt.Sprintf("result.findings[%d].finding.id", i), "duplicate finding ID %q", decision.Finding.ID)
		}
		seenFindings[decision.Finding.ID] = struct{}{}
		if err := validateRoute(decision.Route, fmt.Sprintf("result.findings[%d].route", i)); err != nil {
			return CorroborationResult{}, err
		}
		if err := validateMessage(decision.Reason, MaxEvidenceDescriptionBytes, fmt.Sprintf("result.findings[%d].reason", i)); err != nil {
			return CorroborationResult{}, err
		}
		if decision.Evidence == nil || len(decision.Evidence) > MaxEvidence {
			return CorroborationResult{}, invalid(fmt.Sprintf("result.findings[%d].evidence", i), "must be a bounded non-nil array")
		}
		totalEvidence += len(decision.Evidence)
		if totalEvidence > MaxEvidence {
			return CorroborationResult{}, invalid("result.evidence", "total count exceeds %d", MaxEvidence)
		}
		decision.Evidence = make([]EvidenceDecision, len(source.Evidence))
		copy(decision.Evidence, source.Evidence)

		findingBlocking := false
		for j := range decision.Evidence {
			evidence := &decision.Evidence[j]
			field := fmt.Sprintf("result.findings[%d].evidence[%d]", i, j)
			if err := validateResultEvidence(*evidence, field); err != nil {
				return CorroborationResult{}, err
			}
			if _, exists := seenEvidenceIDs[evidence.ID]; exists {
				return CorroborationResult{}, invalid(field+".id", "duplicate evidence ID %q", evidence.ID)
			}
			seenEvidenceIDs[evidence.ID] = struct{}{}
			if _, exists := seenEvidenceDigests[evidence.Digest]; exists {
				return CorroborationResult{}, invalid(field+".digest", "evidence digest is reused")
			}
			seenEvidenceDigests[evidence.Digest] = struct{}{}

			validatedDeterministic := evidence.Class != EvidenceClassModelAssertion && evidence.Deterministic && evidence.Independent
			if validatedDeterministic {
				derived.DeterministicEvidence++
			}
			if validatedDeterministic && evidence.Corroborates {
				derived.CorroboratingEvidence++
			}
			if evidence.Class == EvidenceClassModelAssertion {
				derived.ModelAssertions++
			}
			expectedRoute := RouteAdvisory
			if evidence.Class != EvidenceClassModelAssertion && validatedDeterministic && evidence.Corroborates {
				expectedRoute = RouteBlocking
				findingBlocking = true
			}
			if evidence.Route != expectedRoute {
				return CorroborationResult{}, invalid(field+".route", "does not match the deterministic corroboration decision")
			}
			if evidence.Class == EvidenceClassModelAssertion && evidence.Corroborates {
				return CorroborationResult{}, invalid(field+".corroborates", "model assertions are advisory-only")
			}
		}
		if findingBlocking {
			derived.BlockingFindings++
		} else {
			derived.AdvisoryFindings++
		}
		expectedFindingRoute := RouteAdvisory
		if findingBlocking {
			expectedFindingRoute = RouteBlocking
		}
		if decision.Route != expectedFindingRoute {
			return CorroborationResult{}, invalid(fmt.Sprintf("result.findings[%d].route", i), "does not match its evidence")
		}
		normalized.Findings[i] = decision
	}
	if result.Counts != derived {
		return CorroborationResult{}, invalid("result.counts", "does not match the derived result")
	}
	normalized.Counts = derived
	sort.Slice(normalized.Findings, func(i, j int) bool {
		return normalized.Findings[i].Finding.ID < normalized.Findings[j].Finding.ID
	})
	for i := range normalized.Findings {
		sort.Slice(normalized.Findings[i].Evidence, func(left, right int) bool {
			return normalized.Findings[i].Evidence[left].ID < normalized.Findings[i].Evidence[right].ID
		})
	}
	return normalized, nil
}

func validateResultEvidence(evidence EvidenceDecision, field string) error {
	if err := validateToken(evidence.ID, MaxEvidenceIDBytes, field+".id"); err != nil {
		return err
	}
	if !isKnownEvidenceClass(evidence.Class) {
		return invalid(field+".class", "unknown evidence class %q", evidence.Class)
	}
	if !validDigest(evidence.Digest) {
		return invalid(field+".digest", "must be a lowercase sha256 digest")
	}
	return validateRoute(evidence.Route, field+".route")
}

func validateRoute(route Route, field string) error {
	if route != RouteBlocking && route != RouteAdvisory {
		return invalid(field, "unknown route %q", route)
	}
	return nil
}

func normalizeInput(input CorroborationInput) (CorroborationInput, map[string]CriticFinding, error) {
	if input.SchemaVersion != SchemaVersion {
		return CorroborationInput{}, nil, invalid("schemaVersion", "must equal %q", SchemaVersion)
	}
	if input.Findings == nil || len(input.Findings) > MaxFindings {
		return CorroborationInput{}, nil, invalid("findings", "must be a bounded non-nil array")
	}
	if input.Evidence == nil || len(input.Evidence) > MaxEvidence {
		return CorroborationInput{}, nil, invalid("evidence", "must be a bounded non-nil array")
	}
	normalized := CorroborationInput{
		SchemaVersion: input.SchemaVersion,
		Findings:      make([]CriticFinding, len(input.Findings)),
		Evidence:      make([]Evidence, len(input.Evidence)),
	}
	copy(normalized.Findings, input.Findings)
	copy(normalized.Evidence, input.Evidence)
	findingByID := make(map[string]CriticFinding, len(normalized.Findings))
	for i, finding := range normalized.Findings {
		if err := validateFinding(finding); err != nil {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("findings[%d]", i), "%v", err)
		}
		if _, exists := findingByID[finding.ID]; exists {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("findings[%d].id", i), "duplicate finding ID %q", finding.ID)
		}
		findingByID[finding.ID] = finding
	}
	sort.Slice(normalized.Findings, func(i, j int) bool { return normalized.Findings[i].ID < normalized.Findings[j].ID })

	seenEvidenceID := make(map[string]struct{}, len(normalized.Evidence))
	seenArtifact := make(map[string]string, len(normalized.Evidence))
	for i, evidence := range normalized.Evidence {
		if err := validateEvidence(evidence, true); err != nil {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("evidence[%d]", i), "%v", err)
		}
		if _, exists := seenEvidenceID[evidence.ID]; exists {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("evidence[%d].id", i), "duplicate evidence ID %q", evidence.ID)
		}
		seenEvidenceID[evidence.ID] = struct{}{}
		finding, exists := findingByID[evidence.Binding.FindingID]
		if !exists {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("evidence[%d].binding.findingID", i), "unknown finding %q", evidence.Binding.FindingID)
		}
		if !sameBinding(finding, evidence.Binding) {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("evidence[%d].binding", i), "does not exactly bind finding %q", finding.ID)
		}
		if previous, reused := seenArtifact[evidence.ArtifactDigest]; reused {
			return CorroborationInput{}, nil, invalid(fmt.Sprintf("evidence[%d].artifactDigest", i), "immutable evidence reused by findings %q and %q", previous, finding.ID)
		}
		seenArtifact[evidence.ArtifactDigest] = finding.ID
	}
	sort.Slice(normalized.Evidence, func(i, j int) bool {
		if normalized.Evidence[i].ID != normalized.Evidence[j].ID {
			return normalized.Evidence[i].ID < normalized.Evidence[j].ID
		}
		return normalized.Evidence[i].Class < normalized.Evidence[j].Class
	})
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return CorroborationInput{}, nil, fmt.Errorf("marshal corroboration input: %w", err)
	}
	if len(encoded) > MaxInputBytes {
		return CorroborationInput{}, nil, invalid("input", "exceeds %d bytes", MaxInputBytes)
	}
	return normalized, findingByID, nil
}

func validateFinding(finding CriticFinding) error {
	if err := validateToken(finding.ID, MaxFindingIDBytes, "finding.id"); err != nil {
		return err
	}
	if err := validatePath(finding.Path); err != nil {
		return err
	}
	if err := validateLocation(finding.Location); err != nil {
		return err
	}
	if err := validateToken(finding.RuleID, MaxRuleIDBytes, "finding.ruleID"); err != nil {
		return err
	}
	return validateMessage(finding.Message, MaxMessageBytes, "finding.message")
}

func validateBinding(binding EvidenceBinding) error {
	if err := validateToken(binding.FindingID, MaxFindingIDBytes, "binding.findingID"); err != nil {
		return err
	}
	if err := validatePath(binding.Path); err != nil {
		return err
	}
	if err := validateLocation(binding.Location); err != nil {
		return err
	}
	return validateToken(binding.RuleID, MaxRuleIDBytes, "binding.ruleID")
}

func validateEvidence(evidence Evidence, requireDigest bool) error {
	if err := validateToken(evidence.ID, MaxEvidenceIDBytes, "evidence.id"); err != nil {
		return err
	}
	if !isKnownEvidenceClass(evidence.Class) {
		return invalid("evidence.class", "unknown class %q", evidence.Class)
	}
	if err := validateBinding(evidence.Binding); err != nil {
		return err
	}
	if !evidence.Immutable {
		return invalid("evidence.immutable", "must be true")
	}
	if !validDigest(evidence.ArtifactDigest) {
		return invalid("evidence.artifactDigest", "must be a lowercase sha256 digest")
	}
	if requireDigest {
		if !validDigest(evidence.Digest) {
			return invalid("evidence.digest", "must be a lowercase sha256 digest")
		}
	} else if evidence.Digest != "" {
		return invalid("evidence.digest", "must be empty while sealing")
	}
	if err := validateToken(evidence.Validation.Validator, MaxValidatorBytes, "evidence.validation.validator"); err != nil {
		return err
	}
	if err := validatePayloadShape(evidence); err != nil {
		return err
	}
	switch evidence.Class {
	case EvidenceClassReproduction:
		if err := validateReproduction(*evidence.Reproduction); err != nil {
			return err
		}
	case EvidenceClassStatic:
		if err := validateStatic(*evidence.Static); err != nil {
			return err
		}
	case EvidenceClassSymbolGraph:
		if err := validateSymbolGraph(*evidence.SymbolGraph); err != nil {
			return err
		}
	case EvidenceClassPolicy:
		if err := validatePolicy(*evidence.Policy, evidence.Binding.RuleID); err != nil {
			return err
		}
	case EvidenceClassModelAssertion:
		if err := validateModelAssertion(*evidence.ModelAssertion); err != nil {
			return err
		}
	}
	if requireDigest {
		expected, err := digestEvidence(evidence)
		if err != nil {
			return err
		}
		if evidence.Digest != expected {
			return invalid("evidence.digest", "does not match canonical evidence content")
		}
	}
	return nil
}

func validatePayloadShape(evidence Evidence) error {
	count := 0
	if evidence.Reproduction != nil {
		count++
	}
	if evidence.Static != nil {
		count++
	}
	if evidence.SymbolGraph != nil {
		count++
	}
	if evidence.Policy != nil {
		count++
	}
	if evidence.ModelAssertion != nil {
		count++
	}
	if count != 1 {
		return invalid("evidence", "must contain exactly one typed payload")
	}
	if evidence.Class == EvidenceClassReproduction && evidence.Reproduction == nil {
		return invalid("evidence.reproduction", "payload is required for class")
	}
	if evidence.Class == EvidenceClassStatic && evidence.Static == nil {
		return invalid("evidence.static", "payload is required for class")
	}
	if evidence.Class == EvidenceClassSymbolGraph && evidence.SymbolGraph == nil {
		return invalid("evidence.symbolGraph", "payload is required for class")
	}
	if evidence.Class == EvidenceClassPolicy && evidence.Policy == nil {
		return invalid("evidence.policy", "payload is required for class")
	}
	if evidence.Class == EvidenceClassModelAssertion && evidence.ModelAssertion == nil {
		return invalid("evidence.modelAssertion", "payload is required for class")
	}
	return nil
}

func validateReproduction(evidence ReproductionEvidence) error {
	if err := validateMessage(evidence.Command, MaxCommandBytes, "reproduction.command"); err != nil {
		return err
	}
	if evidence.Base != OutcomePassed && evidence.Base != OutcomeFailed {
		return invalid("reproduction.base", "unknown outcome")
	}
	if evidence.Candidate != OutcomePassed && evidence.Candidate != OutcomeFailed {
		return invalid("reproduction.candidate", "unknown outcome")
	}
	return nil
}

func validateStatic(evidence StaticEvidence) error {
	if err := validateToken(evidence.Tool, MaxToolBytes, "static.tool"); err != nil {
		return err
	}
	if evidence.MatchCount < 0 || evidence.MatchCount > MaxEvidenceMatches {
		return invalid("static.matchCount", "is out of bounds")
	}
	if evidence.Matched != (evidence.MatchCount > 0) {
		return invalid("static", "matched must agree with matchCount")
	}
	return nil
}

func validateSymbolGraph(evidence SymbolGraphEvidence) error {
	if err := validateMessage(evidence.Relation, MaxRelationBytes, "symbolGraph.relation"); err != nil {
		return err
	}
	if evidence.MatchCount < 0 || evidence.MatchCount > MaxEvidenceMatches {
		return invalid("symbolGraph.matchCount", "is out of bounds")
	}
	if evidence.Matched != (evidence.MatchCount > 0) {
		return invalid("symbolGraph", "matched must agree with matchCount")
	}
	return nil
}

func validatePolicy(evidence PolicyEvidence, ruleID string) error {
	if err := validateToken(evidence.PolicyID, MaxRuleIDBytes, "policy.policyID"); err != nil {
		return err
	}
	if evidence.PolicyID != ruleID {
		return invalid("policy.policyID", "must equal bound ruleID")
	}
	return nil
}

func validateModelAssertion(evidence ModelAssertionEvidence) error {
	if err := validateToken(evidence.Model, MaxValidatorBytes, "modelAssertion.model"); err != nil {
		return err
	}
	return validateMessage(evidence.Claim, MaxClaimBytes, "modelAssertion.claim")
}

func sameBinding(finding CriticFinding, binding EvidenceBinding) bool {
	return finding.ID == binding.FindingID && finding.Path == binding.Path && finding.RuleID == binding.RuleID && sameLocation(finding.Location, binding.Location)
}

func sameLocation(left, right Location) bool {
	return left == right
}

func digestEvidence(evidence Evidence) (string, error) {
	evidence.Digest = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("marshal evidence for digest: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func isKnownEvidenceClass(class EvidenceClass) bool {
	switch class {
	case EvidenceClassReproduction, EvidenceClassStatic, EvidenceClassSymbolGraph, EvidenceClassPolicy, EvidenceClassModelAssertion:
		return true
	default:
		return false
	}
}

func validDigest(value string) bool {
	return digestPattern.MatchString(value)
}

func validateToken(value string, maxBytes int, field string) error {
	if value == "" {
		return invalid(field, "must not be empty")
	}
	if len(value) > maxBytes {
		return invalid(field, "exceeds %d bytes", maxBytes)
	}
	if !utf8.ValidString(value) || !tokenPattern.MatchString(value) {
		return invalid(field, "contains invalid token characters")
	}
	return nil
}

func validateMessage(value string, maxBytes int, field string) error {
	if value == "" {
		return invalid(field, "must not be empty")
	}
	if len(value) > maxBytes {
		return invalid(field, "exceeds %d bytes", maxBytes)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return invalid(field, "must be valid UTF-8 without NUL")
	}
	for _, r := range value {
		// Newlines, carriage returns, and tabs are useful in human-readable
		// claims and reasons. Other controls and Unicode formatting/line
		// separators make an audit record ambiguous or visually misleading.
		if (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return invalid(field, "contains a control or formatting character")
		}
	}
	return nil
}

func validatePath(value string) error {
	if value == "" {
		return invalid("path", "must not be empty")
	}
	if len(value) > MaxPathBytes {
		return invalid("path", "exceeds %d bytes", MaxPathBytes)
	}
	if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return invalid("path", "must be valid UTF-8 without NUL")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return invalid("path", "contains a control or formatting character")
		}
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return invalid("path", "must be a relative slash-separated path")
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return invalid("path", "contains an ambiguous or traversal segment")
		}
	}
	return nil
}

func validateLocation(location Location) error {
	const maxCoordinate = 1_000_000_000
	if location.StartLine < 1 || location.StartLine > maxCoordinate || location.EndLine < 1 || location.EndLine > maxCoordinate {
		return invalid("location", "line is out of bounds")
	}
	if location.StartColumn < 1 || location.StartColumn > maxCoordinate || location.EndColumn < 1 || location.EndColumn > maxCoordinate {
		return invalid("location", "column is out of bounds")
	}
	if location.EndLine < location.StartLine || (location.EndLine == location.StartLine && location.EndColumn < location.StartColumn) {
		return invalid("location", "end must not precede start")
	}
	return nil
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidInput, field, fmt.Sprintf(format, args...))
}

// rejectDuplicateKeys walks every JSON object before decoding into typed
// structs. encoding/json otherwise silently accepts duplicate keys, which can
// make a signed/canonical payload have two competing interpretations.
func rejectDuplicateKeys(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, "$", 0); err != nil {
		return invalid("input", "invalid JSON: %v", err)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return invalid("input", "trailing JSON value %v", token)
		}
		return invalid("input", "trailing JSON: %v", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, location string, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch delimiter := token.(type) {
	case json.Delim:
		if depth >= MaxJSONDepth {
			return fmt.Errorf("JSON nesting exceeds %d levels", MaxJSONDepth)
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object key %q at %s", key, location)
				}
				seen[key] = struct{}{}
				if err := scanJSONValue(decoder, location+"."+key, depth+1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return errors.New("object did not close")
			}
		case '[':
			index := 0
			for decoder.More() {
				if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index), depth+1); err != nil {
					return err
				}
				index++
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return errors.New("array did not close")
			}
		default:
			return fmt.Errorf("unexpected delimiter %q", delimiter)
		}
	}
	return nil
}
