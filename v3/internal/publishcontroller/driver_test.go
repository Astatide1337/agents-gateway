package publishcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

type memoryReader struct {
	objects map[string][]byte
	err     error
	calls   []ArtifactLocation
}

func (r *memoryReader) Get(_ context.Context, location ArtifactLocation) ([]byte, error) {
	r.calls = append(r.calls, location)
	if r.err != nil {
		return nil, r.err
	}
	body, ok := r.objects[location.Bucket+"/"+location.Key]
	if !ok {
		return nil, ErrArtifactNotFound
	}
	return append([]byte(nil), body...), nil
}

type publisherFake struct {
	result publish.Result
	err    error
	calls  []publish.Request
}

func (p *publisherFake) Publish(_ context.Context, request publish.Request) (publish.Result, error) {
	p.calls = append(p.calls, request)
	return p.result, p.err
}

type findingsPublisherFake struct {
	result publish.FindingsResult
	err    error
	calls  []publish.FindingsRequest
}

func (p *findingsPublisherFake) PublishFindings(_ context.Context, request publish.FindingsRequest) (publish.FindingsResult, error) {
	p.calls = append(p.calls, request)
	return p.result, p.err
}

type fixture struct {
	input     Input
	reader    *memoryReader
	publisher *publisherFake
	findings  *findingsPublisherFake
	private   ed25519.PrivateKey
}

