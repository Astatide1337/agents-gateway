package publish

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
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
)

const (
	// Operation is the stable effect-ledger operation name. It is part of the
	// effect-key preimage and must not be changed without a migration.
	Operation = "publish/pr"

	// Evidence and request limits are intentionally below common GitHub API
	// limits so status and audit projections remain bounded too.
	MaxRunUIDBytes         = 128
	MaxRunNameBytes        = 253
	MaxBaseRefBytes        = 256
	MaxBaseSHABytes        = 64
	MaxPatchFiles          = 256
	MaxPatchPathBytes      = 512
	MaxPatchBytes          = 8 << 20
	MaxPatchManifestBytes  = 16 << 20
	MaxTitleBytes          = 256
	MaxBodyBytes           = 16 << 10
	MaxLabels              = 16
	MaxLabelBytes          = 64
	MaxImages              = 16
	MaxSkills              = 32
	MaxImageRefBytes       = 512
	MaxResultRefBytes      = 4096
	MaxCommitMessageBytes  = 256
	MaxPullRequestURLBytes = 1024
	// Bump this when the credential-free ledger result contract changes. Older
	// records are intentionally not replayable because they do not prove the
	// pull-request number needed by findings publication.
	MaxEffectResultVersion = 2
	shortUIDHexBytes       = 12
	branchNameMaxBytes     = 255
	branchNamePrefix       = "agw/"
	defaultTitlePrefix     = "AGW: "
	defaultCommitPrefix    = "agw: "
	effectResultPrefix     = "agw-result-v2:"
	failureResultPrefix    = "agw-failed:"
)

var (
	// These are intentionally static. Publisher never returns an underlying
	// GitHub response, URL, HTTP body, token, or provider error string.
	errClaimAmbiguous      = errors.New("publish: effect claim is ambiguous")
	errClaimConflict       = errors.New("publish: effect claim conflicts")
	errLedgerAmbiguous     = errors.New("publish: effect ledger outcome is ambiguous")
	errGitHubUnavailable   = errors.New("publish: GitHub operation is unavailable")
	errGitHubResponse      = errors.New("publish: GitHub proof is invalid")
	errDeterministicCommit = errors.New("publish: GitHub operation conflicts with requested state")
)

// ResultState is the controller-facing outcome of publication.
type ResultState string

const (
	StateSkipped   ResultState = "skipped"
	StateRejected  ResultState = "rejected"
	StateSucceeded ResultState = "succeeded"
	StateFailed    ResultState = "failed"
	StateUnknown   ResultState = "unknown"
)

// ImageEvidence is the public image identity copied into the PR evidence
// body. Digest-pinned image references are validated before rendering.
type ImageEvidence struct {
	Name   string
	Digest string
}

// GateState is the immutable Gate decision needed by publication. The full
// signed report remains in object storage; only its digest crosses here.
type GateState struct {
	Mode         v1alpha1.GateMode
	Verdict      string
	ReportDigest string
}

// FileChange is a verified, non-binary changed-file manifest entry. A real
// GitHub client converts this manifest to blobs, a tree, and a commit without
// executing repository code. Delete entries must have nil Content.
type FileChange struct {
	Path    string
	Mode    string
	Delete  bool
	Content []byte
}

// Patch is the capture result consumed by the publisher. Digest is the
// sha256 digest of the exact captured patch.diff bytes. Files is an
// independently reconstructed Git-data manifest. Both representations are
// bound into the publication request.
type Patch struct {
	Digest         string
	Raw            []byte
	ManifestDigest string
	Files          []FileChange
}

// Request is a fully resolved, credential-free publication contract. The
// operator constructs it from AgentRun status plus the immutable resolved
// snapshot and Gate report. No token or Secret data belongs in this value.
type Request struct {
	RunUID  string
	RunName string
	Repo    githubapp.Repository

	BaseRef string
	BaseSHA string
	Patch   Patch

	SpecDigest string
	Gate       GateState

	RuntimeImageDigest  string
	VerifierImageDigest string
	HelperImageDigests  []ImageEvidence
	SkillDigests        []string

	PublishMode v1alpha1.PublishMode
	Title       string
	Labels      []string
}

// Result contains only public publication metadata. It is safe to project to
// AgentRun status after validation. A StateUnknown result must never be
// retried automatically.
type Result struct {
	State         ResultState
	EffectKey     string
	RequestDigest string
	BranchName    string
	CommitSHA     string
	// PullRequestNumber is the bounded GitHub identity returned by the patch
	// publication. It is persisted with the ledger result so OutputBoth can
	// target exactly this PR after a replay, without trusting a user-supplied
	// target or parsing an unbound URL.
	PullRequestNumber int64
	PullRequestURL    string
}

