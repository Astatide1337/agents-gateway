package store

import "github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"

// ValidateJSONDocument is the storage-facing name for the shared strict JSON
// contract. Keeping this thin wrapper avoids widening the Store API while
// ensuring memory, PostgreSQL, and replay use the same parser and budgets.
func ValidateJSONDocument(document []byte) error {
	return strictjson.Validate(document)
}

// NormalizeJSONDocument canonicalizes a strict JSON document without deleting
// nulls or reordering arrays.
func NormalizeJSONDocument(document []byte) ([]byte, error) {
	return strictjson.Normalize(document)
}

// JSONDocumentsEqual compares strict JSON structurally and fails closed on any
// invalid or out-of-budget input.
func JSONDocumentsEqual(left, right []byte) bool {
	return strictjson.Equal(left, right)
}