func newFixture(t *testing.T, verdict gate.Verdict) fixture {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	runUID := "run-uid-1"
	specDigest := "sha256:" + strings.Repeat("b", 64)
	baseSHA := strings.Repeat("c", 40)
	patch := []byte("diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -1 +1 @@\n-old\n+new\n")
	files := []publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("new\n")}}
	manifest, manifestDigest, err := publish.MarshalPatchManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	patchDigest := publish.DigestForPatchBytes(patch)
	exitCode := int32(0)
	if verdict == gate.Rejected {
		exitCode = 1
	}
	duration := int64(10)
	filesChanged := int64(1)
	linesChanged := int64(1)
	hasBinary := false
	command := v1alpha1.VerifyCommand{Argv: []string{"go", "test"}}
	decision := gate.Evaluate(gate.Input{
		Scope:        v1alpha1.ScopeSpec{Paths: []string{"src/**"}},
		Requirements: v1alpha1.GateRequirements{ScopeRespected: true, TestStrength: v1alpha1.TestStrengthNone, MaxFilesChanged: 1, MaxDiffLines: 10, NoBinaryFiles: true},
		Commands:     []v1alpha1.VerifyCommand{command},
		Observations: gate.Observations{
			ChangedPaths:   []string{"src/main.go"},
			FilesChanged:   &filesChanged,
			LinesChanged:   &linesChanged,
			HasBinaryFiles: &hasBinary,
			Commands: []gate.CommandObservation{{
				Index: 0, ExitCode: &exitCode, EvidenceDigest: "sha256:" + strings.Repeat("e", 64), DurationMillis: &duration,
			}},
		},
	})
	if decision.Verdict != verdict {
		t.Fatalf("gate verdict=%q, want %q; checks=%#v", decision.Verdict, verdict, decision.Checks)
	}
	runtimeImage := "ghcr.io/example/runtime@sha256:" + strings.Repeat("a", 64)
	verifierImage := "ghcr.io/example/verifier@sha256:" + strings.Repeat("f", 64)
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID: runUID, SpecDigest: specDigest, BaseSHA: baseSHA, PatchDigest: patchDigest,
		GateUID: "gate-uid-1", GateGeneration: 1,
		RuntimeImageDigest: runtimeImage, VerifierImageDigest: verifierImage,
		HelperImageDigests: []gate.ImageEvidence{{Name: "lockdown", Digest: "ghcr.io/example/lockdown@sha256:" + strings.Repeat("1", 64)}},
	}, decision)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := gate.SignReport(report, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	reportBytes, err := gate.SignedReportBytes(signed)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest := digestBytes(reportBytes)
	patchKey := patchKeySuffix(runUID, specDigest, patchDigest)
	manifestKey := manifestKeySuffix(runUID, specDigest, manifestDigest)
	reportKey := reportKeySuffix(runUID, specDigest, patchDigest, reportDigest)
	reader := &memoryReader{objects: map[string][]byte{
		"agw-artifacts/" + patchKey:    patch,
		"agw-artifacts/" + manifestKey: manifest,
		"agw-artifacts/" + reportKey:   reportBytes,
	}}
	publisher := &publisherFake{result: publish.Result{State: publish.StateSucceeded, PullRequestNumber: 7, PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/7"}}
	return fixture{
		input: Input{
			RunUID: runUID, RunName: "fix-main", Repo: githubapp.Repository{Owner: "Astatide1337", Name: "agents-gateway"},
			BaseRef: "main", BaseSHA: baseSHA, SpecDigest: specDigest,
			GateUID: "gate-uid-1", GateGeneration: 1, GateMode: v1alpha1.GateShadow,
			Patch:       v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/" + patchKey, Digest: patchDigest, Kind: patchKind, Name: patchName, MediaType: patchMediaType, SizeBytes: int64(len(patch))},
			Manifest:    v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/" + manifestKey, Digest: manifestDigest, Kind: manifestKind, Name: manifestName, MediaType: manifestMediaType, SizeBytes: int64(len(manifest))},
			GateReport:  v1alpha1.ArtifactRef{URI: "s3://agw-artifacts/" + reportKey, Digest: reportDigest, Kind: reportKind, Name: reportName, MediaType: reportMediaType, SizeBytes: int64(len(reportBytes))},
			PublishMode: v1alpha1.PublishPullRequest,
		},
		reader: reader, publisher: publisher, private: privateKey,
	}
}

func newDriver(t *testing.T, f fixture) *Driver {
	t.Helper()
	publicKey := f.private.Public().(ed25519.PublicKey)
	driver, err := New(Options{Artifacts: f.reader, Publisher: f.publisher, FindingsPublisher: f.findings, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func configureFindingsOutput(t *testing.T, f *fixture, mode v1alpha1.OutputMode) {
	t.Helper()
	corroborationInput := findingsInput(t)
	derived, err := findingcorroboration.Corroborate(corroborationInput)
	if err != nil {
		t.Fatal(err)
	}
	artifact := findingsartifact.Artifact{
		Version: findingsartifact.Version, RunUID: f.input.RunUID, SpecDigest: f.input.SpecDigest,
		BaseSHA: f.input.BaseSHA, PatchDigest: f.input.Patch.Digest, GateReportDigest: f.input.GateReport.Digest,
		Input: corroborationInput, Result: derived,
	}
	body, err := findingsartifact.CanonicalBytes(artifact)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestBytes(body)
	key := findingsKeySuffix(f.input.RunUID, f.input.SpecDigest, f.input.Patch.Digest, digest)
	f.reader.objects["agw-artifacts/"+key] = body
	f.input.OutputMode = mode
	f.input.TargetPullRequest = 7
	f.input.Findings = v1alpha1.ArtifactRef{
		URI: "s3://agw-artifacts/" + key, Digest: digest, Kind: findingsKind,
		Name: findingsName, MediaType: findingsMediaType, SizeBytes: int64(len(body)),
	}
	f.findings = &findingsPublisherFake{result: publish.FindingsResult{
		State: publish.StateSucceeded, PullRequestNumber: 7,
		PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/7",
	}}
}

func assertPatchSuccess(t *testing.T, result Result) {
	t.Helper()
	if result.PatchState != StateSucceeded || result.Effect == nil || result.Effect.State != v1alpha1.EffectSucceeded || result.PullRequestURL == "" {
		t.Fatalf("patch publication was not preserved: %#v", result)
	}
}

func TestOutputBothPatchSuccessFindingsFailedKeepsIndependentStates(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)
	f.findings.result = publish.FindingsResult{State: publish.StateFailed}
	f.findings.err = errors.New("deterministic findings failure")

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("Publish() error=%v, want findings publication failure", err)
	}
	if result.State != StateFailed || result.FindingsState != StateFailed {
		t.Fatalf("aggregate/findings state=%q/%q, want failed/failed: %#v", result.State, result.FindingsState, result)
	}
	assertPatchSuccess(t, result)
	if result.PatchState != StateSucceeded || len(f.publisher.calls) != 1 || len(f.findings.calls) != 1 {
		t.Fatalf("independent publication calls/state are wrong: result=%#v patchCalls=%d findingsCalls=%d", result, len(f.publisher.calls), len(f.findings.calls))
	}
}

func TestOutputBothPatchSuccessFindingsUnknownKeepsPatchEffect(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)
	f.findings.result = publish.FindingsResult{State: publish.StateUnknown}
	f.findings.err = publish.ErrUnknownEffect

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("Publish() error=%v, want unknown findings effect", err)
	}
	if result.State != StateUnknown || result.FindingsState != StateUnknown {
		t.Fatalf("aggregate/findings state=%q/%q, want unknown/unknown: %#v", result.State, result.FindingsState, result)
	}
	assertPatchSuccess(t, result)
	if result.PatchState != StateSucceeded || result.Effect.PullRequestURL == "" || len(f.publisher.calls) != 1 || len(f.findings.calls) != 1 {
		t.Fatalf("patch effect was changed by unknown findings effect: result=%#v", result)
	}
}

func TestFindingsOnlyHasNoPatchPublicationState(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputFindings)

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil {
		t.Fatalf("Publish() error=%v", err)
	}
	if result.State != StateSucceeded || result.PatchState != StateSkipped || result.FindingsState != StateSucceeded || result.Effect != nil {
		t.Fatalf("findings-only state projection=%#v", result)
	}
	if len(f.publisher.calls) != 0 || len(f.findings.calls) != 1 {
		t.Fatalf("findings-only invoked the wrong publishers: patch=%d findings=%d", len(f.publisher.calls), len(f.findings.calls))
	}
}