// Ledger is the small subset of effects.Ledger used by Publisher. Keeping
// this seam narrow makes the claim-before-mutation invariant testable without
// weakening the production object-store ledger.
type Ledger interface {
	Claim(context.Context, effects.Claim) (effects.ClaimDecision, error)
	Commit(context.Context, effects.Outcome) error
	ReadOutcome(context.Context, string, string) (effects.Outcome, error)
}

// Publisher sequences one durable effect. It is safe to construct once and
// use serially or concurrently; the ledger, not process-local state, provides
// idempotency.
type Publisher struct {
	client Client
	ledger Ledger
}

// New validates the two required seams. The client is the only location where
// GitHub credentials may exist; Publisher does not accept a token or minter.
func New(client Client, ledger Ledger) (*Publisher, error) {
	if client == nil || ledger == nil {
		return nil, ErrInvalidRequest
	}
	return &Publisher{client: client, ledger: ledger}, nil
}

// Publish performs one controller-owned pull-request publication. Every
// external mutation follows a successful durable effect claim. Every
// uncertain claim, mutation, or outcome write returns StateUnknown and
// ErrUnknownEffect; the caller must not retry it blindly.
func (p *Publisher) Publish(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, ErrInvalidRequest
	}
	if request.PublishMode == v1alpha1.PublishNone {
		return Result{State: StateSkipped}, nil
	}
	if request.PublishMode != v1alpha1.PublishPullRequest {
		return Result{}, ErrInvalidRequest
	}
	prepared, err := prepare(request)
	if err != nil {
		if errors.Is(err, errRejectedEnforcing) {
			return Result{State: StateRejected}, nil
		}
		return Result{}, err
	}

	decision, err := p.ledger.Claim(ctx, effects.Claim{
		EffectKey:     prepared.effectKey,
		RequestDigest: prepared.requestDigest,
		Operation:     Operation,
		RunUID:        prepared.request.RunUID,
	})
	if err != nil {
		if errors.Is(err, effects.ErrConflict) {
			return Result{State: StateFailed, EffectKey: prepared.effectKey, RequestDigest: prepared.requestDigest, BranchName: prepared.branchName}, errClaimConflict
		}
		// An error during conditional claim creation or claim inspection cannot
		// prove whether another worker owns the effect. Do not call GitHub.
		return unknownResult(prepared, errClaimAmbiguous)
	}
	if decision.Unknown || !decision.Execute {
		return p.replay(ctx, prepared)
	}

	// The plan deliberately re-reads the named base after claiming and before
	// branch creation. The claim prevents a second worker; this read protects
	// the commit from a moving base ref.
	ref, err := p.client.GetRef(ctx, prepared.request.Repo, fullHeadRef(prepared.request.BaseRef))
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failed(ctx, prepared, "base-ref-unavailable", ErrBaseMismatch)
		}
		return p.unknown(ctx, prepared, errGitHubUnavailable)
	}
	if ref.Name != fullHeadRef(prepared.request.BaseRef) {
		return p.unknown(ctx, prepared, errGitHubResponse)
	}
	if ref.SHA != prepared.request.BaseSHA {
		return p.failed(ctx, prepared, "base-sha-mismatch", ErrBaseMismatch)
	}

	branch, err := p.client.EnsureBranch(ctx, prepared.request.Repo, BranchRequest{
		Name: prepared.branchName, BaseSHA: prepared.request.BaseSHA,
	})
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failed(ctx, prepared, "branch-conflict", errDeterministicCommit)
		}
		return p.unknown(ctx, prepared, errGitHubUnavailable)
	}
	if branch.Name != prepared.branchName || branch.SHA != prepared.request.BaseSHA {
		return p.unknown(ctx, prepared, errGitHubResponse)
	}

	commit, err := p.client.EnsureCommit(ctx, prepared.request.Repo, CommitRequest{
		BranchName:     prepared.branchName,
		BaseSHA:        prepared.request.BaseSHA,
		Patch:          clonePatch(prepared.patch),
		PatchDigest:    prepared.patch.Digest,
		ManifestDigest: prepared.patch.ManifestDigest,
		Message:        prepared.commitMessage,
		Author:         agwBotAuthor,
		EffectKey:      prepared.effectKey,
	})
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failed(ctx, prepared, "commit-conflict", errDeterministicCommit)
		}
		return p.unknown(ctx, prepared, errGitHubUnavailable)
	}
	if !validCommitResult(commit, prepared) {
		return p.unknown(ctx, prepared, errGitHubResponse)
	}

	pr, err := p.client.EnsurePullRequest(ctx, prepared.request.Repo, PullRequestRequest{
		BaseRef:    prepared.request.BaseRef,
		HeadBranch: prepared.branchName,
		Title:      prepared.title,
		Body:       prepared.body,
		EffectKey:  prepared.effectKey,
	})
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failed(ctx, prepared, "pull-request-conflict", errDeterministicCommit)
		}
		return p.unknown(ctx, prepared, errGitHubUnavailable)
	}
	if !validPullRequestResult(pr, prepared) {
		return p.unknown(ctx, prepared, errGitHubResponse)
	}

	if err := p.client.EnsureLabels(ctx, prepared.request.Repo, LabelRequest{
		Number: pr.Number,
		Labels: append([]string(nil), prepared.labels...),
	}); err != nil {
		// Labels are a post-PR mutation. Even a typed conflict is treated as
		// unknown because a multi-label request may have applied partially.
		return p.unknown(ctx, prepared, errGitHubUnavailable)
	}

	result := Result{
		State:             StateSucceeded,
		EffectKey:         prepared.effectKey,
		RequestDigest:     prepared.requestDigest,
		BranchName:        prepared.branchName,
		CommitSHA:         commit.SHA,
		PullRequestNumber: pr.Number,
		PullRequestURL:    pr.URL,
	}
	if err := p.commitOutcome(ctx, prepared, effects.Outcome{
		EffectKey:     prepared.effectKey,
		RequestDigest: prepared.requestDigest,
		State:         effects.OutcomeSucceeded,
		ResultDigest:  resultDigest(result),
		ResultRef:     encodeResult(result),
	}); err != nil {
		return unknownResult(prepared, errLedgerAmbiguous)
	}
	return result, nil
}

