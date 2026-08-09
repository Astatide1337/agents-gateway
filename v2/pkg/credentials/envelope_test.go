package credentials

import (
	"bytes"
	"testing"
)

func TestEnvelopeRoundTripAndTenantBinding(t *testing.T) {
	m, err := NewManager("key-1", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := m.Encrypt("org-a", "model-credential", []byte("secret-value"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := m.Decrypt("org-a", "model-credential", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "secret-value" {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}
	if _, err := m.Decrypt("org-b", "model-credential", envelope); err == nil {
		t.Fatal("cross-tenant decrypt succeeded")
	}
	if _, err := m.Decrypt("org-a", "different-purpose", envelope); err == nil {
		t.Fatal("cross-purpose decrypt succeeded")
	}
}

func TestManagerRejectsInvalidKey(t *testing.T) {
	if _, err := NewManager("key", []byte("short")); err == nil {
		t.Fatal("invalid key length accepted")
	}
}
