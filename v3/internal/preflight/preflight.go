// Package preflight defines the admission-time contract for the node and
// runtime security probe.
//
// The attestation is deliberately stored as data in a ConfigMap. A Lease is
// useful for coordination, but it is not an appropriate place for an
// application-defined status condition. Admission consumes only a parsed,
// versioned result and fails closed for every invalid state.
package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// ConfigMapNamespace and ConfigMapName are the sole location used by the
	// admission path. In particular, no Lease conditions are consulted.
	ConfigMapNamespace = "agw-system"
	ConfigMapName      = "agw-preflight"
	// Namespace is kept as a concise alias for callers wiring the fixed
	// preflight location into Kubernetes manager configuration.
	Namespace = ConfigMapNamespace
	// Name is the matching concise alias for the fixed ConfigMap name.
	Name = ConfigMapName

	// ResultDataKey is the ConfigMap data entry containing the JSON result.
	ResultDataKey = "result.json"

	// CurrentSchemaVersion is the only result schema accepted by this module.
	CurrentSchemaVersion = 1

	// MaxResultBytes prevents an untrusted ConfigMap value from becoming an
	// unbounded admission error or parser allocation.
	MaxResultBytes = 16 << 10

	maxTimestampBytes   = 64
	maxFingerprintBytes = 512
)

var (
	ErrMalformed = errors.New("preflight result is malformed")
)

// Reason is a stable, bounded reason for a preflight decision. The strings
// are safe to expose in an admission response and do not include evidence
// contents or Kubernetes API error text.
type Reason string

const (
	ReasonAllowed             Reason = "allowed"
	ReasonMissing             Reason = "missing"
	ReasonMalformed           Reason = "malformed"
	ReasonFailed              Reason = "failed"
	ReasonStale               Reason = "stale"
	ReasonFuture              Reason = "future_timestamp"
	ReasonFingerprintMismatch Reason = "fingerprint_mismatch"
	ReasonInvalidPolicy       Reason = "invalid_policy"
	ReasonUnavailable         Reason = "unavailable"
)

// Decision is the pure result of evaluating one preflight attestation.
// Allowed is the only positive outcome; callers must not infer admission from
// an empty or unknown Reason.
type Decision struct {
	Allowed bool
	Reason  Reason
}

// Fingerprint is the pair of identities that the probe attests. Both values
// must be supplied by the caller and must match the result exactly.
type Fingerprint struct {
	Node    string
	Runtime string
}

// Result is the versioned JSON stored in ConfigMap.Data[ResultDataKey].
// Timestamp is intentionally a time.Time in the Go contract, but parsing is
// strict and requires the JSON value to be an RFC3339 timestamp.
type Result struct {
	SchemaVersion      int       `json:"schema_version"`
	Passed             bool      `json:"passed"`
	Timestamp          time.Time `json:"timestamp"`
	NodeFingerprint    string    `json:"node_fingerprint"`
	RuntimeFingerprint string    `json:"runtime_fingerprint"`
}

// ConfigMapKey returns the only ConfigMap identity accepted by the read
// path. Returning a value rather than a mutable package variable prevents a
// caller from accidentally changing the admission target.
func ConfigMapKey() types.NamespacedName {
	return types.NamespacedName{Namespace: ConfigMapNamespace, Name: ConfigMapName}
}

// ConfigMapKeyFor returns the configured system-namespace location. Empty
// preserves the safe default for backwards-compatible callers.
func ConfigMapKeyFor(namespace string) (types.NamespacedName, error) {
	if namespace == "" {
		namespace = ConfigMapNamespace
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return types.NamespacedName{}, ErrMalformed
	}
	return types.NamespacedName{Namespace: namespace, Name: ConfigMapName}, nil
}

