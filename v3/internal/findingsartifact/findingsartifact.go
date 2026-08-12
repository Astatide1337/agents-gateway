// Package findingsartifact owns the neutral, immutable findings publication
// envelope shared by verification and publication.
//
// The envelope deliberately carries both the critic's canonical input and the
// result derived from that input. The result is retained for auditability, but
// Verify never treats it as authority: it recomputes Corroborate(input) and
// rejects any mismatch before returning the derived value.
package findingsartifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	Version    = 1
	Kind       = "findings-publication"
	Name       = "findings-publication.json"
	MediaType  = "application/vnd.agents-gateway.findings-publication.v1alpha1+json"
	MaxBytes   = strictjson.MaxDocumentBytes
	maxRunUID  = 128
	maxVersion = Version
)

var (
	ErrInvalid          = errors.New("invalid findings publication artifact")
	ErrNonCanonical     = errors.New("findings publication artifact is not canonical")
	ErrIdentityMismatch = errors.New("findings publication artifact identity mismatch")
	ErrResultMismatch   = errors.New("findings publication result does not match its input")
)

// Artifact is the only publication envelope accepted by the findings
// publisher. GateReportDigest is populated only after the signed Gate report
// has been durably written.
type Artifact struct {
	Version          int                                      `json:"version"`
	RunUID           string                                   `json:"runUID"`
	SpecDigest       string                                   `json:"specDigest"`
	BaseSHA          string                                   `json:"baseSHA"`
	PatchDigest      string                                   `json:"patchDigest"`
	GateReportDigest string                                   `json:"gateReportDigest"`
	Input            findingcorroboration.CorroborationInput  `json:"input"`
	Result           findingcorroboration.CorroborationResult `json:"result"`
}

type wireArtifact struct {
	Version          int             `json:"version"`
	RunUID           string          `json:"runUID"`
	SpecDigest       string          `json:"specDigest"`
	BaseSHA          string          `json:"baseSHA"`
	PatchDigest      string          `json:"patchDigest"`
	GateReportDigest string          `json:"gateReportDigest"`
	Input            json.RawMessage `json:"input"`
	Result           json.RawMessage `json:"result"`
}

