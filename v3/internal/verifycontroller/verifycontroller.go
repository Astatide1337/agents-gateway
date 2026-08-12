// Package verifycontroller orchestrates the independent verification phase.
//
// The package is deliberately a small controller seam rather than a
// Kubernetes reconciler. It builds the verify Sandbox through verifyworkload,
// delegates ownership and observation to sandbox.SandboxBackend, and accepts
// a result only after a bounded stdout frame has been identity-checked and its
// raw evidence persisted content-addressably. A process/Sandbox exit is never
// treated as verification success.
//
// Secret creation and credential resolution are intentionally outside this
// package. The caller supplies the exact projected Secret name and keys that
// verifyworkload should mount in its fetch initContainer.
package verifycontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/evidenceattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingsartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/gatescoring"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/sandbox"
	"github.com/Astatide1337/agents-gateway/v3/internal/verificationattestation"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyworkload"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// EvidenceSchemaVersion is the machine-verification result consumed by this
	// package. It intentionally has no "verdict" or "finished" field: those
	// are controller decisions, not claims an execution pod may manufacture.
	EvidenceSchemaVersion = "agents.astatide.com/verification-evidence/v1alpha1"
	// EvidenceFramePrefix identifies the one stdout line carrying verifier
	// evidence. The payload is unpadded base64 of the strict JSON document.
	EvidenceFramePrefix = "AGW_VERIFY_EVIDENCE_V1 "

	DefaultMaxEvidenceBytes int64 = 1 << 20
	MaxEvidenceBytes        int64 = 1 << 20

	defaultRequeueAfter = 5 * time.Second

	reportMediaType        = "application/json"
	reportKind             = "verification-report"
	reportName             = "verification-report.json"
	maxStatusChildUIDBytes = 64
	criticInputKind        = "critic-corroboration-input"
	criticInputName        = "critic-corroboration-input.json"
	criticInputMediaType   = "application/vnd.agents-gateway.corroboration-input.v1alpha1+json"
	criticResultKind       = "critic-corroboration-result"
	criticResultName       = "critic-corroboration-result.json"
	criticResultMediaType  = "application/vnd.agents-gateway.corroboration-result.v1alpha1+json"
)

var (
	ErrInvalidOptions        = errors.New("invalid verification controller options")
	ErrInvalidInput          = errors.New("invalid verification controller input")
	ErrEvidenceMissing       = errors.New("verification evidence is missing")
	ErrEvidenceMalformed     = errors.New("verification evidence is malformed")
	ErrEvidenceNotReady      = errors.New("verification evidence is not ready")
	ErrEvidenceUnavailable   = errors.New("verification evidence source is temporarily unavailable")
	ErrEvidenceConflict      = errors.New("verification evidence conflicts with immutable object")
	ErrReportConflict        = errors.New("verification report conflicts with immutable object")
	ErrStatusProjection      = errors.New("verification result exceeds status bounds")
	ErrSignerBinding         = errors.New("verification signer returned a different report")
	ErrUnsafeArtifactURI     = errors.New("artifact URI is unsafe")
	ErrEvidenceSourceFailed  = errors.New("verification evidence source failed")
	ErrCriticNotReady        = errors.New("critic evidence is not ready")
	ErrCriticUnavailable     = errors.New("critic evidence source is temporarily unavailable")
	ErrCriticMissing         = errors.New("critic evidence is missing")
	ErrCriticMalformed       = errors.New("critic evidence is malformed")
	ErrCriticUnauthenticated = errors.New("critic evidence is unauthenticated")
	ErrCriticConflict        = errors.New("critic evidence conflicts with immutable identity")
	ErrCriticSourceFailed    = errors.New("critic evidence source failed")
)

// EvidenceSource retrieves the exact bounded stdout frame for the verify
// container belonging to a finished SandboxRef. Pod discovery and log access
// remain outside this package. Implementations return ErrEvidenceNotReady when
// the frame is not visible yet, ErrEvidenceUnavailable for transient log/API
// failures, and ErrEvidenceMissing for a definitive absence. The driver
// parses, binds, hashes, and persists the decoded payload itself.
type EvidenceSource interface {
	ReadFrame(context.Context, sandbox.SandboxRef, int64) ([]byte, error)
}

// CriticEvidenceBinding is the identity a critic runner must bind to its
// output. The source is not allowed to return a result for a different run,
// patch, Gate revision, or critic route.
type CriticEvidenceBinding struct {
	RunUID         string
	SpecDigest     string
	BaseSHA        string
	PatchDigest    string
	GateUID        string
	GateGeneration int64
	Route          gate.RouteEvidence
}

// CriticRunInput is the only input crossing into an external critic runner.
// It contains references and immutable identities, never provider credentials
// or a model client.
type CriticRunInput struct {
	Snapshot   resolved.Snapshot
	SpecDigest string
	BaseSHA    string
	Patch      v1alpha1.ArtifactRef
	Binding    CriticEvidenceBinding
}

// CriticRunStatus is an asynchronous runner result. A runner may create a pod,
// a Workflow, or another external execution mechanism; verifycontroller only
// observes this bounded seam. The runner can publish only an input artifact
// reference. It cannot return a CorroborationResult, verdict, or score.
type CriticRunStatus struct {
	ID       string
	Ready    bool
	Finished bool
	InputRef *v1alpha1.ArtifactRef
}

// CriticEvidenceRunner owns critic execution outside this package. It must not
// return a completed status until its producer identity and artifact reference
// have been authenticated by the implementation.
type CriticEvidenceRunner interface {
	Ensure(context.Context, CriticRunInput) (CriticRunStatus, error)
}

// CriticEvidenceArtifact is returned by a trusted source after it has
// authenticated the immutable artifact producer. The bool is deliberately
// explicit: a digest and canonical JSON alone are integrity, not provenance.
// Input is canonical CorroborationInput bytes only. A source has no result
// field because the trusted controller derives CorroborationResult itself.
type CriticEvidenceArtifact struct {
	Input         []byte
	Authenticated bool
	Binding       CriticEvidenceBinding
}

// CriticEvidenceSource reads the exact canonical CorroborationInput artifact
// named by a completed runner. It does not call a model and must never return
// a result, verdict, score, or model prose as trusted authority.
type CriticEvidenceSource interface {
	Read(context.Context, v1alpha1.ArtifactRef, int64) (CriticEvidenceArtifact, error)
}

// ReportSigner is the operator-owned signing boundary. Private signing
// material never enters the driver input or a verify pod.
type ReportSigner interface {
	Sign(gate.VerificationReport) (gate.SignedReport, error)
}

// ReportAttester wraps an already authenticated Gate report in a portable
// cosign/in-toto proof. It cannot change the Gate decision; failure prevents
// the verification phase from being projected as complete.
type ReportAttester interface {
	Attest(context.Context, verificationattestation.Input) (verificationattestation.Result, error)
}

// SignerFunc adapts gate.SignReport or another operator-owned signer to
// ReportSigner without making key management a responsibility of this
// package.
type SignerFunc func(gate.VerificationReport) (gate.SignedReport, error)

func (f SignerFunc) Sign(report gate.VerificationReport) (gate.SignedReport, error) {
	if f == nil {
		return gate.SignedReport{}, ErrInvalidOptions
	}
	return f(report)
}

