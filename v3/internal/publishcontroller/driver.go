// Package publishcontroller binds the immutable AgentRun publication inputs to
// the controller-owned publish protocol.
//
// This package deliberately has no Kubernetes or credential dependency. The
// operator resolves Kubernetes objects and constructs an Input; this driver
// verifies the immutable artifacts and signed Gate report before delegating
// the one external effect to publish.Publisher. The publisher is the only
// seam that can reach GitHub, and its interface has no merge operation.
package publishcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/artifacts"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/gate"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubpublish"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
)

const (
	patchKind         = "patch"
	patchName         = "patch.diff"
	patchMediaType    = "text/x-diff"
	manifestKind      = "patch-manifest"
	manifestName      = "patch-manifest.json"
	manifestMediaType = "application/vnd.agents-gateway.patch-manifest.v1+json"
	reportKind        = "verification-report"
	reportName        = "verification-report.json"
	reportMediaType   = "application/json"

	// MaxStatusURLBytes keeps the result safe to copy directly into
	// AgentRun.status. The API type imposes the same 1024-byte ceiling.
	MaxStatusURLBytes = 1024
	maxRunUIDBytes    = 128
	maxRunNameBytes   = 253
	maxBaseRefBytes   = 256
	maxGateUIDBytes   = 128
)

var (
	ErrInvalidContext         = errors.New("publishcontroller: invalid context")
	ErrInvalidInput           = errors.New("publishcontroller: invalid input")
	ErrArtifactMissing        = errors.New("publishcontroller: required artifact is missing")
	ErrArtifactUnavailable    = errors.New("publishcontroller: required artifact is unavailable")
	ErrMalformedArtifactURI   = errors.New("publishcontroller: malformed artifact URI")
	ErrArtifactDigestMismatch = errors.New("publishcontroller: artifact digest mismatch")
	ErrArtifactIdentity       = errors.New("publishcontroller: artifact identity mismatch")
	ErrManifestInvalid        = errors.New("publishcontroller: changed-file manifest is invalid")
	ErrGateReportInvalid      = errors.New("publishcontroller: signed Gate report is invalid")
	ErrGateReportUntrusted    = errors.New("publishcontroller: signed Gate report is untrusted")
	ErrGateRejected           = errors.New("publishcontroller: Gate rejected the run")
	ErrPublishFailed          = errors.New("publishcontroller: publication failed")
	ErrUnknownEffect          = publish.ErrUnknownEffect
	ErrPending                = errors.New("publishcontroller: publication is pending")
	ErrInvalidOutcome         = errors.New("publishcontroller: publisher returned an invalid outcome")
	ErrArtifactNotFound       = errors.New("publishcontroller: artifact was not found")
)

// ArtifactLocation is the parsed, credential-free object location passed to
// an ArtifactReader. It contains no URL query, fragment, userinfo, or secret.
type ArtifactLocation struct {
	Bucket string
	Key    string
}

// ArtifactReader is the only artifact-read seam. Implementations must perform
// a bounded immutable read of exactly location.Key from location.Bucket. The
// driver verifies the returned bytes and never exposes reader errors directly.
type ArtifactReader interface {
	Get(context.Context, ArtifactLocation) ([]byte, error)
}

// Publisher is intentionally the same narrow operation exposed by
// publish.Publisher. It has no merge method and receives a credential-free
// request. *publish.Publisher satisfies this interface.
type Publisher interface {
	Publish(context.Context, publish.Request) (publish.Result, error)
}

var _ Publisher = (*publish.Publisher)(nil)
var _ publish.Client = (*githubpublish.Client)(nil)
var _ artifacts.Store = (*objectstore.Store)(nil)

// StoreReader adapts the existing artifacts.Store/objectstore contract to the
// URI-aware ArtifactReader seam. Prefix is the object-store adapter prefix,
// not part of the immutable key contract. A production caller should pass the
// same bucket and prefix used to construct its objectstore.Store.
type StoreReader struct {
	store  artifacts.Store
	bucket string
	prefix string
}

