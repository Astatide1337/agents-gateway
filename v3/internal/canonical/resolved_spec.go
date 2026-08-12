// Package canonical contains deterministic representations of resolved v3
// execution inputs.
//
// A resolved specification is content addressed before any execution child is
// created. The representation deliberately excludes credential material. A
// logical secret/credential reference is retained because changing which
// reference is used changes the resolved execution contract; the value behind
// that reference is resolved at runtime and is never part of the digest.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// DigestPrefix is the digest format used by v3 content references.
	DigestPrefix = "sha256:"
	// DigestAlgorithm names the hash used for resolved specifications.
	DigestAlgorithm = "sha256"
)

var (
	ErrNilSpec       = errors.New("resolved spec is nil")
	ErrInvalidSpec   = errors.New("resolved spec is not valid JSON")
	ErrUnsupported   = errors.New("resolved spec cannot be encoded as JSON")
	ErrInvalidDigest = errors.New("resolved spec digest is invalid")
)

// CanonicalizeResolvedSpec returns the exact UTF-8 JSON bytes hashed by
// ResolvedSpecDigest. Object keys are sorted recursively, JSON numbers are
// normalized by the shared strict contract, and secret/credential material is
// omitted. Array order is preserved because arrays are ordered in the v3 API.
//
// spec may be a JSON-compatible Go value, a json.RawMessage, or a []byte
// containing one JSON document. A string is treated as a JSON document when it
// is valid JSON; otherwise it is encoded as an ordinary JSON string.
func CanonicalizeResolvedSpec(spec any) ([]byte, error) {
	raw, err := resolvedSpecJSON(spec)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, ErrInvalidSpec
	}
	if err := strictjson.Validate(raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}

	normalized, err := strictjson.Normalize(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize resolved spec: %w", err)
	}

	value, err := decodeJSON(normalized)
	if err != nil {
		return nil, fmt.Errorf("decode normalized resolved spec: %w", err)
	}
	filtered := excludeCredentialMaterial(value, "")
	canonical, err := marshalCanonical(filtered)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical resolved spec: %w", err)
	}
	if err := strictjson.Validate(canonical); err != nil {
		return nil, fmt.Errorf("canonical resolved spec violates strict contract: %w", err)
	}
	return canonical, nil
}

// CanonicalResolvedSpec is a concise alias for CanonicalizeResolvedSpec.
func CanonicalResolvedSpec(spec any) ([]byte, error) {
	return CanonicalizeResolvedSpec(spec)
}

// ResolvedSpecDigest computes a stable, content-addressed digest for a
// resolved specification. The returned value is always sha256:<lowercase hex>.
func ResolvedSpecDigest(spec any) (string, error) {
	canonical, err := CanonicalizeResolvedSpec(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// ComputeResolvedSpecDigest is an integration-friendly alias for
// ResolvedSpecDigest.
func ComputeResolvedSpecDigest(spec any) (string, error) {
	return ResolvedSpecDigest(spec)
}

// DigestResolvedSpec is an integration-friendly alias for ResolvedSpecDigest.
func DigestResolvedSpec(spec any) (string, error) {
	return ResolvedSpecDigest(spec)
}

// Digest computes the resolved-spec digest for a JSON-compatible value.
func Digest(spec any) (string, error) {
	return ResolvedSpecDigest(spec)
}

// ValidDigest reports whether value has the v3 lowercase SHA-256 format.
func ValidDigest(value string) bool {
	if len(value) != len(DigestPrefix)+sha256.Size*2 || !strings.HasPrefix(value, DigestPrefix) {
		return false
	}
	for _, r := range value[len(DigestPrefix):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func resolvedSpecJSON(spec any) ([]byte, error) {
	if spec == nil {
		return nil, ErrNilSpec
	}
	switch value := spec.(type) {
	case json.RawMessage:
		return append([]byte(nil), value...), nil
	case []byte:
		return append([]byte(nil), value...), nil
	case string:
		trimmed := bytes.TrimSpace([]byte(value))
		if len(trimmed) > 0 && strictjson.Validate(trimmed) == nil {
			return trimmed, nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return encoded, nil
	default:
		encoded, err := json.Marshal(spec)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return encoded, nil
	}
}

func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("resolved spec contains multiple JSON values")
	}
	return value, nil
}

// excludeCredentialMaterial recursively removes fields whose names identify
// secret material. Reference fields are intentionally retained: names such as
// credentialRef and secretKeyRef identify a logical input but do not contain
// its value. The caller still must ensure those references point to external
// storage; this helper does not make arbitrary strings safe by itself.
func excludeCredentialMaterial(value any, parentKey string) any {
	// ToolSet exact-argument policy is itself part of the security boundary.
	// Argument names such as "token" may be constraints rather than embedded
	// credentials; dropping them would weaken policy while retaining a digest.
	if normalizeIdentifier(parentKey) == "exactarguments" {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		name, hasName := typed["name"].(string)
		sensitiveNamedValue := hasName && sensitiveIdentifier(name)
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if excludedField(key) {
				continue
			}
			// A reference object may contain a safe name/key/namespace as well
			// as an accidentally inlined value. Keep the identity fields but
			// remove value-bearing members even when their names are generic.
			if referenceField(normalizeIdentifier(parentKey)) && (key == "value" || key == "data" || key == "stringData") {
				continue
			}
			// Kubernetes-style environment entries commonly carry a name and a
			// value. Do not let a value for a plainly sensitive environment name
			// enter the resolved snapshot.
			if sensitiveNamedValue && (key == "value" || key == "data") {
				continue
			}
			out[key] = excludeCredentialMaterial(child, key)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = excludeCredentialMaterial(child, parentKey)
		}
		return out
	default:
		return value
	}
}

func excludedField(key string) bool {
	normalized := normalizeIdentifier(key)
	if normalized == "" || referenceField(normalized) {
		return false
	}
	if strings.Contains(normalized, "credential") || strings.HasPrefix(normalized, "secret") {
		return true
	}
	for _, term := range []string{
		"password",
		"passphrase",
		"token",
		"apikey",
		"accesstoken",
		"refreshtoken",
		"accesskey",
		"secretkey",
		"privatekey",
		"authorization",
		"bearer",
		"cookie",
	} {
		if normalized == term || strings.HasSuffix(normalized, term) {
			return true
		}
	}
	return false
}

func referenceField(normalized string) bool {
	return strings.HasSuffix(normalized, "ref") || strings.HasSuffix(normalized, "reference") || normalized == "valuefrom"
}

func sensitiveIdentifier(value string) bool {
	normalized := normalizeIdentifier(value)
	if normalized == "" {
		return false
	}
	for _, term := range []string{"secret", "credential", "password", "passphrase", "token", "apikey", "accesskey", "privatekey"} {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return false
}

func normalizeIdentifier(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func marshalCanonical(value any) ([]byte, error) {
	var output bytes.Buffer
	if err := writeCanonical(&output, value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		output.Write(encoded)
	case json.Number:
		if err := strictjson.Validate([]byte(typed.String())); err != nil {
			return fmt.Errorf("invalid JSON number: %w", err)
		}
		output.WriteString(typed.String())
	case []any:
		output.WriteByte('[')
		for index, child := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonical(output, child); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			encodedKey, err := json.Marshal(key)
			if err != nil {
				return err
			}
			output.Write(encodedKey)
			output.WriteByte(':')
			if err := writeCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported normalized JSON value %T", value)
	}
	return nil
}