// CredentialProjection is the explicit Secret projection needed by the
// verifyworkload fetch initContainer. The driver does not create, read, or
// mutate the Secret and does not infer alternative keys.
type CredentialProjection struct {
	SecretName     string
	CloneSecretKey string
}

// Inputs are immutable operator-resolved inputs for one verification attempt.
// Snapshot.BaseSHA and SpecDigest must already be pinned before this driver is
// called. Patch and evidence identities are bound independently.
type Inputs struct {
	Snapshot   resolved.Snapshot
	SpecDigest string
	BaseSHA    string
	Patch      v1alpha1.ArtifactRef

	Credentials CredentialProjection

	ArtifactStoreEndpoint       string
	ArtifactStoreRegion         string
	ArtifactStoreBucket         string
	ArtifactStoreForcePathStyle bool
	ArtifactCredentialTTL       time.Duration
	MaxPatchBytes               int64
	MaxOutputBytes              int64

	FetchImage            string
	ApplyImage            string
	LockdownImage         string
	AllowedRuntimeClasses []string
	AllowedStorageClasses []string

	Now                 time.Time
	ShutdownTime        time.Time
	MaxShutdownDuration time.Duration
	MaxCommandBytes     int
	PolicyChecks        []policycontract.GateCheckDescriptor
}

// Decision is a bounded, status-ready projection of the verification phase.
// The full evidence and signed report remain in object storage. Complete is
// true only after a signed report has been durably persisted; a pending
// decision never contains an accepting Gate result.
type Decision struct {
	Phase v1alpha1.Phase

	Complete           bool
	SandboxReady       bool
	SandboxFinished    bool
	SandboxObservation sandbox.SandboxObservation
	VerifySandboxRef   v1alpha1.ChildRef

	Gate    *v1alpha1.GateResult
	Failure *v1alpha1.FailureStatus
	// Artifacts contains the controller-persisted raw verifier evidence. The
	// signed report remains available through Gate.ReportRef.
	Artifacts []v1alpha1.ArtifactRef
	// Scoring is the bounded signed-report projection. The CRD status continues
	// to carry ReportRef and artifact refs; this internal projection lets a
	// controller/status adapter expose the integer scores without re-evaluating
	// the report.
	Scoring   *gate.ScoringEvidence
	CriticRun *CriticRunStatus

	RequeueAfter time.Duration
}

// StatusProjection returns the portion of AgentRunStatus owned by the
// verification phase. It copies slices and pointers so a caller can safely
// merge it into a controller-owned status object.
func (d Decision) StatusProjection() v1alpha1.AgentRunStatus {
	status := v1alpha1.AgentRunStatus{Phase: d.Phase}
	if d.VerifySandboxRef.Name != "" {
		ref := d.VerifySandboxRef
		status.VerifySandboxRef = &ref
	}
	status.Gate = cloneGateResult(d.Gate)
	status.Failure = cloneFailure(d.Failure)
	status.Artifacts = append([]v1alpha1.ArtifactRef(nil), d.Artifacts...)
	return status
}

// DetailedStatusProjection carries the bounded internal quality projection
// alongside the existing CRD-compatible status. The controller core can copy
// Scoring and CriticRun into a future status adapter without re-reading or
// re-evaluating signed artifacts; the public CRD shape remains untouched in
// this integration slice.
type DetailedStatus struct {
	Status    v1alpha1.AgentRunStatus
	Scoring   *gate.ScoringEvidence
	CriticRun *CriticRunStatus
}

func (d Decision) DetailedStatusProjection() DetailedStatus {
	return DetailedStatus{
		Status:    d.StatusProjection(),
		Scoring:   cloneScoringEvidence(d.Scoring),
		CriticRun: cloneCriticRunStatus(d.CriticRun),
	}
}

// ObservationProjection returns a bounded copy for callers that need the
// Sandbox condition details in addition to StatusProjection.
func (d Decision) ObservationProjection() sandbox.SandboxObservation {
	output := d.SandboxObservation
	output.Conditions = append([]sandbox.ConditionObservation(nil), d.SandboxObservation.Conditions...)
	return output

}

// Options wires the orchestration seams. All dependencies are required even
// when the first reconcile only observes a still-running Sandbox; this avoids
// a later execution path silently becoming non-durable.
type Options struct {
	Backend                sandbox.SandboxBackend
	Evidence               EvidenceSource
	ReportStore            artifacts.Store
	ReportSigner           ReportSigner
	ReportAttester         ReportAttester
	MaxEvidenceBytes       int64
	CriticRunner           CriticEvidenceRunner
	CriticEvidence         CriticEvidenceSource
	MaxCriticEvidenceBytes int64
}

// Driver is deterministic for a fixed Inputs value and dependency result.
type Driver struct {
	backend           sandbox.SandboxBackend
	evidence          EvidenceSource
	reportStore       artifacts.Store
	reportSigner      ReportSigner
	reportAttester    ReportAttester
	maxEvidence       int64
	criticRunner      CriticEvidenceRunner
	criticEvidence    CriticEvidenceSource
	maxCriticEvidence int64
}

// New validates the orchestration dependencies and returns a verification
// driver. It never touches Kubernetes or storage during construction.
func New(options Options) (*Driver, error) {
	if options.Backend == nil || options.Evidence == nil || options.ReportStore == nil || options.ReportSigner == nil {
		return nil, fmt.Errorf("%w: backend, evidence, report store, and report signer are required", ErrInvalidOptions)
	}
	maxEvidence := options.MaxEvidenceBytes
	if maxEvidence == 0 {
		maxEvidence = DefaultMaxEvidenceBytes
	}
	if maxEvidence <= 0 || maxEvidence > MaxEvidenceBytes {
		return nil, fmt.Errorf("%w: max evidence bytes must be in 1..%d", ErrInvalidOptions, MaxEvidenceBytes)
	}
	maxCriticEvidence := options.MaxCriticEvidenceBytes
	if maxCriticEvidence == 0 {
		maxCriticEvidence = findingcorroboration.MaxInputBytes
	}
	if maxCriticEvidence <= 0 || maxCriticEvidence > findingcorroboration.MaxInputBytes {
		return nil, fmt.Errorf("%w: max critic input bytes must be in 1..%d", ErrInvalidOptions, findingcorroboration.MaxInputBytes)
	}
	return &Driver{
		backend:           options.Backend,
		evidence:          options.Evidence,
		reportStore:       options.ReportStore,
		reportSigner:      options.ReportSigner,
		reportAttester:    options.ReportAttester,
		maxEvidence:       maxEvidence,
		criticRunner:      options.CriticRunner,
		criticEvidence:    options.CriticEvidence,
		maxCriticEvidence: maxCriticEvidence,
	}, nil
}

