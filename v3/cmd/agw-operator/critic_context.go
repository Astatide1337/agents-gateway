package main

import (
	"context"
	"errors"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errCriticContextIdentity = errors.New("critic ContextPack identity is unavailable or conflicts with the AgentRun")

// resolveCriticContextRef returns only the ContextPack already projected onto
// the exact AgentRun under verification. It never searches object storage or
// accepts a caller-supplied replacement reference.
func resolveCriticContextRef(ctx context.Context, reader client.Reader, input verifycontroller.CriticRunInput) (v1alpha1.ArtifactRef, error) {
	if ctx == nil || reader == nil || input.Snapshot.Run.Namespace == "" || input.Snapshot.Run.Name == "" || input.Snapshot.Run.UID == "" {
		return v1alpha1.ArtifactRef{}, errCriticContextIdentity
	}
	run := &v1alpha1.AgentRun{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: input.Snapshot.Run.Namespace, Name: input.Snapshot.Run.Name}, run); err != nil {
		return v1alpha1.ArtifactRef{}, errCriticContextIdentity
	}
	if string(run.UID) != input.Snapshot.Run.UID || run.Generation != input.Snapshot.Run.Generation ||
		run.Status.ObservedGeneration != input.Snapshot.Run.Generation || run.Status.Phase != v1alpha1.PhaseVerifying ||
		run.Status.SpecDigest != input.SpecDigest ||
		run.Status.BaseSHA != input.BaseSHA || run.Status.BaseSHA != input.Snapshot.BaseSHA ||
		run.Status.Patch == nil || run.Status.Patch.Ref == nil || *run.Status.Patch.Ref != input.Patch ||
		run.Status.ContextPackRef == nil {
		return v1alpha1.ArtifactRef{}, errCriticContextIdentity
	}
	return *run.Status.ContextPackRef, nil
}