func TestFindingsOnlyRequiresPreexistingPullRequestTarget(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputFindings)
	f.input.TargetPullRequest = 0

	if _, err := newDriver(t, f).Publish(context.Background(), f.input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("findings-only without target error=%v, want invalid input", err)
	}
}

func TestFindingsOnlyPreservesPreexistingPullRequestTarget(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputFindings)
	f.input.TargetPullRequest = 99
	f.findings.result = publish.FindingsResult{
		State: publish.StateSucceeded, PullRequestNumber: 99,
		PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/99",
	}

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil {
		t.Fatalf("Publish() error=%v", err)
	}
	if result.State != StateSucceeded || result.PatchState != StateSkipped || result.FindingsState != StateSucceeded || result.Effect != nil || result.PullRequestNumber != 99 {
		t.Fatalf("findings-only target/result=%#v", result)
	}
	if len(f.publisher.calls) != 0 || len(f.findings.calls) != 1 || f.findings.calls[0].TargetPullRequest != 99 {
		t.Fatalf("findings-only target was not preserved: patch=%d findings=%#v", len(f.publisher.calls), f.findings.calls)
	}
}

func TestFindingsOnlyRequiresPullRequestPublication(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputFindings)
	f.input.PublishMode = v1alpha1.PublishNone

	if _, err := newDriver(t, f).Publish(context.Background(), f.input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("findings-only publish.mode=none error=%v, want invalid input", err)
	}
}

func TestOutputBothRequiresFindingsPublisherBeforePatchMutation(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)
	publicKey := f.private.Public().(ed25519.PublicKey)
	driver, err := New(Options{Artifacts: f.reader, Publisher: f.publisher, TrustedGatePublicKey: publicKey})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := driver.Publish(context.Background(), f.input); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("OutputBoth without findings publisher error=%v, want invalid input", err)
	}
	if len(f.publisher.calls) != 0 {
		t.Fatalf("OutputBoth called patch publisher before validating findings support: %d calls", len(f.publisher.calls))
	}
}

func TestOutputBothSuccessReportsBothPublicationStates(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil {
		t.Fatalf("Publish() error=%v", err)
	}
	if result.State != StateSucceeded || result.PatchState != StateSucceeded || result.FindingsState != StateSucceeded {
		t.Fatalf("both-success state projection=%#v", result)
	}
	assertPatchSuccess(t, result)
	if len(f.publisher.calls) != 1 || len(f.findings.calls) != 1 {
		t.Fatalf("both-success publisher calls: patch=%d findings=%d", len(f.publisher.calls), len(f.findings.calls))
	}
}

