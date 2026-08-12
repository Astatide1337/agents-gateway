package objectstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/cosignattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/retention"
	"github.com/Astatide1337/agents-gateway/v3/internal/verificationattestation"
)

// localReleaseGateCosign is a provider-free cryptographic seam. It verifies
// the real Ed25519 Gate signature, constructs the canonical AGW statement, and
// writes a bounded bundle-shaped artifact. The external cosign adapter is
// covered separately by its opt-in offline binary test; this seam keeps the
// S3 and lifecycle proof independent of a local cosign installation.
type localReleaseGateCosign struct {
	tamperStatement bool
}

func (c localReleaseGateCosign) Attest(_ context.Context, request cosignattestation.Request) (cosignattestation.AttestResult, error) {
	verified, err := evidenceattestation.VerifySignedReportBytes(request.SignedReport, request.TrustedGatePublicKey)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	statement, err := evidenceattestation.StatementFromVerifiedReport(verified, request.EvidenceOptions)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	if c.tamperStatement {
		statement.Subject[0].Digest["sha256"] = strings.Repeat("0", 64)
	}
	body, err := evidenceattestation.StatementBytes(statement)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	if err := os.WriteFile(request.StatementOutputPath, body, 0600); err != nil {
		return cosignattestation.AttestResult{}, err
	}
	if err := os.WriteFile(request.BundlePath, []byte(`{"local":"release-gate"}`), 0600); err != nil {
		return cosignattestation.AttestResult{}, err
	}
	statementDigest, err := evidenceattestation.StatementDigest(statement)
	if err != nil {
		return cosignattestation.AttestResult{}, err
	}
	return cosignattestation.AttestResult{
		Statement: body, StatementDigest: statementDigest,
		SubjectDigest: verified.Report().PatchDigest,
		PredicateType: evidenceattestation.PredicateType,
		MediaType:     evidenceattestation.MediaType,
	}, nil
}

func (c localReleaseGateCosign) Verify(_ context.Context, request cosignattestation.Request) (cosignattestation.VerifyResult, error) {
	verified, err := evidenceattestation.VerifySignedReportBytes(request.SignedReport, request.TrustedGatePublicKey)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	statement, err := evidenceattestation.StatementFromVerifiedReport(verified, request.EvidenceOptions)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	statementDigest, err := evidenceattestation.StatementDigest(statement)
	if err != nil {
		return cosignattestation.VerifyResult{}, err
	}
	return cosignattestation.VerifyResult{
		StatementDigest: statementDigest,
		SubjectDigest:   verified.Report().PatchDigest,
		PredicateType:   evidenceattestation.PredicateType,
		MediaType:       evidenceattestation.MediaType,
	}, nil
}

