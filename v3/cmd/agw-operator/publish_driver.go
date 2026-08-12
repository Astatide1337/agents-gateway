package main

import (
	"context"
	"errors"
	"strings"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	agwcontroller "github.com/Astatide1337/agents-gateway/v3/internal/controller"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publishcontroller"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
)

const defaultPublishRequeue = 2 * time.Second

var errPublishProjection = errors.New("publication inputs are incomplete or invalid")

// publishPhaseAdapter is the Kubernetes-facing edge of the controller-owned
// publication protocol. It deliberately constructs every publication input
// from the immutable resolved snapshot and immutable evidence refs, never
// from the mutable live AgentRun spec.
type publishPhaseAdapter struct {
	driver publicationDriver
}

type publicationDriver interface {
	Publish(context.Context, publishcontroller.Input) (publishcontroller.Result, error)
}

func (a publishPhaseAdapter) Publish(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (agwcontroller.PublishOutcome, error) {
	input, err := publicationInput(run, snapshot)
	if err != nil {
		return agwcontroller.PublishOutcome{}, err
	}
	if a.driver == nil {
		return agwcontroller.PublishOutcome{}, errPublishProjection
	}

	result, publishErr := a.driver.Publish(ctx, input)
	if publishErr != nil {
		if result.State == publishcontroller.StateUnknown || errors.Is(publishErr, publishcontroller.ErrUnknownEffect) {
			return withFindingsProjection(agwcontroller.PublishOutcome{Unknown: true, Effect: result.Effect}, input, result), nil
		}
		if result.State == publishcontroller.StateFailed && errors.Is(publishErr, publishcontroller.ErrPublishFailed) {
			return withFindingsProjection(agwcontroller.PublishOutcome{Failed: true, Effect: result.Effect, Reason: "controller-owned publication failed deterministically"}, input, result), nil
		}
		// Errors without a controller-safe terminal state occur before an
		// external effect is proven. Return only the static classification.
		return agwcontroller.PublishOutcome{}, errPublishProjection
	}
	switch result.State {
	case publishcontroller.StatePending:
		return withFindingsProjection(agwcontroller.PublishOutcome{Pending: true, Effect: result.Effect, RequeueAfter: defaultPublishRequeue}, input, result), nil
	case publishcontroller.StateSucceeded, publishcontroller.StateSkipped:
		return withFindingsProjection(agwcontroller.PublishOutcome{Succeeded: true, Effect: result.Effect}, input, result), nil
	case publishcontroller.StateRejected:
		return withFindingsProjection(agwcontroller.PublishOutcome{Rejected: true}, input, result), nil
	case publishcontroller.StateFailed:
		return withFindingsProjection(agwcontroller.PublishOutcome{Failed: true, Effect: result.Effect, Reason: "controller-owned publication failed deterministically"}, input, result), nil
	case publishcontroller.StateUnknown:
		return withFindingsProjection(agwcontroller.PublishOutcome{Unknown: true, Effect: result.Effect}, input, result), nil
	}

	return agwcontroller.PublishOutcome{}, errPublishProjection
}

func withFindingsProjection(outcome agwcontroller.PublishOutcome, input publishcontroller.Input, result publishcontroller.Result) agwcontroller.PublishOutcome {
	if input.OutputMode == v1alpha1.OutputFindings {
		// Findings-only has no patch effect. Keep the Kubernetes-facing
		// projection safe even if a narrow test/dry-run driver accidentally
		// returns an effect alongside the findings result.
		outcome.Effect = nil
	}
	findingsRequested := input.OutputMode == v1alpha1.OutputFindings || input.OutputMode == v1alpha1.OutputBoth
	if !findingsRequested {
		return outcome
	}
	// A real publication result always carries an explicit component state. The
	// fallback keeps the adapter compatible with narrow test/dry-run drivers that
	// return only the aggregate state.
	outcome.FindingsAttempted = result.FindingsState != publishcontroller.StateSkipped && result.FindingsState != ""
	if !outcome.FindingsAttempted && input.OutputMode == v1alpha1.OutputFindings && result.State != publishcontroller.StatePending {
		outcome.FindingsAttempted = true
	}
	outcome.FindingsPublished = result.FindingsState == publishcontroller.StateSucceeded || (input.OutputMode == v1alpha1.OutputFindings && result.State == publishcontroller.StateSucceeded && result.Effect == nil)
	outcome.FindingsUnknown = result.FindingsState == publishcontroller.StateUnknown || (input.OutputMode == v1alpha1.OutputFindings && result.FindingsState == "" && result.State == publishcontroller.StateUnknown)
	return outcome
}

func publicationInput(run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (publishcontroller.Input, error) {
	if run == nil || run.Status.Patch == nil || run.Status.Patch.Ref == nil || run.Status.Gate == nil || run.Status.Gate.ReportRef == nil {
		return publishcontroller.Input{}, errPublishProjection
	}
	if snapshot.Run.Namespace != run.Namespace || snapshot.Run.Name != run.Name || snapshot.Run.UID != string(run.UID) || snapshot.BaseSHA != run.Status.BaseSHA || run.Status.SpecDigest == "" {
		return publishcontroller.Input{}, errPublishProjection
	}
	repo, err := parseResolvedRepository(snapshot.Spec.Source.Repo)
	if err != nil {
		return publishcontroller.Input{}, errPublishProjection
	}
	outputMode := v1alpha1.OutputPatch
	targetPullRequest := int64(0)
	var findings v1alpha1.ArtifactRef
	if snapshot.Spec.Output != nil {
		if snapshot.Spec.Output.Mode != "" {
			outputMode = snapshot.Spec.Output.Mode
		}
		// A pre-existing target is meaningful only for findings-only output.
		// OutputBoth deliberately derives its target from the patch effect, so
		// do not carry an unrelated user-supplied PR across this boundary.
		if outputMode == v1alpha1.OutputFindings && snapshot.Spec.Output.Target != nil {
			targetPullRequest = snapshot.Spec.Output.Target.PullRequest
		}
	}
	patchPublication := outputMode == v1alpha1.OutputPatch || outputMode == v1alpha1.OutputBoth
	if patchPublication && run.Status.Patch.ManifestRef == nil {
		return publishcontroller.Input{}, errPublishProjection
	}
	if outputMode == v1alpha1.OutputFindings || outputMode == v1alpha1.OutputBoth {
		matches := 0
		for _, artifact := range run.Status.Artifacts {
			if artifact.Kind == findingsartifact.Kind && artifact.Name == findingsartifact.Name && artifact.MediaType == findingsartifact.MediaType {
				findings = artifact
				matches++
			}
		}
		if matches != 1 || (outputMode == v1alpha1.OutputFindings && targetPullRequest <= 0) {
			return publishcontroller.Input{}, errPublishProjection
		}
	}
	manifest := v1alpha1.ArtifactRef{}
	if run.Status.Patch.ManifestRef != nil {
		manifest = *run.Status.Patch.ManifestRef
	}
	return publishcontroller.Input{
		RunUID: snapshot.Run.UID, RunName: snapshot.Run.Name, Repo: repo,
		BaseRef: snapshot.Spec.Source.BaseRef, BaseSHA: snapshot.BaseSHA, SpecDigest: run.Status.SpecDigest,
		GateUID: snapshot.References.Gate.UID, GateGeneration: snapshot.References.Gate.Generation, GateMode: snapshot.Gate.Mode,
		Patch: *run.Status.Patch.Ref, Manifest: manifest, GateReport: *run.Status.Gate.ReportRef,
		OutputMode: outputMode, TargetPullRequest: targetPullRequest, Findings: findings,
		PublishMode: snapshot.Spec.Publish.Mode, Title: snapshot.Spec.Publish.Title,
		Labels: append([]string(nil), snapshot.Spec.Publish.Labels...),
	}, nil
}

func parseResolvedRepository(value string) (githubapp.Repository, error) {
	const prefix = "github.com/"
	if !strings.HasPrefix(value, prefix) || strings.HasSuffix(value, ".git") {
		return githubapp.Repository{}, errPublishProjection
	}
	parts := strings.Split(strings.TrimPrefix(value, prefix), "/")
	if len(parts) != 2 {
		return githubapp.Repository{}, errPublishProjection
	}
	repo := githubapp.Repository{Owner: parts[0], Name: parts[1]}
	if err := repo.Validate(); err != nil {
		return githubapp.Repository{}, errPublishProjection
	}
	return repo, nil
}