type preparedRequest struct {
	request       Request
	patch         Patch
	effectKey     string
	requestDigest string
	branchName    string
	title         string
	commitMessage string
	body          string
	labels        []string
}

func prepare(request Request) (preparedRequest, error) {
	if err := validateRequest(request); err != nil {
		return preparedRequest{}, err
	}
	patch := clonePatch(request.Patch)
	if len(patch.Raw) == 0 || len(patch.Raw) > MaxPatchBytes || DigestForPatchBytes(patch.Raw) != patch.Digest {
		return preparedRequest{}, ErrInvalidRequest
	}
	manifestDigest, err := DigestForPatch(patch.Files)
	if err != nil || manifestDigest != patch.ManifestDigest {
		return preparedRequest{}, ErrInvalidRequest
	}
	effectKey, err := effects.EffectKey(request.RunUID, request.BaseSHA, patch.Digest, Operation)
	if err != nil {
		return preparedRequest{}, ErrInvalidRequest
	}
	branchName, err := BranchName(request.RunName, request.RunUID)
	if err != nil {
		return preparedRequest{}, err
	}
	title := request.Title
	if title == "" {
		title = defaultTitlePrefix + request.RunName
	}
	commitMessage := defaultCommitPrefix + request.RunName + " (" + shortUID(request.RunUID) + ")"
	if len(commitMessage) > MaxCommitMessageBytes {
		return preparedRequest{}, ErrInvalidRequest
	}
	labels := desiredLabels(request.Gate, request.Labels)
	body, err := RenderPRBody(request, effectKey, branchName)
	if err != nil {
		return preparedRequest{}, err
	}
	requestDigest, err := canonical.ResolvedSpecDigest(operationDigestInput{
		Version:             1,
		RunUID:              request.RunUID,
		RunName:             request.RunName,
		Repo:                request.Repo.FullName(),
		BaseRef:             request.BaseRef,
		BaseSHA:             request.BaseSHA,
		PatchDigest:         patch.Digest,
		PatchManifestDigest: patch.ManifestDigest,
		SpecDigest:          request.SpecDigest,
		Gate:                request.Gate,
		RuntimeImageDigest:  request.RuntimeImageDigest,
		VerifierImageDigest: request.VerifierImageDigest,
		HelperImageDigests:  request.HelperImageDigests,
		SkillDigests:        request.SkillDigests,
		BranchName:          branchName,
		Title:               title,
		Labels:              labels,
		BodyDigest:          digestString(body),
	})
	if err != nil {
		return preparedRequest{}, ErrInvalidRequest
	}
	return preparedRequest{
		request:       request,
		patch:         patch,
		effectKey:     effectKey,
		requestDigest: requestDigest,
		branchName:    branchName,
		title:         title,
		commitMessage: commitMessage,
		body:          body,
		labels:        labels,
	}, nil
}

