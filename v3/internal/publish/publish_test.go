package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
)

const testSecret = "ghs_super_secret_should_never_escape"

func TestPublishSuccessClaimsBeforeEveryMutationAndCarriesEvidence(t *testing.T) {
	client := newFakeClient()
	store := newEffectStore()
	ledger, err := effects.New(store, "effects", nil)
	if err != nil {
		t.Fatal(err)
	}

	trace := make([]string, 0, 8)
	client.trace = &trace
	ledgerWrapper := &tracingLedger{Ledger: ledger, trace: &trace}
	publisher, err := New(client, ledgerWrapper)
	if err != nil {
		t.Fatal(err)
	}

	request := validRequest(t)
	result, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateSucceeded || result.PullRequestNumber != 42 || result.PullRequestURL == "" || result.CommitSHA == "" {
		t.Fatalf("unexpected result: %#v", result)
	}
	expectedEffectKey, err := effects.EffectKey(request.RunUID, request.BaseSHA, request.Patch.Digest, Operation)
	if err != nil || result.EffectKey != expectedEffectKey {
		t.Fatalf("effect key = %q, want %q (err=%v)", result.EffectKey, expectedEffectKey, err)
	}
	if got, want := trace, []string{"claim", "get-ref", "ensure-branch", "ensure-commit", "ensure-pr", "ensure-labels", "commit-outcome"}; !equalStrings(got, want) {
		t.Fatalf("operation order = %v, want %v", got, want)
	}
	if client.commitRequest.Author != agwBotAuthor {
		t.Fatalf("commit author = %#v, want %#v", client.commitRequest.Author, agwBotAuthor)
	}
	if client.commitRequest.BaseSHA != request.BaseSHA || client.commitRequest.PatchDigest != request.Patch.Digest || client.commitRequest.ManifestDigest != request.Patch.ManifestDigest {
		t.Fatalf("commit was not bound to exact base/patch: %#v", client.commitRequest)
	}
	if !strings.Contains(client.prRequest.Body, request.Gate.ReportDigest) ||
		!strings.Contains(client.prRequest.Body, request.SpecDigest) ||
		!strings.Contains(client.prRequest.Body, request.RuntimeImageDigest) ||
		!strings.Contains(client.prRequest.Body, request.SkillDigests[0]) ||
		!strings.Contains(client.prRequest.Body, result.EffectKey) {
		t.Fatalf("PR body omitted required evidence: %s", client.prRequest.Body)
	}
	wantLabels := []string{"agw/accepted", "agw/shadow", "triage"}
	if !equalStrings(client.labelRequest.Labels, wantLabels) {
		t.Fatalf("labels = %v, want %v", client.labelRequest.Labels, wantLabels)
	}
	if strings.Contains(client.prRequest.Body, testSecret) {
		t.Fatal("PR body contains a credential")
	}
}

func TestPublishReplayDoesNotCallGitHubAgain(t *testing.T) {
	client := newFakeClient()
	ledger := newTestLedger(t)
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(t)
	first, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	client.calls = nil
	second, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !equalResults(first, second) {
		t.Fatalf("replay result = %#v, first = %#v", second, first)
	}
	if len(client.calls) != 0 {
		t.Fatalf("replay made GitHub calls: %v", client.calls)
	}
}

func TestEffectResultEncodingRequiresBoundedPullRequestNumber(t *testing.T) {
	base := Result{
		State:          StateSucceeded,
		EffectKey:      digestForTest("effect"),
		RequestDigest:  digestForTest("request"),
		BranchName:     "agw/fix-1",
		CommitSHA:      strings.Repeat("a", 40),
		PullRequestURL: "https://github.com/Astatide1337/jobmark/pull/42",
	}
	for _, number := range []int64{0, -1, MaxPullRequestNumber + 1} {
		t.Run(fmt.Sprintf("number-%d", number), func(t *testing.T) {
			encoded := encodeResult(Result{
				State:             base.State,
				EffectKey:         base.EffectKey,
				RequestDigest:     base.RequestDigest,
				BranchName:        base.BranchName,
				CommitSHA:         base.CommitSHA,
				PullRequestNumber: number,
				PullRequestURL:    base.PullRequestURL,
			})
			if _, ok := decodeResult(encoded); ok {
				t.Fatalf("decodeResult accepted unbounded pull request number %d", number)
			}
		})
	}
	base.PullRequestNumber = 42
	if decoded, ok := decodeResult(encodeResult(base)); !ok || decoded.PullRequestNumber != 42 {
		t.Fatalf("valid result did not round-trip: %#v, ok=%v", decoded, ok)
	}
}