// NewStoreReader constructs a strict reader over the existing immutable
// artifacts.Store contract. It never accepts a caller-supplied bucket and
// therefore cannot be redirected by a status URI to another bucket.
func NewStoreReader(store artifacts.Store, bucket, prefix string) (*StoreReader, error) {
	if store == nil || !validBucket(bucket) {
		return nil, ErrInvalidInput
	}
	prefix = strings.Trim(prefix, "/")
	if prefix != "" && !validObjectKey(prefix) {
		return nil, ErrInvalidInput
	}
	return &StoreReader{store: store, bucket: bucket, prefix: prefix}, nil
}

// Get reads the exact relative key represented by location after removing the
// configured object-store prefix. Store errors are intentionally returned to
// the Driver, which replaces them with bounded public classifications.
func (r *StoreReader) Get(ctx context.Context, location ArtifactLocation) ([]byte, error) {
	if r == nil || r.store == nil || ctx == nil || location.Bucket != r.bucket || !validObjectKey(location.Key) {
		return nil, ErrArtifactUnavailable
	}
	key := location.Key
	if r.prefix != "" {
		prefix := r.prefix + "/"
		if !strings.HasPrefix(key, prefix) {
			return nil, ErrArtifactUnavailable
		}
		key = strings.TrimPrefix(key, prefix)
		if !validObjectKey(key) {
			return nil, ErrArtifactUnavailable
		}
	}
	body, err := r.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, ErrArtifactNotFound) {
			return nil, ErrArtifactNotFound
		}
		return nil, ErrArtifactUnavailable
	}
	return append([]byte(nil), body...), nil
}

// Input is the immutable, credential-free publication contract supplied by
// the AgentRun controller. Patch, Manifest, and GateReport are the refs that
// were persisted by capture/verification; the driver does not trust their
// URI or content until both are checked.
type Input struct {
	RunUID  string
	RunName string
	Repo    githubapp.Repository

	BaseRef    string
	BaseSHA    string
	SpecDigest string

	GateUID        string
	GateGeneration int64
	GateMode       v1alpha1.GateMode

	Patch      v1alpha1.ArtifactRef
	Manifest   v1alpha1.ArtifactRef
	GateReport v1alpha1.ArtifactRef
	// OutputMode is the resolved AgentRun.spec.output.mode. Empty is the
	// backwards-compatible patch mode. Findings-only requires a pre-existing
	// target PR; OutputBoth derives its target from the patch result.
	OutputMode        v1alpha1.OutputMode
	TargetPullRequest int64
	Findings          v1alpha1.ArtifactRef

	PublishMode v1alpha1.PublishMode
	Title       string
	Labels      []string
}

// State is the controller-facing publication result. Rejected is a Gate
// decision, while the other states describe the external publication effect.
type State string

const (
	StatePending   State = "pending"
	StateSkipped   State = "skipped"
	StateRejected  State = "rejected"
	StateFailed    State = "failed"
	StateSucceeded State = "succeeded"
	StateUnknown   State = "unknown"
)

// Result contains only bounded status-ready data. State is the aggregate
// publication state. For OutputBoth, callers must inspect PatchState and
// FindingsState to distinguish the two independent effects.
//
// Effect belongs exclusively to the patch publication and is safe to copy into
// AgentRunStatus.Effect. Findings publication never replaces it. Unknown never
// contains a PR URL because a URL returned alongside an unproven effect is not
// trustworthy.
type Result struct {
	State             State
	PatchState        State
	FindingsState     State
	Effect            *v1alpha1.EffectSummary
	PullRequestNumber int64
	PullRequestURL    string
	// Findings contains bounded per-finding publication metadata. It is kept
	// outside AgentRun.status until the controller adds a dedicated projection.
	Findings          []publish.FindingOutcome
	AdvisoryEffectKey string
}

// Options wires the two narrow seams and the trusted Gate verification key.
// The private signing key is intentionally absent; it belongs only to the
// verification controller that produced the report.
type Options struct {
	Artifacts ArtifactReader
	Publisher Publisher
	// FindingsPublisher is optional for backwards-compatible patch-only
	// callers. The production *publish.Publisher implements this seam; a
	// findings/both run fails closed when it is absent.
	FindingsPublisher    FindingsPublisher
	TrustedGatePublicKey ed25519.PublicKey
}

