package publishcontroller

import (
	"context"
	"errors"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

const (
	findingsKind             = findingsartifact.Kind
	findingsName             = findingsartifact.Name
	findingsMediaType        = findingsartifact.MediaType
	maxFindingsArtifactBytes = findingsartifact.MaxBytes
	findingsArtifactVersion  = findingsartifact.Version
)

func parseFindingsArtifact(body []byte, input Input, expectedPatchDigest string) (findingcorroboration.CorroborationResult, error) {
	artifact, derived, err := findingsartifact.Verify(body)
	if err != nil {
		if errors.Is(err, findingsartifact.ErrResultMismatch) {
			return findingcorroboration.CorroborationResult{}, ErrArtifactIdentity
		}
		return findingcorroboration.CorroborationResult{}, ErrManifestInvalid
	}
	if artifact.RunUID != input.RunUID || artifact.SpecDigest != input.SpecDigest || artifact.BaseSHA != input.BaseSHA || artifact.PatchDigest != expectedPatchDigest || artifact.GateReportDigest != input.GateReport.Digest {
		return findingcorroboration.CorroborationResult{}, ErrArtifactIdentity
	}
	return derived, nil
}

func findingsKeySuffix(runUID, specDigest, patchDigest, artifactDigest string) string {
	return "runs/" + runUID + "/findings/" + digestHex(specDigest) + "/" + digestHex(patchDigest) + "/" + digestHex(artifactDigest) + ".json"
}

func findingsArtifactRef(ref v1alpha1.ArtifactRef) bool {
	return ref.Kind == findingsKind && ref.Name == findingsName && ref.MediaType == findingsMediaType && canonical.ValidDigest(ref.Digest) && ref.SizeBytes > 0 && ref.SizeBytes <= maxFindingsArtifactBytes
}

func findingsOutputMode(mode v1alpha1.OutputMode) v1alpha1.OutputMode {
	if mode == "" {
		return v1alpha1.OutputPatch
	}
	return mode
}

func findingsMode(mode v1alpha1.OutputMode) bool {
	mode = findingsOutputMode(mode)
	return mode == v1alpha1.OutputFindings || mode == v1alpha1.OutputBoth
}

func findingsOnlyMode(mode v1alpha1.OutputMode) bool {
	return findingsOutputMode(mode) == v1alpha1.OutputFindings
}

func outputModeValid(mode v1alpha1.OutputMode) bool {
	mode = findingsOutputMode(mode)
	return mode == v1alpha1.OutputPatch || mode == v1alpha1.OutputFindings || mode == v1alpha1.OutputBoth
}

func findingsInputRequest(input Input, targetPullRequest int64, signedReportPatchDigest string, result findingcorroboration.CorroborationResult, artifactDigest string) publish.FindingsRequest {
	return publish.FindingsRequest{
		RunUID: input.RunUID, RunName: input.RunName, Repo: input.Repo,
		BaseRef: input.BaseRef, BaseSHA: input.BaseSHA, SpecDigest: input.SpecDigest,
		PatchDigest: signedReportPatchDigest, GateReportDigest: input.GateReport.Digest,
		ArtifactDigest: artifactDigest, TargetPullRequest: targetPullRequest, Result: result,
	}
}

func (d *Driver) loadFindings(ctx context.Context, input Input, patchDigest string) (findingcorroboration.CorroborationResult, string, error) {
	location, err := locationFor(input.Findings, findingsKind, findingsName, findingsMediaType)
	if err != nil {
		return findingcorroboration.CorroborationResult{}, "", err
	}
	expected := findingsKeySuffix(input.RunUID, input.SpecDigest, patchDigest, input.Findings.Digest)
	if !keyHasSuffix(location.Key, expected) {
		return findingcorroboration.CorroborationResult{}, "", ErrArtifactIdentity
	}
	body, err := d.read(ctx, location, int64(maxFindingsArtifactBytes), input.Findings.SizeBytes, false)
	if err != nil {
		return findingcorroboration.CorroborationResult{}, "", err
	}
	if digestBytes(body) != input.Findings.Digest {
		return findingcorroboration.CorroborationResult{}, "", ErrArtifactDigestMismatch
	}
	result, err := parseFindingsArtifact(body, input, patchDigest)
	if err != nil {
		return findingcorroboration.CorroborationResult{}, "", err
	}
	return result, input.Findings.Digest, nil
}

func (d *Driver) publishFindings(ctx context.Context, input Input, targetPullRequest int64, patchDigest, artifactDigest string, result findingcorroboration.CorroborationResult) (Result, error) {
	if d == nil || d.findings == nil {
		return Result{}, ErrInvalidInput
	}
	if targetPullRequest <= 0 || targetPullRequest > publish.MaxPullRequestNumber {
		return Result{State: StateUnknown, PatchState: StateSkipped, FindingsState: StateUnknown}, ErrUnknownEffect
	}
	findings, err := d.findings.PublishFindings(ctx, findingsInputRequest(input, targetPullRequest, patchDigest, result, artifactDigest))
	return mapFindingsOutcome(findings, targetPullRequest, err)
}

func mapFindingsOutcome(findings publish.FindingsResult, targetPullRequest int64, err error) (Result, error) {
	result := Result{
		PatchState:        StateSkipped,
		PullRequestNumber: findings.PullRequestNumber,
		PullRequestURL:    findings.PullRequestURL,
		Findings:          append([]publish.FindingOutcome(nil), findings.Outcomes...),
		AdvisoryEffectKey: findings.AdvisoryEffectKey,
	}
	if findings.PullRequestNumber != 0 && (findings.PullRequestNumber <= 0 || findings.PullRequestNumber > publish.MaxPullRequestNumber) {
		result.State = StateUnknown
		result.FindingsState = StateUnknown
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrUnknownEffect
	}
	if findings.PullRequestURL != "" && (!validPullRequestURL(findings.PullRequestURL) || findings.PullRequestNumber != targetPullRequest || !validPullRequestURLForNumber(findings.PullRequestURL, targetPullRequest)) {
		result.State = StateUnknown
		result.FindingsState = StateUnknown
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrUnknownEffect
	}
	if findings.State == publish.StateUnknown || errors.Is(err, publish.ErrUnknownEffect) {
		result.State = StateUnknown
		result.FindingsState = StateUnknown
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrUnknownEffect
	}
	if err != nil {
		if findings.State == publish.StateFailed {
			result.State = StateFailed
			result.FindingsState = StateFailed
			result.PullRequestNumber = 0
			result.PullRequestURL = ""
			return result, ErrPublishFailed
		}
		result.State = StateUnknown
		result.FindingsState = StateUnknown
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrUnknownEffect
	}
	switch findings.State {
	case publish.StateSucceeded:
		if findings.PullRequestNumber != targetPullRequest || !validPullRequestURL(findings.PullRequestURL) || !validPullRequestURLForNumber(findings.PullRequestURL, targetPullRequest) {
			result.State = StateUnknown
			result.FindingsState = StateUnknown
			result.PullRequestNumber = 0
			result.PullRequestURL = ""
			return result, ErrUnknownEffect
		}
		result.State = StateSucceeded
		result.FindingsState = StateSucceeded
		return result, nil
	case publish.StateSkipped:
		result.State = StateSkipped
		result.FindingsState = StateSkipped
		return result, nil
	case publish.StateFailed:
		result.State = StateFailed
		result.FindingsState = StateFailed
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrPublishFailed
	default:
		result.State = StateUnknown
		result.FindingsState = StateUnknown
		result.PullRequestNumber = 0
		result.PullRequestURL = ""
		return result, ErrUnknownEffect
	}
}
