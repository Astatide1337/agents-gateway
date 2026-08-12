package canonical

import "testing"

func TestDigestIsStableAcrossObjectKeyOrder(t *testing.T) {
	left, err := Digest(map[string]any{"z": 2, "a": []any{true, "x"}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Digest(map[string]any{"a": []any{true, "x"}, "z": 2})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("digests differ: %s != %s", left, right)
	}

	encoded, err := CanonicalizeResolvedSpec(map[string]any{"z": 2, "a": []any{true, "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"a":[true,"x"],"z":2}`; got != want {
		t.Fatalf("canonical JSON = %s, want %s", got, want)
	}
}

func TestCanonicalizerRejectsTrailingValues(t *testing.T) {
	if _, err := CanonicalizeResolvedSpec([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("expected trailing JSON to be rejected")
	}
}
