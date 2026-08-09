package store

import "testing"

func TestJSONDocumentsEqualUsesStrictSemanticContract(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{name: "object key order", left: `{"a":1,"nested":{"x":true,"y":[1,2]}}`, right: `{"nested":{"y":[1,2],"x":true},"a":1}`, want: true},
		{name: "mathematically equal numbers", left: `{"value":2}`, right: `{"value":2.0}`, want: true},
		{name: "equivalent exponent numbers", left: `{"value":1e3}`, right: `{"value":1000}`, want: true},
		{name: "arbitrary precision remains distinct", left: `{"value":9007199254740993}`, right: `{"value":9007199254740992}`, want: false},
		{name: "array order remains significant", left: `[1,2]`, right: `[2,1]`, want: false},
		{name: "duplicate keys fail closed", left: `{"value":1,"value":1}`, right: `{"value":1}`, want: false},
		{name: "trailing value fails closed", left: `{"value":1}{"value":1}`, right: `{"value":1}`, want: false},
		{name: "invalid JSON fails closed", left: `{"value":`, right: `{"value":1}`, want: false},
		{name: "lone high surrogate fails closed", left: `{"x":"\ud800"}`, right: `{"x":"�"}`, want: false},
		{name: "lone low surrogate fails closed", left: `{"x":"\udc00"}`, right: `{"x":"�"}`, want: false},
		{name: "nested lone surrogate fails closed", left: `{"items":[{"x":"\ud800"}]}`, right: `{"items":[{"x":"�"}]}`, want: false},
		{name: "valid surrogate pair equals literal", left: `{"x":"\ud83d\ude00"}`, right: `{"x":"😀"}`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := JSONDocumentsEqual([]byte(test.left), []byte(test.right)); got != test.want {
				t.Fatalf("JSONDocumentsEqual()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateJSONDocumentRejectsAmbiguousInput(t *testing.T) {
	for _, document := range []string{
		`{"value":1,"value":2}`,
		`{"value":1} null`,
		`{"value":`,
		`{"x":"\ud800"}`,
		`{"x":"\udc00"}`,
	} {
		if err := ValidateJSONDocument([]byte(document)); err == nil {
			t.Fatalf("ValidateJSONDocument accepted %q", document)
		}
	}
	if err := ValidateJSONDocument([]byte(`{"value":9007199254740993}`)); err != nil {
		t.Fatalf("valid arbitrary-precision JSON rejected: %v", err)
	}
	if err := ValidateJSONDocument([]byte(`{"x":"\ud83d\ude00"}`)); err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
	if err := ValidateJSONDocument([]byte(`{"x":"�"}`)); err != nil {
		t.Fatalf("literal replacement character rejected: %v", err)
	}
}

func TestNormalizeJSONDocumentCanonicalizesStrictInput(t *testing.T) {
	normalized, err := NormalizeJSONDocument([]byte(`{"z":2.0,"a":{"b":1e3,"a":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(normalized), `{"a":{"a":true,"b":1e3},"z":2}`; got != want {
		t.Fatalf("normalized JSON=%s want=%s", got, want)
	}
	if _, err := NormalizeJSONDocument([]byte(`{"x":"\ud800"}`)); err == nil {
		t.Fatal("normalization accepted lone surrogate")
	}
}