func TestAmbiguousClaimNeverCallsGitHub(t *testing.T) {
	client := newFakeClient()
	ledger := &fakeLedger{claimErr: errors.New("timeout Authorization: " + testSecret)}
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), validRequest(t))
	if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
		t.Fatalf("ambiguous claim = %#v, %v", result, err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("ambiguous claim called GitHub: %v", client.calls)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("claim error leaked credential")
	}
}

func TestIncompleteClaimIsTerminalUnknownAndNeverReexecuted(t *testing.T) {
	client := newFakeClient()
	ledger := newTestLedger(t)
	request := validRequest(t)
	prepared, err := prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	// Claim once without an outcome, simulating a controller crash after the
	// durable claim and before the first GitHub call.
	if decision, err := ledger.Claim(context.Background(), effects.Claim{
		EffectKey: prepared.effectKey, RequestDigest: prepared.requestDigest, Operation: Operation, RunUID: request.RunUID,
	}); err != nil || !decision.Execute {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("initial incomplete claim did not execute")
	}
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), request)
	if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
		t.Fatalf("incomplete claim = %#v, %v", result, err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("incomplete claim called GitHub: %v", client.calls)
	}
}

func TestAmbiguousBoundaryAtEveryGitHubMutationStopsPipeline(t *testing.T) {
	steps := []string{"get-ref", "ensure-branch", "ensure-commit", "ensure-pr", "ensure-labels"}
	for _, step := range steps {
		t.Run(step, func(t *testing.T) {
			client := newFakeClient()
			client.failStep = step
			client.failErr = errors.New("transport body token=" + testSecret)
			publisher, err := New(client, newTestLedger(t))
			if err != nil {
				t.Fatal(err)
			}
			result, err := publisher.Publish(context.Background(), validRequest(t))
			if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
				t.Fatalf("ambiguous %s = %#v, %v", step, result, err)
			}
			if strings.Contains(err.Error(), testSecret) {
				t.Fatal("GitHub error leaked credential")
			}
			if index := indexOf(client.calls, step); index >= 0 && index+1 < len(client.calls) {
				t.Fatalf("pipeline continued after ambiguous %s: %v", step, client.calls)
			}
		})
	}
}

func TestAmbiguousLedgerCommitIsUnknownWhenReadCannotProveOutcome(t *testing.T) {
	client := newFakeClient()
	ledger := &fakeLedger{commitErr: errors.New("write timeout token=" + testSecret), readErr: effects.ErrNotFound}
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), validRequest(t))
	if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
		t.Fatalf("ambiguous outcome commit = %#v, %v", result, err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("ledger error leaked credential")
	}
}

func TestBaseConflictIsDurablyFailedBeforeBranchMutation(t *testing.T) {
	client := newFakeClient()
	client.ref.SHA = strings.Repeat("b", 40)
	ledger := newTestLedger(t)
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(t)
	result, err := publisher.Publish(context.Background(), request)
	if !errors.Is(err, ErrBaseMismatch) || result.State != StateFailed {
		t.Fatalf("base conflict = %#v, %v", result, err)
	}
	if len(client.calls) != 1 || client.calls[0] != "get-ref" {
		t.Fatalf("base conflict continued: %v", client.calls)
	}
	outcome, err := readOutcomeFor(t, ledger, request)
	if err != nil || outcome.State != effects.OutcomeFailed {
		t.Fatalf("base conflict outcome = %#v, %v", outcome, err)
	}
}

func TestKnownGitHubConflictIsFailedNotUnknown(t *testing.T) {
	client := newFakeClient()
	client.failStep = "ensure-branch"
	client.failErr = ErrGitHubConflict
	publisher, err := New(client, newTestLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), validRequest(t))
	if !errors.Is(err, errDeterministicCommit) || result.State != StateFailed {
		t.Fatalf("known branch conflict = %#v, %v", result, err)
	}
	if len(client.calls) != 2 || client.calls[1] != "ensure-branch" {
		t.Fatalf("known conflict continued: %v", client.calls)
	}
}

