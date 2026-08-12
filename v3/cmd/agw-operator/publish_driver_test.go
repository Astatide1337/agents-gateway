package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/publishcontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type publicationDriverFake struct {
	result publishcontroller.Result
	err    error
	input  publishcontroller.Input
	calls  int
}

func (f *publicationDriverFake) Publish(_ context.Context, input publishcontroller.Input) (publishcontroller.Result, error) {
	f.calls++
	f.input = input
	return f.result, f.err
}

func TestPublishAdapterUsesOnlyImmutableSnapshotAndEvidence(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	// Simulate a mutable post-admission edit. Publication must ignore it.
	run.Spec.Source.Repo = "github.com/attacker/redirected"
	run.Spec.Publish.Title = "mutable title"
	run.Spec.Publish.Labels = []string{"mutable"}
	effect := &v1alpha1.EffectSummary{Key: "sha256:" + strings.Repeat("a", 64), State: v1alpha1.EffectSucceeded, PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/1"}
	driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded, Effect: effect, PullRequestURL: effect.PullRequestURL}}

	outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Succeeded || outcome.Effect == nil || driver.calls != 1 {
		t.Fatalf("unexpected outcome=%#v calls=%d", outcome, driver.calls)
	}
	if driver.input.Repo.Owner != "Astatide1337" || driver.input.Repo.Name != "agents-gateway" || driver.input.Title != "immutable title" || len(driver.input.Labels) != 1 || driver.input.Labels[0] != "immutable" {
		t.Fatalf("publication was redirected by mutable spec: %#v", driver.input)
	}
	if driver.input.GateUID != "gate-uid" || driver.input.GateGeneration != 7 || driver.input.Patch.Digest != run.Status.Patch.Ref.Digest || driver.input.Manifest.Digest != run.Status.Patch.ManifestRef.Digest || driver.input.GateReport.Digest != run.Status.Gate.ReportRef.Digest {
		t.Fatalf("immutable evidence identity was not projected: %#v", driver.input)
	}
}

func TestPublishAdapterMapsAmbiguousEffectToTerminalUnknown(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	effect := &v1alpha1.EffectSummary{Key: "sha256:" + strings.Repeat("a", 64), State: v1alpha1.EffectUnknown}
	driver := &publicationDriverFake{
		result: publishcontroller.Result{State: publishcontroller.StateUnknown, Effect: effect},
		err:    publishcontroller.ErrUnknownEffect,
	}
	outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
	if err != nil || !outcome.Unknown || outcome.Effect == nil || outcome.Effect.State != v1alpha1.EffectUnknown {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func TestPublishAdapterFailsClosedOnIncompleteEvidence(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	run.Status.Patch.ManifestRef = nil
	_, err := (publishPhaseAdapter{driver: &publicationDriverFake{}}).Publish(context.Background(), run, snapshot)
	if !errors.Is(err, errPublishProjection) {
		t.Fatalf("error=%v, want static projection failure", err)
	}
}

func TestPublishAdapterProjectsFindingsFromImmutableSnapshotAndExactArtifact(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
	ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
	run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
	driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded}}
	if _, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot); err != nil {
		t.Fatal(err)
	}
	if driver.input.OutputMode != v1alpha1.OutputFindings || driver.input.TargetPullRequest != 42 || driver.input.Findings != ref {
		t.Fatalf("findings projection = %#v", driver.input)
	}
}

func TestPublishAdapterFindingsOnlyDoesNotRequirePatchManifest(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
	run.Status.Patch.ManifestRef = nil
	ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
	run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
	driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded, FindingsState: publishcontroller.StateSucceeded}}
	if _, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot); err != nil {
		t.Fatalf("findings-only projection rejected without an unused manifest: %v", err)
	}
}

func TestPublishAdapterRejectsMissingOrAmbiguousFindingsArtifact(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
	if _, err := publicationInput(run, snapshot); !errors.Is(err, errPublishProjection) {
		t.Fatalf("missing findings error = %v", err)
	}
	ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
	run.Status.Artifacts = []v1alpha1.ArtifactRef{ref, ref}
	if _, err := publicationInput(run, snapshot); !errors.Is(err, errPublishProjection) {
		t.Fatalf("ambiguous findings error = %v", err)
	}
}

func TestPublishAdapterAllowsOutputBothToDeriveItsTarget(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth, Target: &v1alpha1.OutputTarget{PullRequest: 99}}
	ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
	run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
	input, err := publicationInput(run, snapshot)
	if err != nil {
		t.Fatalf("OutputBoth with an unrelated predeclared target was rejected: %v", err)
	}
	if input.TargetPullRequest != 0 {
		t.Fatalf("OutputBoth retained pre-existing target %d; target must come from patch publication", input.TargetPullRequest)
	}
}

