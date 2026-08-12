package main

import (
	"context"
	"errors"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextartifact"
	agwcontroller "github.com/Astatide1337/agents-gateway/v3/internal/controller"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/runplan"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifycontroller"
)

type verifyPhaseAdapter struct {
	driver                *verifycontroller.Driver
	factory               *runplan.Factory
	store                 objectstore.Config
	fetchImage            string
	applyImage            string
	lockdownImage         string
	allowedRuntimeClasses []string
	allowedStorageClasses []string
	maxShutdown           time.Duration
	artifactTTL           time.Duration
	contextStore          contextartifact.Store
	clock                 func() time.Time
}

func (a verifyPhaseAdapter) Verify(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) (agwcontroller.VerifyOutcome, error) {
	if a.driver == nil || a.factory == nil || run == nil || run.Status.Patch == nil || run.Status.Patch.Ref == nil || a.fetchImage == "" || a.applyImage == "" || a.lockdownImage == "" {
		return agwcontroller.VerifyOutcome{}, errors.New("verification phase is not configured")
	}
	maxShutdown := a.maxShutdown
	if maxShutdown == 0 {
		maxShutdown = sandbox.DefaultMaxShutdownDuration
	}
	timeout, err := time.ParseDuration(snapshot.Gate.Verify.Timeout)
	if err != nil || timeout <= 0 || timeout > maxShutdown {
		return agwcontroller.VerifyOutcome{}, errors.New("verification timeout is invalid")
	}
	policyChecks, err := a.loadPolicyChecks(ctx, run, snapshot)
	if err != nil {
		return agwcontroller.VerifyOutcome{}, err
	}
	now := time.Now().UTC()
	if a.clock != nil {
		now = a.clock().UTC()
	}
	credentials, err := a.factory.MaterializeVerifyCredentials(ctx, run, snapshot)
	if err != nil {
		return agwcontroller.VerifyOutcome{}, err
	}
	decision, err := a.driver.Reconcile(ctx, verifycontroller.Inputs{
		Snapshot: snapshot, SpecDigest: run.Status.SpecDigest, BaseSHA: run.Status.BaseSHA, Patch: *run.Status.Patch.Ref,
		Credentials: verifycontroller.CredentialProjection{
			SecretName: credentials.SecretName, CloneSecretKey: credentials.CloneSecretKey,
		},
		ArtifactStoreEndpoint: a.store.Endpoint, ArtifactStoreRegion: a.store.Region,
		ArtifactStoreBucket: a.store.Bucket, ArtifactStoreForcePathStyle: a.store.ForcePathStyle,
		ArtifactCredentialTTL: a.artifactTTL,
		MaxPatchBytes:         snapshot.Spec.Limits.MaxPatchBytes,
		MaxOutputBytes:        snapshot.Spec.Limits.MaxOutputBytes,
		FetchImage:            a.fetchImage, ApplyImage: a.applyImage, LockdownImage: a.lockdownImage,
		AllowedRuntimeClasses: append([]string(nil), a.allowedRuntimeClasses...),
		AllowedStorageClasses: append([]string(nil), a.allowedStorageClasses...),
		Now:                   now, ShutdownTime: now.Add(timeout), MaxShutdownDuration: maxShutdown,
		PolicyChecks: policyChecks,
	})
	if err != nil {
		return agwcontroller.VerifyOutcome{}, err
	}
	var ref *v1alpha1.ChildRef
	if decision.VerifySandboxRef.Name != "" {
		copy := decision.VerifySandboxRef
		ref = &copy
	}
	return agwcontroller.VerifyOutcome{
		Pending: !decision.Complete, Complete: decision.Complete, RequeueAfter: decision.RequeueAfter,
		VerifySandboxRef: ref, Gate: decision.Gate, Failure: decision.Failure,
		Artifacts: append([]v1alpha1.ArtifactRef(nil), decision.Artifacts...),
	}, nil
}

func (a verifyPhaseAdapter) loadPolicyChecks(ctx context.Context, run *v1alpha1.AgentRun, snapshot resolved.Snapshot) ([]policycontract.GateCheckDescriptor, error) {
	if len(snapshot.Policies) == 0 {
		return nil, nil
	}
	if a.contextStore == nil || run == nil || run.Status.ContextPackRef == nil {
		return nil, errors.New("policy contract is required but the validated ContextPack is unavailable")
	}
	body, uri, err := contextartifact.Load(ctx, a.contextStore, snapshot.Run.UID, run.Status.SpecDigest)
	if err != nil {
		return nil, err
	}
	bundle, ref, err := contextartifact.Decode(body, uri, snapshot.Run.UID, run.Status.SpecDigest, snapshot.BaseSHA)
	if err != nil {
		return nil, err
	}
	if *run.Status.ContextPackRef != ref {
		return nil, errors.New("validated ContextPack reference does not match AgentRun status")
	}
	if bundle.PolicyContractDigest == "" || len(bundle.PolicyContractManifest) == 0 {
		return nil, errors.New("validated ContextPack does not contain the referenced policy contract")
	}
	compiled, err := policycontract.DecodeManifest(bundle.PolicyContractManifest)
	if err != nil {
		return nil, err
	}
	manifest := compiled.Manifest()
	if manifest.BaseSHA != snapshot.BaseSHA || manifest.ResolvedSpecDigest != run.Status.SpecDigest || compiled.Digest() != bundle.PolicyContractDigest {
		return nil, errors.New("policy contract identity does not match the resolved run")
	}
	checks := compiled.GateChecks()
	if err := policycontract.ValidateGateChecks(checks); err != nil {
		return nil, err
	}
	return checks, nil
}