func TestOutputBothDerivesFindingsTargetFromPatchPullRequest(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)
	// A predeclared target must not be used by OutputBoth. The patch publisher
	// returns PR 7, while the input deliberately names an unrelated PR.
	f.input.TargetPullRequest = 99

	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil {
		t.Fatalf("Publish() error=%v", err)
	}
	if result.State != StateSucceeded || result.PullRequestNumber != 7 || result.PullRequestURL != "https://github.com/Astatide1337/agents-gateway/pull/7" {
		t.Fatalf("patch publication identity=%#v", result)
	}
	if len(f.findings.calls) != 1 || f.findings.calls[0].TargetPullRequest != 7 {
		t.Fatalf("findings target=%d, want patch PR 7; calls=%#v", len(f.findings.calls), f.findings.calls)
	}
}

func TestOutputBothReplayKeepsDeterministicIndependentRequests(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	configureFindingsOutput(t, &f, v1alpha1.OutputBoth)
	driver := newDriver(t, f)

	first, firstErr := driver.Publish(context.Background(), f.input)
	second, secondErr := driver.Publish(context.Background(), f.input)
	if firstErr != nil || secondErr != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("both replay results differ: first=%#v/%v second=%#v/%v", first, firstErr, second, secondErr)
	}
	if len(f.publisher.calls) != 2 || !reflect.DeepEqual(f.publisher.calls[0], f.publisher.calls[1]) {
		t.Fatalf("patch replay request changed")
	}
	if len(f.findings.calls) != 2 || !reflect.DeepEqual(f.findings.calls[0], f.findings.calls[1]) {
		t.Fatalf("findings replay request changed")
	}
}

func TestPublishBuildsRequestAndReturnsBoundedStatus(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if result.State != StateSucceeded || result.Effect == nil || result.Effect.State != v1alpha1.EffectSucceeded || result.PullRequestURL == "" || result.Effect.PullRequestURL != result.PullRequestURL {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(f.publisher.calls) != 1 {
		t.Fatalf("publisher calls=%d, want 1", len(f.publisher.calls))
	}
	request := f.publisher.calls[0]
	if request.RunUID != f.input.RunUID || request.SpecDigest != f.input.SpecDigest || request.BaseSHA != f.input.BaseSHA || request.Gate.ReportDigest != f.input.GateReport.Digest || request.Gate.Verdict != string(gate.Accepted) {
		t.Fatalf("request identity was not bound: %#v", request)
	}
	if publish.DigestForPatchBytes(request.Patch.Raw) != f.input.Patch.Digest || len(request.Patch.Files) != 1 || request.Patch.Files[0].Path != "src/main.go" {
		t.Fatalf("request patch was not verified: %#v", request.Patch)
	}
}

func TestReplayProducesTheSameStatusAndRequest(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	driver := newDriver(t, f)
	first, firstErr := driver.Publish(context.Background(), f.input)
	second, secondErr := driver.Publish(context.Background(), f.input)
	if firstErr != nil || secondErr != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("replay results differ: first=%#v/%v second=%#v/%v", first, firstErr, second, secondErr)
	}
	if len(f.publisher.calls) != 2 || !reflect.DeepEqual(f.publisher.calls[0], f.publisher.calls[1]) {
		t.Fatalf("replay did not receive the same deterministic request")
	}
}

