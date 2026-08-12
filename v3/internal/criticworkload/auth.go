package criticworkload

import (
	"crypto/ed25519"
	"errors"
	"fmt"
)

const MaxSignatureBytes = 8 << 10

var (
	ErrAuthenticatorConfig = errors.New("invalid critic output authenticator configuration")
	ErrAuthentication      = errors.New("critic output authentication failed")
)

// RecordAuthenticator signs controller-authored output metadata and verifies
// it before any metadata or critic bytes are decoded. Private key material is
// accepted only by the trusted Runner; a critic Job has no access to it.
type RecordAuthenticator interface {
	Sign([]byte) ([]byte, error)
	Verify([]byte, []byte) error
}

// Ed25519Authenticator is a small standard-library implementation. Production
// callers should load its private key from the operator's existing signing
// Secret or KMS-backed signer and pass only the public key to Source.
type Ed25519Authenticator struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
}

func NewEd25519Authenticator(private ed25519.PrivateKey, public ed25519.PublicKey) (*Ed25519Authenticator, error) {
	if len(private) != 0 && len(private) != ed25519.PrivateKeySize {
		return nil, ErrAuthenticatorConfig
	}
	if len(public) != 0 && len(public) != ed25519.PublicKeySize {
		return nil, ErrAuthenticatorConfig
	}
	if len(public) == 0 && len(private) == ed25519.PrivateKeySize {
		public = private.Public().(ed25519.PublicKey)
	}
	if len(public) != ed25519.PublicKeySize {
		return nil, ErrAuthenticatorConfig
	}
	return &Ed25519Authenticator{private: append(ed25519.PrivateKey(nil), private...), public: append(ed25519.PublicKey(nil), public...)}, nil
}

func (a *Ed25519Authenticator) Sign(body []byte) ([]byte, error) {
	if a == nil || len(a.private) != ed25519.PrivateKeySize || len(body) == 0 || len(body) > MaxRecordBytes {
		return nil, ErrAuthenticatorConfig
	}
	signature := ed25519.Sign(a.private, body)
	return append([]byte(nil), signature...), nil
}

func (a *Ed25519Authenticator) Verify(body, signature []byte) error {
	if a == nil || len(a.public) != ed25519.PublicKeySize || len(body) == 0 || len(body) > MaxRecordBytes || len(signature) != ed25519.SignatureSize {
		return ErrAuthentication
	}
	if !ed25519.Verify(a.public, body, signature) {
		return fmt.Errorf("%w: signature mismatch", ErrAuthentication)
	}
	return nil
}