// FindingsPublisher is the separate review publication operation. It must
// use the same controller-owned GitHub App client and effect ledger as the
// patch Publisher.
type FindingsPublisher interface {
	PublishFindings(context.Context, publish.FindingsRequest) (publish.FindingsResult, error)
}

// Driver validates immutable publication inputs and executes exactly one
// publish.Publisher operation. It is safe for concurrent calls when the
// injected reader and publisher are safe for concurrent calls.
type Driver struct {
	artifacts ArtifactReader
	publisher Publisher
	findings  FindingsPublisher
	trusted   ed25519.PublicKey
}

// New constructs a fail-closed publication driver.
func New(options Options) (*Driver, error) {
	if options.Artifacts == nil || options.Publisher == nil || len(options.TrustedGatePublicKey) != ed25519.PublicKeySize {
		return nil, ErrInvalidInput
	}
	findingsPublisher := options.FindingsPublisher
	if findingsPublisher == nil {
		if candidate, ok := options.Publisher.(FindingsPublisher); ok {
			findingsPublisher = candidate
		}
	}
	return &Driver{
		artifacts: options.Artifacts,
		publisher: options.Publisher,
		findings:  findingsPublisher,
		trusted:   append(ed25519.PublicKey(nil), options.TrustedGatePublicKey...),
	}, nil
}

// Publish loads and verifies the exact patch, changed-file manifest, and
// signed Gate report, then delegates to the claim-before-mutation publisher.
// It never retries a publisher operation and never performs a merge.
func (d *Driver) Publish(ctx context.Context, input Input) (Result, error) {
	if ctx == nil {
		return Result{}, ErrInvalidContext
	}
	if d == nil || d.artifacts == nil || d.publisher == nil || len(d.trusted) != ed25519.PublicKeySize {
		return Result{}, ErrInvalidInput
	}
	if err := validateInput(input); err != nil {
		return Result{}, err
	}

	mode := findingsOutputMode(input.OutputMode)
	if findingsMode(mode) && d.findings == nil {
		// Check this before loading or publishing the patch so an OutputBoth
		// run can never create a PR and only then discover that its findings
		// publication seam is unavailable.
		return Result{}, ErrInvalidInput
	}
	signed, err := d.loadReport(ctx, input)
	if err != nil {
		return Result{}, err
	}
	var patch []byte
	var files []publish.FileChange
	if !findingsOnlyMode(mode) {
		patch, err = d.loadPatch(ctx, input)
		if err != nil {
			return Result{}, err
		}
		files, err = d.loadManifest(ctx, input)
		if err != nil {
			return Result{}, err
		}
	}
	if err := bindReport(input, signed, files); err != nil {
		return Result{}, err
	}
	var findingsResult findingcorroboration.CorroborationResult
	var findingsArtifactDigest string
	if findingsMode(mode) {
		findingsResult, findingsArtifactDigest, err = d.loadFindings(ctx, input, signed.Report.PatchDigest)
		if err != nil {
			return Result{}, err
		}
	}

	request := publish.Request{
		RunUID:     input.RunUID,
		RunName:    input.RunName,
		Repo:       input.Repo,
		BaseRef:    input.BaseRef,
		BaseSHA:    input.BaseSHA,
		Patch:      publish.Patch{Digest: input.Patch.Digest, Raw: patch, ManifestDigest: input.Manifest.Digest, Files: files},
		SpecDigest: input.SpecDigest,
		Gate: publish.GateState{
			Mode:         input.GateMode,
			Verdict:      string(signed.Report.Verdict),
			ReportDigest: input.GateReport.Digest,
		},
		RuntimeImageDigest:  signed.Report.RuntimeImageDigest,
		VerifierImageDigest: signed.Report.VerifierImageDigest,
		HelperImageDigests:  helperImages(signed.Report.HelperImageDigests),
		SkillDigests:        append([]string(nil), signed.Report.SkillDigests...),
		PublishMode:         input.PublishMode,
		Title:               input.Title,
		Labels:              append([]string(nil), input.Labels...),
	}

	// A rejected enforcing Gate is a normal, terminal Gate result, but it is
	// never a publication request. This check is repeated by publish.Publisher
	// as defense in depth; the driver must not rely on that implementation detail.
	if input.GateMode == v1alpha1.GateEnforcing && signed.Report.Verdict == gate.Rejected {
		return Result{State: StateRejected, PatchState: StateRejected, FindingsState: StateSkipped}, nil
	}

	if findingsOnlyMode(mode) {
		return d.publishFindings(ctx, input, input.TargetPullRequest, signed.Report.PatchDigest, findingsArtifactDigest, findingsResult)
	}

	if input.PublishMode == v1alpha1.PublishNone {
		if signed.Report.Verdict == gate.Rejected {
			return Result{State: StateRejected, PatchState: StateRejected, FindingsState: StateSkipped}, nil
		}
		if !findingsMode(mode) {
			return Result{State: StateSkipped, PatchState: StateSkipped, FindingsState: StateSkipped}, nil
		}
	}

	effectKey, err := effects.EffectKey(input.RunUID, input.BaseSHA, input.Patch.Digest, publish.Operation)
	if err != nil {
		return Result{}, ErrInvalidInput
	}
	published, publishErr := d.publisher.Publish(ctx, request)
	patchResult, patchErr := mapOutcome(effectKey, published, publishErr)
	if patchErr != nil || !findingsMode(mode) {
		return patchResult, patchErr
	}
	// Findings target the pull request created by the patch publication. A
	// pending, skipped, rejected, or otherwise non-successful patch outcome
	// must not start a second external effect or be relabeled as a findings
	// result. The component state remains explicit for the caller.
	if patchResult.PatchState != StateSucceeded {
		return patchResult, nil
	}
	if patchResult.PullRequestNumber <= 0 || patchResult.PullRequestNumber > publish.MaxPullRequestNumber {
		return Result{State: StateUnknown, PatchState: StateUnknown, FindingsState: StateSkipped}, ErrUnknownEffect
	}
	findings, findingsErr := d.publishFindings(ctx, input, patchResult.PullRequestNumber, signed.Report.PatchDigest, findingsArtifactDigest, findingsResult)
	patchResult.FindingsState = findings.FindingsState
	patchResult.Findings = findings.Findings
	patchResult.AdvisoryEffectKey = findings.AdvisoryEffectKey
	if findingsErr != nil {
		patchResult.State = combinedState(patchResult.PatchState, patchResult.FindingsState)
		return patchResult, findingsErr
	}
	patchResult.State = combinedState(patchResult.PatchState, patchResult.FindingsState)
	return patchResult, nil
}

