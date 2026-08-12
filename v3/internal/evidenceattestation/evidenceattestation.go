// Package evidenceattestation adapts a trusted Agents Gateway Gate report to
// an in-toto v1 Statement.
//
// The package deliberately stops at the unsigned statement boundary. It does
// not implement DSSE, a signing algorithm, key storage, or OCI publication;
// cosign owns those operations. The existing Ed25519 Gate report remains the
// source of truth. A statement can only be built from a report that was
// verified with the trusted Gate signer, so parsing a plausible predicate is
// never sufficient to establish Gate proof.
package evidenceattestation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	intotov1 "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// StatementType is the official in-toto v1 Statement type URI.
	StatementType = intotov1.StatementTypeUri
	// PredicateType is the versioned Agents Gateway Gate-evidence predicate.
	// The v1beta1 report adds mandatory scoring and critic provenance fields.
	PredicateType = "https://agents.astatide.com/verification/v1beta1"
	// SchemaVersion is the canonical AGW report schema carried by the
	// predicate. It is intentionally the existing Gate report schema.
	SchemaVersion = gate.ReportSchemaVersion
	// MediaType is the artifact media type for the unsigned statement bytes.
	// DSSE payloads use the standard application/vnd.in-toto+json type when
	// cosign wraps these bytes; this package does not create that envelope.
	MediaType = "application/vnd.agents-gateway.verification-attestation.v1+json"
	// DSSEPayloadType is the standard payload type cosign uses for an in-toto
	// statement. It is exported for callers constructing an external envelope.
	DSSEPayloadType = "application/vnd.in-toto+json"

	// PatchSubjectName is the stable logical name used for the single subject.
	PatchSubjectName = "patch.diff"

	// MaxStatementBytes bounds parsing and serialization. It leaves a small
	// envelope allowance above the existing canonical report ceiling without
	// expanding that report's own limit.
	MaxStatementBytes = gate.MaxReportBytes + (64 << 10)
	// MaxEvidenceArtifactDigests bounds optional raw-evidence artifact
	// identities supplied by the caller. Command-level evidence digests remain
	// in the canonical Gate report and are always carried in the predicate.
	MaxEvidenceArtifactDigests = 32
	// MaxCheckSeverityBindings bounds the optional advisory/blocking
	// classification projection.
	MaxCheckSeverityBindings = gate.MaxChecks

	maxJSONDepth        = 64
	maxJSONObjectFields = 256
	maxJSONArrayItems   = gate.MaxChangedPaths + gate.MaxChecks + gate.MaxCommands + gate.MaxHelperImages + 64
)

var (
	// ErrInvalidInput identifies a malformed trusted report or build option.
	ErrInvalidInput = errors.New("evidenceattestation: invalid input")
	// ErrUntrustedReport identifies a report that was not authenticated by the
	// trusted existing Gate signer.
	ErrUntrustedReport = errors.New("evidenceattestation: Gate report is not trusted")
	// ErrStatementInvalid identifies malformed or semantically invalid
	// statement bytes.
	ErrStatementInvalid = errors.New("evidenceattestation: invalid statement")
	// ErrStatementNonCanonical identifies valid JSON that is not the exact
	// deterministic representation emitted by StatementBytes.
	ErrStatementNonCanonical = errors.New("evidenceattestation: statement is not canonical")
	// ErrBindingMismatch identifies a statement whose Gate bindings differ from
	// the independently verified report and caller-supplied artifact identities.
	ErrBindingMismatch = errors.New("evidenceattestation: Gate binding mismatch")
	// ErrMediaTypeMismatch identifies a caller that supplied a different
	// artifact media type.
	ErrMediaTypeMismatch = errors.New("evidenceattestation: media type mismatch")
)

// CheckSeverity preserves the quality contract's advisory/blocking
// classification alongside the current GateCheck fields. The current Gate
// engine emits only machine-checkable checks; callers with a compiled Policy
// may supply the classification explicitly through Options.
type CheckSeverity string

const (
	SeverityBlocking CheckSeverity = "blocking"
	SeverityAdvisory CheckSeverity = "advisory"
)