// CanonicalBytes returns the only accepted encoding for an artifact. It does
// not infer or rewrite the supplied result; callers that need a trusted result
// must call Verify.
func CanonicalBytes(artifact Artifact) ([]byte, error) {
	if err := ValidateIdentity(artifact); err != nil {
		return nil, err
	}
	inputBytes, err := findingcorroboration.CanonicalInputBytes(artifact.Input)
	if err != nil {
		return nil, fmt.Errorf("canonicalize findings input: %w", err)
	}
	resultBytes, err := findingcorroboration.CanonicalResultBytes(artifact.Result)
	if err != nil {
		return nil, fmt.Errorf("canonicalize findings result: %w", err)
	}
	encoded, err := json.Marshal(wireArtifact{
		Version: artifact.Version, RunUID: artifact.RunUID, SpecDigest: artifact.SpecDigest,
		BaseSHA: artifact.BaseSHA, PatchDigest: artifact.PatchDigest,
		GateReportDigest: artifact.GateReportDigest, Input: inputBytes, Result: resultBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal findings publication artifact: %w", err)
	}
	if len(encoded) > MaxBytes {
		return nil, fmt.Errorf("%w: encoded size exceeds %d bytes", ErrInvalid, MaxBytes)
	}
	return encoded, nil
}

// ParseCanonical strictly decodes an artifact and requires its bytes to be
// the canonical encoding. Nested input and result documents are independently
// strict and canonical as well.
func ParseCanonical(body []byte) (Artifact, error) {
	var zero Artifact
	if len(body) == 0 || len(body) > MaxBytes || strictjson.ValidateObject(body) != nil {
		return zero, fmt.Errorf("%w: bounded JSON object required", ErrInvalid)
	}
	if err := rejectDuplicateKeys(body); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var wire wireArtifact
	if err := decoder.Decode(&wire); err != nil {
		return zero, fmt.Errorf("%w: decode: %v", ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return zero, fmt.Errorf("%w: trailing JSON", ErrNonCanonical)
	}
	input, err := findingcorroboration.ParseCanonicalInput(wire.Input)
	if err != nil {
		return zero, fmt.Errorf("%w: input: %v", ErrInvalid, err)
	}
	result, err := parseCanonicalResult(wire.Result)
	if err != nil {
		return zero, fmt.Errorf("%w: result: %v", ErrInvalid, err)
	}
	artifact := Artifact{
		Version: wire.Version, RunUID: wire.RunUID, SpecDigest: wire.SpecDigest,
		BaseSHA: wire.BaseSHA, PatchDigest: wire.PatchDigest,
		GateReportDigest: wire.GateReportDigest, Input: input, Result: result,
	}
	canonicalBody, err := CanonicalBytes(artifact)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBody, body) {
		return zero, ErrNonCanonical
	}
	return artifact, nil
}

// Verify parses and validates an artifact, then recomputes the result from its
// input. The returned result is always the locally derived value, never the
// result supplied by the artifact.
func Verify(body []byte) (Artifact, findingcorroboration.CorroborationResult, error) {
	artifact, err := ParseCanonical(body)
	if err != nil {
		return Artifact{}, findingcorroboration.CorroborationResult{}, err
	}
	derived, err := findingcorroboration.Corroborate(artifact.Input)
	if err != nil {
		return Artifact{}, findingcorroboration.CorroborationResult{}, fmt.Errorf("%w: derive result: %v", ErrInvalid, err)
	}
	want, err := findingcorroboration.CanonicalResultBytes(derived)
	if err != nil {
		return Artifact{}, findingcorroboration.CorroborationResult{}, fmt.Errorf("%w: canonicalize derived result: %v", ErrInvalid, err)
	}
	got, err := findingcorroboration.CanonicalResultBytes(artifact.Result)
	if err != nil {
		return Artifact{}, findingcorroboration.CorroborationResult{}, fmt.Errorf("%w: canonicalize supplied result: %v", ErrInvalid, err)
	}
	if !bytes.Equal(want, got) {
		return Artifact{}, findingcorroboration.CorroborationResult{}, ErrResultMismatch
	}
	return artifact, derived, nil
}

// ValidateIdentity validates only the outer binding fields. It intentionally
// does not validate the nested decision result as authority.
func ValidateIdentity(artifact Artifact) error {
	if artifact.Version != maxVersion || !validRunUID(artifact.RunUID) ||
		!canonical.ValidDigest(artifact.SpecDigest) || !resolved.ValidBaseSHA(artifact.BaseSHA) ||
		!canonical.ValidDigest(artifact.PatchDigest) || !canonical.ValidDigest(artifact.GateReportDigest) {
		return fmt.Errorf("%w: invalid run/spec/base/patch/Gate-report identity", ErrInvalid)
	}
	return nil
}

func parseCanonicalResult(body []byte) (findingcorroboration.CorroborationResult, error) {
	var zero findingcorroboration.CorroborationResult
	if len(body) == 0 || len(body) > findingcorroboration.MaxResultBytes || strictjson.ValidateObject(body) != nil {
		return zero, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result findingcorroboration.CorroborationResult
	if err := decoder.Decode(&result); err != nil {
		return zero, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return zero, ErrNonCanonical
	}
	canonicalBody, err := findingcorroboration.CanonicalResultBytes(result)
	if err != nil {
		return zero, err
	}
	if !bytes.Equal(canonicalBody, body) {
		return zero, ErrNonCanonical
	}
	return result, nil
}

func rejectDuplicateKeys(body []byte) error {
	// strictjson recursively rejects duplicate keys. Keep this helper local so
	// the outer parser's contract remains explicit even if its implementation
	// changes in the future.
	if err := strictjson.ValidateObject(body); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func validRunUID(value string) bool {
	return len(value) > 0 && len(value) <= maxRunUID && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "\x00\r\n\t /")
}
