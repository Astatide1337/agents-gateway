// Package credentials provides envelope encryption for tenant-owned secrets.
// The deployment key-encryption key must be supplied from outside PostgreSQL.
package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

const Version = 1

type Envelope struct {
	Version      int    `json:"version"`
	KeyID        string `json:"keyId"`
	WrappedDEK   string `json:"wrappedDek"`
	WrappedNonce string `json:"wrappedNonce"`
	Ciphertext   string `json:"ciphertext"`
	DataNonce    string `json:"dataNonce"`
}

type Manager struct {
	keyID string
	kek   []byte
	rand  io.Reader
}

func NewManager(keyID string, key []byte) (*Manager, error) {
	if keyID == "" {
		return nil, errors.New("key ID is required")
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key-encryption key must be 32 bytes, got %d", len(key))
	}
	return &Manager{keyID: keyID, kek: append([]byte(nil), key...), rand: rand.Reader}, nil
}

func (m *Manager) Encrypt(organizationID, purpose string, plaintext []byte) (Envelope, error) {
	if organizationID == "" || purpose == "" {
		return Envelope{}, errors.New("organization and purpose are required")
	}
	dek := make([]byte, 32)
	if _, err := io.ReadFull(m.rand, dek); err != nil {
		return Envelope{}, fmt.Errorf("generate data key: %w", err)
	}
	defer zero(dek)

	wrapped, wrappedNonce, err := seal(m.kek, dek, []byte("agw:dek:"+organizationID), m.rand)
	if err != nil {
		return Envelope{}, err
	}
	ciphertext, dataNonce, err := seal(dek, plaintext, aad(organizationID, purpose), m.rand)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Version: Version, KeyID: m.keyID,
		WrappedDEK: encode(wrapped), WrappedNonce: encode(wrappedNonce),
		Ciphertext: encode(ciphertext), DataNonce: encode(dataNonce),
	}, nil
}

func (m *Manager) Decrypt(organizationID, purpose string, envelope Envelope) ([]byte, error) {
	if envelope.Version != Version {
		return nil, fmt.Errorf("unsupported envelope version %d", envelope.Version)
	}
	if envelope.KeyID != m.keyID {
		return nil, fmt.Errorf("envelope uses key %q, manager has %q", envelope.KeyID, m.keyID)
	}
	wrapped, err := decode(envelope.WrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped key: %w", err)
	}
	wrappedNonce, err := decode(envelope.WrappedNonce)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped nonce: %w", err)
	}
	dek, err := open(m.kek, wrappedNonce, wrapped, []byte("agw:dek:"+organizationID))
	if err != nil {
		return nil, fmt.Errorf("unwrap data key: %w", err)
	}
	defer zero(dek)
	ciphertext, err := decode(envelope.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	dataNonce, err := decode(envelope.DataNonce)
	if err != nil {
		return nil, fmt.Errorf("decode data nonce: %w", err)
	}
	plaintext, err := open(dek, dataNonce, ciphertext, aad(organizationID, purpose))
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return plaintext, nil
}

func seal(key, plaintext, aad []byte, random io.Reader) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, aad), nonce, nil
}

func open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, aad)
}

func aad(organizationID, purpose string) []byte {
	return []byte("agw:secret:" + organizationID + ":" + purpose)
}
func encode(value []byte) string          { return base64.RawURLEncoding.EncodeToString(value) }
func decode(value string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(value) }
func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