func TestUnprovenClientProofIsUnknown(t *testing.T) {
	t.Run("base ref identity", func(t *testing.T) {
		client := newFakeClient()
		client.ref.Name = "refs/heads/not-main"
		publisher, err := New(client, newTestLedger(t))
		if err != nil {
			t.Fatal(err)
		}
		result, err := publisher.Publish(context.Background(), validRequest(t))
		if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
			t.Fatalf("invalid base proof = %#v, %v", result, err)
		}
		if len(client.calls) != 1 {
			t.Fatalf("invalid base proof continued: %v", client.calls)
		}
	})

	t.Run("pull request repository", func(t *testing.T) {
		client := newFakeClient()
		client.prResult = PullRequestResult{
			Number:     42,
			URL:        "https://github.com/other/repo/pull/42",
			BaseRef:    "main",
			HeadBranch: "", // filled below after the branch proof is observed
		}
		request := validRequest(t)
		branch, err := BranchName(request.RunName, request.RunUID)
		if err != nil {
			t.Fatal(err)
		}
		client.prResult.HeadBranch = branch
		client.prResult.EffectKey, err = effects.EffectKey(request.RunUID, request.BaseSHA, request.Patch.Digest, Operation)
		if err != nil {
			t.Fatal(err)
		}
		publisher, err := New(client, newTestLedger(t))
		if err != nil {
			t.Fatal(err)
		}
		result, err := publisher.Publish(context.Background(), request)
		if !errors.Is(err, ErrUnknownEffect) || result.State != StateUnknown {
			t.Fatalf("invalid PR proof = %#v, %v", result, err)
		}
		if indexOf(client.calls, "ensure-labels") >= 0 {
			t.Fatalf("invalid PR proof continued to labels: %v", client.calls)
		}
	})
}

func TestRejectedEnforcingGateDoesNotClaimOrPublish(t *testing.T) {
	client := newFakeClient()
	ledger := &fakeLedger{}
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(t)
	request.Gate.Mode = v1alpha1.GateEnforcing
	request.Gate.Verdict = "Rejected"
	result, err := publisher.Publish(context.Background(), request)
	if err != nil || result.State != StateRejected {
		t.Fatalf("rejected enforcing = %#v, %v", result, err)
	}
	if ledger.claims != 0 || len(client.calls) != 0 {
		t.Fatalf("rejected enforcing had side effects: claims=%d calls=%v", ledger.claims, client.calls)
	}
}