type operationDigestInput struct {
	Version             int             `json:"version"`
	RunUID              string          `json:"runUID"`
	RunName             string          `json:"runName"`
	Repo                string          `json:"repo"`
	BaseRef             string          `json:"baseRef"`
	BaseSHA             string          `json:"baseSHA"`
	PatchDigest         string          `json:"patchDigest"`
	PatchManifestDigest string          `json:"patchManifestDigest"`
	SpecDigest          string          `json:"specDigest"`
	Gate                GateState       `json:"gate"`
	RuntimeImageDigest  string          `json:"runtimeImageDigest"`
	VerifierImageDigest string          `json:"verifierImageDigest"`
	HelperImageDigests  []ImageEvidence `json:"helperImageDigests"`
	SkillDigests        []string        `json:"skillDigests"`
	BranchName          string          `json:"branchName"`
	Title               string          `json:"title"`
	Labels              []string        `json:"labels"`
	BodyDigest          string          `json:"bodyDigest"`
}

func (p *Publisher) replay(ctx context.Context, prepared preparedRequest) (Result, error) {
	outcome, err := p.ledger.ReadOutcome(ctx, prepared.effectKey, prepared.requestDigest)
	if err != nil {
		return unknownResult(prepared, errClaimAmbiguous)
	}
	switch outcome.State {
	case effects.OutcomeSucceeded:
		result, ok := decodeResult(outcome.ResultRef)
		if !ok || resultDigest(result) != outcome.ResultDigest || result.EffectKey != prepared.effectKey || result.RequestDigest != prepared.requestDigest || result.BranchName != prepared.branchName || !validPullRequestIdentity(result.PullRequestNumber, result.PullRequestURL, prepared.request.Repo) {
			return unknownResult(prepared, errLedgerAmbiguous)
		}
		return result, nil
	case effects.OutcomeFailed:
		return Result{State: StateFailed, EffectKey: prepared.effectKey, RequestDigest: prepared.requestDigest, BranchName: prepared.branchName}, ErrPreviouslyFailed
	case effects.OutcomeUnknown:
		return unknownResult(prepared, ErrUnknownEffect)
	default:
		return unknownResult(prepared, errLedgerAmbiguous)
	}
}

func (p *Publisher) failed(ctx context.Context, prepared preparedRequest, code string, publicErr error) (Result, error) {
	result := Result{State: StateFailed, EffectKey: prepared.effectKey, RequestDigest: prepared.requestDigest, BranchName: prepared.branchName}
	if err := p.commitOutcome(ctx, prepared, effects.Outcome{
		EffectKey:     prepared.effectKey,
		RequestDigest: prepared.requestDigest,
		State:         effects.OutcomeFailed,
		ResultDigest:  digestString(failureResultPrefix + code),
		ResultRef:     failureResultPrefix + code,
	}); err != nil {
		return unknownResult(prepared, errLedgerAmbiguous)
	}
	return result, publicErr
}

func (p *Publisher) unknown(ctx context.Context, prepared preparedRequest, publicErr error) (Result, error) {
	// This best-effort write is itself never retried. If it is uncertain, the
	// returned result remains UnknownEffect and reconciliation is manual.
	_ = p.commitOutcome(ctx, prepared, effects.Outcome{
		EffectKey:     prepared.effectKey,
		RequestDigest: prepared.requestDigest,
		State:         effects.OutcomeUnknown,
		ResultRef:     "unknown",
	})
	return unknownResult(prepared, publicErr)
}

func (p *Publisher) commitOutcome(ctx context.Context, prepared preparedRequest, outcome effects.Outcome) error {
	if err := p.ledger.Commit(ctx, outcome); err == nil {
		return nil
	}
	// A conditional outcome write can have succeeded before the transport
	// failed. Read-only reconciliation may prove the exact outcome; otherwise
	// the caller remains in UnknownEffect and must not issue another write.
	recorded, err := p.ledger.ReadOutcome(ctx, prepared.effectKey, prepared.requestDigest)
	if err != nil || recorded.State != outcome.State || recorded.ResultDigest != outcome.ResultDigest || recorded.ResultRef != outcome.ResultRef {
		return errLedgerAmbiguous
	}
	return nil
}

func unknownResult(prepared preparedRequest, publicErr error) (Result, error) {
	if publicErr == nil {
		publicErr = ErrUnknownEffect
	}
	return Result{State: StateUnknown, EffectKey: prepared.effectKey, RequestDigest: prepared.requestDigest, BranchName: prepared.branchName}, wrapUnknown(publicErr)
}

func wrapUnknown(_ error) error {
	// Do not wrap or expose the client/ledger error. Implementations frequently
	// include response bodies, URLs, or authorization headers in their errors.
	return ErrUnknownEffect
}

func deterministicGitHubError(err error) bool {
	return errors.Is(err, ErrGitHubConflict) || errors.Is(err, ErrGitHubNotFound) || errors.Is(err, ErrInvalidRequest)
}