// Reconcile builds and idempotently ensures the independent verify Sandbox,
// observes its bounded status, and—only after Finished is observed—consumes
// and evaluates immutable machine evidence. It does not infer success from a
// pod/process exit, Ready, or an absent error.
func (d *Driver) Reconcile(ctx context.Context, input Inputs) (Decision, error) {
	if d == nil || d.backend == nil || d.evidence == nil || d.reportStore == nil || d.reportSigner == nil {
		return Decision{}, ErrInvalidOptions
	}
	if ctx == nil {
		return Decision{}, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}

	plan, err := d.buildPlan(input)
	if err != nil {
		return Decision{}, err
	}
	ref, err := d.backend.Ensure(ctx, plan)
	if err != nil {
		return Decision{}, fmt.Errorf("ensure verify Sandbox: %w", err)
	}
	if err := validateSandboxIdentity(ref, input); err != nil {
		return Decision{}, err
	}

	observation, err := d.backend.Observe(ctx, ref)
	if err != nil {
		return Decision{}, fmt.Errorf("observe verify Sandbox: %w", err)
	}
	if len(observation.Conditions) > sandbox.MaxObservedConditions {
		return Decision{}, fmt.Errorf("%w: verify Sandbox returned too many conditions", ErrInvalidInput)
	}
	if err := validateSandboxIdentity(observation.Ref, input); err != nil {
		return Decision{}, err
	}
	statusRef, err := childRef(observation.Ref)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		Phase:              v1alpha1.PhaseVerifying,
		SandboxReady:       observation.Ready,
		SandboxFinished:    observation.Finished,
		SandboxObservation: observation,
		VerifySandboxRef:   statusRef,
		RequeueAfter:       defaultRequeueAfter,
	}
	if !observation.Exists {
		decision.Failure = failure("VerifySandboxMissing", "the independent verify Sandbox is not observable", true)
		return decision, nil
	}
	if !observation.Finished {
		return decision, nil
	}

	var critic *criticExecution
	if criticConfigured(input) {
		if d.criticRunner == nil || d.criticEvidence == nil {
			decision.Failure = failure("CriticIntegrationMissing", "critic signal is configured but no authenticated runner/source is installed", false)
			return decision, nil
		}
		binding, err := criticBinding(input)
		if err != nil {
			return Decision{}, err
		}
		status, err := d.criticRunner.Ensure(ctx, CriticRunInput{
			Snapshot: input.Snapshot, SpecDigest: input.SpecDigest, BaseSHA: input.BaseSHA,
			Patch: input.Patch, Binding: binding,
		})
		if errors.Is(err, ErrCriticNotReady) || errors.Is(err, ErrCriticUnavailable) {
			decision.Failure = failure("CriticEvidenceNotReady", "critic evidence has not been published", true)
			return decision, nil
		}
		if err != nil {
			return Decision{}, fmt.Errorf("%w: ensure critic run: %v", ErrCriticSourceFailed, err)
		}
		if err := validateCriticRunStatus(status); err != nil {
			decision.Failure = failure("CriticEvidenceMalformed", boundedMessage(err.Error()), false)
			return decision, nil
		}
		decision.CriticRun = cloneCriticRunStatus(&status)
		if !status.Finished {
			decision.Failure = failure("CriticEvidenceNotReady", "critic runner is still working", true)
			return decision, nil
		}
		if status.InputRef == nil {
			decision.Failure = failure("CriticEvidenceMissing", "finished critic runner did not publish a corroboration input artifact", false)
			return decision, nil
		}
		critic = &criticExecution{status: status, binding: binding, inputRef: *status.InputRef}
	}

	return d.finish(ctx, input, decision, critic)
}

func (d *Driver) buildPlan(input Inputs) (sandbox.SandboxPlan, error) {
	if input.SpecDigest == "" || !canonical.ValidDigest(input.SpecDigest) {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: spec digest is required", ErrInvalidInput)
	}
	computedDigest, err := canonical.ResolvedSpecDigest(input.Snapshot)
	if err != nil {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: compute resolved spec digest: %v", ErrInvalidInput, err)
	}
	if computedDigest != input.SpecDigest {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: spec digest does not match resolved snapshot", ErrInvalidInput)
	}
	if !resolved.ValidBaseSHA(input.BaseSHA) || input.Snapshot.BaseSHA != input.BaseSHA {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: base SHA does not match resolved snapshot", ErrInvalidInput)
	}
	options := verifyworkload.Options{
		FetchImage:                  input.FetchImage,
		ApplyImage:                  input.ApplyImage,
		LockdownImage:               input.LockdownImage,
		AllowedRuntimeClasses:       append([]string(nil), input.AllowedRuntimeClasses...),
		AllowedStorageClasses:       append([]string(nil), input.AllowedStorageClasses...),
		SecretName:                  input.Credentials.SecretName,
		CloneSecretKey:              input.Credentials.CloneSecretKey,
		ArtifactStoreEndpoint:       input.ArtifactStoreEndpoint,
		ArtifactStoreRegion:         input.ArtifactStoreRegion,
		ArtifactStoreBucket:         input.ArtifactStoreBucket,
		ArtifactStoreForcePathStyle: input.ArtifactStoreForcePathStyle,
		ArtifactCredentialTTL:       input.ArtifactCredentialTTL,
		MaxPatchBytes:               input.MaxPatchBytes,
		MaxOutputBytes:              input.MaxOutputBytes,
		Now:                         input.Now,
		ShutdownTime:                input.ShutdownTime,
		MaxShutdownDuration:         input.MaxShutdownDuration,
		MaxCommandBytes:             input.MaxCommandBytes,
		PolicyChecks:                append([]policycontract.GateCheckDescriptor(nil), input.PolicyChecks...),
	}
	plan, err := verifyworkload.Build(input.Snapshot, input.Patch, input.BaseSHA, options)
	if err != nil {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: build verify Sandbox: %v", ErrInvalidInput, err)
	}
	if plan.SpecDigest != input.SpecDigest {
		return sandbox.SandboxPlan{}, fmt.Errorf("%w: verify Sandbox plan changed spec identity", ErrInvalidInput)
	}
	return plan, nil
}

type criticExecution struct {
	status   CriticRunStatus
	binding  CriticEvidenceBinding
	inputRef v1alpha1.ArtifactRef
}

func criticConfigured(input Inputs) bool {
	return input.Snapshot.Gate.Signals != nil && input.Snapshot.Gate.Signals.Critic != nil
}

func criticBinding(input Inputs) (CriticEvidenceBinding, error) {
	if !criticConfigured(input) || input.Snapshot.CriticModelRoute == nil || input.Snapshot.References.CriticModelRoute == nil {
		return CriticEvidenceBinding{}, fmt.Errorf("%w: resolved critic route is missing", ErrInvalidInput)
	}
	providers := append([]v1alpha1.ModelProvider(nil), input.Snapshot.CriticModelRoute.Providers...)
	if len(providers) == 0 {
		return CriticEvidenceBinding{}, fmt.Errorf("%w: resolved critic route has no providers", ErrInvalidInput)
	}
	sort.SliceStable(providers, func(left, right int) bool {
		if providers[left].Priority != providers[right].Priority {
			return providers[left].Priority < providers[right].Priority
		}
		return providers[left].Name < providers[right].Name
	})
	selected := providers[0]
	ref := *input.Snapshot.References.CriticModelRoute
	if !validExternalIdentity(ref.Name) || !validExternalIdentity(ref.UID) || ref.Generation <= 0 || !validExternalIdentity(selected.Name) || !validExternalIdentity(selected.Family) {
		return CriticEvidenceBinding{}, fmt.Errorf("%w: critic route identity is invalid", ErrInvalidInput)
	}
	return CriticEvidenceBinding{
		RunUID: input.Snapshot.Run.UID, SpecDigest: input.SpecDigest, BaseSHA: input.BaseSHA,
		PatchDigest: input.Patch.Digest, GateUID: input.Snapshot.References.Gate.UID,
		GateGeneration: input.Snapshot.References.Gate.Generation,
		Route:          gate.RouteEvidence{Name: ref.Name, UID: ref.UID, Generation: ref.Generation, Provider: selected.Name, Family: selected.Family},
	}, nil
}

