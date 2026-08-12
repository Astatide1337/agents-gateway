package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCanonicalizeResolvedSpecSortsObjectKeysRecursively(t *testing.T) {
	first := []byte(`{"z":{"b":2,"a":1},"items":[{"y":true,"x":false}],"a":"first"}`)
	second := []byte(`{"a":"first","items":[{"x":false,"y":true}],"z":{"a":1,"b":2}}`)

	canonicalFirst, err := CanonicalizeResolvedSpec(first)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSecond, err := CanonicalizeResolvedSpec(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalFirst) != string(canonicalSecond) {
		t.Fatalf("canonical forms differ:\n%s\n%s", canonicalFirst, canonicalSecond)
	}
	if got, want := string(canonicalFirst), `{"a":"first","items":[{"x":false,"y":true}],"z":{"a":1,"b":2}}`; got != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
}

func TestResolvedSpecDigestIsStableAndUsesSHA256(t *testing.T) {
	spec := map[string]any{
		"apiVersion": "agents.astatide.com/v1alpha1",
		"spec": map[string]any{
			"image":  "ghcr.io/example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"limits": map[string]any{"maxToolCalls": 12},
		},
	}

	first, err := ResolvedSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolvedSpecDigest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest was not stable: %q != %q", first, second)
	}
	if !ValidDigest(first) {
		t.Fatalf("digest has invalid format: %q", first)
	}

	canonical, err := CanonicalizeResolvedSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	want := DigestPrefix + hex.EncodeToString(sum[:])
	if first != want {
		t.Fatalf("digest = %q, want SHA-256 of canonical bytes %q", first, want)
	}
}

func TestResolvedSpecDigestExcludesSecretAndCredentialMaterial(t *testing.T) {
	first := map[string]any{
		"name":           "run",
		"credential":     "first-credential-value",
		"credentials":    map[string]any{"username": "agent", "password": "first-password"},
		"apiKey":         "first-api-key",
		"secretData":     "first-secret-data",
		"credentialsRef": "github-app",
		"spec": map[string]any{
			"image": "agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	second := map[string]any{
		"name":           "run",
		"credential":     "second-credential-value",
		"credentials":    map[string]any{"username": "different", "password": "second-password"},
		"apiKey":         "second-api-key",
		"secretData":     "second-secret-data",
		"credentialsRef": "github-app",
		"spec": map[string]any{
			"image": "agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	firstDigest, err := ResolvedSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ResolvedSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("credential material changed digest: %s != %s", firstDigest, secondDigest)
	}
	canonical, err := CanonicalizeResolvedSpec(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"first-credential-value", "first-password", "first-api-key", "first-secret-data"} {
		if strings.Contains(string(canonical), forbidden) {
			t.Fatalf("canonical form contains excluded credential material %q: %s", forbidden, canonical)
		}
	}
	if !strings.Contains(string(canonical), `"credentialsRef":"github-app"`) {
		t.Fatalf("logical credential reference was unexpectedly removed: %s", canonical)
	}
}

func TestResolvedSpecDigestChangesForNonSecretInput(t *testing.T) {
	first := map[string]any{"spec": map[string]any{"baseSHA": "aaaaaaaa", "task": "repair"}}
	second := map[string]any{"spec": map[string]any{"baseSHA": "bbbbbbbb", "task": "repair"}}

	firstDigest, err := ResolvedSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ResolvedSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatalf("changed resolved input retained digest %q", firstDigest)
	}
}

func TestResolvedSpecDigestPreservesTokenBudgetsAndExactArgumentPolicy(t *testing.T) {
	first := map[string]any{"spec": map[string]any{
		"maxModelTokens": 1000,
		"exactArguments": map[string]any{"contactId": "required-policy-value"},
	}}
	second := map[string]any{"spec": map[string]any{
		"maxModelTokens": 2000,
		"exactArguments": map[string]any{"contactId": "different-policy-value"},
	}}
	firstCanonical, err := CanonicalizeResolvedSpec(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"maxModelTokens":`, `"contactId":"required-policy-value"`} {
		if !strings.Contains(string(firstCanonical), required) {
			t.Fatalf("security-relevant input %s was removed from %s", required, firstCanonical)
		}
	}
	firstDigest, err := ResolvedSpecDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ResolvedSpecDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("changed token budget and exact-argument policy retained the same digest")
	}
}

func TestCanonicalizeResolvedSpecRejectsInvalidOrDuplicateJSON(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"value":`),
		[]byte(`{"value":1,"value":2}`),
	} {
		if _, err := CanonicalizeResolvedSpec(raw); err == nil {
			t.Fatalf("invalid input was accepted: %s", raw)
		}
	}
}