func TestRejectedShadowPublishesRejectedShadowLabels(t *testing.T) {
	client := newFakeClient()
	publisher, err := New(client, newTestLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	request := validRequest(t)
	request.Gate.Verdict = "Rejected"
	result, err := publisher.Publish(context.Background(), request)
	if err != nil || result.State != StateSucceeded {
		t.Fatalf("rejected shadow = %#v, %v", result, err)
	}
	if !equalStrings(client.labelRequest.Labels[:3], []string{"agw/rejected", "agw/shadow", "triage"}) {
		t.Fatalf("rejected labels = %v", client.labelRequest.Labels)
	}
}

func TestPublishNoneSkipsValidationLedgerAndClient(t *testing.T) {
	client := newFakeClient()
	ledger := &fakeLedger{}
	publisher, err := New(client, ledger)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(context.Background(), Request{PublishMode: v1alpha1.PublishNone})
	if err != nil || result.State != StateSkipped {
		t.Fatalf("publish none = %#v, %v", result, err)
	}
	if ledger.claims != 0 || len(client.calls) != 0 {
		t.Fatalf("publish none had side effects")
	}
}

func TestPatchDigestAndEvidenceValidationAreExact(t *testing.T) {
	request := validRequest(t)
	request.Patch.Files[0].Content = []byte("changed")
	publisher, err := New(newFakeClient(), newTestLedger(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("patch digest mismatch = %v", err)
	}

	files := []FileChange{{Path: "empty.txt", Mode: "100644", Content: []byte{}}}
	digest, err := DigestForPatch(files)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("empty regular file did not receive a digest")
	}
	if _, err := DigestForPatch([]FileChange{{Path: "empty.txt", Mode: "100644"}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("nil content for a regular file was accepted")
	}
	request = validRequest(t)
	request.Patch.Raw = append(request.Patch.Raw, 'x')
	if _, err := publisher.Publish(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("raw patch digest mismatch = %v", err)
	}
}

func TestPatchManifestCanonicalRoundTrip(t *testing.T) {
	files := []FileChange{
		{Path: "z/delete.txt", Mode: "100644", Delete: true},
		{Path: "a/main.go", Mode: "100755", Content: []byte("package main\n")},
	}
	raw, digest, err := MarshalPatchManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePatchManifest(raw, digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[0].Path != "a/main.go" || decoded[1].Path != "z/delete.txt" || decoded[1].Content != nil {
		t.Fatalf("unexpected canonical manifest: %#v", decoded)
	}
	if _, err := DecodePatchManifest(append(append([]byte(nil), raw...), '\n'), digest); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-canonical manifest error=%v", err)
	}
	if _, err := DecodePatchManifest(raw, DigestForPatchBytes([]byte("other"))); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("digest mismatch error=%v", err)
	}
}

func TestPatchManifestPreservesZeroByteAndDeletedContent(t *testing.T) {
	files := []FileChange{
		{Path: "empty.txt", Mode: "100644", Content: []byte{}},
		{Path: "deleted.txt", Mode: "100644", Delete: true},
	}
	raw, digest, err := MarshalPatchManifest(files)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePatchManifest(raw, digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded %d files, want 2: %#v", len(decoded), decoded)
	}
	if decoded[0].Path != "deleted.txt" || !decoded[0].Delete || decoded[0].Content != nil {
		t.Fatalf("deleted file lost nil content: %#v", decoded[0])
	}
	if decoded[1].Path != "empty.txt" || decoded[1].Delete || decoded[1].Content == nil || len(decoded[1].Content) != 0 {
		t.Fatalf("zero-byte file lost non-nil empty content: %#v", decoded[1])
	}
}

func TestBranchNameAndBodyAreBoundedAndDeterministic(t *testing.T) {
	request := validRequest(t)
	branch, err := BranchName(request.RunName, request.RunUID)
	if err != nil {
		t.Fatal(err)
	}
	branchAgain, err := BranchName(request.RunName, request.RunUID)
	if err != nil || branch != branchAgain || len(branch) > branchNameMaxBytes {
		t.Fatalf("branch = %q, again = %q, err=%v", branch, branchAgain, err)
	}
	effectKey := digestForTest("effect")
	body, err := RenderPRBody(request, effectKey, branch)
	if err != nil || len(body) > MaxBodyBytes {
		t.Fatalf("body len=%d err=%v", len(body), err)
	}
	if strings.Contains(body, "Authorization") || strings.Contains(body, "Bearer") || strings.Contains(body, testSecret) {
		t.Fatal("evidence body contains credential-like material")
	}
}

type fakeClient struct {
	mu sync.Mutex

	calls    []string
	trace    *[]string
	failStep string
	failErr  error

	ref          Reference
	branchResult BranchResult
	commitResult CommitResult
	prResult     PullRequestResult

	commitRequest CommitRequest
	prRequest     PullRequestRequest
	labelRequest  LabelRequest
}

func newFakeClient() *fakeClient {
	return &fakeClient{ref: Reference{Name: "refs/heads/main", SHA: strings.Repeat("a", 40)}}
}

func (f *fakeClient) step(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.trace != nil {
		*f.trace = append(*f.trace, name)
	}
	if f.failStep == name {
		return f.failErr
	}
	return nil
}

func (f *fakeClient) GetRef(_ context.Context, _ githubapp.Repository, _ string) (Reference, error) {
	if err := f.step("get-ref"); err != nil {
		return Reference{}, err
	}
	return f.ref, nil
}

func (f *fakeClient) EnsureBranch(_ context.Context, _ githubapp.Repository, request BranchRequest) (BranchResult, error) {
	if err := f.step("ensure-branch"); err != nil {
		return BranchResult{}, err
	}
	if f.branchResult.Name == "" {
		return BranchResult{Name: request.Name, SHA: request.BaseSHA}, nil
	}
	return f.branchResult, nil
}

func (f *fakeClient) EnsureCommit(_ context.Context, _ githubapp.Repository, request CommitRequest) (CommitResult, error) {
	if err := f.step("ensure-commit"); err != nil {
		return CommitResult{}, err
	}
	f.commitRequest = request
	if f.commitResult.SHA == "" {
		return CommitResult{SHA: strings.Repeat("c", 40), BranchName: request.BranchName, BaseSHA: request.BaseSHA, PatchDigest: request.PatchDigest, ManifestDigest: request.ManifestDigest, Author: request.Author}, nil
	}
	return f.commitResult, nil
}

func (f *fakeClient) EnsurePullRequest(_ context.Context, _ githubapp.Repository, request PullRequestRequest) (PullRequestResult, error) {
	if err := f.step("ensure-pr"); err != nil {
		return PullRequestResult{}, err
	}
	f.prRequest = request
	if f.prResult.Number == 0 {
		return PullRequestResult{Number: 42, URL: "https://github.com/Astatide1337/jobmark/pull/42", BaseRef: request.BaseRef, HeadBranch: request.HeadBranch, EffectKey: request.EffectKey}, nil
	}
	return f.prResult, nil
}

func (f *fakeClient) EnsureLabels(_ context.Context, _ githubapp.Repository, request LabelRequest) error {
	if err := f.step("ensure-labels"); err != nil {
		return err
	}
	f.labelRequest = request
	return nil
}

type effectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newEffectStore() *effectStore { return &effectStore{objects: map[string][]byte{}} }

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

type tracingLedger struct {
	Ledger effectsLedger
	trace  *[]string
}

type effectsLedger interface {
	Claim(context.Context, effects.Claim) (effects.ClaimDecision, error)
	Commit(context.Context, effects.Outcome) error
	ReadOutcome(context.Context, string, string) (effects.Outcome, error)
}

func (l *tracingLedger) Claim(ctx context.Context, claim effects.Claim) (effects.ClaimDecision, error) {
	*l.trace = append(*l.trace, "claim")
	return l.Ledger.Claim(ctx, claim)
}

func (l *tracingLedger) Commit(ctx context.Context, outcome effects.Outcome) error {
	*l.trace = append(*l.trace, "commit-outcome")
	return l.Ledger.Commit(ctx, outcome)
}

func (l *tracingLedger) ReadOutcome(ctx context.Context, key, digest string) (effects.Outcome, error) {
	return l.Ledger.ReadOutcome(ctx, key, digest)
}

type fakeLedger struct {
	mu sync.Mutex

	claims int

	claimDecision effects.ClaimDecision
	claimErr      error
	commitErr     error
	readErr       error
	outcome       effects.Outcome
}

func (l *fakeLedger) Claim(_ context.Context, _ effects.Claim) (effects.ClaimDecision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims++
	if l.claimErr != nil {
		return l.claimDecision, l.claimErr
	}
	if l.claimDecision.Execute || l.claimDecision.Unknown {
		return l.claimDecision, nil
	}
	return effects.ClaimDecision{Execute: true}, nil
}

func (l *fakeLedger) Commit(_ context.Context, outcome effects.Outcome) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.commitErr != nil {
		return l.commitErr
	}
	l.outcome = outcome
	return nil
}