func validateCriticRunStatus(status CriticRunStatus) error {
	if !validExternalIdentity(status.ID) {
		return fmt.Errorf("%w: critic runner returned an invalid run identity", ErrCriticMalformed)
	}
	if !status.Finished && status.InputRef != nil {
		return fmt.Errorf("%w: unfinished critic runner returned input", ErrCriticMalformed)
	}
	if status.InputRef == nil {
		return nil
	}
	ref := status.InputRef
	if ref.Kind != criticInputKind || ref.Name != criticInputName || ref.MediaType != criticInputMediaType || ref.SizeBytes <= 0 || ref.SizeBytes > findingcorroboration.MaxInputBytes || !canonical.ValidDigest(ref.Digest) || !safeArtifactURI(ref.URI) {
		return fmt.Errorf("%w: critic input artifact reference is invalid", ErrCriticMalformed)
	}
	return nil
}

func (d *Driver) loadCriticEvidence(ctx context.Context, execution criticExecution) (findingcorroboration.CorroborationInput, []byte, error) {
	artifact, err := d.criticEvidence.Read(ctx, execution.inputRef, d.maxCriticEvidence)
	if err != nil {
		switch {
		case errors.Is(err, ErrCriticNotReady):
			return findingcorroboration.CorroborationInput{}, nil, ErrCriticNotReady
		case errors.Is(err, ErrCriticUnavailable):
			return findingcorroboration.CorroborationInput{}, nil, ErrCriticUnavailable
		case errors.Is(err, ErrCriticMissing):
			return findingcorroboration.CorroborationInput{}, nil, ErrCriticMissing
		default:
			return findingcorroboration.CorroborationInput{}, nil, fmt.Errorf("%w: read critic input: %v", ErrCriticSourceFailed, err)
		}
	}
	if !artifact.Authenticated {
		return findingcorroboration.CorroborationInput{}, nil, ErrCriticUnauthenticated
	}
	if !sameCriticBinding(artifact.Binding, execution.binding) {
		return findingcorroboration.CorroborationInput{}, nil, ErrCriticConflict
	}
	if len(artifact.Input) == 0 || int64(len(artifact.Input)) > d.maxCriticEvidence || digestBytes(artifact.Input) != execution.inputRef.Digest {
		return findingcorroboration.CorroborationInput{}, nil, ErrCriticMalformed
	}
	criticInput, err := decodeCanonicalCorroborationInput(artifact.Input)
	if err != nil {
		return findingcorroboration.CorroborationInput{}, append([]byte(nil), artifact.Input...), fmt.Errorf("%w: %v", ErrCriticMalformed, err)
	}
	return criticInput, append([]byte(nil), artifact.Input...), nil
}

func decodeCanonicalCorroborationInput(body []byte) (findingcorroboration.CorroborationInput, error) {
	input, err := findingcorroboration.ParseCanonicalInput(body)
	if err != nil {
		return findingcorroboration.CorroborationInput{}, err
	}
	return input, nil
}

func sameCriticBinding(left, right CriticEvidenceBinding) bool {
	return left.RunUID == right.RunUID && left.SpecDigest == right.SpecDigest && left.BaseSHA == right.BaseSHA && left.PatchDigest == right.PatchDigest && left.GateUID == right.GateUID && left.GateGeneration == right.GateGeneration && left.Route == right.Route
}

func (d *Driver) persistCriticArtifacts(ctx context.Context, input Inputs, criticInput findingcorroboration.CorroborationInput, rawInput []byte, result findingcorroboration.CorroborationResult) (v1alpha1.ArtifactRef, v1alpha1.ArtifactRef, error) {
	canonicalInput, err := findingcorroboration.CanonicalInputBytes(criticInput)
	if err != nil || !bytes.Equal(canonicalInput, rawInput) {
		return v1alpha1.ArtifactRef{}, v1alpha1.ArtifactRef{}, ErrCriticMalformed
	}
	canonicalResult, err := findingcorroboration.CanonicalResultBytes(result)
	if err != nil {
		return v1alpha1.ArtifactRef{}, v1alpha1.ArtifactRef{}, fmt.Errorf("canonicalize derived critic result: %w", err)
	}
	baseKey := "runs/" + input.Snapshot.Run.UID + "/verification/" + trimDigest(input.SpecDigest) + "/" + trimDigest(input.Patch.Digest) + "/critic/"
	inputRef, err := d.persistImmutableArtifact(ctx, baseKey+"input/"+trimDigest(digestBytes(canonicalInput))+".json", canonicalInput, criticInputMediaType, criticInputKind, criticInputName)
	if err != nil {
		return v1alpha1.ArtifactRef{}, v1alpha1.ArtifactRef{}, fmt.Errorf("persist critic input: %w", err)
	}
	resultRef, err := d.persistImmutableArtifact(ctx, baseKey+"result/"+trimDigest(digestBytes(canonicalResult))+".json", canonicalResult, criticResultMediaType, criticResultKind, criticResultName)
	if err != nil {
		return v1alpha1.ArtifactRef{}, v1alpha1.ArtifactRef{}, fmt.Errorf("persist derived critic result: %w", err)
	}
	return inputRef, resultRef, nil
}

func (d *Driver) persistFindingsPublication(ctx context.Context, input Inputs, body []byte) (v1alpha1.ArtifactRef, error) {
	if len(body) == 0 || len(body) > findingsartifact.MaxBytes {
		return v1alpha1.ArtifactRef{}, ErrCriticMalformed
	}
	digest := digestBytes(body)
	key := "runs/" + input.Snapshot.Run.UID + "/findings/" + trimDigest(input.SpecDigest) + "/" + trimDigest(input.Patch.Digest) + "/" + trimDigest(digest) + ".json"
	return d.persistImmutableArtifact(ctx, key, body, findingsartifact.MediaType, findingsartifact.Kind, findingsartifact.Name)
}

func (d *Driver) persistImmutableArtifact(ctx context.Context, key string, body []byte, mediaType, kind, name string) (v1alpha1.ArtifactRef, error) {
	digest := digestBytes(body)
	created, uri, err := d.reportStore.Put(ctx, key, append([]byte(nil), body...), mediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	if !created {
		existing, getErr := d.reportStore.Get(ctx, key)
		if getErr != nil {
			return v1alpha1.ArtifactRef{}, getErr
		}
		if !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrCriticConflict
		}
	}
	if !safeArtifactURI(uri) {
		return v1alpha1.ArtifactRef{}, ErrUnsafeArtifactURI
	}
	return v1alpha1.ArtifactRef{URI: uri, Digest: digest, Kind: kind, Name: name, MediaType: mediaType, SizeBytes: int64(len(body))}, nil
}

