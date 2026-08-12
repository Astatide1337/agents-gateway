package criticworkload

import (
	"context"
	"errors"
	"fmt"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
)

// ContextRefResolver supplies the controller-persisted ContextPack reference
// for the exact AgentRun being verified. Keeping this callback explicit avoids
// allowing the critic runner to discover or substitute a context artifact.
type ContextRefResolver func(context.Context, verifycontroller.CriticRunInput) (v1alpha1.ArtifactRef, error)

// VerifyAdapter connects the production Job runner/source to the existing
// verifycontroller interfaces without moving any decision authority into the
// critic package. The adapter is intentionally thin: it translates status and
// binding types, while Runner and Source retain the full generation/context/
// Job/Pod identity checks.
type VerifyAdapter struct {
	Runner         *Runner
	Source         *Source
	ResolveContext ContextRefResolver
}

var _ verifycontroller.CriticEvidenceRunner = (*VerifyAdapter)(nil)
var _ verifycontroller.CriticEvidenceSource = (*VerifyAdapter)(nil)

// Ensure creates or observes the immutable critic Job. It returns only the
// output artifact reference; no critic-produced result is representable.
func (a *VerifyAdapter) Ensure(ctx context.Context, input verifycontroller.CriticRunInput) (verifycontroller.CriticRunStatus, error) {
	if a == nil || a.Runner == nil || a.ResolveContext == nil || ctx == nil {
		return verifycontroller.CriticRunStatus{}, verifycontroller.ErrInvalidOptions
	}
	contextRef, err := a.ResolveContext(ctx, input)
	if err != nil {
		return verifycontroller.CriticRunStatus{}, fmt.Errorf("%w: resolve context pack: %v", verifycontroller.ErrCriticUnavailable, err)
	}
	contract, err := FromSnapshot(input.Snapshot, input.SpecDigest, input.Patch, contextRef)
	if err != nil {
		return verifycontroller.CriticRunStatus{}, fmt.Errorf("%w: build critic contract: %v", verifycontroller.ErrCriticSourceFailed, err)
	}
	if err := bindingMatchesVerifyInput(contract.Binding(), input.Binding); err != nil {
		return verifycontroller.CriticRunStatus{}, err
	}
	status, err := a.Runner.Ensure(ctx, contract)
	if errors.Is(err, ErrJobNotReady) || errors.Is(err, ErrOutputNotReady) {
		return verifycontroller.CriticRunStatus{}, verifycontroller.ErrCriticNotReady
	}
	if err != nil {
		return verifycontroller.CriticRunStatus{}, fmt.Errorf("%w: critic Job: %v", verifycontroller.ErrCriticSourceFailed, err)
	}
	output := verifycontroller.CriticRunStatus{ID: status.ID, Ready: status.Ready, Finished: status.Finished}
	if status.InputRef != nil {
		ref := *status.InputRef
		output.InputRef = &ref
	}
	return output, nil
}

// Read authenticates the output through Source and exposes only the canonical
// CorroborationInput bytes to verifycontroller. The controller derives the
// CorroborationResult independently after this method returns.
func (a *VerifyAdapter) Read(ctx context.Context, ref v1alpha1.ArtifactRef, maxBytes int64) (verifycontroller.CriticEvidenceArtifact, error) {
	if a == nil || a.Source == nil || ctx == nil {
		return verifycontroller.CriticEvidenceArtifact{}, verifycontroller.ErrInvalidOptions
	}
	artifact, err := a.Source.Read(ctx, ref, maxBytes)
	if errors.Is(err, ErrSourceMissing) {
		return verifycontroller.CriticEvidenceArtifact{}, verifycontroller.ErrCriticMissing
	}
	if errors.Is(err, ErrOutputNotReady) {
		return verifycontroller.CriticEvidenceArtifact{}, verifycontroller.ErrCriticNotReady
	}
	if errors.Is(err, ErrSourceUnavailable) {
		return verifycontroller.CriticEvidenceArtifact{}, verifycontroller.ErrCriticUnavailable
	}
	if err != nil {
		return verifycontroller.CriticEvidenceArtifact{}, fmt.Errorf("%w: read authenticated critic evidence: %v", verifycontroller.ErrCriticSourceFailed, err)
	}
	binding := artifact.Binding
	return verifycontroller.CriticEvidenceArtifact{
		Input: append([]byte(nil), artifact.Input...), Authenticated: artifact.Authenticated,
		Binding: verifycontroller.CriticEvidenceBinding{
			RunUID: binding.RunUID, SpecDigest: binding.SpecDigest, BaseSHA: binding.BaseSHA, PatchDigest: binding.PatchDigest,
			GateUID: binding.Gate.UID, GateGeneration: binding.Gate.Generation,
			Route: gate.RouteEvidence{Name: binding.CriticRoute.Name, UID: binding.CriticRoute.UID, Generation: binding.CriticRoute.Generation, Provider: binding.CriticRoute.Selected.Name, Family: binding.CriticRoute.Selected.Family},
		},
	}, nil
}

func bindingMatchesVerifyInput(actual Binding, expected verifycontroller.CriticEvidenceBinding) error {
	if actual.RunUID != expected.RunUID || actual.SpecDigest != expected.SpecDigest || actual.BaseSHA != expected.BaseSHA || actual.PatchDigest != expected.PatchDigest || actual.Gate.UID != expected.GateUID || actual.Gate.Generation != expected.GateGeneration || actual.CriticRoute.Name != expected.Route.Name || actual.CriticRoute.UID != expected.Route.UID || actual.CriticRoute.Generation != expected.Route.Generation || actual.CriticRoute.Selected.Name != expected.Route.Provider || actual.CriticRoute.Selected.Family != expected.Route.Family {
		return verifycontroller.ErrCriticConflict
	}
	return nil
}