// CheckSeverityBinding associates one canonical Gate check name with its
// enforcement level. When Options.CheckSeverities is nil, every current Gate
// check is conservatively represented as blocking. A non-nil list must cover
// every check exactly once.
type CheckSeverityBinding struct {
	Name     string        `json:"name"`
	Severity CheckSeverity `json:"severity"`
}

// Options supplies identities that are not present in the current
// VerificationReport itself. EvidenceArtifactDigests may contain the raw
// verifier-evidence artifact refs retained by verifycontroller. The signed
// report identity is computed from the verified signed report bytes and is
// never caller-controlled.
type Options struct {
	EvidenceArtifactDigests []string
	CheckSeverities         []CheckSeverityBinding
}

// VerifiedReport is an opaque proof that the existing signed Gate report was
// canonical, authenticated by the trusted public key, and internally valid.
// Its mutable fields are private; accessors return defensive copies.
type VerifiedReport struct {
	report             gate.VerificationReport
	canonicalReport    []byte
	signedReport       []byte
	reportDigest       string
	signedReportDigest string
}

// Report returns a defensive copy of the verified canonical Gate report.
func (v VerifiedReport) Report() gate.VerificationReport {
	return cloneReport(v.report)
}

// CanonicalReportBytes returns a defensive copy of the canonical unsigned
// report bytes covered by the existing Gate signature.
func (v VerifiedReport) CanonicalReportBytes() []byte {
	return append([]byte(nil), v.canonicalReport...)
}

// SignedReportBytes returns a defensive copy of the canonical signed report
// artifact bytes. Its digest is the report identity carried by the predicate.
func (v VerifiedReport) SignedReportBytes() []byte {
	return append([]byte(nil), v.signedReport...)
}

// ReportDigest is the digest of canonical unsigned VerificationReport bytes.
func (v VerifiedReport) ReportDigest() string { return v.reportDigest }

// SignedReportDigest is the digest of canonical SignedReport artifact bytes.
func (v VerifiedReport) SignedReportDigest() string { return v.signedReportDigest }

// VerifySignedReport authenticates the current Gate report format and returns
// an opaque proof suitable for StatementFromVerifiedReport. A trusted key is
// required; embedded-key-only verification is intentionally not enough for a
// Gate proof.
func VerifySignedReport(signed gate.SignedReport, trusted ed25519.PublicKey) (VerifiedReport, error) {
	if len(trusted) != ed25519.PublicKeySize {
		return VerifiedReport{}, fmt.Errorf("%w: trusted Gate public key is required", ErrUntrustedReport)
	}
	encoded, err := gate.SignedReportBytes(signed)
	if err != nil {
		return VerifiedReport{}, fmt.Errorf("%w: canonical signed report: %v", ErrUntrustedReport, err)
	}
	return VerifySignedReportBytes(encoded, trusted)
}

// VerifySignedReportBytes parses, canonicalizes, authenticates, and binds the
// existing signed Gate report. The input must already be the exact canonical
// signed-report representation; this mirrors publishcontroller's strict
// artifact check and prevents alternate JSON encodings from becoming a second
// identity.
func VerifySignedReportBytes(encoded []byte, trusted ed25519.PublicKey) (VerifiedReport, error) {
	if len(trusted) != ed25519.PublicKeySize {
		return VerifiedReport{}, fmt.Errorf("%w: trusted Gate public key is required", ErrUntrustedReport)
	}
	if len(encoded) == 0 || len(encoded) > gate.MaxReportBytes+1024 || !utf8.Valid(encoded) {
		return VerifiedReport{}, fmt.Errorf("%w: signed report size is out of bounds", ErrUntrustedReport)
	}
	signed, err := gate.ParseSignedReport(encoded)
	if err != nil {
		return VerifiedReport{}, fmt.Errorf("%w: parse signed report: %v", ErrUntrustedReport, err)
	}
	canonicalSigned, err := gate.SignedReportBytes(signed)
	if err != nil || !bytes.Equal(canonicalSigned, encoded) {
		return VerifiedReport{}, fmt.Errorf("%w: signed report is not canonical", ErrUntrustedReport)
	}
	if err := gate.VerifySignedReport(signed, trusted); err != nil {
		return VerifiedReport{}, fmt.Errorf("%w: %v", ErrUntrustedReport, err)
	}
	canonicalReport, err := gate.CanonicalReportBytes(signed.Report)
	if err != nil {
		return VerifiedReport{}, fmt.Errorf("%w: canonical report: %v", ErrUntrustedReport, err)
	}
	reportDigest, err := gate.ReportDigest(signed.Report)
	if err != nil {
		return VerifiedReport{}, fmt.Errorf("%w: report digest: %v", ErrUntrustedReport, err)
	}
	return VerifiedReport{
		report:             cloneReport(signed.Report),
		canonicalReport:    append([]byte(nil), canonicalReport...),
		signedReport:       append([]byte(nil), canonicalSigned...),
		reportDigest:       reportDigest,
		signedReportDigest: digestBytes(canonicalSigned),
	}, nil
}