func sameCorroborationResult(left, right findingcorroboration.CorroborationResult) bool {
	leftBytes, leftErr := findingcorroboration.CanonicalResultBytes(left)
	rightBytes, rightErr := findingcorroboration.CanonicalResultBytes(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func cloneCriticRunStatus(input *CriticRunStatus) *CriticRunStatus {
	if input == nil {
		return nil
	}
	output := *input
	if input.InputRef != nil {
		ref := *input.InputRef
		output.InputRef = &ref
	}
	return &output
}

func cloneScoringEvidence(input *gate.ScoringEvidence) *gate.ScoringEvidence {
	if input == nil {
		return nil
	}
	output := *input
	output.BlockingReasons = append([]string(nil), input.BlockingReasons...)
	if input.CriticRoute != nil {
		route := *input.CriticRoute
		output.CriticRoute = &route
	}
	return &output
}

func validExternalIdentity(value string) bool {
	return len(value) > 0 && len(value) <= 253 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func criticFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrCriticMissing):
		return "CriticEvidenceMissing"
	case errors.Is(err, ErrCriticUnauthenticated):
		return "CriticEvidenceUnauthenticated"
	case errors.Is(err, ErrCriticConflict):
		return "CriticEvidenceConflict"
	default:
		return "CriticEvidenceMalformed"
	}
}

func criticFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrCriticMissing):
		return "the finished critic runner did not provide a usable corroboration artifact"
	case errors.Is(err, ErrCriticUnauthenticated):
		return "critic evidence was not authenticated by its source"
	case errors.Is(err, ErrCriticConflict):
		return "critic evidence was bound to a different run, patch, Gate, or route"
	default:
		return "critic evidence was malformed or noncanonical"
	}
}