func (d *Driver) loadPatch(ctx context.Context, input Input) ([]byte, error) {
	location, err := locationFor(input.Patch, patchKind, patchName, patchMediaType)
	if err != nil {
		return nil, err
	}
	expected := patchKeySuffix(input.RunUID, input.SpecDigest, input.Patch.Digest)
	if !keyHasSuffix(location.Key, expected) {
		return nil, ErrArtifactIdentity
	}
	body, err := d.read(ctx, location, int64(publish.MaxPatchBytes), input.Patch.SizeBytes, false)
	if err != nil {
		return nil, err
	}
	if publish.DigestForPatchBytes(body) != input.Patch.Digest {
		return nil, ErrArtifactDigestMismatch
	}
	return body, nil
}

func (d *Driver) loadManifest(ctx context.Context, input Input) ([]publish.FileChange, error) {
	location, err := locationFor(input.Manifest, manifestKind, manifestName, manifestMediaType)
	if err != nil {
		if errors.Is(err, ErrArtifactMissing) {
			return nil, err
		}
		return nil, err
	}
	expected := manifestKeySuffix(input.RunUID, input.SpecDigest, input.Manifest.Digest)
	if !keyHasSuffix(location.Key, expected) {
		return nil, ErrArtifactIdentity
	}
	body, err := d.read(ctx, location, int64(publish.MaxPatchManifestBytes), input.Manifest.SizeBytes, true)
	if err != nil {
		return nil, err
	}
	files, err := publish.DecodePatchManifest(body, input.Manifest.Digest)
	if err != nil {
		return nil, ErrManifestInvalid
	}
	return files, nil
}