// Statement is the strict JSON wire representation of an in-toto v1
// Statement. It intentionally uses a typed predicate instead of map[string]any
// so unknown fields, duplicate keys, and accidental loss of 64-bit values are
// detectable at this boundary.
type Statement struct {
	Type          string                `json:"_type"`
	Subject       []Subject             `json:"subject"`
	PredicateType string                `json:"predicateType"`
	Predicate     VerificationPredicate `json:"predicate"`
}

// Subject is the one patch resource descriptor in the statement. The
// validator requires exactly one sha256 digest and rejects additional map
// algorithms.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// VerificationPredicate is a lossless, versioned projection of the current
// canonical Gate VerificationReport plus the identities needed to relate it
// to its signed report and optional raw evidence artifacts.
type VerificationPredicate struct {
	SchemaVersion           string               `json:"schemaVersion"`
	ReportDigest            string               `json:"reportDigest"`
	SignedReportDigest      string               `json:"signedReportDigest"`
	EvidenceArtifactDigests []string             `json:"evidenceArtifactDigests"`
	RunUID                  string               `json:"runUID"`
	SpecDigest              string               `json:"specDigest"`
	BaseSHA                 string               `json:"baseSHA"`
	PatchDigest             string               `json:"patchDigest"`
	GateUID                 string               `json:"gateUID"`
	GateGeneration          int64                `json:"gateGeneration"`
	RuntimeImageDigest      string               `json:"runtimeImageDigest"`
	VerifierImageDigest     string               `json:"verifierImageDigest"`
	HelperImageDigests      []gate.ImageEvidence `json:"helperImageDigests"`
	SkillDigests            []string             `json:"skillDigests"`
	Evidence                gate.EvidenceSummary `json:"evidence"`
	Commands                []gate.ReportCommand `json:"commands"`
	Scoring                 gate.ScoringEvidence `json:"scoring"`
	Checks                  []CheckBinding       `json:"checks"`
	Verdict                 gate.Verdict         `json:"verdict"`
}

// CheckBinding is the Gate report check plus its preserved quality-contract
// severity. Severity never changes the existing report verdict; it records
// whether a check is advisory or blocking for downstream consumers.
type CheckBinding struct {
	Name     string        `json:"name"`
	Passed   bool          `json:"passed"`
	Message  string        `json:"message,omitempty"`
	Severity CheckSeverity `json:"severity"`
}