func validCommitResult(result CommitResult, prepared preparedRequest) bool {
	return validSHA(result.SHA) && result.BranchName == prepared.branchName && result.BaseSHA == prepared.request.BaseSHA && result.PatchDigest == prepared.patch.Digest && result.ManifestDigest == prepared.patch.ManifestDigest && result.Author == agwBotAuthor
}

func validPullRequestResult(result PullRequestResult, prepared preparedRequest) bool {
	if result.Number <= 0 || result.BaseRef != prepared.request.BaseRef || result.HeadBranch != prepared.branchName || result.EffectKey != prepared.effectKey {
		return false
	}
	wantPath := "/" + prepared.request.Repo.Owner + "/" + prepared.request.Repo.Name + "/pull/" + fmt.Sprint(result.Number)
	return validPullRequestURLForRepo(result.URL, prepared.request.Repo) && parsedPath(result.URL) == wantPath
}

func validPullRequestIdentity(number int64, value string, repo githubapp.Repository) bool {
	if number <= 0 || number > MaxPullRequestNumber {
		return false
	}
	return validPullRequestURLForRepo(value, repo) && parsedPath(value) == "/"+repo.Owner+"/"+repo.Name+"/pull/"+fmt.Sprint(number)
}

func validPullRequestURLForRepo(value string, repo githubapp.Repository) bool {
	if !safeURL(value) {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	prefix := "/" + repo.Owner + "/" + repo.Name + "/pull/"
	if !strings.HasPrefix(parsed.Path, prefix) {
		return false
	}
	number := strings.TrimPrefix(parsed.Path, prefix)
	if number == "" {
		return false
	}
	for _, digit := range number {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return number != "0"
}

func parsedPath(value string) string {
	parsed, _ := url.Parse(value)
	return parsed.Path
}

// BranchName returns the deterministic branch used by the publish protocol.
// The UID is hashed before it enters the ref so the short suffix is bounded,
// safe, and independent of UUID formatting.
func BranchName(runName, runUID string) (string, error) {
	if !validRunName(runName) || !validRunUID(runUID) {
		return "", ErrInvalidRequest
	}
	name := runName
	maxName := branchNameMaxBytes - len(branchNamePrefix) - 1 - shortUIDHexBytes
	if len(name) > maxName {
		name = strings.TrimRight(name[:maxName], "-")
	}
	if name == "" {
		return "", ErrInvalidRequest
	}
	return branchNamePrefix + name + "-" + shortUID(runUID), nil
}

// DigestForPatch computes the content digest of a canonical changed-file
// manifest. It complements, but never replaces, the patch.diff digest.
func DigestForPatch(files []FileChange) (string, error) {
	normalized, err := normalizePatchFiles(files)
	if err != nil {
		return "", err
	}
	return canonical.ResolvedSpecDigest(patchDigestInput{Version: 1, Files: normalized})
}

// MarshalPatchManifest returns the canonical, bounded changed-file manifest
// consumed by the GitHub Git-data adapter. It is deliberately separate from
// patch.diff: one digest binds the exact diff verified by the Gate and the
// other binds the exact final bytes and modes the controller may publish.
func MarshalPatchManifest(files []FileChange) ([]byte, string, error) {
	normalized, err := normalizePatchFiles(files)
	if err != nil {
		return nil, "", err
	}
	document := patchDigestInput{Version: 1, Files: normalized}
	encoded, err := canonical.CanonicalizeResolvedSpec(document)
	if err != nil || len(encoded) == 0 || len(encoded) > MaxPatchManifestBytes {
		return nil, "", ErrInvalidRequest
	}
	digest, err := canonical.ResolvedSpecDigest(document)
	if err != nil {
		return nil, "", ErrInvalidRequest
	}
	return encoded, digest, nil
}

// DecodePatchManifest accepts only the exact canonical representation emitted
// by MarshalPatchManifest. Unknown fields, duplicate or unsafe paths,
// unsupported modes, oversized contents, digest mismatches, and alternate
// JSON encodings all fail closed.
func DecodePatchManifest(raw []byte, expectedDigest string) ([]FileChange, error) {
	if len(raw) == 0 || len(raw) > MaxPatchManifestBytes || !canonical.ValidDigest(expectedDigest) {
		return nil, ErrInvalidRequest
	}
	var document patchDigestInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, ErrInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || document.Version != 1 {
		return nil, ErrInvalidRequest
	}
	files := make([]FileChange, len(document.Files))
	for index, file := range document.Files {
		files[index] = FileChange{
			Path: file.Path, Mode: file.Mode, Delete: file.Delete,
			Content: cloneContent(file.Content),
		}
		if file.Delete {
			files[index].Content = nil
		}
	}
	canonicalBytes, digest, err := MarshalPatchManifest(files)
	if err != nil || digest != expectedDigest || !bytes.Equal(canonicalBytes, raw) {
		return nil, ErrInvalidRequest
	}
	return files, nil
}

// DigestForPatchBytes computes the artifact digest used by AgentRun.status,
// Gate reports, the publication effect key, and the PR evidence table.
func DigestForPatchBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type patchDigestInput struct {
	Version int               `json:"version"`
	Files   []patchDigestFile `json:"files"`
}

type patchDigestFile struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Delete  bool   `json:"delete"`
	Content []byte `json:"content"`
}

func normalizePatchFiles(files []FileChange) ([]patchDigestFile, error) {
	if len(files) == 0 || len(files) > MaxPatchFiles {
		return nil, ErrInvalidRequest
	}
	copyFiles := append([]FileChange(nil), files...)
	sort.Slice(copyFiles, func(i, j int) bool { return copyFiles[i].Path < copyFiles[j].Path })
	normalized := make([]patchDigestFile, 0, len(copyFiles))
	var total int
	for index, file := range copyFiles {
		if !validPath(file.Path) || len(file.Path) > MaxPatchPathBytes || !validFileMode(file.Mode) {
			return nil, ErrInvalidRequest
		}
		if index > 0 && copyFiles[index-1].Path == file.Path {
			return nil, ErrInvalidRequest
		}
		if file.Delete && file.Content != nil {
			return nil, ErrInvalidRequest
		}
		if !file.Delete && file.Content == nil {
			return nil, ErrInvalidRequest
		}
		total += len(file.Content)
		if total > MaxPatchBytes {
			return nil, ErrInvalidRequest
		}
		normalized = append(normalized, patchDigestFile{Path: file.Path, Mode: file.Mode, Delete: file.Delete, Content: cloneContent(file.Content)})
	}
	return normalized, nil
}

// cloneContent preserves the semantic distinction between nil and an empty
// byte slice. In a patch manifest, nil means a deleted file while a non-nil
// empty slice means a zero-byte file. A plain append to a nil slice loses that
// distinction when the source has length zero.
func cloneContent(content []byte) []byte {
	if content == nil {
		return nil
	}
	clone := make([]byte, len(content))
	copy(clone, content)
	return clone
}

func clonePatch(patch Patch) Patch {
	clone := Patch{
		Digest: patch.Digest, Raw: append([]byte(nil), patch.Raw...),
		ManifestDigest: patch.ManifestDigest, Files: make([]FileChange, len(patch.Files)),
	}
	for index, file := range patch.Files {
		clone.Files[index] = FileChange{Path: file.Path, Mode: file.Mode, Delete: file.Delete, Content: cloneContent(file.Content)}
	}
	sort.Slice(clone.Files, func(i, j int) bool { return clone.Files[i].Path < clone.Files[j].Path })
	return clone
}

// RenderPRBody renders the bounded, credential-free evidence attached to the
// PR. It is deterministic for one prepared request and intentionally omits
// task text, patch contents, URLs from artifacts, and all Secret values.
func RenderPRBody(request Request, effectKey, branchName string) (string, error) {
	if err := validateRequest(request); err != nil || !canonical.ValidDigest(effectKey) || !strings.HasPrefix(branchName, branchNamePrefix) {
		return "", ErrInvalidRequest
	}
	helpers := append([]ImageEvidence(nil), request.HelperImageDigests...)
	sort.Slice(helpers, func(i, j int) bool { return helpers[i].Name < helpers[j].Name })
	skills := append([]string(nil), request.SkillDigests...)
	sort.Strings(skills)
	labels := desiredLabels(request.Gate, request.Labels)
	var b strings.Builder
	b.WriteString("## Agents Gateway verification\n\n")
	b.WriteString("This pull request was created by the Agents Gateway controller. It was not merged automatically.\n\n")
	b.WriteString("| Evidence | Value |\n| --- | --- |\n")
	fmt.Fprintf(&b, "| Repository | `%s` |\n", request.Repo.FullName())
	fmt.Fprintf(&b, "| Base ref | `%s` |\n", request.BaseRef)
	fmt.Fprintf(&b, "| Base SHA | `%s` |\n", request.BaseSHA)
	fmt.Fprintf(&b, "| Branch | `%s` |\n", branchName)
	fmt.Fprintf(&b, "| Gate verdict | `%s` (`%s`) |\n", request.Gate.Verdict, request.Gate.Mode)
	fmt.Fprintf(&b, "| Verification report digest | `%s` |\n", request.Gate.ReportDigest)
	fmt.Fprintf(&b, "| Resolved spec digest | `%s` |\n", request.SpecDigest)
	fmt.Fprintf(&b, "| Patch digest | `%s` |\n", request.Patch.Digest)
	fmt.Fprintf(&b, "| Effect key | `%s` |\n", effectKey)
	fmt.Fprintf(&b, "| Runtime image | `%s` |\n", request.RuntimeImageDigest)
	fmt.Fprintf(&b, "| Verifier image | `%s` |\n", request.VerifierImageDigest)
	b.WriteString("\n### Helper image digests\n\n")
	for _, image := range helpers {
		fmt.Fprintf(&b, "- `%s`: `%s`\n", image.Name, image.Digest)
	}
	if len(helpers) == 0 {
		b.WriteString("- none\n")
	}
	b.WriteString("\n### Skill digests\n\n")
	for _, skill := range skills {
		fmt.Fprintf(&b, "- `%s`\n", skill)
	}
	if len(skills) == 0 {
		b.WriteString("- none\n")
	}
	b.WriteString("\n### Controller labels\n\n")
	for _, label := range labels {
		fmt.Fprintf(&b, "- `%s`\n", label)
	}
	body := b.String()
	if len(body) > MaxBodyBytes {
		return "", ErrBodyTooLarge
	}
	return body, nil
}

func validateRequest(request Request) error {
	if !validRunUID(request.RunUID) || !validRunName(request.RunName) || request.Repo.Validate() != nil {
		return ErrInvalidRequest
	}
	if !validBaseRef(request.BaseRef) || !validSHA(request.BaseSHA) || !canonical.ValidDigest(request.SpecDigest) || !canonical.ValidDigest(request.Gate.ReportDigest) {
		return ErrInvalidRequest
	}
	if request.Gate.Mode != v1alpha1.GateShadow && request.Gate.Mode != v1alpha1.GateEnforcing {
		return ErrInvalidRequest
	}
	if request.Gate.Verdict != "Accepted" && request.Gate.Verdict != "Rejected" {
		return ErrInvalidRequest
	}
	if request.Gate.Verdict == "Rejected" && request.Gate.Mode == v1alpha1.GateEnforcing {
		// The caller receives StateRejected before it can reach the ledger.
		return errRejectedEnforcing
	}
	if len(request.Patch.Raw) == 0 || len(request.Patch.Raw) > MaxPatchBytes || !canonical.ValidDigest(request.Patch.Digest) || !canonical.ValidDigest(request.Patch.ManifestDigest) {
		return ErrInvalidRequest
	}
	if _, err := normalizePatchFiles(request.Patch.Files); err != nil {
		return ErrInvalidRequest
	}
	if !validImageDigest(request.RuntimeImageDigest) || !validImageDigest(request.VerifierImageDigest) {
		return ErrInvalidRequest
	}
	if err := validateImages(request.HelperImageDigests); err != nil {
		return ErrInvalidRequest
	}
	if err := validateSkills(request.SkillDigests); err != nil {
		return ErrInvalidRequest
	}
	if len(request.Title) > MaxTitleBytes || strings.ContainsAny(request.Title, "\x00\r\n") || !utf8.ValidString(request.Title) {
		return ErrInvalidRequest
	}
	if err := validateLabels(request.Labels); err != nil {
		return ErrInvalidRequest
	}
	return nil
}

var errRejectedEnforcing = errors.New("publish: rejected enforcing Gate")

func desiredLabels(gate GateState, custom []string) []string {
	labels := make([]string, 0, len(custom)+2)
	if gate.Verdict == "Accepted" {
		labels = append(labels, "agw/accepted")
	} else {
		labels = append(labels, "agw/rejected")
	}
	if gate.Mode == v1alpha1.GateShadow {
		labels = append(labels, "agw/shadow")
	}
	labels = append(labels, custom...)
	sort.Strings(labels)
	result := labels[:0]
	for _, label := range labels {
		if len(result) == 0 || result[len(result)-1] != label {
			result = append(result, label)
		}
	}
	return result
}

func validateImages(images []ImageEvidence) error {
	if len(images) > MaxImages {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(images))
	for _, image := range images {
		if !validImageRole(image.Name) || !validImageDigest(image.Digest) {
			return ErrInvalidRequest
		}
		if _, ok := seen[image.Name]; ok {
			return ErrInvalidRequest
		}
		seen[image.Name] = struct{}{}
	}
	return nil
}

func validateSkills(skills []string) error {
	if len(skills) > MaxSkills {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		if !canonical.ValidDigest(skill) {
			return ErrInvalidRequest
		}
		if _, ok := seen[skill]; ok {
			return ErrInvalidRequest
		}
		seen[skill] = struct{}{}
	}
	return nil
}

func validateLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return ErrInvalidRequest
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > MaxLabelBytes || !validRefPart(label) || label == "agw/accepted" || label == "agw/rejected" || label == "agw/shadow" {
			return ErrInvalidRequest
		}
	}
	return nil
}

