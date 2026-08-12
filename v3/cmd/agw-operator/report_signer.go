package main

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const reportSigningPrivateKeyKey = "private-key"

var errReportSigningKey = errors.New("verification report signing key is unavailable or invalid")

type reportSigner struct{ privateKey ed25519.PrivateKey }

func (s *reportSigner) PublicKey() (ed25519.PublicKey, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return nil, errReportSigningKey
	}
	public, ok := s.privateKey.Public().(ed25519.PublicKey)
	if !ok || len(public) != ed25519.PublicKeySize {
		return nil, errReportSigningKey
	}
	return append(ed25519.PublicKey(nil), public...), nil
}

func (s *reportSigner) Sign(report gate.VerificationReport) (gate.SignedReport, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return gate.SignedReport{}, errReportSigningKey
	}
	return gate.SignReport(report, s.privateKey)
}

// loadReportSigner performs one exact, uncached Secret read. The Secret must
// be immutable and contain only a raw 32-byte Ed25519 seed or 64-byte private
// key under "private-key". PEM/text ambiguity is deliberately not accepted.
func loadReportSigner(ctx context.Context, reader client.Reader, namespace, name string) (*reportSigner, error) {
	if ctx == nil || reader == nil || namespace == "" || name == "" {
		return nil, errReportSigningKey
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, errReportSigningKey
	}
	if secret.Namespace != namespace || secret.Name != name || secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 || len(secret.StringData) != 0 {
		return nil, errReportSigningKey
	}
	material, ok := secret.Data[reportSigningPrivateKeyKey]
	if !ok {
		return nil, errReportSigningKey
	}
	var privateKey ed25519.PrivateKey
	switch len(material) {
	case ed25519.SeedSize:
		privateKey = ed25519.NewKeyFromSeed(append([]byte(nil), material...))
	case ed25519.PrivateKeySize:
		privateKey = append(ed25519.PrivateKey(nil), material...)
	default:
		return nil, errReportSigningKey
	}
	return &reportSigner{privateKey: privateKey}, nil
}