func (d *Driver) finish(ctx context.Context, input Inputs, pending Decision, critic *criticExecution) (Decision, error) {
	evidence, rawEvidence, evidenceErr := d.loadEvidence(ctx, pending.SandboxObservation.Ref, input)
	if errors.Is(evidenceErr, ErrEvidenceNotReady) || errors.Is(evidenceErr, ErrEvidenceUnavailable) {
		code := "EvidenceNotReady"
		message := "verification evidence has not been published"
		if errors.Is(evidenceErr, ErrEvidenceUnavailable) {
			code = "EvidenceUnavailable"
			message = "verification stdout is temporarily unavailable"
		}
		pending.Failure = failure(code, message, true)
		pending.RequeueAfter = defaultRequeueAfter
		return pending, nil
	}

	evidenceValid := evidenceErr == nil
	if evidenceErr != nil && !errors.Is(evidenceErr, ErrEvidenceMissing) && !errors.Is(evidenceErr, ErrEvidenceMalformed) {
		return Decision{}, fmt.Errorf("%w: %v", ErrEvidenceSourceFailed, evidenceErr)
	}
	if len(rawEvidence) > 0 {
		evidenceRef, err := d.persistEvidence(ctx, input, rawEvidence)
		if err != nil {
			return Decision{}, err
		}
		pending.Artifacts = append(pending.Artifacts, evidenceRef)
		if len(pending.Artifacts) > 16 {
			return Decision{}, fmt.Errorf("%w: verification artifact count exceeds 16", ErrStatusProjection)
		}
	}
	if !evidenceValid {
		// A zero observation set is deliberately sent through the real Gate
		// engine. Its configured commands and evidence rules become failed
		// checks, giving the operator a signed, machine-readable rejection.
		evidence = MachineEvidence{}
	}

	var criticInput *findingcorroboration.CorroborationInput
	var criticResult *findingcorroboration.CorroborationResult
	criticInputArtifactDigest := ""
	if critic != nil {
		candidateInput, rawCriticInput, err := d.loadCriticEvidence(ctx, *critic)
		if errors.Is(err, ErrCriticNotReady) || errors.Is(err, ErrCriticUnavailable) {
			pending.Failure = failure("CriticEvidenceNotReady", "critic evidence is not available yet", true)
			pending.RequeueAfter = defaultRequeueAfter
			return pending, nil
		}
		if err != nil {
			pending.Failure = failure(criticFailureCode(err), criticFailureMessage(err), false)
			return pending, nil
		}
		// The external runner/source supplied only CorroborationInput. This is
		// the trusted boundary: derive the result here, exactly once, before
		// anything can influence Gate scoring or publication.
		derived, err := findingcorroboration.Corroborate(candidateInput)
		if err != nil {
			pending.Failure = failure("CriticEvidenceMalformed", "critic input could not be independently corroborated", false)
			return pending, nil
		}
		inputRef, resultRef, err := d.persistCriticArtifacts(ctx, input, candidateInput, rawCriticInput, derived)
		if err != nil {
			return Decision{}, err
		}
		pending.Artifacts = append(pending.Artifacts, inputRef, resultRef)
		if len(pending.Artifacts) > 16 {
			return Decision{}, fmt.Errorf("%w: verification artifact count exceeds 16", ErrStatusProjection)
		}
		criticInput = &candidateInput
		criticResult = &derived
		criticInputArtifactDigest = inputRef.Digest
	}

	gateDecision := gate.Evaluate(gate.Input{
		Scope:        input.Snapshot.Spec.Scope,
		Requirements: input.Snapshot.Gate.Require,
		Commands:     input.Snapshot.Gate.Verify.Commands,
		PolicyRules:  policyRuleProjection(input.PolicyChecks),
		Observations: evidence.observations(),
	})
	if !evidenceValid && gateDecision.Accepted() {
		return Decision{}, fmt.Errorf("%w: invalid evidence produced an accepting Gate decision", ErrEvidenceMalformed)
	}
	executionChecks := make([]gatescoring.ExecutionCheck, 0, len(gateDecision.Checks))
	for _, check := range gateDecision.Checks {
		executionChecks = append(executionChecks, gatescoring.ExecutionCheck{Name: check.Name, Passed: check.Passed, Blocking: true})
	}
	executionScore := int32(0)
	if gateDecision.Accepted() {
		executionScore = v1alpha1.GateScoreScale
	}
	var criticSignal *gatescoring.CriticSignal
	if criticResult != nil {
		criticSignal = &gatescoring.CriticSignal{Result: *criticResult}
	}
	scoring, err := gatescoring.Evaluate(gatescoring.Input{
		Signals:   input.Snapshot.Gate.Signals,
		Execution: gatescoring.ExecutionSignal{ScoreBasisPoints: executionScore, Checks: executionChecks},
		Critic:    criticSignal,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("evaluate Gate scoring: %w", err)
	}
	var route *gate.RouteEvidence
	if critic != nil {
		route = &critic.binding.Route
	}
	reportRef, scoringProjection, signedReportBytes, err := d.persistReport(ctx, input, gateDecision, scoring, route, criticInputArtifactDigest)
	if err != nil {
		return Decision{}, err
	}
	if d.reportAttester != nil {
		evidenceDigests := make([]string, 0, len(pending.Artifacts))
		for _, artifact := range pending.Artifacts {
			if artifact.Kind == "verification-evidence" || artifact.Kind == "verification-evidence-invalid" {
				evidenceDigests = append(evidenceDigests, artifact.Digest)
			}
		}
		attested, attestErr := d.reportAttester.Attest(ctx, verificationattestation.Input{
			RunUID: input.Snapshot.Run.UID, SpecDigest: input.SpecDigest,
			PatchDigest: input.Patch.Digest, ReportRef: reportRef,
			SignedReport:    signedReportBytes,
			EvidenceOptions: evidenceattestation.Options{EvidenceArtifactDigests: evidenceDigests},
		})
		if attestErr != nil {
			return Decision{}, fmt.Errorf("attest authenticated verification report: %w", attestErr)
		}
		if err := validateAttestationResult(attested); err != nil {
			return Decision{}, err
		}
		pending.Artifacts = append(pending.Artifacts, attested.StatementRef, attested.BundleRef)
		if len(pending.Artifacts) > 16 {
			return Decision{}, fmt.Errorf("%w: verification artifact count exceeds 16", ErrStatusProjection)
		}
	}
	if criticInput != nil && criticResult != nil {
		publication := findingsartifact.Artifact{
			Version:          findingsartifact.Version,
			RunUID:           input.Snapshot.Run.UID,
			SpecDigest:       input.SpecDigest,
			BaseSHA:          input.BaseSHA,
			PatchDigest:      input.Patch.Digest,
			GateReportDigest: reportRef.Digest,
			Input:            *criticInput,
			Result:           *criticResult,
		}
		publicationBytes, err := findingsartifact.CanonicalBytes(publication)
		if err != nil {
			return Decision{}, fmt.Errorf("build findings publication artifact: %w", err)
		}
		// Recompute independently from the exact bytes that will be published.
		_, recomputed, err := findingsartifact.Verify(publicationBytes)
		if err != nil {
			return Decision{}, fmt.Errorf("verify findings publication artifact: %w", err)
		}
		if !sameCorroborationResult(recomputed, *criticResult) {
			return Decision{}, ErrCriticConflict
		}
		publicationRef, err := d.persistFindingsPublication(ctx, input, publicationBytes)
		if err != nil {
			return Decision{}, err
		}
		completedArtifacts := append([]v1alpha1.ArtifactRef(nil), pending.Artifacts...)
		completedArtifacts = append(completedArtifacts, publicationRef)
		if len(completedArtifacts) > 16 {
			return Decision{}, fmt.Errorf("%w: verification artifact count exceeds 16", ErrStatusProjection)
		}
		pending.Artifacts = completedArtifacts
	}
	statusChecks := gateDecision.Checks
	if len(statusChecks) > v1alpha1.MaxStatusChecks {
		// The signed report retains the complete Gate decision. Kubernetes
		// status is a bounded projection and carries only the deterministic
		// prefix; ReportRef is the source of truth for all checks.
		statusChecks = statusChecks[:v1alpha1.MaxStatusChecks]
	}
	gateResult := &v1alpha1.GateResult{
		Name:       input.Snapshot.References.Gate.Name,
		UID:        input.Snapshot.References.Gate.UID,
		Generation: input.Snapshot.References.Gate.Generation,
		Mode:       input.Snapshot.Gate.Mode,
		Verdict:    string(scoring.Verdict),
		ReportRef:  &reportRef,
		Checks:     append([]v1alpha1.GateCheck(nil), statusChecks...),
	}
	completed := pending
	completed.Complete = true
	completed.RequeueAfter = 0
	completed.Gate = gateResult
	completed.Scoring = &scoringProjection
	// Verification always hands a completed decision to the controller in
	// Gated. Shadow-mode rejected results still publish a labelled PR; only the
	// controller, which has the immutable Gate mode, may turn an enforcing
	// rejection into terminal Rejected.
	completed.Phase = v1alpha1.PhaseGated
	if !evidenceValid {
		completed.Failure = failure(evidenceFailureCode(evidenceErr), evidenceFailureMessage(evidenceErr), false)
	}
	return completed, nil
}

func validateAttestationResult(result verificationattestation.Result) error {
	statement := result.StatementRef
	bundle := result.BundleRef
	if statement.Kind != verificationattestation.StatementKind || statement.Name != verificationattestation.StatementName || statement.MediaType != evidenceattestation.MediaType ||
		bundle.Kind != verificationattestation.BundleKind || bundle.Name != verificationattestation.BundleName || bundle.MediaType != verificationattestation.BundleMediaType {
		return fmt.Errorf("%w: verification attestation returned an unexpected artifact contract", ErrStatusProjection)
	}
	for _, ref := range []v1alpha1.ArtifactRef{statement, bundle} {
		if !canonical.ValidDigest(ref.Digest) || ref.SizeBytes <= 0 || !safeArtifactURI(ref.URI) {
			return fmt.Errorf("%w: verification attestation returned an invalid artifact reference", ErrStatusProjection)
		}
	}
	if statement.Digest == bundle.Digest || statement.URI == bundle.URI {
		return fmt.Errorf("%w: verification attestation artifacts must be distinct", ErrStatusProjection)
	}
	return nil
}

func (d *Driver) loadEvidence(ctx context.Context, ref sandbox.SandboxRef, input Inputs) (MachineEvidence, []byte, error) {
	frameLimit, err := evidenceFrameLimit(d.maxEvidence)
	if err != nil {
		return MachineEvidence{}, nil, err
	}
	frame, err := d.evidence.ReadFrame(ctx, ref, frameLimit)
	if err != nil {
		switch {
		case errors.Is(err, ErrEvidenceNotReady):
			return MachineEvidence{}, nil, ErrEvidenceNotReady
		case errors.Is(err, ErrEvidenceUnavailable):
			return MachineEvidence{}, nil, ErrEvidenceUnavailable
		case errors.Is(err, ErrEvidenceMissing):
			return MachineEvidence{}, nil, ErrEvidenceMissing
		default:
			return MachineEvidence{}, nil, fmt.Errorf("%w: retrieve verify stdout frame: %v", ErrEvidenceSourceFailed, err)
		}
	}
	body, err := DecodeEvidenceFrame(frame, d.maxEvidence)
	if err != nil {
		return MachineEvidence{}, nil, err
	}
	evidence, err := decodeEvidence(body)
	if err != nil {
		return MachineEvidence{}, body, fmt.Errorf("%w: %v", ErrEvidenceMalformed, err)
	}
	if err := evidence.bind(input); err != nil {
		return MachineEvidence{}, body, fmt.Errorf("%w: %v", ErrEvidenceMalformed, err)
	}
	return evidence, body, nil
}

// EncodeEvidenceFrame produces the exact one-line stdout contract expected by
// EvidenceSource. It accepts only a bounded strict JSON object and emits one
// optional-final-newline-safe record with no surrounding log text.
func EncodeEvidenceFrame(body []byte) ([]byte, error) {
	if len(body) == 0 || int64(len(body)) > MaxEvidenceBytes || strictjson.ValidateObject(body) != nil {
		return nil, ErrEvidenceMalformed
	}
	encoded := base64.RawStdEncoding.EncodeToString(body)
	frame := make([]byte, 0, len(EvidenceFramePrefix)+len(encoded)+1)
	frame = append(frame, EvidenceFramePrefix...)
	frame = append(frame, encoded...)
	frame = append(frame, '\n')
	return frame, nil
}

// DecodeEvidenceFrame extracts the raw verifier evidence from exactly one
// bounded stdout frame. It does not parse the JSON payload; callers can persist
// a decodable-but-malformed payload for audit before rejecting it.
func DecodeEvidenceFrame(frame []byte, maxEvidenceBytes int64) ([]byte, error) {
	limit, err := evidenceFrameLimit(maxEvidenceBytes)
	if err != nil {
		return nil, err
	}
	if len(frame) == 0 {
		return nil, ErrEvidenceMissing
	}
	if int64(len(frame)) > limit {
		return nil, ErrEvidenceMalformed
	}
	line := frame
	if line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) == 0 || bytes.ContainsAny(line, "\r\n") || !bytes.HasPrefix(line, []byte(EvidenceFramePrefix)) {
		return nil, ErrEvidenceMalformed
	}
	encoded := line[len(EvidenceFramePrefix):]
	if len(encoded) == 0 {
		return nil, ErrEvidenceMissing
	}
	body := make([]byte, base64.RawStdEncoding.DecodedLen(len(encoded)))
	n, err := base64.RawStdEncoding.Decode(body, encoded)
	if err != nil {
		return nil, ErrEvidenceMalformed
	}
	body = body[:n]
	if len(body) == 0 {
		return nil, ErrEvidenceMissing
	}
	if int64(len(body)) > maxEvidenceBytes {
		return nil, ErrEvidenceMalformed
	}
	return body, nil
}

