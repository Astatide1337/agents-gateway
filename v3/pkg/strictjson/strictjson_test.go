package strictjson

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidateRejectsAmbiguousStringsAndStructure(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "recursive duplicate", raw: []byte(`{"outer":{"value":1,"value":2}}`)},
		{name: "duplicate in array", raw: []byte(`{"items":[{"value":1,"value":2}]}`)},
		{name: "trailing value", raw: []byte(`{"value":1} null`)},
		{name: "escaped NUL", raw: []byte(`{"value":"\u0000"}`)},
		{name: "lone high surrogate", raw: []byte(`{"value":"\ud800"}`)},
		{name: "lone low surrogate", raw: []byte(`{"value":"\udc00"}`)},
		{name: "invalid escape", raw: []byte(`{"value":"\u12zz"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.raw)
			if err == nil {
				t.Fatal("ambiguous JSON was accepted")
			}
			if strings.Contains(err.Error(), "value") || strings.Contains(err.Error(), "d800") || strings.Contains(err.Error(), "0000") {
				t.Fatalf("parser error echoed caller-controlled data: %v", err)
			}
		})
	}
	invalidUTF8 := []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
	if err := Validate(invalidUTF8); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestValidateAcceptsLiteralReplacementAndSurrogatePair(t *testing.T) {
	if err := Validate([]byte(`{"value":"�"}`)); err != nil {
		t.Fatalf("literal replacement character rejected: %v", err)
	}
	if !Equal([]byte(`{"value":"\ud83d\ude00"}`), []byte(`{"value":"😀"}`)) {
		t.Fatal("valid surrogate pair did not equal literal rune")
	}
}

func TestObjectEqualityIsStructuralButKeepsArrays(t *testing.T) {
	if !EqualObjects([]byte(`{"a":{"n":1.0},"items":[1,2]}`), []byte(`{"items":[1.00,2e0],"a":{"n":1}}`)) {
		t.Fatal("structural object equality failed")
	}
	if Equal([]byte(`[1,2]`), []byte(`[2,1]`)) {
		t.Fatal("array order was ignored")
	}
	if Equal([]byte(`{"value":9007199254740993}`), []byte(`{"value":9007199254740992}`)) {
		t.Fatal("large integer distinction was lost")
	}
	if !Equal([]byte(`{"value":-0}`), []byte(`{"value":0.0e12}`)) {
		t.Fatal("signed zero was not mathematical zero")
	}
}

func TestNumberBudgetsHaveExplicitBoundaries(t *testing.T) {
	withinInteger := append(append([]byte(`{"value":`), bytes.Repeat([]byte{'1'}, MaxMantissaDigits)...), []byte(`e4096}`)...)
	outsideExponent := []byte(`{"value":1e4097}`)
	withinFraction := []byte(`{"value":1e-4096}`)
	outsideFraction := []byte(`{"value":1e-4097}`)
	if err := Validate(withinInteger); err != nil {
		t.Fatalf("integer boundary rejected: %v", err)
	}
	if err := Validate(withinFraction); err != nil {
		t.Fatalf("fraction boundary rejected: %v", err)
	}
	for _, raw := range [][]byte{outsideExponent, outsideFraction} {
		if err := Validate(raw); err == nil {
			t.Fatal("out-of-budget number accepted")
		}
	}
	mantissa := bytes.Repeat([]byte{'1'}, MaxMantissaDigits+1)
	if err := Validate(append(append([]byte(`{"value":`), mantissa...), '}')); err == nil {
		t.Fatal("mantissa beyond budget accepted")
	}
	exponentFlood := []byte(`{"value":1e0000000}`)
	if err := Validate(exponentFlood); err == nil {
		t.Fatal("exponent digit flood accepted")
	}
	lexeme := append([]byte(`{"value":1`), bytes.Repeat([]byte{'0'}, MaxNumberLexemeBytes)...)
	lexeme = append(lexeme, '}')
	if err := Validate(lexeme); err == nil {
		t.Fatal("number lexeme beyond budget accepted")
	}
}

func TestNormalizeKeepsNullsArrayOrderAndTypedIntegerCompatibility(t *testing.T) {
	normalized, err := Normalize([]byte(`{"z":2.0,"args":{"tags":["b","a"],"value":1e-4096,"empty":null},"a":1e3}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(normalized)
	if !strings.Contains(got, `"empty":null`) || !strings.Contains(got, `"tags":["b","a"]`) || !strings.Contains(got, `1e-4096`) {
		t.Fatalf("normalization changed opaque meaning: %s", got)
	}
	if !strings.HasPrefix(got, `{"a":1000,"args":`) {
		t.Fatalf("object keys were not canonicalized: %s", got)
	}
}

func TestObjectConstraintRejectsNonObjects(t *testing.T) {
	if err := ValidateObject([]byte(`[]`)); err == nil {
		t.Fatal("array accepted as object constraint")
	}
	if err := ValidateObject([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("object rejected: %v", err)
	}
}
