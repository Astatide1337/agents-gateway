package verificationattestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/cosignattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
)

type memoryStore struct {
	values       map[string][]byte
	corruptReads bool
	missingReads bool
	getErr       error
}

func (s *memoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	if prior, ok := s.values[key]; ok {
		_ = prior
		return false, "s3://agw-artifacts/" + key, nil
	}
	s.values[key] = append([]byte(nil), body...)
	return true, "s3://agw-artifacts/" + key, nil
}

func artifactRef(body []byte) v1alpha1.ArtifactRef {
	sum := sha256.Sum256(body)
	d := "sha256:" + fmt.Sprintf("%x", sum[:])
	return v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/runs/run-uid-1/verification-report.json", Digest: d, Kind: "verification-report", Name: "verification-report.json", MediaType: "application/json", SizeBytes: int64(len(body))}
}
func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.missingReads {
		return nil, nil
	}
	body := append([]byte(nil), s.values[key]...)
	if s.corruptReads && len(body) > 0 {
		body[0] ^= 0xff
	}
	return body, nil
}

type fakeCosign struct {
	mismatch    bool
	bundleBytes []byte
}

func (f fakeCosign) Attest(_ context.Context, request cosignattestation.Request) (cosignattestation.AttestResult, error) {
	verified, err := evidenceattestation.VerifySignedReportBytes(request.SignedReport, request.TrustedGatePublicKey)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	statement, err := evidenceattestation.StatementFromVerifiedReport(verified, request.EvidenceOptions)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	body, err := evidenceattestation.StatementBytes(statement)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	d, err := evidenceattestation.StatementDigest(statement)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	if err := os.WriteFile(request.StatementOutputPath, body, 0600); err != nil {
		return cosignattestation.AttestResult{}, err
	}
	bundle := f.bundleBytes
	if len(bundle) == 0 {
		bundle = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
	}
	if err := os.WriteFile(request.BundlePath, bundle, 0600); err != nil {
		return cosignattestation.AttestResult{}, err
	}
	return cosignattestation.AttestResult{Statement: body, StatementDigest: d, SubjectDigest: verified.Report().PatchDigest, PredicateType: evidenceattestation.PredicateType, MediaType: evidenceattestation.MediaType, BundlePath: request.BundlePath}, nil
}

func (f fakeCosign) Verify(_ context.Context, request cosignattestation.Request) (cosignattestation.VerifyResult, error) {
	verified, err := evidenceattestation.VerifySignedReportBytes(request.SignedReport, request.TrustedGatePublicKey)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	statement, err := evidenceattestation.StatementFromVerifiedReport(verified, request.EvidenceOptions)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	d, err := evidenceattestation.StatementDigest(statement)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	if f.mismatch {
		d = "sha256:" + strings.Repeat("0", 64)
	}
	return cosignattestation.VerifyResult{StatementDigest: d, SubjectDigest: verified.Report().PatchDigest, PredicateType: evidenceattestation.PredicateType, MediaType: evidenceattestation.MediaType, BundlePath: request.BundlePath}, nil
}

func TestLifecycleAttestsVerifiesAndPersistsImmutableArtifacts(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}}
	lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	result, err := lifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatementRef.Kind != StatementKind || result.BundleRef.Kind != BundleKind || len(store.values) != 2 {
		t.Fatalf("unexpected attestation result: %#v objects=%d", result, len(store.values))
	}
	second, err := lifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if second.StatementRef.Digest != result.StatementRef.Digest || second.BundleRef.Digest != result.BundleRef.Digest || len(store.values) != 2 {
		t.Fatalf("idempotent replay changed identity: %#v", second)
	}
}

func TestLifecycleFailsClosedOnBindingMismatchAndReportRefTamper(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}}
	lifecycle, err := New(Config{Cosign: fakeCosign{mismatch: true}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Attest(context.Background(), input); err == nil {
		t.Fatal("cosign verification mismatch was accepted")
	}
	if len(store.values) != 0 {
		t.Fatal("failed verification persisted artifacts")
	}
	input.ReportRef.Digest = "sha256:" + strings.Repeat("0", 64)
	lifecycle, err = New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Attest(context.Background(), input); err == nil {
		t.Fatal("tampered report reference was accepted")
	}
}

func TestLifecycleRejectsCallerIdentityMismatchWithAuthenticatedReport(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}}
	lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	input.RunUID = "different-run"
	if _, err := lifecycle.Attest(context.Background(), input); err == nil {
		t.Fatal("attestation accepted a run identity different from the signed report")
	}
	if len(store.values) != 0 {
		t.Fatal("identity mismatch persisted attestation artifacts")
	}
}

func TestLifecycleFailsClosedWhenReadAfterWriteChangesImmutableBytes(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}, corruptReads: true}
	lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Attest(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("corrupted read-after-write was accepted: %v", err)
	}
	if len(store.values) != 1 {
		t.Fatalf("unexpected persistence after failed reconciliation: %d objects", len(store.values))
	}
}