func TestPublishAdapterProjectsIndependentFindingsStateWithoutPatchEffect(t *testing.T) {
	t.Run("findings only", func(t *testing.T) {
		run, snapshot := publishAdapterFixture()
		snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
		ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
		run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
		driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded, FindingsState: publishcontroller.StateSucceeded}}
		outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
		if err != nil || !outcome.Succeeded || !outcome.FindingsAttempted || !outcome.FindingsPublished || outcome.Effect != nil {
			t.Fatalf("findings-only outcome=%#v err=%v", outcome, err)
		}
	})

	t.Run("findings only rejects a patch effect projection", func(t *testing.T) {
		run, snapshot := publishAdapterFixture()
		snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
		ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
		run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
		effect := &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/7"}
		driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded, FindingsState: publishcontroller.StateSucceeded, Effect: effect}}
		outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
		if err != nil || !outcome.Succeeded || outcome.Effect != nil {
			t.Fatalf("findings-only projected a patch effect: outcome=%#v err=%v", outcome, err)
		}
	})

	t.Run("findings-only unknown aggregate becomes unknown findings condition", func(t *testing.T) {
		run, snapshot := publishAdapterFixture()
		snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputFindings, Target: &v1alpha1.OutputTarget{PullRequest: 42}}
		ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
		run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
		driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateUnknown}}
		outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
		if err != nil || !outcome.Unknown || !outcome.FindingsAttempted || !outcome.FindingsUnknown || outcome.Effect != nil {
			t.Fatalf("findings-only unknown projection=%#v err=%v", outcome, err)
		}
	})

	t.Run("both retains patch effect when findings is unknown", func(t *testing.T) {
		run, snapshot := publishAdapterFixture()
		snapshot.Spec.Output = &v1alpha1.AgentRunOutputSpec{Mode: v1alpha1.OutputBoth}
		ref := v1alpha1.ArtifactRef{URI: "s3://bucket/findings", Digest: "sha256:" + strings.Repeat("1", 64), Kind: findingsartifact.Kind, Name: findingsartifact.Name, MediaType: findingsartifact.MediaType, SizeBytes: 128}
		run.Status.Artifacts = []v1alpha1.ArtifactRef{ref}
		effect := &v1alpha1.EffectSummary{State: v1alpha1.EffectSucceeded, PullRequestURL: "https://github.com/Astatide1337/agents-gateway/pull/7"}
		driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateUnknown, PatchState: publishcontroller.StateSucceeded, FindingsState: publishcontroller.StateUnknown, Effect: effect}}
		outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
		if err != nil || !outcome.Unknown || !outcome.FindingsAttempted || !outcome.FindingsUnknown || outcome.Effect != effect {
			t.Fatalf("OutputBoth outcome=%#v err=%v", outcome, err)
		}
	})
}

func TestPublishAdapterRejectsSuccessStateAccompaniedByError(t *testing.T) {
	run, snapshot := publishAdapterFixture()
	driver := &publicationDriverFake{result: publishcontroller.Result{State: publishcontroller.StateSucceeded}, err: errors.New("provider detail")}
	outcome, err := (publishPhaseAdapter{driver: driver}).Publish(context.Background(), run, snapshot)
	if !errors.Is(err, errPublishProjection) || outcome.Succeeded || outcome.Unknown {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func publishAdapterFixture() (*v1alpha1.AgentRun, resolved.Snapshot) {
	run := &v1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Namespace: "agw-runs", Name: "fix-one", UID: types.UID("run-uid"), Generation: 3},
		Spec: v1alpha1.AgentRunSpec{
			Source:  v1alpha1.SourceSpec{Repo: "github.com/Astatide1337/agents-gateway", BaseRef: "main"},
			Publish: v1alpha1.PublishSpec{Mode: v1alpha1.PublishPullRequest, CredentialRef: "github-app", Title: "immutable title", Labels: []string{"immutable"}},
		},
	}
	run.Status.BaseSHA = strings.Repeat("b", 40)
	run.Status.SpecDigest = "sha256:" + strings.Repeat("c", 64)
	run.Status.Patch = &v1alpha1.PatchSummary{
		Ref:         &v1alpha1.ArtifactRef{URI: "s3://bucket/patch", Digest: "sha256:" + strings.Repeat("d", 64)},
		ManifestRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/manifest", Digest: "sha256:" + strings.Repeat("e", 64)},
	}
	run.Status.Gate = &v1alpha1.GateResult{Verdict: "Accepted", ReportRef: &v1alpha1.ArtifactRef{URI: "s3://bucket/report", Digest: "sha256:" + strings.Repeat("f", 64)}}
	snapshot := resolved.Snapshot{
		SchemaVersion: resolved.SchemaVersion,
		Run:           resolved.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID), Generation: run.Generation},
		Spec:          *run.Spec.DeepCopy(), BaseSHA: run.Status.BaseSHA,
		Gate:       v1alpha1.GateSpec{Mode: v1alpha1.GateEnforcing},
		References: resolved.References{Gate: resolved.ObjectVersion{UID: "gate-uid", Generation: 7}},
	}
	return run, snapshot
}