func TestReplayUsesEffectLedgerWithoutSecondGitHubMutation(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	store := &effectStore{objects: make(map[string][]byte)}
	ledger, err := effects.New(store, "effects", func() time.Time { return time.Unix(100, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	client := &countingPublishClient{baseSHA: f.input.BaseSHA}
	publisher, err := publish.New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := New(Options{Artifacts: f.reader, Publisher: publisher, TrustedGatePublicKey: f.private.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	first, firstErr := driver.Publish(context.Background(), f.input)
	second, secondErr := driver.Publish(context.Background(), f.input)
	if firstErr != nil || secondErr != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("ledger replay first=%#v/%v second=%#v/%v", first, firstErr, second, secondErr)
	}
	if client.mutations != 4 {
		t.Fatalf("GitHub mutation/read count=%d, want 4 on first publication only", client.mutations)
	}
}

func TestUnknownEffectIsTerminalAndRedacted(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	secret := "s3://agw-artifacts/runs/run-uid-1/publish/installation-token-secret"
	f.publisher.result = publish.Result{State: publish.StateUnknown}
	f.publisher.err = errors.New(secret)
	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if result.State != StateUnknown || result.Effect == nil || result.Effect.State != v1alpha1.EffectUnknown || !errors.Is(err, ErrUnknownEffect) {
		t.Fatalf("unknown result=%#v err=%v", result, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown error leaked provider detail: %q", err)
	}
}

func TestTamperedPatchAndManifestFailClosed(t *testing.T) {
	t.Run("patch", func(t *testing.T) {
		f := newFixture(t, gate.Accepted)
		f.reader.objects["agw-artifacts/"+patchKeySuffix(f.input.RunUID, f.input.SpecDigest, f.input.Patch.Digest)] = []byte("tampered")
		_, err := newDriver(t, f).Publish(context.Background(), f.input)
		if !errors.Is(err, ErrArtifactDigestMismatch) {
			t.Fatalf("error=%v, want patch digest mismatch", err)
		}
	})
	t.Run("manifest", func(t *testing.T) {
		f := newFixture(t, gate.Accepted)
		key := "agw-artifacts/" + manifestKeySuffix(f.input.RunUID, f.input.SpecDigest, f.input.Manifest.Digest)
		f.reader.objects[key] = append(append([]byte(nil), f.reader.objects[key]...), '\n')
		_, err := newDriver(t, f).Publish(context.Background(), f.input)
		if !errors.Is(err, ErrManifestInvalid) && !errors.Is(err, ErrArtifactDigestMismatch) {
			t.Fatalf("error=%v, want manifest integrity failure", err)
		}
	})
}

func TestEnforcingRejectionDoesNotPublishButShadowRejectionDoes(t *testing.T) {
	t.Run("enforcing", func(t *testing.T) {
		f := newFixture(t, gate.Rejected)
		f.input.GateMode = v1alpha1.GateEnforcing
		result, err := newDriver(t, f).Publish(context.Background(), f.input)
		if err != nil || result.State != StateRejected || result.Effect != nil || len(f.publisher.calls) != 0 {
			t.Fatalf("enforcing rejection result=%#v err=%v calls=%d", result, err, len(f.publisher.calls))
		}
	})
	t.Run("shadow", func(t *testing.T) {
		f := newFixture(t, gate.Rejected)
		result, err := newDriver(t, f).Publish(context.Background(), f.input)
		if err != nil || result.State != StateSucceeded || len(f.publisher.calls) != 1 || f.publisher.calls[0].Gate.Verdict != string(gate.Rejected) || f.publisher.calls[0].Gate.Mode != v1alpha1.GateShadow {
			t.Fatalf("shadow rejection result=%#v err=%v calls=%d", result, err, len(f.publisher.calls))
		}
	})
}

func TestMissingManifestAndMalformedURIFailClosed(t *testing.T) {
	t.Run("missing manifest", func(t *testing.T) {
		f := newFixture(t, gate.Accepted)
		f.input.Manifest = v1alpha1.ArtifactRef{}
		_, err := newDriver(t, f).Publish(context.Background(), f.input)
		if !errors.Is(err, ErrArtifactMissing) {
			t.Fatalf("error=%v, want missing manifest", err)
		}
	})
	t.Run("malformed URI", func(t *testing.T) {
		f := newFixture(t, gate.Accepted)
		f.input.Patch.URI = "https://objects.example/patch.diff?secret=token"
		_, err := newDriver(t, f).Publish(context.Background(), f.input)
		if !errors.Is(err, ErrMalformedArtifactURI) {
			t.Fatalf("error=%v, want malformed URI", err)
		}
	})
}

func TestRunIdentityMustMatchTheSignedReportAndArtifactKeys(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Input)
	}{
		{name: "run UID", mutate: func(input *Input) { input.RunUID = "different-run" }},
		{name: "spec digest", mutate: func(input *Input) { input.SpecDigest = "sha256:" + strings.Repeat("1", 64) }},
		{name: "base SHA", mutate: func(input *Input) { input.BaseSHA = strings.Repeat("d", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t, gate.Accepted)
			test.mutate(&f.input)
			_, err := newDriver(t, f).Publish(context.Background(), f.input)
			if !errors.Is(err, ErrArtifactIdentity) {
				t.Fatalf("error=%v, want identity mismatch", err)
			}
		})
	}
}

func TestReaderErrorsAreRedacted(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	secret := "s3://agw-artifacts/runs/run-uid-1/secret-installation-token"
	f.reader.err = errors.New(secret)
	_, err := newDriver(t, f).Publish(context.Background(), f.input)
	if !errors.Is(err, ErrArtifactUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("redaction failure: err=%v", err)
	}
}

func TestPendingOutcomeIsStatusReady(t *testing.T) {
	f := newFixture(t, gate.Accepted)
	f.publisher.err = ErrPending
	result, err := newDriver(t, f).Publish(context.Background(), f.input)
	if err != nil || result.State != StatePending || result.Effect == nil || result.Effect.State != v1alpha1.EffectPending || result.Effect.Key == "" || result.PullRequestURL != "" {
		t.Fatalf("pending result=%#v err=%v", result, err)
	}
}

func TestStoreReaderUsesConfiguredBucketAndPrefix(t *testing.T) {
	store := &keyStore{body: []byte("ok")}
	reader, err := NewStoreReader(store, "agw-artifacts", "objects")
	if err != nil {
		t.Fatal(err)
	}
	body, err := reader.Get(context.Background(), ArtifactLocation{Bucket: "agw-artifacts", Key: "objects/runs/run/file"})
	if err != nil || !bytes.Equal(body, []byte("ok")) || store.key != "runs/run/file" {
		t.Fatalf("reader body=%q err=%v key=%q", body, err, store.key)
	}
}

type keyStore struct {
	key  string
	body []byte
}

func (s *keyStore) Get(_ context.Context, key string) ([]byte, error) {
	s.key = key
	return append([]byte(nil), s.body...), nil
}

func (s *keyStore) Put(_ context.Context, _ string, _ []byte, _ string) (bool, string, error) {
	return false, "", nil
}

type effectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *effectStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, nil
}

func (s *effectStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, effects.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

type countingPublishClient struct {
	baseSHA   string
	mutations int
}

func (c *countingPublishClient) GetRef(_ context.Context, _ githubapp.Repository, name string) (publish.Reference, error) {
	c.mutations++
	return publish.Reference{Name: name, SHA: c.baseSHA}, nil
}

func (c *countingPublishClient) EnsureBranch(_ context.Context, _ githubapp.Repository, request publish.BranchRequest) (publish.BranchResult, error) {
	c.mutations++
	return publish.BranchResult{Name: request.Name, SHA: request.BaseSHA}, nil
}

func (c *countingPublishClient) EnsureCommit(_ context.Context, _ githubapp.Repository, request publish.CommitRequest) (publish.CommitResult, error) {
	c.mutations++
	return publish.CommitResult{SHA: strings.Repeat("d", 40), BranchName: request.BranchName, BaseSHA: request.BaseSHA, PatchDigest: request.PatchDigest, ManifestDigest: request.ManifestDigest, Author: publish.CommitAuthor{Name: "agw-bot", Email: "agw-bot@users.noreply.github.com"}}, nil
}

func (c *countingPublishClient) EnsurePullRequest(_ context.Context, repo githubapp.Repository, request publish.PullRequestRequest) (publish.PullRequestResult, error) {
	c.mutations++
	return publish.PullRequestResult{Number: 7, URL: "https://github.com/" + repo.FullName() + "/pull/7", BaseRef: request.BaseRef, HeadBranch: request.HeadBranch, EffectKey: request.EffectKey}, nil
}

func (c *countingPublishClient) EnsureLabels(context.Context, githubapp.Repository, publish.LabelRequest) error {
	return nil
}