// ParseResult parses one complete, bounded, strict JSON result. Unknown and
// duplicate fields are rejected so a producer cannot smuggle a second
// interpretation of an attestation through the ConfigMap.
func ParseResult(raw []byte) (Result, error) {
	var result Result
	if len(bytes.TrimSpace(raw)) == 0 || len(raw) > MaxResultBytes {
		return result, ErrMalformed
	}
	if err := strictjson.ValidateObject(raw); err != nil {
		return result, ErrMalformed
	}

	// Pointers distinguish a required field that is absent from its zero value.
	var wire struct {
		SchemaVersion      *int    `json:"schema_version"`
		Passed             *bool   `json:"passed"`
		Timestamp          *string `json:"timestamp"`
		NodeFingerprint    *string `json:"node_fingerprint"`
		RuntimeFingerprint *string `json:"runtime_fingerprint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return result, ErrMalformed
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return result, ErrMalformed
	}
	if wire.SchemaVersion == nil || wire.Passed == nil || wire.Timestamp == nil ||
		wire.NodeFingerprint == nil || wire.RuntimeFingerprint == nil {
		return result, ErrMalformed
	}
	if *wire.SchemaVersion != CurrentSchemaVersion {
		return result, ErrMalformed
	}
	if len(*wire.Timestamp) == 0 || len(*wire.Timestamp) > maxTimestampBytes {
		return result, ErrMalformed
	}
	timestamp, err := time.Parse(time.RFC3339Nano, *wire.Timestamp)
	if err != nil {
		return result, ErrMalformed
	}
	if !validFingerprint(*wire.NodeFingerprint) || !validFingerprint(*wire.RuntimeFingerprint) {
		return result, ErrMalformed
	}

	result = Result{
		SchemaVersion:      *wire.SchemaVersion,
		Passed:             *wire.Passed,
		Timestamp:          timestamp,
		NodeFingerprint:    *wire.NodeFingerprint,
		RuntimeFingerprint: *wire.RuntimeFingerprint,
	}
	if err := validateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// Evaluate is the pure fail-closed preflight decision function. A timestamp
// in the future is rejected rather than treated as fresh; that avoids a
// producer extending the usable lifetime by writing a future clock value.
func Evaluate(result Result, now time.Time, ttl time.Duration, expected Fingerprint) Decision {
	if ttl <= 0 || now.IsZero() || !validFingerprint(expected.Node) || !validFingerprint(expected.Runtime) {
		return Decision{Reason: ReasonInvalidPolicy}
	}
	if err := validateResult(result); err != nil {
		return Decision{Reason: ReasonMalformed}
	}
	if !result.Passed {
		return Decision{Reason: ReasonFailed}
	}
	if result.Timestamp.After(now) {
		return Decision{Reason: ReasonFuture}
	}
	if now.Sub(result.Timestamp) > ttl {
		return Decision{Reason: ReasonStale}
	}
	if result.NodeFingerprint != expected.Node || result.RuntimeFingerprint != expected.Runtime {
		return Decision{Reason: ReasonFingerprintMismatch}
	}
	return Decision{Allowed: true, Reason: ReasonAllowed}
}

// Fresh reports the timestamp portion of the contract without considering
// the attested result fields. It is useful to callers that need the same
// boundary semantics in diagnostics, while Evaluate remains authoritative.
func Fresh(timestamp, now time.Time, ttl time.Duration) bool {
	return ttl > 0 && !now.IsZero() && !timestamp.IsZero() &&
		!timestamp.After(now) && now.Sub(timestamp) <= ttl
}

// FingerprintMatches is the pure identity portion of the contract.
func FingerprintMatches(result Result, expected Fingerprint) bool {
	return validFingerprint(expected.Node) && validFingerprint(expected.Runtime) &&
		result.NodeFingerprint == expected.Node && result.RuntimeFingerprint == expected.Runtime
}

// EvaluateConfigMap extracts and evaluates the result from a ConfigMap. It
// performs no API calls and is therefore suitable for unit tests and webhook
// adapters. A missing key is distinct from malformed JSON, but both deny.
func EvaluateConfigMap(configMap *corev1.ConfigMap, now time.Time, ttl time.Duration, expected Fingerprint) Decision {
	if configMap == nil || configMap.Data == nil {
		return Decision{Reason: ReasonMissing}
	}
	raw, ok := configMap.Data[ResultDataKey]
	if !ok {
		return Decision{Reason: ReasonMissing}
	}
	result, err := ParseResult([]byte(raw))
	if err != nil {
		return Decision{Reason: ReasonMalformed}
	}
	return Evaluate(result, now, ttl, expected)
}

// CheckConfigMap reads agw-system/agw-preflight through the current
// controller-runtime client API and then delegates to the pure decision
// function. Missing objects and all read failures deny admission.
func CheckConfigMap(ctx context.Context, reader client.Reader, now time.Time, ttl time.Duration, expected Fingerprint) Decision {
	return CheckConfigMapInNamespace(ctx, reader, ConfigMapNamespace, now, ttl, expected)
}

// CheckConfigMapInNamespace reads the one named preflight ConfigMap from the
// configured system namespace. It never lists ConfigMaps.
func CheckConfigMapInNamespace(ctx context.Context, reader client.Reader, namespace string, now time.Time, ttl time.Duration, expected Fingerprint) Decision {
	if reader == nil {
		return Decision{Reason: ReasonUnavailable}
	}
	key, err := ConfigMapKeyFor(namespace)
	if err != nil {
		return Decision{Reason: ReasonMalformed}
	}
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(ctx, client.ObjectKey(key), configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return Decision{Reason: ReasonMissing}
		}
		return Decision{Reason: ReasonUnavailable}
	}
	return EvaluateConfigMap(configMap, now, ttl, expected)
}

func validateResult(result Result) error {
	if result.SchemaVersion != CurrentSchemaVersion || result.Timestamp.IsZero() ||
		!validFingerprint(result.NodeFingerprint) || !validFingerprint(result.RuntimeFingerprint) {
		return ErrMalformed
	}
	return nil
}

func validFingerprint(value string) bool {
	if len(value) == 0 || len(value) > maxFingerprintBytes || strings.TrimSpace(value) != value {
		return false
	}
	for _, runeValue := range value {
		if unicode.IsControl(runeValue) || unicode.IsSpace(runeValue) {
			return false
		}
	}
	return true
}