func validRunUID(value string) bool {
	return len(value) > 0 && len(value) <= MaxRunUIDBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n\t ")
}

func validRunName(value string) bool {
	if len(value) == 0 || len(value) > MaxRunNameBytes || value[0] == '-' || value[len(value)-1] == '-' {
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
	if len(value) == 0 || len(value) > MaxBaseRefBytes || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "..") || strings.ContainsAny(value, "\x00\r\n\t ") {
		return false
	}
	return validRefPart(value)
}

func validRefPart(value string) bool {
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func validPath(value string) bool {
	return value != "" && utf8.ValidString(value) && !strings.HasPrefix(value, "/") && !strings.Contains(value, "..") && !strings.ContainsAny(value, "\x00\r\n")
}

func validFileMode(value string) bool { return value == "100644" || value == "100755" }

func validSHA(value string) bool {
	if len(value) < 40 || len(value) > MaxBaseSHABytes {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func validImageDigest(value string) bool {
	if len(value) == 0 || len(value) > MaxImageRefBytes || strings.ContainsAny(value, "\x00\r\n\t `") {
		return false
	}
	marker := strings.LastIndex(value, "@sha256:")
	return marker > 0 && canonical.ValidDigest(value[marker+1:]) && validRefPart(strings.ReplaceAll(value[:marker], ":", ""))
}

func validImageRole(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for index, r := range value {
		if index == 0 && !(r >= 'a' && r <= 'z') {
			return false
		}
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func safeURL(value string) bool {
	if len(value) == 0 || len(value) > MaxPullRequestURLBytes || strings.ContainsAny(value, "\x00\r\n\t @?#") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Path != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func fullHeadRef(baseRef string) string { return "refs/heads/" + baseRef }

func shortUID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:shortUIDHexBytes]
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

type resultRecord struct {
	Version           int         `json:"version"`
	State             ResultState `json:"state"`
	EffectKey         string      `json:"effectKey"`
	RequestDigest     string      `json:"requestDigest"`
	BranchName        string      `json:"branchName"`
	CommitSHA         string      `json:"commitSHA"`
	PullRequestNumber int64       `json:"pullRequestNumber"`
	PullRequestURL    string      `json:"pullRequestURL"`
}

func resultDigest(result Result) string {
	digest, err := canonical.ResolvedSpecDigest(resultRecord{Version: MaxEffectResultVersion, State: result.State, EffectKey: result.EffectKey, RequestDigest: result.RequestDigest, BranchName: result.BranchName, CommitSHA: result.CommitSHA, PullRequestNumber: result.PullRequestNumber, PullRequestURL: result.PullRequestURL})
	if err != nil {
		return ""
	}
	return digest
}

func encodeResult(result Result) string {
	encoded, err := canonical.CanonicalizeResolvedSpec(resultRecord{Version: MaxEffectResultVersion, State: result.State, EffectKey: result.EffectKey, RequestDigest: result.RequestDigest, BranchName: result.BranchName, CommitSHA: result.CommitSHA, PullRequestNumber: result.PullRequestNumber, PullRequestURL: result.PullRequestURL})
	if err != nil {
		return ""
	}
	return effectResultPrefix + base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeResult(value string) (Result, bool) {
	if !strings.HasPrefix(value, effectResultPrefix) || len(value) > MaxResultRefBytes {
		return Result{}, false
	}
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, effectResultPrefix))
	if err != nil || len(encoded) == 0 {
		return Result{}, false
	}
	var record resultRecord
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return Result{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Result{}, false
	}
	canonicalEncoded, err := canonical.CanonicalizeResolvedSpec(record)
	if err != nil || string(canonicalEncoded) != string(encoded) || record.Version != MaxEffectResultVersion || record.State != StateSucceeded || !canonical.ValidDigest(record.EffectKey) || !canonical.ValidDigest(record.RequestDigest) || !validSHA(record.CommitSHA) || !strings.HasPrefix(record.BranchName, branchNamePrefix) || record.PullRequestNumber <= 0 || record.PullRequestNumber > MaxPullRequestNumber || !safeURL(record.PullRequestURL) {
		return Result{}, false
	}
	return Result{State: record.State, EffectKey: record.EffectKey, RequestDigest: record.RequestDigest, BranchName: record.BranchName, CommitSHA: record.CommitSHA, PullRequestNumber: record.PullRequestNumber, PullRequestURL: record.PullRequestURL}, true
}
