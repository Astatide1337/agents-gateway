package verifycontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verificationattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
	"github.com/Astatide1337/agents-gateway/v3/internal/workload"
	"k8s.io/apimachinery/pkg/types"
)

func TestReconcileEnsuresAndWaitsForFinishedEvidence(t *testing.T) {
	input, body := testInputs(t)
	backend := newFakeBackend(input, false)
	source := &fakeEvidenceSource{frame: mustEvidenceFrame(t, body)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseVerifying || decision.Complete || decision.SandboxFinished {
		t.Fatalf("decision=%#v, want pending verification", decision)
	}
	if backend.ensureCalls != 1 || backend.observeCalls != 1 || source.reads != 0 || len(store.objects) != 0 {
		t.Fatalf("ensure=%d observe=%d reads=%d reports=%d", backend.ensureCalls, backend.observeCalls, source.reads, len(store.objects))
	}
	secretProjectionFound := false
	for _, volume := range backend.plan.Sandbox.Spec.PodTemplate.Spec.Volumes {
		if volume.Projected == nil {
			continue
		}
		keys := make(map[string]struct{})
		for _, source := range volume.Projected.Sources {
			if source.Secret == nil || source.Secret.Name != input.Credentials.SecretName {
				continue
			}
			for _, item := range source.Secret.Items {
				keys[item.Key] = struct{}{}
			}
		}
		_, cloneOK := keys[input.Credentials.CloneSecretKey]
		_, accessOK := keys[verifyworkload.ArtifactAccessKeyIDSecretKey]
		_, secretOK := keys[verifyworkload.ArtifactSecretAccessKeySecretKey]
		secretProjectionFound = cloneOK && accessOK && secretOK
	}
	if !secretProjectionFound {
		t.Fatalf("explicit credential projection was not passed to verifyworkload: %#v", backend.plan.Sandbox.Spec.PodTemplate.Spec.Volumes)
	}
	if decision.StatusProjection().VerifySandboxRef == nil {
		t.Fatal("status projection omitted verify Sandbox reference")
	}
}

func TestReconcileDoesNotAcceptFinishedSandboxWithoutEvidence(t *testing.T) {
	input, _ := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{err: ErrEvidenceMissing}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseGated || !decision.Complete || decision.Gate == nil || decision.Gate.Verdict != string(gate.Rejected) {
		t.Fatalf("decision=%#v, want signed rejection", decision)
	}
	if decision.Failure == nil || decision.Failure.Code != "EvidenceMissing" || decision.Failure.Retryable {
		t.Fatalf("failure=%#v, want terminal missing-evidence failure", decision.Failure)
	}
	if source.reads != 1 || len(store.objects) != 1 {
		t.Fatalf("reads=%d objects=%d, want one stdout read and one rejection report", source.reads, len(store.objects))
	}
}

func TestReconcileEvaluatesAndPersistsIdentityBoundSignedReport(t *testing.T) {
	input, body := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{frame: mustEvidenceFrame(t, body)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseGated || !decision.Complete || decision.Gate == nil || decision.Gate.Verdict != string(gate.Accepted) {
		t.Fatalf("decision=%#v, want accepted Gate", decision)
	}
	if decision.Gate.ReportRef == nil || decision.Gate.ReportRef.Digest == "" {
		t.Fatalf("decision gate report=%#v, want immutable report ref", decision.Gate)
	}
	if decision.Gate.Name != "go-default" || decision.Gate.UID != "gate-uid" || decision.Gate.Generation != 4 || decision.Gate.Mode != v1alpha1.GateShadow {
		t.Fatalf("decision gate identity=%#v, want immutable shadow Gate revision", decision.Gate)
	}
	if len(decision.Artifacts) != 1 || decision.Artifacts[0].Digest != digest(body) || decision.Artifacts[0].Kind != "verification-evidence" {
		t.Fatalf("artifacts=%#v, want content-addressed raw evidence", decision.Artifacts)
	}
	if len(store.objects) != 2 {
		t.Fatalf("objects=%d, want raw evidence and signed report", len(store.objects))
	}
	if len(store.puts) != 2 || !strings.Contains(store.puts[0], "/evidence/") || strings.Contains(store.puts[1], "/evidence/") {
		t.Fatalf("immutable write order=%v, want evidence before report", store.puts)
	}
	if source.lastRef != backend.ref || source.max <= MaxEvidenceBytes {
		t.Fatalf("source ref/max=(%#v,%d), want finished SandboxRef and encoded frame bound", source.lastRef, source.max)
	}
	var encoded []byte
	for key, value := range store.objects {
		if !strings.Contains(key, "/evidence/") {
			encoded = value
		}
	}
	signed, err := gate.ParseSignedReport(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.VerifySignedReport(signed, nil); err != nil {
		t.Fatal(err)
	}
	if signed.Report.RunUID != input.Snapshot.Run.UID || signed.Report.SpecDigest != input.SpecDigest || signed.Report.BaseSHA != input.BaseSHA || signed.Report.PatchDigest != input.Patch.Digest {
		t.Fatalf("report identity=%#v, input=(%s,%s,%s,%s)", signed.Report, input.Snapshot.Run.UID, input.SpecDigest, input.BaseSHA, input.Patch.Digest)
	}
	projection := decision.StatusProjection()
	if projection.Gate.ReportRef == nil || len(projection.Artifacts) != 1 || projection.Artifacts[0].Digest != digest(body) {
		t.Fatal("status projection lost report or raw evidence ref")
	}
}

func TestReconcileRequiresConfiguredAttestationBeforeGateCompletion(t *testing.T) {
	input, body := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{frame: mustEvidenceFrame(t, body)}
	store := newMemoryStore()
	attester := &fakeReportAttester{}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	driver, err := New(Options{
		Backend: backend, Evidence: source, ReportStore: store,
		ReportSigner:   SignerFunc(func(report gate.VerificationReport) (gate.SignedReport, error) { return gate.SignReport(report, key) }),
		ReportAttester: attester,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Complete || attester.calls != 1 || len(decision.Artifacts) != 3 {
		t.Fatalf("decision=%#v attester calls=%d, want evidence plus statement and bundle", decision, attester.calls)
	}
	if decision.Artifacts[1].Kind != verificationattestation.StatementKind || decision.Artifacts[2].Kind != verificationattestation.BundleKind {
		t.Fatalf("attestation refs=%#v", decision.Artifacts)
	}
	if attester.last.ReportRef.Digest != digest(attester.last.SignedReport) || attester.last.PatchDigest != input.Patch.Digest || len(attester.last.EvidenceOptions.EvidenceArtifactDigests) != 1 {
		t.Fatalf("attestation input was not bound: %#v", attester.last)
	}
	attester.err = errors.New("kms unavailable")
	if _, err := driver.Reconcile(context.Background(), input); err == nil || !strings.Contains(err.Error(), "attest authenticated verification report") {
		t.Fatalf("attestation failure did not fail closed: %v", err)
	}
}

func TestReconcileRejectsEvidenceIdentityMismatch(t *testing.T) {
	input, body := testInputs(t)
	var evidence MachineEvidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.BaseSHA = strings.Repeat("e", 40)
	mutated, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{frame: rawEvidenceFrame(mutated)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseGated || decision.Gate == nil || decision.Gate.Verdict != string(gate.Rejected) {
		t.Fatalf("decision=%#v, want rejected identity mismatch", decision)
	}
	if decision.Failure == nil || decision.Failure.Code != "EvidenceMalformed" {
		t.Fatalf("failure=%#v, want malformed evidence failure", decision.Failure)
	}
	if len(decision.Artifacts) != 1 || decision.Artifacts[0].Digest != digest(mutated) {
		t.Fatalf("identity-mismatched raw evidence was not retained: %#v", decision.Artifacts)
	}
}

func TestReconcileKeepsUnavailableEvidencePending(t *testing.T) {
	input, _ := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{err: ErrEvidenceNotReady}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseVerifying || decision.Complete || decision.Gate != nil {
		t.Fatalf("decision=%#v, want pending evidence", decision)
	}
	if decision.Failure == nil || decision.Failure.Code != "EvidenceNotReady" || !decision.Failure.Retryable {
		t.Fatalf("failure=%#v, want retryable evidence-not-ready failure", decision.Failure)
	}
	if len(store.objects) != 0 {
		t.Fatal("pending evidence unexpectedly produced a report")
	}
}

func TestReconcileKeepsTransientStdoutFailurePending(t *testing.T) {
	input, _ := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{err: fmt.Errorf("pod logs: %w", ErrEvidenceUnavailable)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Phase != v1alpha1.PhaseVerifying || decision.Complete || decision.Gate != nil || decision.Failure == nil || decision.Failure.Code != "EvidenceUnavailable" || !decision.Failure.Retryable {
		t.Fatalf("decision=%#v, want retryable stdout-source failure", decision)
	}
	if len(store.objects) != 0 {
		t.Fatal("transient stdout failure unexpectedly persisted artifacts")
	}
}

func TestCriticConfigurationWithoutRunnerFailsClosed(t *testing.T) {
	input, body := criticTestInputs(t, false)
	backend := newFakeBackend(input, true)
	driver := newTestDriver(t, backend, &fakeEvidenceSource{frame: mustEvidenceFrame(t, body)}, newMemoryStore())
	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Gate != nil || decision.Complete || decision.Failure == nil || decision.Failure.Code != "CriticIntegrationMissing" || decision.Failure.Retryable {
		t.Fatalf("decision=%#v, want terminal no-accept critic integration failure", decision)
	}
}

func TestCriticEvidenceMustBeAuthenticatedAndCanonical(t *testing.T) {
	input, verifierBody := criticTestInputs(t, false)
	criticBody := emptyCriticInput(t)
	binding, err := criticBinding(input)
	if err != nil {
		t.Fatal(err)
	}
	ref := criticArtifactRef(criticBody)
	runner := &fakeCriticRunner{status: CriticRunStatus{ID: "critic-run", Ready: true, Finished: true, InputRef: &ref}}
	source := &fakeCriticSource{artifact: CriticEvidenceArtifact{Input: criticBody, Authenticated: false, Binding: binding}}
	store := newMemoryStore()
	driver := newCriticDriver(t, newFakeBackend(input, true), &fakeEvidenceSource{frame: mustEvidenceFrame(t, verifierBody)}, source, runner, store)
	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Gate != nil || decision.Complete || decision.Failure == nil || decision.Failure.Code != "CriticEvidenceUnauthenticated" || decision.Failure.Retryable {
		t.Fatalf("decision=%#v, want terminal unauthenticated critic failure", decision)
	}

	source.artifact.Authenticated = true
	source.artifact.Input = append([]byte(criticBody), '\n')
	decision, err = driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Gate != nil || decision.Complete || decision.Failure == nil || decision.Failure.Code != "CriticEvidenceMalformed" || decision.Failure.Retryable {
		t.Fatalf("decision=%#v, want terminal noncanonical critic failure", decision)
	}
}

func TestForgedCriticResultCannotEnterGateScoring(t *testing.T) {
	input, verifierBody := criticTestInputs(t, false)
	forged := findingcorroboration.CorroborationResult{
		SchemaVersion: findingcorroboration.SchemaVersion,
		Findings:      make([]findingcorroboration.FindingDecision, 0),
		Counts:        findingcorroboration.Counts{},
	}
	forgedBody, err := findingcorroboration.CanonicalResultBytes(forged)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := criticBinding(input)
	if err != nil {
		t.Fatal(err)
	}
	ref := criticArtifactRef(forgedBody)
	runner := &fakeCriticRunner{status: CriticRunStatus{ID: "critic-run", Ready: true, Finished: true, InputRef: &ref}}
	source := &fakeCriticSource{artifact: CriticEvidenceArtifact{Input: forgedBody, Authenticated: true, Binding: binding}}
	driver := newCriticDriver(t, newFakeBackend(input, true), &fakeEvidenceSource{frame: mustEvidenceFrame(t, verifierBody)}, source, runner, newMemoryStore())
	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Gate != nil || decision.Scoring != nil || decision.Complete || decision.Failure == nil || decision.Failure.Code != "CriticEvidenceMalformed" {
		t.Fatalf("forged critic result affected Gate: decision=%#v", decision)
	}
}

func TestCriticScoringBindsRouteArtifactAndBlockingFinding(t *testing.T) {
	input, verifierBody := criticTestInputs(t, true)
	criticBody := blockingCriticInput(t)
	binding, err := criticBinding(input)
	if err != nil {
		t.Fatal(err)
	}
	ref := criticArtifactRef(criticBody)
	runner := &fakeCriticRunner{status: CriticRunStatus{ID: "critic-run", Ready: true, Finished: true, InputRef: &ref}}
	source := &fakeCriticSource{artifact: CriticEvidenceArtifact{Input: criticBody, Authenticated: true, Binding: binding}}
	store := newMemoryStore()
	driver := newCriticDriver(t, newFakeBackend(input, true), &fakeEvidenceSource{frame: mustEvidenceFrame(t, verifierBody)}, source, runner, store)
	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Gate == nil || decision.Gate.Verdict != string(gate.Rejected) || decision.Scoring == nil {
		t.Fatalf("decision=%#v, want rejected scored Gate", decision)
	}
	if decision.Scoring.CriticRoute == nil || decision.Scoring.CriticRoute.Name != "critic-route" || decision.Scoring.CriticRoute.Family != "anthropic" || decision.Scoring.CorroborationArtifactDigest != ref.Digest {
		t.Fatalf("scoring projection=%#v", decision.Scoring)
	}
	if !containsTestString(decision.Scoring.BlockingReasons, "critic.finding-1") {
		t.Fatalf("blocking reasons=%v", decision.Scoring.BlockingReasons)
	}
	if decision.Scoring.WeightedScoreBasisPoints >= input.Snapshot.Gate.Signals.MinScoreBasisPoints {
		t.Fatalf("weighted score=%d unexpectedly met minimum", decision.Scoring.WeightedScoreBasisPoints)
	}
	if got := len(decision.Artifacts); got != 4 {
		t.Fatalf("artifact count=%d, want verifier, input, derived result, and publication artifacts", got)
	}
	if decision.Artifacts[1].Kind != criticInputKind || decision.Artifacts[2].Kind != criticResultKind || decision.Artifacts[3].Kind != findingsartifact.Kind {
		t.Fatalf("critic artifact sequence=%#v", decision.Artifacts)
	}
	publicationBody := store.objects["runs/"+input.Snapshot.Run.UID+"/findings/"+trimDigest(input.SpecDigest)+"/"+trimDigest(input.Patch.Digest)+"/"+trimDigest(decision.Artifacts[3].Digest)+".json"]
	if _, derived, err := findingsartifact.Verify(publicationBody); err != nil || derived.Counts.BlockingFindings != 1 {
		t.Fatalf("publication artifact verification err=%v derived=%#v", err, derived)
	}
	detailed := decision.DetailedStatusProjection()
	if detailed.Scoring == nil || detailed.Scoring.ResultDigest != decision.Scoring.ResultDigest {
		t.Fatalf("detailed status lost scoring projection=%#v", detailed)
	}
}

func TestEvidenceFrameRoundTripAndRejectsSurroundingLogs(t *testing.T) {
	_, body := testInputs(t)
	frame := mustEvidenceFrame(t, body)
	decoded, err := DecodeEvidenceFrame(frame, DefaultMaxEvidenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, body) {
		t.Fatal("frame round trip changed evidence bytes")
	}
	for _, malformed := range [][]byte{
		nil,
		[]byte("log before\n" + string(frame)),
		append(append([]byte(nil), frame...), frame...),
		[]byte(EvidenceFramePrefix + "%%%\n"),
	} {
		if _, err := DecodeEvidenceFrame(malformed, DefaultMaxEvidenceBytes); err == nil {
			t.Fatalf("DecodeEvidenceFrame(%q) unexpectedly succeeded", malformed)
		}
	}
}

type fakeCriticRunner struct {
	status CriticRunStatus
	err    error
}

func (f *fakeCriticRunner) Ensure(context.Context, CriticRunInput) (CriticRunStatus, error) {
	if f.err != nil {
		return CriticRunStatus{}, f.err
	}
	return f.status, nil
}

type fakeCriticSource struct {
	artifact CriticEvidenceArtifact
	err      error
}

type fakeReportAttester struct {
	calls int
	last  verificationattestation.Input
	err   error
}

func (f *fakeReportAttester) Attest(_ context.Context, input verificationattestation.Input) (verificationattestation.Result, error) {
	f.calls++
	f.last = input
	f.last.SignedReport = append([]byte(nil), input.SignedReport...)
	if f.err != nil {
		return verificationattestation.Result{}, f.err
	}
	return verificationattestation.Result{
		StatementRef: v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/statement", Digest: "sha256:" + strings.Repeat("8", 64), Kind: verificationattestation.StatementKind, Name: verificationattestation.StatementName, MediaType: "application/vnd.agents-gateway.verification-attestation.v1+json", SizeBytes: 10},
		BundleRef:    v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/bundle", Digest: "sha256:" + strings.Repeat("9", 64), Kind: verificationattestation.BundleKind, Name: verificationattestation.BundleName, MediaType: verificationattestation.BundleMediaType, SizeBytes: 10},
	}, nil
}

func (f *fakeCriticSource) Read(context.Context, v1alpha1.ArtifactRef, int64) (CriticEvidenceArtifact, error) {
	if f.err != nil {
		return CriticEvidenceArtifact{}, f.err
	}
	output := f.artifact
	output.Input = append([]byte(nil), f.artifact.Input...)
	return output, nil
}

func newCriticDriver(t *testing.T, backend sandbox.SandboxBackend, source EvidenceSource, criticSource CriticEvidenceSource, runner CriticEvidenceRunner, store *memoryStore) *Driver {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	driver, err := New(Options{
		Backend: backend, Evidence: source, ReportStore: store,
		ReportSigner: SignerFunc(func(report gate.VerificationReport) (gate.SignedReport, error) { return gate.SignReport(report, key) }),
		CriticRunner: runner, CriticEvidence: criticSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func criticTestInputs(t *testing.T, blocking bool) (Inputs, []byte) {
	t.Helper()
	input, verifierBody := testInputs(t)
	input.Snapshot.Gate.Signals = &v1alpha1.GateSignalsSpec{
		ExecutionWeightBasisPoints: 7000,
		MinScoreBasisPoints:        8500,
		Critic:                     &v1alpha1.GateCriticSignalSpec{WeightBasisPoints: 3000, ModelRouteRef: "critic-route", MaxFindings: 8},
	}
	input.Snapshot.CriticModelRoute = &v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{Name: "critic-provider", Kind: "anthropic-messages", Model: "claude-test", Family: "anthropic", Priority: 1}}}
	input.Snapshot.References.CriticModelRoute = &resolved.ObjectVersion{Name: "critic-route", UID: "critic-route-uid", Generation: 2}
	specDigest, err := canonical.ResolvedSpecDigest(input.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	input.SpecDigest = specDigest
	var evidence MachineEvidence
	if err := json.Unmarshal(verifierBody, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.SpecDigest = specDigest
	verifierBody, err = json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !blocking {
		return input, verifierBody
	}
	return input, verifierBody
}

func emptyCriticInput(t *testing.T) []byte {
	t.Helper()
	input := findingcorroboration.CorroborationInput{SchemaVersion: findingcorroboration.SchemaVersion, Findings: make([]findingcorroboration.CriticFinding, 0), Evidence: make([]findingcorroboration.Evidence, 0)}
	body, err := findingcorroboration.CanonicalInputBytes(input)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func blockingCriticInput(t *testing.T) []byte {
	t.Helper()
	finding := findingcorroboration.CriticFinding{ID: "finding-1", Path: "src/main.go", Location: findingcorroboration.Location{StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2}, RuleID: "static-bug", Message: "deterministic finding"}
	evidence := findingcorroboration.Evidence{
		ID: "evidence-1", Class: findingcorroboration.ClassStatic, Binding: findingcorroboration.EvidenceBinding{FindingID: finding.ID, Path: finding.Path, Location: finding.Location, RuleID: finding.RuleID}, Immutable: true,
		ArtifactDigest: "sha256:" + strings.Repeat("1", 64), Validation: findingcorroboration.EvidenceValidation{Validator: "ast-grep", Deterministic: true, Independent: true},
		Static: &findingcorroboration.StaticEvidence{Tool: "ast-grep", Matched: true, MatchCount: 1},
	}
	sealed, err := findingcorroboration.SealEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	body, err := findingcorroboration.CanonicalInputBytes(findingcorroboration.CorroborationInput{SchemaVersion: findingcorroboration.SchemaVersion, Findings: []findingcorroboration.CriticFinding{finding}, Evidence: []findingcorroboration.Evidence{sealed}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func criticArtifactRef(body []byte) v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{URI: "s3://reports/critic-corroboration-input.json", Digest: digest(body), Kind: criticInputKind, Name: criticInputName, MediaType: criticInputMediaType, SizeBytes: int64(len(body))}
}

func containsTestString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestReconcileIsIdempotentForTheSameImmutableReport(t *testing.T) {
	input, body := testInputs(t)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{frame: mustEvidenceFrame(t, body)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	first, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Gate == nil || second.Gate == nil || first.Gate.ReportRef == nil || second.Gate.ReportRef == nil || first.Gate.ReportRef.Digest != second.Gate.ReportRef.Digest {
		t.Fatalf("report refs changed across retries: first=%#v second=%#v", first.Gate, second.Gate)
	}
	if len(store.objects) != 2 {
		t.Fatalf("object count=%d, want one evidence object and one report after retry", len(store.objects))
	}
}

func TestReconcileRejectsMalformedEvidenceButNeverAcceptsIt(t *testing.T) {
	input, _ := testInputs(t)
	malformed := []byte(`{"schemaVersion":"agents.astatide.com/verification-evidence/v1alpha1","runUID":"wrong","unexpected":true}`)
	backend := newFakeBackend(input, true)
	source := &fakeEvidenceSource{frame: rawEvidenceFrame(malformed)}
	store := newMemoryStore()
	driver := newTestDriver(t, backend, source, store)

	decision, err := driver.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Complete && decision.Gate != nil && decision.Gate.Verdict == string(gate.Accepted) {
		t.Fatal("malformed evidence was accepted")
	}
	if decision.Phase != v1alpha1.PhaseGated || decision.Gate == nil || decision.Gate.ReportRef == nil {
		t.Fatalf("decision=%#v, want bounded signed rejection", decision)
	}
	if len(decision.Artifacts) != 1 || decision.Artifacts[0].Kind != "verification-evidence" {
		t.Fatalf("strict but schema-malformed raw evidence was not retained: %#v", decision.Artifacts)
	}
}

func TestReconcileRejectsInputIdentityBeforeEnsuringSandbox(t *testing.T) {
	input, _ := testInputs(t)
	input.BaseSHA = strings.Repeat("e", 40)
	backend := newFakeBackend(input, true)
	driver := newTestDriver(t, backend, &fakeEvidenceSource{}, newMemoryStore())

	if _, err := driver.Reconcile(context.Background(), input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Reconcile() error=%v, want invalid input", err)
	}
	if backend.ensureCalls != 0 {
		t.Fatalf("ensure calls=%d, want zero for identity mismatch", backend.ensureCalls)
	}
}

type fakeBackend struct {
	ref          sandbox.SandboxRef
	observation  sandbox.SandboxObservation
	plan         sandbox.SandboxPlan
	ensureCalls  int
	observeCalls int
}

func newFakeBackend(input Inputs, finished bool) *fakeBackend {
	name, _ := sandbox.ChildName(types.UID(input.Snapshot.Run.UID), sandbox.RoleVerify)
	ref := sandbox.SandboxRef{
		Namespace:       input.Snapshot.Run.Namespace,
		Name:            name,
		Kind:            sandbox.ChildKindSandbox,
		UID:             types.UID("sandbox-uid"),
		OwnerUID:        types.UID(input.Snapshot.Run.UID),
		Role:            sandbox.RoleVerify,
		SpecDigest:      input.SpecDigest,
		PlanFingerprint: "sha256:" + strings.Repeat("f", 64),
	}
	return &fakeBackend{
		ref: ref,
		observation: sandbox.SandboxObservation{
			Ref:      ref,
			Exists:   true,
			Ready:    finished,
			Finished: finished,
		},
	}
}

func (f *fakeBackend) Ensure(_ context.Context, plan sandbox.SandboxPlan) (sandbox.SandboxRef, error) {
	f.ensureCalls++
	f.plan = plan
	return f.ref, nil
}

func (f *fakeBackend) Observe(_ context.Context, _ sandbox.SandboxRef) (sandbox.SandboxObservation, error) {
	f.observeCalls++
	return f.observation, nil
}

func (f *fakeBackend) Delete(context.Context, sandbox.SandboxRef) error { return nil }

type fakeEvidenceSource struct {
	frame   []byte
	err     error
	reads   int
	lastRef sandbox.SandboxRef
	max     int64
}

func (f *fakeEvidenceSource) ReadFrame(_ context.Context, ref sandbox.SandboxRef, max int64) ([]byte, error) {
	f.reads++
	f.lastRef = ref
	f.max = max
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), f.frame...), nil
}

type memoryStore struct {
	objects map[string][]byte
	puts    []string
}

func newMemoryStore() *memoryStore { return &memoryStore{objects: make(map[string][]byte)} }

func (s *memoryStore) Put(_ context.Context, key string, body []byte, _ string) (bool, string, error) {
	s.puts = append(s.puts, key)
	if existing, ok := s.objects[key]; ok {
		if bytes.Equal(existing, body) {
			return false, "s3://reports/" + key, nil
		}
		return false, "s3://reports/" + key, fmt.Errorf("immutable collision")
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, "s3://reports/" + key, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return append([]byte(nil), body...), nil
}

func newTestDriver(t *testing.T, backend sandbox.SandboxBackend, source EvidenceSource, store *memoryStore) *Driver {
	t.Helper()
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	key := ed25519.NewKeyFromSeed(seed)
	driver, err := New(Options{
		Backend:      backend,
		Evidence:     source,
		ReportStore:  store,
		ReportSigner: SignerFunc(func(report gate.VerificationReport) (gate.SignedReport, error) { return gate.SignReport(report, key) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func testInputs(t *testing.T) (Inputs, []byte) {
	t.Helper()
	baseSHA := strings.Repeat("d", 40)
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	snapshot := resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: "agw-runs", Name: "verify-run", UID: "1b9f4f6e-95ed-4cc1-b9ef-1e30a7f77d15", Generation: 3},
		BaseSHA:       baseSHA,
		Spec: v1alpha1.AgentRunSpec{
			Source: v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/jobmark", BaseRef: "main", Depth: 1},
			Scope:  v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
		},
		Agent: v1alpha1.AgentSpec{Runtime: v1alpha1.AgentRuntimeSpec{Harness: v1alpha1.HarnessCodex, Image: "ghcr.io/astatide/agw-runtime@" + imageDigest}},
		Gate: v1alpha1.GateSpec{
			Verify: v1alpha1.VerifySpec{
				Image: "ghcr.io/astatide/agw-verify@" + imageDigest, FromCleanCheckout: true,
				Commands: []v1alpha1.VerifyCommand{{Argv: []string{"go", "test", "./..."}}}, Timeout: "20m",
			},
			Require: v1alpha1.GateRequirements{
				ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone,
				MaxFilesChanged: 5, MaxDiffLines: 100, NoBinaryFiles: true,
			},
			OnFail: "Rejected", Mode: v1alpha1.GateShadow,
		},
		References: resolved.References{Gate: resolved.ObjectVersion{Name: "go-default", UID: "gate-uid", Generation: 4}},
	}
	specDigest, err := canonical.ResolvedSpecDigest(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	patch := v1alpha1.ArtifactRef{
		URI: "s3://agw-artifacts/runs/verify-run/patch.diff", Digest: "sha256:" + strings.Repeat("b", 64),
		Kind: "patch", Name: "patch.diff", MediaType: "text/x-diff", SizeBytes: 128,
	}
	now := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	input := Inputs{
		Snapshot: snapshot, SpecDigest: specDigest, BaseSHA: baseSHA, Patch: patch,
		Credentials:         CredentialProjection{SecretName: workload.VerifySecretName(snapshot.Run.UID), CloneSecretKey: "clone-token"},
		ArtifactStoreRegion: "us-east-1", ArtifactStoreBucket: "agw-artifacts",
		ArtifactCredentialTTL: 45 * time.Minute,
		FetchImage:            "ghcr.io/astatide/agw-fetch@" + imageDigest, ApplyImage: "ghcr.io/astatide/agw-apply@" + imageDigest, LockdownImage: "ghcr.io/astatide/agw-lockdown@" + imageDigest,
		Now: now, ShutdownTime: now.Add(20 * time.Minute), MaxShutdownDuration: time.Hour,
	}
	evidence := MachineEvidence{
		SchemaVersion: EvidenceSchemaVersion, RunUID: snapshot.Run.UID, SpecDigest: specDigest, BaseSHA: baseSHA, PatchDigest: patch.Digest,
		ChangedPaths: []string{"src/main.go"}, FilesChanged: int64Ptr(1), LinesChanged: int64Ptr(2), HasBinaryFiles: boolPtr(false),
		Commands: []CommandEvidence{{Index: 0, ExitCode: int32Ptr(0), EvidenceDigest: "sha256:" + strings.Repeat("c", 64), DurationMillis: int64Ptr(25)}},
	}
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return input, body
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func mustEvidenceFrame(t *testing.T, body []byte) []byte {
	t.Helper()
	frame, err := EncodeEvidenceFrame(body)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func rawEvidenceFrame(body []byte) []byte {
	return []byte(EvidenceFramePrefix + base64.RawStdEncoding.EncodeToString(body) + "\n")
}

func int32Ptr(value int32) *int32 { return &value }
func int64Ptr(value int64) *int64 { return &value }
func boolPtr(value bool) *bool    { return &value }