func TestLifecycleClassifiesMissingReadAfterWriteAsRetryable(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}, missingReads: true}
	lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifecycle.Attest(context.Background(), input)
	if !IsRetryable(err) || IsPermanent(err) {
		t.Fatalf("missing read error=%v, want retryable and not permanent", err)
	}
}

func TestLifecycleClassifiesReadTimeoutAsRetryable(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}, getErr: context.DeadlineExceeded}
	lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifecycle.Attest(context.Background(), input)
	if !IsRetryable(err) || IsPermanent(err) {
		t.Fatalf("read timeout error=%v, want retryable and not permanent", err)
	}
}

func TestLifecycleRejectsMalformedArtifactURI(t *testing.T) {
	input, publicKey := testInput(t)
	for _, uri := range []string{"https://example.com", "s3://", "s3://bucket?version=1", "s3://user@bucket/object"} {
		t.Run(uri, func(t *testing.T) {
			input.ReportRef.URI = uri
			lifecycle, err := New(Config{Cosign: fakeCosign{}, Store: &memoryStore{values: map[string][]byte{}}, TrustedGatePublicKey: publicKey})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lifecycle.Attest(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("URI=%q error=%v, want ErrInvalidInput", uri, err)
			}
		})
	}
}

func TestLifecycleKeyRotationPreservesRollbackArtifact(t *testing.T) {
	input, publicKey := testInput(t)
	store := &memoryStore{values: map[string][]byte{}}
	oldLifecycle, err := New(Config{
		Cosign: fakeCosign{bundleBytes: []byte(`{"key":"old"}`)}, Store: store, TrustedGatePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldResult, err := oldLifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	newLifecycle, err := New(Config{
		Cosign: fakeCosign{bundleBytes: []byte(`{"key":"new"}`)}, Store: store, TrustedGatePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	newResult, err := newLifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if oldResult.StatementRef.Digest != newResult.StatementRef.Digest || oldResult.BundleRef.Digest == newResult.BundleRef.Digest {
		t.Fatalf("rotation identities=%#v -> %#v", oldResult, newResult)
	}
	rolledBack, err := oldLifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.BundleRef.Digest != oldResult.BundleRef.Digest || len(store.values) != 3 {
		t.Fatalf("rollback result=%#v objects=%d", rolledBack, len(store.values))
	}
}

func TestRealCosignLocalKeyRoundTrip(t *testing.T) {
	cosignPath := os.Getenv("AGW_COSIGN_PATH")
	keyRef := os.Getenv("AGW_COSIGN_KEY_REF")
	verifyKeyRef := os.Getenv("AGW_COSIGN_VERIFY_KEY_REF")
	if cosignPath == "" || keyRef == "" || verifyKeyRef == "" {
		t.Skip("set AGW_COSIGN_PATH, AGW_COSIGN_KEY_REF, and AGW_COSIGN_VERIFY_KEY_REF for the opt-in cosign round trip")
	}
	input, publicKey := testInput(t)
	adapter, err := cosignattestation.New(cosignattestation.Config{
		CosignPath: cosignPath, KeyRef: keyRef, VerifyKeyRef: verifyKeyRef,
		PredicateType: evidenceattestation.PredicateType, MediaType: evidenceattestation.MediaType,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{values: map[string][]byte{}}
	lifecycle, err := New(Config{Cosign: adapter, Store: store, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	result, err := lifecycle.Attest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.BundleRef.Digest == "" || result.StatementRef.Digest == "" || len(store.values) != 2 {
		t.Fatalf("real cosign round trip did not persist both proofs: %#v", result)
	}
}

func testInput(t *testing.T) (Input, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID: "run-uid-1", SpecDigest: "sha256:" + strings.Repeat("b", 64), BaseSHA: strings.Repeat("c", 40), PatchDigest: "sha256:" + strings.Repeat("d", 64),
		GateUID: "gate-uid-1", GateGeneration: 4,
		RuntimeImageDigest:  "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64),
		VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
		HelperImageDigests: []gate.ImageEvidence{
			{Name: "fetch", Digest: "ghcr.io/example/fetch@sha256:" + strings.Repeat("1", 64)},
			{Name: "apply", Digest: "ghcr.io/example/apply@sha256:" + strings.Repeat("2", 64)},
			{Name: "lockdown", Digest: "ghcr.io/example/lockdown@sha256:" + strings.Repeat("3", 64)},
		},
	}, gate.Evaluate(gate.Input{}))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := gate.SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	body, err := gate.SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	return Input{
		RunUID: report.RunUID, SpecDigest: report.SpecDigest, PatchDigest: report.PatchDigest,
		ReportRef: artifactRef(body), SignedReport: body,
		EvidenceOptions: evidenceattestation.Options{EvidenceArtifactDigests: []string{"sha256:" + strings.Repeat("f", 64)}},
	}, publicKey
}