func (d *Driver) loadReport(ctx context.Context, input Input) (gate.SignedReport, error) {
	location, err := locationFor(input.GateReport, reportKind, reportName, reportMediaType)
	if err != nil {
		return gate.SignedReport{}, err
	}
	expected := reportKeySuffix(input.RunUID, input.SpecDigest, input.Patch.Digest, input.GateReport.Digest)
	if !keyHasSuffix(location.Key, expected) {
		return gate.SignedReport{}, ErrArtifactIdentity
	}
	body, err := d.read(ctx, location, int64(gate.MaxReportBytes+1024), input.GateReport.SizeBytes, false)
	if err != nil {
		return gate.SignedReport{}, err
	}
	if digestBytes(body) != input.GateReport.Digest {
		return gate.SignedReport{}, ErrArtifactDigestMismatch
	}
	signed, err := gate.ParseSignedReport(body)
	if err != nil {
		return gate.SignedReport{}, ErrGateReportInvalid
	}
	canonicalBytes, err := gate.SignedReportBytes(signed)
	if err != nil || !bytes.Equal(canonicalBytes, body) {
		return gate.SignedReport{}, ErrGateReportInvalid
	}
	if err := gate.VerifySignedReport(signed, d.trusted); err != nil {
		return gate.SignedReport{}, ErrGateReportUntrusted
	}
	return signed, nil
}

func (d *Driver) read(ctx context.Context, location ArtifactLocation, max, expectedSize int64, missingIsMissing bool) ([]byte, error) {
	body, err := d.artifacts.Get(ctx, location)
	if err != nil {
		if missingIsMissing && errors.Is(err, ErrArtifactNotFound) {
			return nil, ErrArtifactMissing
		}
		return nil, ErrArtifactUnavailable
	}
	if len(body) == 0 || int64(len(body)) > max || (expectedSize > 0 && int64(len(body)) != expectedSize) {
		return nil, ErrArtifactDigestMismatch
	}
	return append([]byte(nil), body...), nil
}

func validateInput(input Input) error {
	if !validIdentifier(input.RunUID, maxRunUIDBytes) || !validRunName(input.RunName) || input.Repo.Validate() != nil || !validBaseRef(input.BaseRef) || !resolved.ValidBaseSHA(input.BaseSHA) || !canonical.ValidDigest(input.SpecDigest) || !validIdentifier(input.GateUID, maxGateUIDBytes) || input.GateGeneration <= 0 {
		return ErrInvalidInput
	}
	if input.GateMode != v1alpha1.GateShadow && input.GateMode != v1alpha1.GateEnforcing {
		return ErrInvalidInput
	}
	if input.PublishMode != v1alpha1.PublishNone && input.PublishMode != v1alpha1.PublishPullRequest {
		return ErrInvalidInput
	}
	if !outputModeValid(input.OutputMode) {
		return ErrInvalidInput
	}
	mode := findingsOutputMode(input.OutputMode)
	if err := validateRef(input.Patch, patchKind, patchName, patchMediaType); err != nil {
		return err
	}
	if !findingsOnlyMode(mode) {
		if input.Manifest.URI == "" || input.Manifest.Digest == "" {
			return ErrArtifactMissing
		}
		if err := validateRef(input.Manifest, manifestKind, manifestName, manifestMediaType); err != nil {
			return err
		}
	}
	if findingsMode(mode) {
		if !findingsArtifactRef(input.Findings) {
			return ErrInvalidInput
		}
		if _, err := parseArtifactURI(input.Findings.URI); err != nil {
			return ErrMalformedArtifactURI
		}
	}
	if findingsOnlyMode(mode) && (input.TargetPullRequest <= 0 || input.TargetPullRequest > publish.MaxPullRequestNumber) {
		return ErrInvalidInput
	}
	if findingsMode(mode) && input.PublishMode != v1alpha1.PublishPullRequest {
		return ErrInvalidInput
	}
	if err := validateRef(input.GateReport, reportKind, reportName, reportMediaType); err != nil {
		return err
	}
	return nil
}

