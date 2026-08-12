package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLoadReportSignerAcceptsOnlyImmutableRawKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	immutable := true
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "signer", Namespace: "agw-system"}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{reportSigningPrivateKeyKey: privateKey}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	signer, err := loadReportSigner(context.Background(), reader, "agw-system", "signer")
	if err != nil || signer == nil || len(signer.privateKey) != ed25519.PrivateKeySize {
		t.Fatalf("signer=%#v err=%v", signer, err)
	}
	if &signer.privateKey[0] == &privateKey[0] {
		t.Fatal("signer retained the Kubernetes Secret backing slice")
	}
}

func TestLoadReportSignerRejectsMutableExtraOrMalformedSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	immutable := true
	for _, test := range []struct {
		name   string
		secret *corev1.Secret
	}{
		{name: "mutable", secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: "agw-system"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{reportSigningPrivateKeyKey: make([]byte, ed25519.SeedSize)}}},
		{name: "extra", secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: "agw-system"}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{reportSigningPrivateKeyKey: make([]byte, ed25519.SeedSize), "other": {1}}}},
		{name: "malformed", secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: "agw-system"}, Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{reportSigningPrivateKeyKey: {1, 2, 3}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test.secret).Build()
			if _, err := loadReportSigner(context.Background(), reader, "agw-system", "key"); !errors.Is(err, errReportSigningKey) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