func evidenceFrameLimit(maxEvidenceBytes int64) (int64, error) {
	if maxEvidenceBytes <= 0 || maxEvidenceBytes > MaxEvidenceBytes {
		return 0, fmt.Errorf("%w: max evidence bytes must be in 1..%d", ErrInvalidOptions, MaxEvidenceBytes)
	}
	encoded := base64.RawStdEncoding.EncodedLen(int(maxEvidenceBytes))
	return int64(len(EvidenceFramePrefix) + encoded + 1), nil
}

func decodeEvidence(body []byte) (MachineEvidence, error) {
	if err := strictjson.ValidateObject(body); err != nil {
		return MachineEvidence{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var evidence MachineEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return MachineEvidence{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return MachineEvidence{}, errors.New("evidence contains trailing JSON")
	}
	if evidence.SchemaVersion != EvidenceSchemaVersion {
		return MachineEvidence{}, errors.New("unsupported evidence schema")
	}
	if len(evidence.ChangedPaths) > gate.MaxChangedPaths || len(evidence.Commands) > gate.MaxCommandEvidence || len(evidence.PolicyChecks) > 512 {
		return MachineEvidence{}, errors.New("evidence exceeds bounded observation limits")
	}
	return evidence, nil
}

func (e MachineEvidence) bind(input Inputs) error {
	if e.RunUID != input.Snapshot.Run.UID || e.SpecDigest != input.SpecDigest || e.BaseSHA != input.BaseSHA || e.PatchDigest != input.Patch.Digest {
		return errors.New("evidence identity does not match run, spec, base, or patch")
	}
	return nil
}

func (e MachineEvidence) observations() gate.Observations {
	return gate.Observations{
		ChangedPaths:       append([]string(nil), e.ChangedPaths...),
		FilesChanged:       cloneInt64(e.FilesChanged),
		LinesChanged:       cloneInt64(e.LinesChanged),
		HasBinaryFiles:     cloneBool(e.HasBinaryFiles),
		CoverageDelta:      cloneString(e.CoverageDelta),
		NewTestsFailOnBase: cloneBool(e.NewTestsFailOnBase),
		Commands:           append([]gate.CommandObservation(nil), e.commandObservations()...),
		PolicyChecks:       policyObservations(e.PolicyChecks),
	}
}

func policyRuleProjection(checks []policycontract.GateCheckDescriptor) []gate.PolicyRule {
	output := make([]gate.PolicyRule, 0, len(checks))
	for _, check := range checks {
		output = append(output, gate.PolicyRule{
			RuleID: check.RuleID, ContractDigest: check.ContractDigest, ScriptDigest: check.ScriptDigest,
			Blocking: check.RejectsOnFailure, HasScript: check.HasScript,
		})
	}
	return output
}

func policyObservations(observations []policycontract.CheckObservation) []gate.PolicyObservation {
	output := make([]gate.PolicyObservation, 0, len(observations))
	for _, observation := range observations {
		output = append(output, gate.PolicyObservation{
			RuleID: observation.RuleID, ContractDigest: observation.ContractDigest,
			ScriptDigest: observation.ScriptDigest, ExitCode: cloneInt32(observation.ExitCode), Skipped: observation.Skipped,
		})
	}
	return output
}

func (e MachineEvidence) commandObservations() []gate.CommandObservation {
	commands := make([]gate.CommandObservation, 0, len(e.Commands))
	for _, command := range e.Commands {
		commands = append(commands, gate.CommandObservation{
			Index:          command.Index,
			ExitCode:       cloneInt32(command.ExitCode),
			EvidenceDigest: command.EvidenceDigest,
			DurationMillis: cloneInt64(command.DurationMillis),
		})
	}
	return commands
}

func (d *Driver) persistEvidence(ctx context.Context, input Inputs, body []byte) (v1alpha1.ArtifactRef, error) {
	if len(body) == 0 || int64(len(body)) > d.maxEvidence {
		return v1alpha1.ArtifactRef{}, ErrEvidenceMalformed
	}
	sum := sha256.Sum256(body)
	digest := canonical.DigestPrefix + hex.EncodeToString(sum[:])
	mediaType := "application/json"
	kind := "verification-evidence"
	name := "verification-evidence.json"
	extension := ".json"
	if strictjson.ValidateObject(body) != nil {
		mediaType = "application/octet-stream"
		kind = "verification-evidence-invalid"
		name = "verification-evidence.bin"
		extension = ".bin"
	}
	key := "runs/" + input.Snapshot.Run.UID + "/verification/" + trimDigest(input.SpecDigest) + "/" + trimDigest(input.Patch.Digest) + "/evidence/" + trimDigest(digest) + extension
	created, uri, err := d.reportStore.Put(ctx, key, append([]byte(nil), body...), mediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("persist raw verification evidence: %w", err)
	}
	if !created {
		existing, getErr := d.reportStore.Get(ctx, key)
		if getErr != nil {
			return v1alpha1.ArtifactRef{}, fmt.Errorf("verify existing raw verification evidence: %w", getErr)
		}
		if !bytes.Equal(existing, body) {
			return v1alpha1.ArtifactRef{}, ErrEvidenceConflict
		}
	}
	if !safeArtifactURI(uri) {
		return v1alpha1.ArtifactRef{}, ErrUnsafeArtifactURI
	}
	return v1alpha1.ArtifactRef{
		URI: uri, Digest: digest, Kind: kind, Name: name,
		MediaType: mediaType, SizeBytes: int64(len(body)),
	}, nil
}

func (d *Driver) persistReport(ctx context.Context, input Inputs, decision gate.Decision, scoring gatescoring.Result, criticRoute *gate.RouteEvidence, corroborationArtifactDigest string) (v1alpha1.ArtifactRef, gate.ScoringEvidence, []byte, error) {
	gateUID := input.Snapshot.References.Gate.UID
	gateGeneration := input.Snapshot.References.Gate.Generation
	helpers := []gate.ImageEvidence{
		{Name: "fetch", Digest: input.FetchImage},
		{Name: "apply", Digest: input.ApplyImage},
		{Name: "lockdown", Digest: input.LockdownImage},
	}
	skills := make([]string, 0, len(input.Snapshot.Agent.Skills))
	for _, skill := range input.Snapshot.Agent.Skills {
		skills = append(skills, skill.Digest)
	}
	report, err := gate.BuildReport(gate.ReportContext{
		RunUID:              input.Snapshot.Run.UID,
		SpecDigest:          input.SpecDigest,
		BaseSHA:             input.BaseSHA,
		PatchDigest:         input.Patch.Digest,
		GateUID:             gateUID,
		GateGeneration:      gateGeneration,
		RuntimeImageDigest:  input.Snapshot.Agent.Runtime.Image,
		VerifierImageDigest: input.Snapshot.Gate.Verify.Image,
		HelperImageDigests:  helpers,
		SkillDigests:        skills,
		Scoring:             &gate.ScoringContext{Result: scoring, CriticRoute: criticRoute, CorroborationArtifactDigest: corroborationArtifactDigest},
	}, decision)
	if err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("build verification report: %w", err)
	}
	signed, err := d.reportSigner.Sign(report)
	if err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("sign verification report: %w", err)
	}
	wantBytes, err := gate.CanonicalReportBytes(report)
	if err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("canonicalize verification report: %w", err)
	}
	gotBytes, err := gate.CanonicalReportBytes(signed.Report)
	if err != nil || !bytes.Equal(wantBytes, gotBytes) {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, ErrSignerBinding
	}
	if err := gate.VerifySignedReport(signed, nil); err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("verify signed verification report: %w", err)
	}
	encoded, err := gate.SignedReportBytes(signed)
	if err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("encode signed verification report: %w", err)
	}
	sum := sha256.Sum256(encoded)
	reportDigest := canonical.DigestPrefix + hex.EncodeToString(sum[:])
	key := reportKey(input.Snapshot.Run.UID, input.SpecDigest, input.Patch.Digest, reportDigest)
	created, uri, err := d.reportStore.Put(ctx, key, append([]byte(nil), encoded...), reportMediaType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("persist signed verification report: %w", err)
	}
	if !created {
		existing, getErr := d.reportStore.Get(ctx, key)
		if getErr != nil {
			return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, fmt.Errorf("verify existing signed verification report: %w", getErr)
		}
		if !bytes.Equal(existing, encoded) {
			return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, ErrReportConflict
		}
	}
	if !safeArtifactURI(uri) {
		return v1alpha1.ArtifactRef{}, gate.ScoringEvidence{}, nil, ErrUnsafeArtifactURI
	}
	return v1alpha1.ArtifactRef{
		URI:       uri,
		Digest:    reportDigest,
		Kind:      reportKind,
		Name:      reportName,
		MediaType: reportMediaType,
		SizeBytes: int64(len(encoded)),
	}, report.Scoring, append([]byte(nil), encoded...), nil
}