func validateRef(ref v1alpha1.ArtifactRef, kind, name, mediaType string) error {
	if ref.Kind != kind || ref.Name != name || ref.MediaType != mediaType || !canonical.ValidDigest(ref.Digest) || ref.SizeBytes < 0 || ref.SizeBytes > 1<<30 {
		return ErrInvalidInput
	}
	if _, err := parseArtifactURI(ref.URI); err != nil {
		return ErrMalformedArtifactURI
	}
	return nil
}

func locationFor(ref v1alpha1.ArtifactRef, kind, name, mediaType string) (ArtifactLocation, error) {
	if ref.URI == "" || ref.Digest == "" {
		if kind == manifestKind {
			return ArtifactLocation{}, ErrArtifactMissing
		}
		return ArtifactLocation{}, ErrInvalidInput
	}
	if err := validateRef(ref, kind, name, mediaType); err != nil {
		return ArtifactLocation{}, err
	}
	return parseArtifactURI(ref.URI)
}

func parseArtifactURI(raw string) (ArtifactLocation, error) {
	if len(raw) == 0 || len(raw) > 1024 || strings.ContainsAny(raw, "\x00\r\n") {
		return ArtifactLocation{}, ErrMalformedArtifactURI
	}
	location, err := verifyfetch.ParseS3URI(raw)
	if err != nil || location.Bucket == "" || location.Key == "" {
		return ArtifactLocation{}, ErrMalformedArtifactURI
	}
	return ArtifactLocation{Bucket: location.Bucket, Key: location.Key}, nil
}

func bindReport(input Input, signed gate.SignedReport, files []publish.FileChange) error {
	report := signed.Report
	if report.RunUID != input.RunUID || report.SpecDigest != input.SpecDigest || report.BaseSHA != input.BaseSHA || report.PatchDigest != input.Patch.Digest || report.GateUID != input.GateUID || report.GateGeneration != input.GateGeneration {
		return ErrArtifactIdentity
	}
	if report.Verdict != gate.Accepted && report.Verdict != gate.Rejected {
		return ErrGateReportInvalid
	}
	if files != nil {
		paths := make([]string, len(files))
		for i := range files {
			paths[i] = files[i].Path
		}
		if len(paths) != len(report.Evidence.ChangedPaths) {
			return ErrArtifactIdentity
		}
		for i := range paths {
			if paths[i] != report.Evidence.ChangedPaths[i] {
				return ErrArtifactIdentity
			}
		}
		if report.Evidence.FilesChanged != nil && *report.Evidence.FilesChanged != int64(len(files)) {
			return ErrArtifactIdentity
		}
	}
	return nil
}

func mapOutcome(effectKey string, result publish.Result, err error) (Result, error) {
	if errors.Is(err, ErrPending) {
		return effectResult(StatePending, effectKey, 0, ""), nil
	}
	if result.State == publish.StateUnknown || errors.Is(err, publish.ErrUnknownEffect) {
		return effectResult(StateUnknown, effectKey, 0, ""), ErrUnknownEffect
	}
	if err != nil {
		if result.State == publish.StateFailed {
			return effectResult(StateFailed, effectKey, 0, ""), ErrPublishFailed
		}
		// An error without a proven terminal state is deliberately treated as
		// unknown. The controller must never retry a possibly-applied effect.
		return effectResult(StateUnknown, effectKey, 0, ""), ErrUnknownEffect
	}
	if result.EffectKey != "" && result.EffectKey != effectKey {
		return effectResult(StateUnknown, effectKey, 0, ""), ErrUnknownEffect
	}
	switch result.State {
	case publish.StateSucceeded:
		if !validPullRequestURLForNumber(result.PullRequestURL, result.PullRequestNumber) {
			return effectResult(StateUnknown, effectKey, 0, ""), ErrUnknownEffect
		}
		return effectResult(StateSucceeded, effectKey, result.PullRequestNumber, result.PullRequestURL), nil
	case publish.StateFailed:
		return effectResult(StateFailed, effectKey, 0, ""), ErrPublishFailed
	case publish.StateRejected:
		return effectResult(StateRejected, effectKey, 0, ""), nil
	case publish.StateSkipped:
		return effectResult(StateSkipped, effectKey, 0, ""), nil
	default:
		return effectResult(StateUnknown, effectKey, 0, ""), ErrUnknownEffect
	}
}