func TestLocalSDKStoreReleaseGateAttestationAndETagCleanup(t *testing.T) {
	fixture := newLocalS3(t)
	store := newLocalSDKStore(t, fixture)
	client := newLocalSDKClient(t, fixture)
	ctx := context.Background()
	input, publicKey := localReleaseGateInput(t)

	lifecycle, err := verificationattestation.New(verificationattestation.Config{
		Cosign: localReleaseGateCosign{}, Store: store, TrustedGatePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := lifecycle.Attest(ctx, input)
	if err != nil {
		t.Fatalf("first attestation = %v", err)
	}

	// Rebuild both adapters around the same endpoint to model an operator
	// restart. Content-addressed Put conflicts must be read and compared, not
	// treated as a new artifact or an overwrite.
	restartedStore := newLocalSDKStore(t, fixture)
	restarted, err := verificationattestation.New(verificationattestation.Config{
		Cosign: localReleaseGateCosign{}, Store: restartedStore, TrustedGatePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Attest(ctx, input)
	if err != nil {
		t.Fatalf("replayed attestation = %v", err)
	}
	if first.StatementRef != replayed.StatementRef || first.BundleRef != replayed.BundleRef {
		t.Fatalf("restart changed immutable refs: first=%#v replay=%#v", first, replayed)
	}

	for _, ref := range []v1alpha1.ArtifactRef{first.StatementRef, first.BundleRef} {
		actualKey := strings.TrimPrefix(ref.URI, "s3://agw-test-bucket/")
		logicalKey := strings.TrimPrefix(actualKey, "runs/")
		body, getErr := restartedStore.Get(ctx, logicalKey)
		if getErr != nil {
			t.Fatalf("read persisted %s: %v", ref.Kind, getErr)
		}
		sum := sha256.Sum256(body)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != ref.Digest || int64(len(body)) != ref.SizeBytes {
			t.Fatalf("%s identity mismatch: digest=%q size=%d ref=%#v", ref.Kind, got, len(body), ref)
		}
	}

	// The lifecycle independently verifies that the attestation statement is
	// bound to the trusted Gate report before it writes anything to S3.
	tampered, err := verificationattestation.New(verificationattestation.Config{
		Cosign: localReleaseGateCosign{tamperStatement: true}, Store: restartedStore, TrustedGatePublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tampered.Attest(ctx, input); err == nil {
		t.Fatal("tampered Gate binding was accepted")
	}

	if _, _, err := restartedStore.Put(ctx, "cleanup-sentinel", []byte("retain"), "application/octet-stream"); err != nil {
		t.Fatalf("create cleanup sentinel: %v", err)
	}
	inventory, err := retention.NewS3InventoryWithClient(retention.S3Config{
		Bucket: "agw-test-bucket", Prefix: "runs", Region: "us-east-1",
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	objects, complete, err := inventory.List(ctx, "runs/", 100)
	if err != nil || !complete {
		t.Fatalf("inventory before cleanup = (%#v, %t, %v)", objects, complete, err)
	}
	targetPrefix := "runs/runs/" + input.RunUID + "/verification/"
	targets := 0
	for _, object := range objects {
		if !strings.HasPrefix(object.Key, targetPrefix) {
			continue
		}
		targets++
		if err := inventory.Delete(ctx, object.Key, `"wrong-etag"`); !errors.Is(err, retention.ErrFenceConflict) {
			t.Fatalf("wrong ETag delete for %s = %v, want fence conflict", object.Key, err)
		}
		if err := inventory.Delete(ctx, object.Key, object.ETag); err != nil {
			t.Fatalf("owned ETag delete for %s = %v", object.Key, err)
		}
	}
	if targets != 2 {
		t.Fatalf("cleanup target count=%d, want statement and bundle", targets)
	}
	remaining, complete, err := inventory.List(ctx, "runs/", 100)
	if err != nil || !complete {
		t.Fatalf("inventory after cleanup = (%#v, %t, %v)", remaining, complete, err)
	}
	for _, object := range remaining {
		if strings.HasPrefix(object.Key, targetPrefix) {
			t.Fatalf("attestation object survived cleanup: %s", object.Key)
		}
	}
	if len(remaining) != 1 || remaining[0].Key != "runs/cleanup-sentinel" {
		t.Fatalf("cleanup removed the wrong objects: %#v", remaining)
	}
}

func localReleaseGateInput(t *testing.T) (verificationattestation.Input, ed25519.PublicKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID: "run-uid-1", SpecDigest: "sha256:" + strings.Repeat("b", 64), BaseSHA: strings.Repeat("c", 40), PatchDigest: "sha256:" + strings.Repeat("d", 64),
		GateUID: "gate-uid-1", GateGeneration: 4,
		RuntimeImageDigest: "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64), VerifierImageDigest: "ghcr.io/example/verifier@sha256:" + strings.Repeat("e", 64),
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
	signedBytes, err := gate.SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(signedBytes)
	reportRef := v1alpha1.ArtifactRef{
		URI: "s3://agw-test-bucket/runs/run-uid-1/verification-report.json", Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Kind: "verification-report", Name: "verification-report.json", MediaType: "application/json", SizeBytes: int64(len(signedBytes)),
	}
	return verificationattestation.Input{
		RunUID: report.RunUID, SpecDigest: report.SpecDigest, PatchDigest: report.PatchDigest,
		ReportRef: reportRef, SignedReport: signedBytes,
		EvidenceOptions: evidenceattestation.Options{EvidenceArtifactDigests: []string{"sha256:" + strings.Repeat("f", 64)}},
	}, publicKey
}