// StatementFromVerifiedReport builds a deterministic statement from trusted
// report bytes. The function is the only statement-construction path; callers
// cannot manufacture an Accepted Gate predicate by filling a struct manually.
func StatementFromVerifiedReport(verified VerifiedReport, options Options) (Statement, error) {
	if !verified.valid() {
		return Statement{}, fmt.Errorf("%w: verified report proof is incomplete", ErrUntrustedReport)
	}
	normalized, err := normalizeOptions(options, verified.report.Checks)
	if err != nil {
		return Statement{}, err
	}
	report := cloneReport(verified.report)
	predicate := VerificationPredicate{
		SchemaVersion:           SchemaVersion,
		ReportDigest:            verified.reportDigest,
		SignedReportDigest:      verified.signedReportDigest,
		EvidenceArtifactDigests: append([]string(nil), normalized.EvidenceArtifactDigests...),
		RunUID:                  report.RunUID,
		SpecDigest:              report.SpecDigest,
		BaseSHA:                 report.BaseSHA,
		PatchDigest:             report.PatchDigest,
		GateUID:                 report.GateUID,
		GateGeneration:          report.GateGeneration,
		RuntimeImageDigest:      report.RuntimeImageDigest,
		VerifierImageDigest:     report.VerifierImageDigest,
		HelperImageDigests:      append([]gate.ImageEvidence(nil), report.HelperImageDigests...),
		SkillDigests:            append([]string(nil), report.SkillDigests...),
		Evidence:                cloneEvidence(report.Evidence),
		Commands:                cloneCommands(report.Commands),
		Scoring:                 cloneScoring(report.Scoring),
		Checks:                  make([]CheckBinding, 0, len(report.Checks)),
		Verdict:                 report.Verdict,
	}
	severityByName := make(map[string]CheckSeverity, len(normalized.CheckSeverities))
	for _, binding := range normalized.CheckSeverities {
		severityByName[binding.Name] = binding.Severity
	}
	for _, check := range report.Checks {
		severity := SeverityBlocking
		if normalized.CheckSeverities != nil {
			severity = severityByName[check.Name]
		}
		predicate.Checks = append(predicate.Checks, CheckBinding{
			Name: check.Name, Passed: check.Passed, Message: check.Message, Severity: severity,
		})
	}
	statement := Statement{
		Type:          StatementType,
		Subject:       []Subject{{Name: PatchSubjectName, Digest: map[string]string{"sha256": digestHex(report.PatchDigest)}}},
		PredicateType: PredicateType,
		Predicate:     predicate,
	}
	if err := validateStatement(statement); err != nil {
		return Statement{}, err
	}
	return statement, nil
}

// StatementBytes returns the deterministic compact JSON representation of a
// valid statement. It performs no signing.
func StatementBytes(statement Statement) ([]byte, error) {
	if err := validateStatement(statement); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(statement)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal statement: %v", ErrStatementInvalid, err)
	}
	if len(encoded) == 0 || len(encoded) > MaxStatementBytes {
		return nil, fmt.Errorf("%w: statement exceeds bounded size", ErrStatementInvalid)
	}
	return encoded, nil
}