func effectResult(state State, key string, pullRequestNumber int64, pullRequestURL string) Result {
	if state == StateSkipped || state == StateRejected {
		return Result{State: state, PatchState: state, FindingsState: StateSkipped}
	}
	effectState := v1alpha1.EffectState(state)
	effect := &v1alpha1.EffectSummary{Key: key, State: effectState, PullRequestURL: pullRequestURL}
	return Result{State: state, PatchState: state, FindingsState: StateSkipped, Effect: effect, PullRequestNumber: pullRequestNumber, PullRequestURL: pullRequestURL}
}

// combinedState is the aggregate state for OutputBoth. Component states stay
// independent on Result; this value only answers whether the requested
// publication as a whole is complete. Unknown is deliberately stronger than
// failed because one ambiguous effect must not be retried automatically.
func combinedState(patch, findings State) State {
	switch {
	case patch == StateUnknown || findings == StateUnknown:
		return StateUnknown
	case patch == StateFailed || findings == StateFailed:
		return StateFailed
	case patch == StateRejected || findings == StateRejected:
		return StateRejected
	case patch == StatePending || findings == StatePending:
		return StatePending
	case patch == StateSkipped:
		return findings
	case findings == StateSkipped:
		return patch
	case patch == StateSucceeded && findings == StateSucceeded:
		return StateSucceeded
	default:
		return StateUnknown
	}
}

func helperImages(images []gate.ImageEvidence) []publish.ImageEvidence {
	output := make([]publish.ImageEvidence, len(images))
	for i := range images {
		output[i] = publish.ImageEvidence{Name: images[i].Name, Digest: images[i].Digest}
	}
	return output
}

func patchKeySuffix(runUID, specDigest, patchDigest string) string {
	return "runs/" + runUID + "/patches/" + digestHex(specDigest) + "/" + digestHex(patchDigest) + ".diff"
}

func manifestKeySuffix(runUID, specDigest, manifestDigest string) string {
	return "runs/" + runUID + "/patches/" + digestHex(specDigest) + "/" + digestHex(manifestDigest) + ".manifest.json"
}

func reportKeySuffix(runUID, specDigest, patchDigest, reportDigest string) string {
	return "runs/" + runUID + "/verification/" + digestHex(specDigest) + "/" + digestHex(patchDigest) + "/" + digestHex(reportDigest) + ".json"
}

func digestHex(value string) string { return strings.TrimPrefix(value, canonical.DigestPrefix) }

func keyHasSuffix(key, suffix string) bool {
	return key == suffix || strings.HasSuffix(key, "/"+suffix)
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func validPullRequestURL(value string) bool {
	if value == "" || len(value) > MaxStatusURLBytes || strings.ContainsAny(value, "\x00\r\n\t ") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && strings.HasPrefix(parsed.Path, "/")
}

func validPullRequestURLForNumber(value string, number int64) bool {
	if number <= 0 || number > publish.MaxPullRequestNumber || !validPullRequestURL(value) {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && strings.HasSuffix(parsed.Path, "/pull/"+strconv.FormatInt(number, 10))
}

func validText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func validIdentifier(value string, max int) bool {
	return validText(value, max) && !strings.ContainsAny(value, "/\\")
}

func validRunName(value string) bool {
	if value == "" || len(value) > maxRunNameBytes || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func validBaseRef(value string) bool {
	if !validText(value, maxBaseRefBytes) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func validBucket(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.ToLower(value) != value || strings.Contains(value, "..") || strings.HasPrefix(value, "xn--") || strings.HasSuffix(value, "-s3alias") || strings.HasSuffix(value, "--ol-s3") || strings.HasSuffix(value, ".mrap") || strings.HasSuffix(value, "--x-s3") || strings.HasSuffix(value, "--table") {
		return false
	}
	for i, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-') || (i == 0 && r == '-') || (i == len(value)-1 && r == '-') {
			return false
		}
	}
	return true
}

func validObjectKey(value string) bool {
	if value == "" || len(value) > 1024 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