// MachineEvidence is the bounded, pod-produced verification contract. It has
// no verdict field; the Gate engine owns acceptance. Pointer values preserve
// missing-versus-zero evidence semantics.
type MachineEvidence struct {
	SchemaVersion string `json:"schemaVersion"`
	RunUID        string `json:"runUID"`
	SpecDigest    string `json:"specDigest"`
	BaseSHA       string `json:"baseSHA"`
	PatchDigest   string `json:"patchDigest"`

	ChangedPaths       []string                          `json:"changedPaths"`
	FilesChanged       *int64                            `json:"filesChanged,omitempty"`
	LinesChanged       *int64                            `json:"linesChanged,omitempty"`
	HasBinaryFiles     *bool                             `json:"hasBinaryFiles,omitempty"`
	CoverageDelta      *string                           `json:"coverageDelta,omitempty"`
	NewTestsFailOnBase *bool                             `json:"newTestsFailOnBase,omitempty"`
	Commands           []CommandEvidence                 `json:"commands"`
	PolicyChecks       []policycontract.CheckObservation `json:"policyChecks,omitempty"`
}

// CommandEvidence is one machine-observed command result. The per-command
// evidence digest binds the command's detailed output without placing that
// unbounded output into AgentRun status.
type CommandEvidence struct {
	Index          int    `json:"index"`
	ExitCode       *int32 `json:"exitCode,omitempty"`
	EvidenceDigest string `json:"evidenceDigest,omitempty"`
	DurationMillis *int64 `json:"durationMillis,omitempty"`
}

func childRef(ref sandbox.SandboxRef) (v1alpha1.ChildRef, error) {
	if ref.Name == "" || len(ref.Name) > 253 || !sandbox.ValidChildKind(ref.Kind) || ref.OwnerUID == "" || len(string(ref.UID)) > maxStatusChildUIDBytes || len(string(ref.Role)) > 64 || !canonical.ValidDigest(ref.SpecDigest) || !canonical.ValidDigest(ref.PlanFingerprint) {
		return v1alpha1.ChildRef{}, fmt.Errorf("%w: verify child reference is not status-safe", ErrInvalidInput)
	}
	return v1alpha1.ChildRef{
		Name:            ref.Name,
		Kind:            ref.Kind,
		UID:             string(ref.UID),
		Role:            string(ref.Role),
		SpecDigest:      ref.SpecDigest,
		PlanFingerprint: ref.PlanFingerprint,
	}, nil
}

func validateSandboxIdentity(ref sandbox.SandboxRef, input Inputs) error {
	if ref.Namespace != input.Snapshot.Run.Namespace || !sandbox.ValidChildKind(ref.Kind) || ref.OwnerUID != types.UID(input.Snapshot.Run.UID) || ref.Role != sandbox.RoleVerify || ref.SpecDigest != input.SpecDigest || !canonical.ValidDigest(ref.PlanFingerprint) {
		return fmt.Errorf("%w: verify child identity does not match run, role, or spec digest", ErrInvalidInput)
	}
	return nil
}

func reportKey(runUID, specDigest, patchDigest, reportDigest string) string {
	return "runs/" + runUID + "/verification/" + trimDigest(specDigest) + "/" + trimDigest(patchDigest) + "/" + trimDigest(reportDigest) + ".json"
}

func trimDigest(value string) string { return strings.TrimPrefix(value, canonical.DigestPrefix) }

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func safeArtifactURI(value string) bool {
	if len(value) == 0 || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "@") || strings.Contains(value, "?") || strings.Contains(value, "#") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && parsed.User == nil && (parsed.Scheme == "s3" || parsed.Scheme == "https")
}

func evidenceFailureCode(err error) string {
	switch {
	case errors.Is(err, ErrEvidenceMissing):
		return "EvidenceMissing"
	default:
		return "EvidenceMalformed"
	}
}

func evidenceFailureMessage(err error) string {
	switch {
	case errors.Is(err, ErrEvidenceMissing):
		return "the finished verify Sandbox did not emit an evidence frame"
	default:
		return "the verifier stdout frame was malformed or its evidence identity did not match"
	}
}

func failure(code, message string, retryable bool) *v1alpha1.FailureStatus {
	return &v1alpha1.FailureStatus{Code: code, Message: boundedMessage(message), Retryable: retryable}
}

func boundedMessage(value string) string {
	if len(value) <= v1alpha1.MaxStatusMessage {
		return value
	}
	value = value[:v1alpha1.MaxStatusMessage]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func cloneGateResult(input *v1alpha1.GateResult) *v1alpha1.GateResult {
	if input == nil {
		return nil
	}
	output := &v1alpha1.GateResult{
		Name:       input.Name,
		UID:        input.UID,
		Generation: input.Generation,
		Mode:       input.Mode,
		Verdict:    input.Verdict,
		Checks:     append([]v1alpha1.GateCheck(nil), input.Checks...),
	}
	if input.ReportRef != nil {
		ref := *input.ReportRef
		output.ReportRef = &ref
	}
	return output
}

func cloneFailure(input *v1alpha1.FailureStatus) *v1alpha1.FailureStatus {
	if input == nil {
		return nil
	}
	output := *input
	return &output
}

func cloneInt32(input *int32) *int32 {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneInt64(input *int64) *int64 {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneBool(input *bool) *bool {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneString(input *string) *string {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

var _ ReportSigner = SignerFunc(nil)