// StatementDigest returns the sha256 digest of StatementBytes.
func StatementDigest(statement Statement) (string, error) {
	body, err := StatementBytes(statement)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

// ParseStatement parses exactly one canonical statement. It rejects unknown
// and duplicate JSON keys recursively, enforces semantic bounds, validates the
// official in-toto v1 shape, and requires the input to equal StatementBytes.
// Parsing alone is not Gate proof; use VerifyStatement with VerifiedReport.
func ParseStatement(body []byte) (Statement, error) {
	if len(body) == 0 || len(body) > MaxStatementBytes || !utf8.Valid(body) {
		return Statement{}, fmt.Errorf("%w: statement size is out of bounds", ErrStatementInvalid)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return Statement{}, fmt.Errorf("%w: strict JSON: %v", ErrStatementInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var statement Statement
	if err := decoder.Decode(&statement); err != nil {
		return Statement{}, fmt.Errorf("%w: decode: %v", ErrStatementInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Statement{}, fmt.Errorf("%w: trailing JSON value", ErrStatementInvalid)
		}
		return Statement{}, fmt.Errorf("%w: trailing JSON: %v", ErrStatementInvalid, err)
	}
	if err := validateStatement(statement); err != nil {
		return Statement{}, err
	}
	canonical, err := StatementBytes(statement)
	if err != nil {
		return Statement{}, err
	}
	if !bytes.Equal(canonical, body) {
		return Statement{}, ErrStatementNonCanonical
	}
	return statement, nil
}

// ParseArtifact verifies the exact artifact media type before parsing the
// statement bytes.
func ParseArtifact(mediaType string, body []byte) (Statement, error) {
	if mediaType != MediaType {
		return Statement{}, ErrMediaTypeMismatch
	}
	return ParseStatement(body)
}

// VerifyStatement reconstructs the expected statement from the authenticated
// current Gate report and compares every canonical byte, including patch,
// signed-report, raw-evidence, image, skill, check-severity, check, and verdict
// identities. It does not verify a DSSE signature; cosign performs that outer
// operation.
func VerifyStatement(body []byte, verified VerifiedReport, options Options) error {
	got, err := ParseStatement(body)
	if err != nil {
		return err
	}
	want, err := StatementFromVerifiedReport(verified, options)
	if err != nil {
		return err
	}
	gotBytes, err := StatementBytes(got)
	if err != nil {
		return err
	}
	wantBytes, err := StatementBytes(want)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		return ErrBindingMismatch
	}
	return nil
}

// VerifyArtifact combines media-type validation and Gate-binding
// verification.
func VerifyArtifact(mediaType string, body []byte, verified VerifiedReport, options Options) error {
	if mediaType != MediaType {
		return ErrMediaTypeMismatch
	}
	return VerifyStatement(body, verified, options)
}

func (v VerifiedReport) valid() bool {
	if len(v.canonicalReport) == 0 || len(v.signedReport) == 0 || !validDigest(v.reportDigest) || !validDigest(v.signedReportDigest) {
		return false
	}
	canonical, err := gate.CanonicalReportBytes(v.report)
	if err != nil || !bytes.Equal(canonical, v.canonicalReport) {
		return false
	}
	reportDigest, err := gate.ReportDigest(v.report)
	return err == nil && reportDigest == v.reportDigest && digestBytes(v.signedReport) == v.signedReportDigest
}

type normalizedOptions struct {
	EvidenceArtifactDigests []string
	CheckSeverities         []CheckSeverityBinding
}

func normalizeOptions(options Options, checks []v1alpha1.GateCheck) (normalizedOptions, error) {
	output := normalizedOptions{
		EvidenceArtifactDigests: append([]string(nil), options.EvidenceArtifactDigests...),
		CheckSeverities:         append([]CheckSeverityBinding(nil), options.CheckSeverities...),
	}
	if len(output.EvidenceArtifactDigests) > MaxEvidenceArtifactDigests {
		return normalizedOptions{}, fmt.Errorf("%w: evidence artifact identity count is out of bounds", ErrInvalidInput)
	}
	sort.Strings(output.EvidenceArtifactDigests)
	for index, digest := range output.EvidenceArtifactDigests {
		if !validDigest(digest) || (index > 0 && output.EvidenceArtifactDigests[index-1] == digest) {
			return normalizedOptions{}, fmt.Errorf("%w: evidence artifact identities are invalid", ErrInvalidInput)
		}
	}
	if options.CheckSeverities == nil {
		return output, nil
	}
	if len(output.CheckSeverities) != len(checks) || len(output.CheckSeverities) > MaxCheckSeverityBindings {
		return normalizedOptions{}, fmt.Errorf("%w: check severity bindings must cover every Gate check", ErrInvalidInput)
	}
	sort.Slice(output.CheckSeverities, func(i, j int) bool { return output.CheckSeverities[i].Name < output.CheckSeverities[j].Name })
	for index, binding := range output.CheckSeverities {
		if !validIdentifier(binding.Name, 128) || (binding.Severity != SeverityBlocking && binding.Severity != SeverityAdvisory) {
			return normalizedOptions{}, fmt.Errorf("%w: invalid check severity binding", ErrInvalidInput)
		}
		if index > 0 && output.CheckSeverities[index-1].Name == binding.Name {
			return normalizedOptions{}, fmt.Errorf("%w: duplicate check severity binding", ErrInvalidInput)
		}
		if !containsCheck(checks, binding.Name) {
			return normalizedOptions{}, fmt.Errorf("%w: check severity binding names an unknown check", ErrInvalidInput)
		}
	}
	return output, nil
}

func containsCheck(checks []v1alpha1.GateCheck, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func validateStatement(statement Statement) error {
	if statement.Type != StatementType || statement.PredicateType != PredicateType {
		return fmt.Errorf("%w: exact in-toto or predicate type required", ErrStatementInvalid)
	}
	if len(statement.Subject) != 1 {
		return fmt.Errorf("%w: exactly one patch subject is required", ErrStatementInvalid)
	}
	subject := statement.Subject[0]
	if subject.Name != PatchSubjectName || len(subject.Digest) != 1 {
		return fmt.Errorf("%w: subject must be the patch sha256 digest", ErrStatementInvalid)
	}
	patchHex, ok := subject.Digest["sha256"]
	if !ok || len(patchHex) != sha256.Size*2 || !isLowerHex(patchHex) {
		return fmt.Errorf("%w: subject must contain one lowercase sha256 digest", ErrStatementInvalid)
	}
	if err := validatePredicate(statement.Predicate); err != nil {
		return err
	}
	if patchHex != digestHex(statement.Predicate.PatchDigest) {
		return fmt.Errorf("%w: subject patch digest does not match predicate", ErrBindingMismatch)
	}
	if err := validateOfficialStatement(statement); err != nil {
		return err
	}
	return nil
}

func validatePredicate(predicate VerificationPredicate) error {
	if predicate.SchemaVersion != SchemaVersion || !validDigest(predicate.ReportDigest) || !validDigest(predicate.SignedReportDigest) || !validDigest(predicate.SpecDigest) || !validDigest(predicate.PatchDigest) || !validBaseSHA(predicate.BaseSHA) || !validIdentifier(predicate.RunUID, gate.MaxRunUIDBytes) || !validIdentifier(predicate.GateUID, gate.MaxRunUIDBytes) || predicate.GateGeneration <= 0 {
		return fmt.Errorf("%w: predicate identity is invalid", ErrStatementInvalid)
	}
	if len(predicate.EvidenceArtifactDigests) > MaxEvidenceArtifactDigests {
		return fmt.Errorf("%w: evidence artifact identities exceed bounds", ErrStatementInvalid)
	}
	for index, digest := range predicate.EvidenceArtifactDigests {
		if !validDigest(digest) || (index > 0 && predicate.EvidenceArtifactDigests[index-1] >= digest) {
			return fmt.Errorf("%w: evidence artifact identities are not canonical", ErrStatementInvalid)
		}
	}
	if len(predicate.Checks) == 0 || len(predicate.Checks) > gate.MaxChecks {
		return fmt.Errorf("%w: predicate checks are out of bounds", ErrStatementInvalid)
	}
	for index, check := range predicate.Checks {
		if !validIdentifier(check.Name, 128) || len(check.Message) > gate.MaxReportMessageBytes || !utf8.ValidString(check.Message) || strings.IndexByte(check.Message, 0) >= 0 || (check.Severity != SeverityBlocking && check.Severity != SeverityAdvisory) {
			return fmt.Errorf("%w: predicate check is invalid", ErrStatementInvalid)
		}
		if index > 0 && predicate.Checks[index-1].Name >= check.Name {
			return fmt.Errorf("%w: predicate checks are not canonical", ErrStatementInvalid)
		}
	}
	if !sortedHelperImages(predicate.HelperImageDigests) || !sortedStrings(predicate.SkillDigests) || !sortedStrings(predicate.Evidence.ChangedPaths) || !sortedCommands(predicate.Commands) {
		return fmt.Errorf("%w: predicate report collections are not canonical", ErrStatementInvalid)
	}
	checks := make([]v1alpha1.GateCheck, 0, len(predicate.Checks))
	for _, check := range predicate.Checks {
		checks = append(checks, v1alpha1.GateCheck{Name: check.Name, Passed: check.Passed, Message: check.Message})
	}
	report := gate.VerificationReport{
		SchemaVersion:       SchemaVersion,
		RunUID:              predicate.RunUID,
		SpecDigest:          predicate.SpecDigest,
		BaseSHA:             predicate.BaseSHA,
		PatchDigest:         predicate.PatchDigest,
		GateUID:             predicate.GateUID,
		GateGeneration:      predicate.GateGeneration,
		RuntimeImageDigest:  predicate.RuntimeImageDigest,
		VerifierImageDigest: predicate.VerifierImageDigest,
		HelperImageDigests:  append([]gate.ImageEvidence(nil), predicate.HelperImageDigests...),
		SkillDigests:        append([]string(nil), predicate.SkillDigests...),
		Evidence:            cloneEvidence(predicate.Evidence),
		Commands:            cloneCommands(predicate.Commands),
		Scoring:             cloneScoring(predicate.Scoring),
		Checks:              checks,
		Verdict:             predicate.Verdict,
	}
	canonical, err := gate.CanonicalReportBytes(report)
	if err != nil {
		return fmt.Errorf("%w: predicate is not a valid Gate report projection: %v", ErrStatementInvalid, err)
	}
	reportDigest, err := gate.ReportDigest(report)
	if err != nil || reportDigest != predicate.ReportDigest {
		return fmt.Errorf("%w: predicate report digest does not match its report projection", ErrBindingMismatch)
	}
	if len(canonical) > gate.MaxReportBytes {
		return fmt.Errorf("%w: projected Gate report exceeds bounds", ErrStatementInvalid)
	}
	return nil
}

func validateOfficialStatement(statement Statement) error {
	body, err := json.Marshal(statement)
	if err != nil {
		return fmt.Errorf("%w: marshal official statement: %v", ErrStatementInvalid, err)
	}
	official := &intotov1.Statement{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, official); err != nil {
		return fmt.Errorf("%w: official in-toto v1 validation: %v", ErrStatementInvalid, err)
	}
	if err := official.Validate(); err != nil {
		return fmt.Errorf("%w: official in-toto v1 validation: %v", ErrStatementInvalid, err)
	}
	return nil
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := walkJSON(decoder, 0); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting exceeds bounds")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		count := 0
		for decoder.More() {
			count++
			if count > maxJSONObjectFields {
				return errors.New("JSON object field count exceeds bounds")
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		count := 0
		for decoder.More() {
			count++
			if count > maxJSONArrayItems {
				return errors.New("JSON array item count exceeds bounds")
			}
			if err := walkJSON(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	}
	return nil
}

func sortedHelperImages(images []gate.ImageEvidence) bool {
	for index := 1; index < len(images); index++ {
		if images[index-1].Name >= images[index].Name {
			return false
		}
	}
	return true
}

func sortedCommands(commands []gate.ReportCommand) bool {
	for index := 1; index < len(commands); index++ {
		if commands[index-1].Index >= commands[index].Index {
			return false
		}
	}
	return true
}

func sortedStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func validIdentifier(value string, max int) bool {
	return len(value) > 0 && len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func validBaseSHA(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	return isLowerHex(value)
}

func validDigest(value string) bool {
	return len(value) == len("sha256:")+sha256.Size*2 && strings.HasPrefix(value, "sha256:") && isLowerHex(value[len("sha256:"):])
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func digestHex(value string) string {
	return strings.TrimPrefix(value, "sha256:")
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneReport(input gate.VerificationReport) gate.VerificationReport {
	output := input
	output.HelperImageDigests = append([]gate.ImageEvidence(nil), input.HelperImageDigests...)
	output.SkillDigests = append([]string(nil), input.SkillDigests...)
	output.Evidence = cloneEvidence(input.Evidence)
	output.Commands = cloneCommands(input.Commands)
	output.Scoring = cloneScoring(input.Scoring)
	output.Checks = append([]v1alpha1.GateCheck(nil), input.Checks...)
	return output
}

func cloneScoring(input gate.ScoringEvidence) gate.ScoringEvidence {
	output := input
	output.BlockingReasons = append([]string(nil), input.BlockingReasons...)
	if input.CriticRoute != nil {
		route := *input.CriticRoute
		output.CriticRoute = &route
	}
	return output
}

func cloneEvidence(input gate.EvidenceSummary) gate.EvidenceSummary {
	output := input
	output.ChangedPaths = append([]string(nil), input.ChangedPaths...)
	output.FilesChanged = cloneInt64(input.FilesChanged)
	output.LinesChanged = cloneInt64(input.LinesChanged)
	output.HasBinaryFiles = cloneBool(input.HasBinaryFiles)
	output.CoverageDelta = cloneString(input.CoverageDelta)
	output.NewTestsFailOnBase = cloneBool(input.NewTestsFailOnBase)
	return output
}

func cloneCommands(input []gate.ReportCommand) []gate.ReportCommand {
	output := append([]gate.ReportCommand(nil), input...)
	for index := range output {
		output[index].ExitCode = cloneInt32(output[index].ExitCode)
		output[index].DurationMillis = cloneInt64(output[index].DurationMillis)
	}
	return output
}

func cloneInt32(input *int32) *int32 {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneInt64(input *int64) *int64 {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneBool(input *bool) *bool {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneString(input *string) *string {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}