func (l *fakeLedger) ReadOutcome(_ context.Context, _, _ string) (effects.Outcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.readErr != nil {
		return effects.Outcome{}, l.readErr
	}
	if l.outcome.State == "" {
		return effects.Outcome{}, effects.ErrNotFound
	}
	return l.outcome, nil
}

func newTestLedger(t *testing.T) *effects.Ledger {
	t.Helper()
	ledger, err := effects.New(newEffectStore(), "effects", nil)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func validRequest(t *testing.T) Request {
	t.Helper()
	files := []FileChange{{Path: "src/main.go", Mode: "100644", Content: []byte("package main\n")}}
	manifestDigest, err := DigestForPatch(files)
	if err != nil {
		t.Fatal(err)
	}
	rawPatch := []byte("diff --git a/src/main.go b/src/main.go\n--- a/src/main.go\n+++ b/src/main.go\n@@ -0,0 +1 @@\n+package main\n")
	patchDigest := DigestForPatchBytes(rawPatch)
	return Request{
		RunUID:              "2a6f92b4-7fc7-4b6b-ae1e-1a7cbf3d7a10",
		RunName:             "jobmark-fix-427",
		Repo:                githubapp.Repository{Owner: "Astatide1337", Name: "jobmark"},
		BaseRef:             "main",
		BaseSHA:             strings.Repeat("a", 40),
		Patch:               Patch{Digest: patchDigest, Raw: rawPatch, ManifestDigest: manifestDigest, Files: files},
		SpecDigest:          digestForTest("spec"),
		Gate:                GateState{Mode: v1alpha1.GateShadow, Verdict: "Accepted", ReportDigest: digestForTest("report")},
		RuntimeImageDigest:  "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("1", 64),
		VerifierImageDigest: "ghcr.io/astatide/agw-verifier@sha256:" + strings.Repeat("2", 64),
		HelperImageDigests:  []ImageEvidence{{Name: "clone", Digest: "ghcr.io/astatide/agw-clone@sha256:" + strings.Repeat("3", 64)}},
		SkillDigests:        []string{digestForTest("skill")},
		PublishMode:         v1alpha1.PublishPullRequest,
		Labels:              []string{"triage"},
	}
}

func readOutcomeFor(t *testing.T, ledger *effects.Ledger, request Request) (effects.Outcome, error) {
	t.Helper()
	prepared, err := prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.ReadOutcome(context.Background(), prepared.effectKey, prepared.requestDigest)
}

func digestForTest(value string) string {
	return "sha256:" + strings.Repeat(fmt.Sprintf("%x", value[0]), 64)[:64]
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalResults(left, right Result) bool {
	return left.State == right.State && left.EffectKey == right.EffectKey && left.RequestDigest == right.RequestDigest && left.BranchName == right.BranchName && left.CommitSHA == right.CommitSHA && left.PullRequestNumber == right.PullRequestNumber && left.PullRequestURL == right.PullRequestURL
}
