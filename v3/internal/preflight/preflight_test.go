package preflight

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func validResult(at time.Time) Result {
	return Result{
		SchemaVersion:      CurrentSchemaVersion,
		Passed:             true,
		Timestamp:          at,
		NodeFingerprint:    "node-sha256:abc",
		RuntimeFingerprint: "runtime-sha256:def",
	}
}

func expectedFingerprint() Fingerprint {
	return Fingerprint{Node: "node-sha256:abc", Runtime: "runtime-sha256:def"}
}

func encodeResult(t *testing.T, result Result) string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEvaluateFreshnessFailsClosed(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	ttl := 5 * time.Minute
	result := validResult(now.Add(-ttl))

	if decision := Evaluate(result, now, ttl, expectedFingerprint()); !decision.Allowed {
		t.Fatalf("fresh evidence at the TTL boundary was rejected: %#v", decision)
	}

	result.Timestamp = now.Add(-(ttl + time.Nanosecond))
	if decision := Evaluate(result, now, ttl, expectedFingerprint()); decision.Allowed || decision.Reason != ReasonStale {
		t.Fatalf("stale evidence was not rejected as stale: %#v", decision)
	}

	result.Timestamp = now.Add(time.Nanosecond)
	if decision := Evaluate(result, now, ttl, expectedFingerprint()); decision.Allowed || decision.Reason != ReasonFuture {
		t.Fatalf("future evidence was not rejected: %#v", decision)
	}

	result = validResult(now.Add(-time.Minute))
	result.Passed = false
	if decision := Evaluate(result, now, ttl, expectedFingerprint()); decision.Allowed || decision.Reason != ReasonFailed {
		t.Fatalf("failed evidence was not rejected: %#v", decision)
	}

	if Fresh(now.Add(-ttl), now, ttl) != true || Fresh(now.Add(-(ttl+time.Nanosecond)), now, ttl) {
		t.Fatal("Fresh does not match the inclusive TTL boundary")
	}
}

func TestEvaluateFingerprintRequiresBothIdentities(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	result := validResult(now)

	for _, expected := range []Fingerprint{
		{Node: "other-node", Runtime: result.RuntimeFingerprint},
		{Node: result.NodeFingerprint, Runtime: "other-runtime"},
		{},
	} {
		decision := Evaluate(result, now, time.Minute, expected)
		if decision.Allowed || decision.Reason != ReasonFingerprintMismatch && decision.Reason != ReasonInvalidPolicy {
			t.Fatalf("fingerprint mismatch was accepted: expected=%#v decision=%#v", expected, decision)
		}
	}
	if !FingerprintMatches(result, expectedFingerprint()) {
		t.Fatal("matching node/runtime fingerprints were not recognized")
	}
}

func TestParseResultRejectsMalformedAndUnknownEvidence(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	valid := encodeResult(t, validResult(now))
	for _, raw := range []string{
		"",
		"{",
		"null",
		`{"schema_version":2,"passed":true,"timestamp":"2026-08-11T12:00:00Z","node_fingerprint":"n","runtime_fingerprint":"r"}`,
		`{"schema_version":1,"passed":true,"timestamp":"not-a-time","node_fingerprint":"n","runtime_fingerprint":"r"}`,
		`{"schema_version":1,"passed":true,"timestamp":"2026-08-11T12:00:00Z","node_fingerprint":"n","runtime_fingerprint":"r","extra":true}`,
		`{"schema_version":1,"passed":true,"timestamp":"2026-08-11T12:00:00Z","node_fingerprint":"n","node_fingerprint":"other","runtime_fingerprint":"r"}`,
	} {
		if _, err := ParseResult([]byte(raw)); err == nil {
			t.Fatalf("malformed evidence was accepted: %q", raw)
		}
	}
	if _, err := ParseResult([]byte(valid)); err != nil {
		t.Fatalf("valid evidence was rejected: %v", err)
	}
	if _, err := ParseResult([]byte(strings.Repeat("x", MaxResultBytes+1))); err == nil {
		t.Fatal("oversized evidence was accepted")
	}
}

func TestEvaluateConfigMapRejectsMissingAndMalformedEvidence(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	expected := expectedFingerprint()

	for _, configMap := range []*corev1.ConfigMap{
		nil,
		{},
		{Data: map[string]string{}},
		{Data: map[string]string{"other": "{}"}},
	} {
		decision := EvaluateConfigMap(configMap, now, time.Minute, expected)
		if decision.Allowed || decision.Reason != ReasonMissing {
			t.Fatalf("missing ConfigMap evidence was not rejected as missing: %#v", decision)
		}
	}

	decision := EvaluateConfigMap(&corev1.ConfigMap{Data: map[string]string{ResultDataKey: "{"}}, now, time.Minute, expected)
	if decision.Allowed || decision.Reason != ReasonMalformed {
		t.Fatalf("malformed ConfigMap evidence was not rejected: %#v", decision)
	}

	decision = EvaluateConfigMap(&corev1.ConfigMap{Data: map[string]string{ResultDataKey: encodeResult(t, validResult(now))}}, now, time.Minute, expected)
	if !decision.Allowed || decision.Reason != ReasonAllowed {
		t.Fatalf("valid ConfigMap evidence was not accepted: %#v", decision)
	}
}

func TestCheckConfigMapUsesConfiguredNamespace(t *testing.T) {
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "custom-system"},
		Data:       map[string]string{ResultDataKey: encodeResult(t, validResult(now))},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	decision := CheckConfigMapInNamespace(context.Background(), reader, "custom-system", now, time.Minute, expectedFingerprint())
	if !decision.Allowed {
		t.Fatalf("configured preflight namespace was ignored: %#v", decision)
	}
	if decision := CheckConfigMapInNamespace(context.Background(), reader, "INVALID_NAMESPACE", now, time.Minute, expectedFingerprint()); decision.Allowed || decision.Reason != ReasonMalformed {
		t.Fatalf("invalid namespace did not fail closed: %#v", decision)
	}
}
