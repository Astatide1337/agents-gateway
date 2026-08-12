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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// FindingsOperation names one individual blocking review effect in the
	// durable ledger. It is deliberately separate from publish/pr.
	FindingsOperation = "publish/finding"
	// AdvisorySectionOperation names the one PR-body mutation for one findings
	// artifact. It is not a review effect and never creates review state.
	AdvisorySectionOperation = "publish/advisory-section"

	MaxFindingReviewBodyBytes = 8 << 10
	MaxAdvisorySectionBytes   = 16 << 10
	MaxPullRequestBodyBytes   = 64 << 10
	MaxReviewLine             = 1_000_000_000
	MaxPullRequestNumber      = int64(2_147_483_647)
	MaxFindingOutcomes        = findingcorroboration.MaxFindings

	findingMarkerPrefix          = "<!-- agw-finding-effect:"
	advisoryMarkerStart          = "<!-- agw-advisory-findings:v1 -->"
	advisoryMarkerEnd            = "<!-- /agw-advisory-findings:v1 -->"
	findingResultPrefix          = "agw-finding-result-v1:"
	advisoryResultPrefix         = "agw-advisory-result-v1:"
	findingsResultVersion        = 1
	findingsRequestDigestVersion = 1
)

var (
	ErrFindingsUnavailable = errors.New("publish: findings review client is unavailable")
	ErrFindingsFailed      = errors.New("publish: findings publication failed")
	ErrFindingsConflict    = errors.New("publish: findings publication conflicts with requested state")
	ErrFindingsResponse    = errors.New("publish: findings proof is invalid")
)

// ReviewClient is the only authenticated boundary used for findings output.
// Implementations must use the controller-owned GitHub App credential and must
// prove every requested state before returning success. The interface is
// intentionally separate from Client so the patch protocol remains unchanged.
type ReviewClient interface {
	GetPullRequest(context.Context, githubapp.Repository, int64) (PullRequestTarget, error)
	EnsureBlockingFindingReview(context.Context, githubapp.Repository, BlockingFindingRequest) (BlockingFindingResult, error)
	EnsureAdvisorySection(context.Context, githubapp.Repository, AdvisorySectionRequest) (AdvisorySectionResult, error)
}

// PullRequestTarget is the read-only identity proof used before any findings
// mutation. Body is bounded by the adapter and is only used to merge the
// AGW-owned advisory section.
type PullRequestTarget struct {
	Number     int64
	URL        string
	BaseRef    string
	HeadBranch string
	HeadSHA    string
	Body       string
}

// BlockingFindingRequest is one exact line-level review mutation. Body must
// contain MarkerForFinding(EffectKey). No credential or model transcript is
// represented by this request.
type BlockingFindingRequest struct {
	PullRequestNumber int64
	CommitSHA         string
	EffectKey         string
	FindingID         string
	Path              string
	StartLine         int
	EndLine           int
	Body              string
}

// BlockingFindingResult is the proof returned after GitHub has read back one
// requested review state.
type BlockingFindingResult struct {
	PullRequestNumber int64
	ReviewID          int64
	CommentID         int64
	CommitSHA         string
	EffectKey         string
	FindingID         string
}

// AdvisorySectionRequest describes a complete AGW-owned section replacement.
// The adapter must preserve all bytes outside the marker pair and must prove
// the resulting body after a mutation.
type AdvisorySectionRequest struct {
	PullRequestNumber int64
	ExpectedHeadSHA   string
	EffectKey         string
	SectionDigest     string
	Section           string
}

// AdvisorySectionResult proves the exact target and section digest after the
// idempotent body merge.
type AdvisorySectionResult struct {
	PullRequestNumber int64
	URL               string
	HeadSHA           string
	EffectKey         string
	SectionDigest     string
}

// FindingsRequest is the credential-free, immutable findings publication
// contract. The controller must load and authenticate the canonical findings
// artifact before constructing this value. Result is still revalidated here
// before it crosses the GitHub seam.
type FindingsRequest struct {
	RunUID            string
	RunName           string
	Repo              githubapp.Repository
	BaseRef           string
	BaseSHA           string
	SpecDigest        string
	PatchDigest       string
	GateReportDigest  string
	ArtifactDigest    string
	TargetPullRequest int64
	Result            findingcorroboration.CorroborationResult
}

// FindingOutcome is bounded publication metadata for one finding. Advisory
// findings share AdvisoryEffectKey and never receive a review ID.
type FindingOutcome struct {
	FindingID string
	Route     findingcorroboration.Route
	State     ResultState
	EffectKey string
	ReviewID  int64
	CommentID int64
}

// FindingsResult is safe to project into a controller status/artifact. A
// StateUnknown result is terminal and must not be retried automatically.
type FindingsResult struct {
	State             ResultState
	RequestDigest     string
	PullRequestNumber int64
	PullRequestURL    string
	AdvisoryEffectKey string
	AdvisoryState     ResultState
	Outcomes          []FindingOutcome
}

type preparedFindings struct {
	request         FindingsRequest
	result          findingcorroboration.CorroborationResult
	resultBytes     []byte
	resultDigest    string
	requestDigest   string
	advisorySection string
	sectionDigest   string
	target          PullRequestTarget
	decisions       []findingcorroboration.FindingDecision
}

type findingsRequestDigestInput struct {
	Version          int    `json:"version"`
	RunUID           string `json:"runUID"`
	RunName          string `json:"runName"`
	Repo             string `json:"repo"`
	BaseRef          string `json:"baseRef"`
	BaseSHA          string `json:"baseSHA"`
	SpecDigest       string `json:"specDigest"`
	PatchDigest      string `json:"patchDigest"`
	GateReportDigest string `json:"gateReportDigest"`
	ArtifactDigest   string `json:"artifactDigest"`
	PullRequest      int64  `json:"pullRequest"`
	ResultDigest     string `json:"resultDigest"`
}

type findingResultRecord struct {
	Version           int    `json:"version"`
	State             string `json:"state"`
	EffectKey         string `json:"effectKey"`
	RequestDigest     string `json:"requestDigest"`
	FindingID         string `json:"findingID"`
	Route             string `json:"route"`
	PullRequestNumber int64  `json:"pullRequestNumber"`
	CommitSHA         string `json:"commitSHA"`
	ReviewID          int64  `json:"reviewID,omitempty"`
	CommentID         int64  `json:"commentID,omitempty"`
}

type advisoryResultRecord struct {
	Version           int    `json:"version"`
	State             string `json:"state"`
	EffectKey         string `json:"effectKey"`
	RequestDigest     string `json:"requestDigest"`
	PullRequestNumber int64  `json:"pullRequestNumber"`
	URL               string `json:"url"`
	HeadSHA           string `json:"headSHA"`
	SectionDigest     string `json:"sectionDigest"`
}

// PublishFindings publishes an independently corroborated finding artifact to
// an existing pull request. It never creates a branch, commit, or pull request.
// Each blocking finding is a separate effect-ledger transaction; advisory
// findings are merged into one clearly-labelled PR-body section.
func (p *Publisher) PublishFindings(ctx context.Context, request FindingsRequest) (FindingsResult, error) {
	if ctx == nil {
		return FindingsResult{}, ErrInvalidRequest
	}
	if p == nil || p.client == nil || p.ledger == nil {
		return FindingsResult{}, ErrInvalidRequest
	}
	reviewClient, ok := p.client.(ReviewClient)
	if !ok || reviewClient == nil {
		return FindingsResult{}, ErrFindingsUnavailable
	}
	prepared, err := prepareFindings(request)
	if err != nil {
		return FindingsResult{}, err
	}
	target, err := reviewClient.GetPullRequest(ctx, request.Repo, request.TargetPullRequest)
	if err != nil {
		return FindingsResult{}, findingsReadFailure(err)
	}
	if !validTarget(target, prepared.request) {
		return FindingsResult{}, ErrFindingsConflict
	}
	prepared.target = target

	result := FindingsResult{
		State:             StateSucceeded,
		RequestDigest:     prepared.requestDigest,
		PullRequestNumber: target.Number,
		PullRequestURL:    target.URL,
		Outcomes:          make([]FindingOutcome, 0, len(prepared.decisions)),
	}

	if prepared.advisorySection != "" {
		key, err := advisoryEffectKey(prepared.request)
		if err != nil {
			return FindingsResult{}, ErrInvalidRequest
		}
		result.AdvisoryEffectKey = key
		result.AdvisoryState = StateSucceeded
		advisory, advisoryErr := p.publishAdvisorySection(ctx, prepared, reviewClient, key)
		if advisoryErr != nil {
			result.AdvisoryState = advisory.State
			return result, advisoryErr
		}
		result.AdvisoryState = advisory.State
	}

	for _, decision := range prepared.decisions {
		outcome := FindingOutcome{FindingID: decision.Finding.ID, Route: decision.Route, State: StateSucceeded}
		if decision.Route == findingcorroboration.RouteAdvisory {
			// Advisory findings are represented only in the section. They never
			// cross the review-comment seam and never carry a review ID.
			outcome.EffectKey = result.AdvisoryEffectKey
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}

		key, err := findingEffectKey(prepared.request, decision.Finding.ID)
		if err != nil {
			return FindingsResult{}, ErrInvalidRequest
		}
		outcome.EffectKey = key
		findingResult, findingErr := p.publishBlockingFinding(ctx, prepared, reviewClient, decision, key)
		if findingErr != nil {
			outcome.State = findingResult.State
			outcome.ReviewID = findingResult.ReviewID
			outcome.CommentID = findingResult.CommentID
			result.Outcomes = append(result.Outcomes, outcome)
			result.State = findingResult.State
			return result, findingErr
		}
		outcome.ReviewID = findingResult.ReviewID
		outcome.CommentID = findingResult.CommentID
		result.Outcomes = append(result.Outcomes, outcome)
	}
	return result, nil
}

func prepareFindings(request FindingsRequest) (preparedFindings, error) {
	if !validFindingsRequest(request) {
		return preparedFindings{}, ErrInvalidRequest
	}
	resultBytes, err := findingcorroboration.CanonicalResultBytes(request.Result)
	if err != nil || len(resultBytes) == 0 || strictjson.ValidateObject(resultBytes) != nil {
		return preparedFindings{}, ErrInvalidRequest
	}
	result, err := decodeCanonicalFindingResult(resultBytes)
	if err != nil {
		return preparedFindings{}, ErrInvalidRequest
	}
	resultDigest := DigestForPatchBytes(resultBytes)
	digestInput := findingsRequestDigestInput{
		Version: findingsRequestDigestVersion, RunUID: request.RunUID, RunName: request.RunName,
		Repo: request.Repo.FullName(), BaseRef: request.BaseRef, BaseSHA: request.BaseSHA,
		SpecDigest: request.SpecDigest, PatchDigest: request.PatchDigest,
		GateReportDigest: request.GateReportDigest, ArtifactDigest: request.ArtifactDigest,
		PullRequest: request.TargetPullRequest, ResultDigest: resultDigest,
	}
	requestDigest, err := canonical.ResolvedSpecDigest(digestInput)
	if err != nil {
		return preparedFindings{}, ErrInvalidRequest
	}
	decisions := append([]findingcorroboration.FindingDecision(nil), result.Findings...)
	sort.Slice(decisions, func(left, right int) bool { return decisions[left].Finding.ID < decisions[right].Finding.ID })
	section, err := RenderAdvisorySection(request, result, requestDigest)
	if err != nil {
		return preparedFindings{}, err
	}
	sectionDigest := ""
	if section != "" {
		sectionDigest = DigestForPatchBytes([]byte(section))
	}
	return preparedFindings{
		request: request, result: result, resultBytes: resultBytes, resultDigest: resultDigest,
		requestDigest: requestDigest, advisorySection: section, sectionDigest: sectionDigest,
		decisions: decisions,
	}, nil
}

func decodeCanonicalFindingResult(encoded []byte) (findingcorroboration.CorroborationResult, error) {
	var result findingcorroboration.CorroborationResult
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return findingcorroboration.CorroborationResult{}, ErrInvalidRequest
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return findingcorroboration.CorroborationResult{}, ErrInvalidRequest
	}
	canonicalBytes, err := findingcorroboration.CanonicalResultBytes(result)
	if err != nil || !bytes.Equal(canonicalBytes, encoded) {
		return findingcorroboration.CorroborationResult{}, ErrInvalidRequest
	}
	return result, nil
}

func validFindingsRequest(request FindingsRequest) bool {
	return validRunUID(request.RunUID) && validRunName(request.RunName) && request.Repo.Validate() == nil &&
		validBaseRef(request.BaseRef) && validSHA(request.BaseSHA) && canonical.ValidDigest(request.SpecDigest) &&
		canonical.ValidDigest(request.PatchDigest) && canonical.ValidDigest(request.GateReportDigest) &&
		canonical.ValidDigest(request.ArtifactDigest) && request.TargetPullRequest > 0 && request.TargetPullRequest <= MaxPullRequestNumber
}

func validTarget(target PullRequestTarget, request FindingsRequest) bool {
	return target.Number == request.TargetPullRequest && validPullRequestURLForRepo(target.URL, request.Repo) &&
		target.BaseRef == request.BaseRef && validReviewBranch(target.HeadBranch) && validSHA(target.HeadSHA) &&
		len(target.Body) <= MaxPullRequestBodyBytes
}

func validReviewBranch(value string) bool {
	if value == "" || len(value) > 255 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n ~^:?*[") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "..") {
			return false
		}
	}
	return true
}

func findingsReadFailure(err error) error {
	if errors.Is(err, ErrGitHubConflict) || errors.Is(err, ErrGitHubNotFound) || errors.Is(err, ErrInvalidRequest) {
		return ErrFindingsConflict
	}
	return ErrFindingsFailed
}

func advisoryEffectKey(request FindingsRequest) (string, error) {
	return effects.EffectKey(request.RunUID, request.BaseSHA, request.ArtifactDigest, AdvisorySectionOperation)
}

func findingEffectKey(request FindingsRequest, findingID string) (string, error) {
	if findingID == "" {
		return "", ErrInvalidRequest
	}
	identity := sha256.Sum256([]byte(findingID))
	operation := FindingsOperation + "/" + hex.EncodeToString(identity[:])
	return effects.EffectKey(request.RunUID, request.BaseSHA, request.PatchDigest, operation)
}

func (p *Publisher) publishAdvisorySection(ctx context.Context, prepared preparedFindings, client ReviewClient, key string) (FindingsResult, error) {
	requestDigest, err := advisoryRequestDigest(prepared)
	if err != nil {
		return FindingsResult{State: StateFailed, AdvisoryEffectKey: key}, ErrInvalidRequest
	}
	decision, err := p.ledger.Claim(ctx, effects.Claim{EffectKey: key, RequestDigest: requestDigest, Operation: AdvisorySectionOperation, RunUID: prepared.request.RunUID})
	if err != nil {
		if errors.Is(err, effects.ErrConflict) {
			return FindingsResult{State: StateFailed, AdvisoryEffectKey: key}, ErrFindingsConflict
		}
		return unknownFindingsResult(key), ErrUnknownEffect
	}
	if !decision.Execute {
		advisory, replayErr := p.replayAdvisory(ctx, key, requestDigest, prepared)
		if replayErr != nil {
			return advisory, replayErr
		}
		return advisory, nil
	}
	response, err := client.EnsureAdvisorySection(ctx, prepared.request.Repo, AdvisorySectionRequest{
		PullRequestNumber: prepared.request.TargetPullRequest, ExpectedHeadSHA: prepared.target.HeadSHA,
		EffectKey: key, SectionDigest: prepared.sectionDigest, Section: prepared.advisorySection,
	})
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failedFindings(ctx, key, requestDigest, "advisory-conflict", ErrFindingsConflict)
		}
		return p.unknownFindings(ctx, key, requestDigest, ErrUnknownEffect)
	}
	if response.PullRequestNumber != prepared.request.TargetPullRequest || response.EffectKey != key || response.SectionDigest != prepared.sectionDigest || !validPullRequestURLForRepo(response.URL, prepared.request.Repo) {
		return p.unknownFindings(ctx, key, requestDigest, ErrUnknownEffect)
	}
	record := advisoryResultRecord{Version: findingsResultVersion, State: string(StateSucceeded), EffectKey: key, RequestDigest: requestDigest, PullRequestNumber: response.PullRequestNumber, URL: response.URL, HeadSHA: prepared.target.HeadSHA, SectionDigest: response.SectionDigest}
	if err := p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeSucceeded, digestRecord(record), encodeRecord(advisoryResultPrefix, record)); err != nil {
		return unknownFindingsResult(key), ErrUnknownEffect
	}
	return FindingsResult{State: StateSucceeded, AdvisoryEffectKey: key, AdvisoryState: StateSucceeded}, nil
}

func (p *Publisher) publishBlockingFinding(ctx context.Context, prepared preparedFindings, client ReviewClient, decision findingcorroboration.FindingDecision, key string) (FindingOutcome, error) {
	requestDigest, err := findingRequestDigest(prepared, decision)
	if err != nil {
		return FindingOutcome{FindingID: decision.Finding.ID, State: StateFailed, EffectKey: key}, ErrInvalidRequest
	}
	claim, err := p.ledger.Claim(ctx, effects.Claim{EffectKey: key, RequestDigest: requestDigest, Operation: FindingsOperation, RunUID: prepared.request.RunUID})
	if err != nil {
		if errors.Is(err, effects.ErrConflict) {
			return FindingOutcome{FindingID: decision.Finding.ID, State: StateFailed, EffectKey: key}, ErrFindingsConflict
		}
		return FindingOutcome{FindingID: decision.Finding.ID, State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	if !claim.Execute {
		replayed, replayErr := p.replayFinding(ctx, key, requestDigest, prepared, decision)
		return replayed, replayErr
	}
	if decision.Route != findingcorroboration.RouteBlocking {
		return FindingOutcome{FindingID: decision.Finding.ID, State: StateFailed, EffectKey: key}, ErrFindingsConflict
	}
	body, err := RenderBlockingFinding(decision, key)
	if err != nil {
		return FindingOutcome{FindingID: decision.Finding.ID, State: StateFailed, EffectKey: key}, ErrInvalidRequest
	}
	response, err := client.EnsureBlockingFindingReview(ctx, prepared.request.Repo, BlockingFindingRequest{
		PullRequestNumber: prepared.request.TargetPullRequest, CommitSHA: prepared.target.HeadSHA, EffectKey: key,
		FindingID: decision.Finding.ID, Path: decision.Finding.Path, StartLine: decision.Finding.Location.StartLine,
		EndLine: decision.Finding.Location.EndLine, Body: body,
	})
	if err != nil {
		if deterministicGitHubError(err) {
			return p.failedFinding(ctx, key, requestDigest, decision.Finding.ID, ErrFindingsConflict)
		}
		return p.unknownFinding(ctx, key, requestDigest, decision.Finding.ID, ErrUnknownEffect)
	}
	if response.PullRequestNumber != prepared.request.TargetPullRequest || response.EffectKey != key || response.FindingID != decision.Finding.ID || response.CommitSHA != prepared.target.HeadSHA || response.ReviewID <= 0 || response.CommentID <= 0 {
		return p.unknownFinding(ctx, key, requestDigest, decision.Finding.ID, ErrUnknownEffect)
	}
	record := findingResultRecord{Version: findingsResultVersion, State: string(StateSucceeded), EffectKey: key, RequestDigest: requestDigest, FindingID: decision.Finding.ID, Route: string(decision.Route), PullRequestNumber: response.PullRequestNumber, CommitSHA: response.CommitSHA, ReviewID: response.ReviewID, CommentID: response.CommentID}
	if err := p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeSucceeded, digestRecord(record), encodeRecord(findingResultPrefix, record)); err != nil {
		return FindingOutcome{FindingID: decision.Finding.ID, State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	return FindingOutcome{FindingID: decision.Finding.ID, State: StateSucceeded, EffectKey: key, ReviewID: response.ReviewID, CommentID: response.CommentID}, nil
}

func findingRequestDigest(prepared preparedFindings, decision findingcorroboration.FindingDecision) (string, error) {
	canonicalDecision, err := canonical.CanonicalizeResolvedSpec(decision)
	if err != nil {
		return "", ErrInvalidRequest
	}
	digest := DigestForPatchBytes(canonicalDecision)
	return canonical.ResolvedSpecDigest(struct {
		Version       int    `json:"version"`
		RequestDigest string `json:"requestDigest"`
		FindingDigest string `json:"findingDigest"`
		Target        int64  `json:"target"`
		HeadSHA       string `json:"headSHA"`
	}{1, prepared.requestDigest, digest, prepared.request.TargetPullRequest, prepared.target.HeadSHA})
}

func advisoryRequestDigest(prepared preparedFindings) (string, error) {
	if prepared.request.TargetPullRequest <= 0 || !validSHA(prepared.target.HeadSHA) || !canonical.ValidDigest(prepared.sectionDigest) {
		return "", ErrInvalidRequest
	}
	return canonical.ResolvedSpecDigest(struct {
		Version       int    `json:"version"`
		RequestDigest string `json:"requestDigest"`
		Target        int64  `json:"target"`
		HeadSHA       string `json:"headSHA"`
		SectionDigest string `json:"sectionDigest"`
	}{1, prepared.requestDigest, prepared.request.TargetPullRequest, prepared.target.HeadSHA, prepared.sectionDigest})
}

func (p *Publisher) replayFinding(ctx context.Context, key, requestDigest string, prepared preparedFindings, decision findingcorroboration.FindingDecision) (FindingOutcome, error) {
	outcome, err := p.ledger.ReadOutcome(ctx, key, requestDigest)
	if err != nil {
		return FindingOutcome{State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	if outcome.State == effects.OutcomeUnknown {
		return FindingOutcome{State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	if outcome.State == effects.OutcomeFailed {
		return FindingOutcome{State: StateFailed, EffectKey: key}, ErrFindingsFailed
	}
	record, ok := decodeFindingRecord(outcome, key, requestDigest)
	if !ok || record.Route != string(findingcorroboration.RouteBlocking) || record.FindingID != decision.Finding.ID || record.PullRequestNumber != prepared.request.TargetPullRequest || record.CommitSHA != prepared.target.HeadSHA || record.ReviewID <= 0 || record.CommentID <= 0 {
		return FindingOutcome{State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	return FindingOutcome{FindingID: record.FindingID, State: StateSucceeded, EffectKey: key, ReviewID: record.ReviewID, CommentID: record.CommentID}, nil
}

func (p *Publisher) replayAdvisory(ctx context.Context, key, requestDigest string, prepared preparedFindings) (FindingsResult, error) {
	outcome, err := p.ledger.ReadOutcome(ctx, key, requestDigest)
	if err != nil || outcome.State == effects.OutcomeUnknown {
		return unknownFindingsResult(key), ErrUnknownEffect
	}
	if outcome.State == effects.OutcomeFailed {
		return FindingsResult{State: StateFailed, AdvisoryEffectKey: key, AdvisoryState: StateFailed}, ErrFindingsFailed
	}
	record, ok := decodeAdvisoryRecord(outcome, key, requestDigest)
	if !ok || record.PullRequestNumber != prepared.request.TargetPullRequest || record.HeadSHA != prepared.target.HeadSHA || record.SectionDigest != prepared.sectionDigest || !validPullRequestURLForRepo(record.URL, prepared.request.Repo) {
		return unknownFindingsResult(key), ErrUnknownEffect
	}
	return FindingsResult{State: StateSucceeded, AdvisoryEffectKey: key, AdvisoryState: StateSucceeded, PullRequestNumber: record.PullRequestNumber, PullRequestURL: record.URL}, nil
}

func (p *Publisher) commitFindingsOutcome(ctx context.Context, key, requestDigest string, state effects.OutcomeState, resultDigest, resultRef string) error {
	if err := p.ledger.Commit(ctx, effects.Outcome{EffectKey: key, RequestDigest: requestDigest, State: state, ResultDigest: resultDigest, ResultRef: resultRef}); err == nil {
		return nil
	}
	recorded, err := p.ledger.ReadOutcome(ctx, key, requestDigest)
	if err != nil || recorded.State != state || recorded.ResultDigest != resultDigest || recorded.ResultRef != resultRef {
		return ErrUnknownEffect
	}
	return nil
}

func (p *Publisher) failedFindings(ctx context.Context, key, requestDigest, code string, publicErr error) (FindingsResult, error) {
	if err := p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeFailed, DigestForPatchBytes([]byte("failed:"+code)), "failed:"+code); err != nil {
		return unknownFindingsResult(key), ErrUnknownEffect
	}
	return FindingsResult{State: StateFailed, AdvisoryEffectKey: key, AdvisoryState: StateFailed}, publicErr
}

func (p *Publisher) unknownFindings(ctx context.Context, key, requestDigest string, publicErr error) (FindingsResult, error) {
	_ = p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeUnknown, "", "unknown")
	return unknownFindingsResult(key), publicErr
}

func (p *Publisher) failedFinding(ctx context.Context, key, requestDigest, findingID string, publicErr error) (FindingOutcome, error) {
	if err := p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeFailed, DigestForPatchBytes([]byte("failed:finding")), "failed:finding"); err != nil {
		return FindingOutcome{FindingID: findingID, State: StateUnknown, EffectKey: key}, ErrUnknownEffect
	}
	return FindingOutcome{FindingID: findingID, State: StateFailed, EffectKey: key}, publicErr
}

func (p *Publisher) unknownFinding(ctx context.Context, key, requestDigest, findingID string, publicErr error) (FindingOutcome, error) {
	_ = p.commitFindingsOutcome(ctx, key, requestDigest, effects.OutcomeUnknown, "", "unknown")
	return FindingOutcome{FindingID: findingID, State: StateUnknown, EffectKey: key}, publicErr
}

func unknownFindingsResult(key string) FindingsResult {
	return FindingsResult{State: StateUnknown, AdvisoryEffectKey: key, AdvisoryState: StateUnknown}
}

func decodeFindingRecord(outcome effects.Outcome, key, requestDigest string) (findingResultRecord, bool) {
	if outcome.State != effects.OutcomeSucceeded || !strings.HasPrefix(outcome.ResultRef, findingResultPrefix) || outcome.EffectKey != key || outcome.RequestDigest != requestDigest {
		return findingResultRecord{}, false
	}
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(outcome.ResultRef, findingResultPrefix))
	if err != nil || len(encoded) == 0 || DigestForPatchBytes(encoded) != outcome.ResultDigest {
		return findingResultRecord{}, false
	}
	var record findingResultRecord
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || func() bool { var trailing any; return decoder.Decode(&trailing) != io.EOF }() || record.Version != findingsResultVersion || record.State != string(StateSucceeded) || record.EffectKey != key || record.RequestDigest != requestDigest || !validSHA(record.CommitSHA) {
		return findingResultRecord{}, false
	}
	return record, true
}

func decodeAdvisoryRecord(outcome effects.Outcome, key, requestDigest string) (advisoryResultRecord, bool) {
	if outcome.State != effects.OutcomeSucceeded || !strings.HasPrefix(outcome.ResultRef, advisoryResultPrefix) || outcome.EffectKey != key || outcome.RequestDigest != requestDigest {
		return advisoryResultRecord{}, false
	}
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(outcome.ResultRef, advisoryResultPrefix))
	if err != nil || len(encoded) == 0 || DigestForPatchBytes(encoded) != outcome.ResultDigest {
		return advisoryResultRecord{}, false
	}
	var record advisoryResultRecord
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || func() bool { var trailing any; return decoder.Decode(&trailing) != io.EOF }() || record.Version != findingsResultVersion || record.State != string(StateSucceeded) || record.EffectKey != key || record.RequestDigest != requestDigest || !validSHA(record.HeadSHA) || !canonical.ValidDigest(record.SectionDigest) {
		return advisoryResultRecord{}, false
	}
	return record, true
}

func digestRecord(value any) string {
	encoded, err := canonical.CanonicalizeResolvedSpec(value)
	if err != nil {
		return ""
	}
	return DigestForPatchBytes(encoded)
}

func encodeRecord(prefix string, value any) string {
	encoded, err := canonical.CanonicalizeResolvedSpec(value)
	if err != nil || len(encoded) > MaxResultRefBytes {
		return ""
	}
	return prefix + base64.RawURLEncoding.EncodeToString(encoded)
}

// MarkerForFinding returns the exact immutable marker embedded in a blocking
// review body. It is exported for the GitHub adapter and tests only.
func MarkerForFinding(effectKey string) string { return findingMarkerPrefix + effectKey + " -->" }

// RenderBlockingFinding renders only bounded finding fields and deterministic
// evidence metadata. It never includes model assertion claims or transcripts.
func RenderBlockingFinding(decision findingcorroboration.FindingDecision, effectKey string) (string, error) {
	if decision.Route != findingcorroboration.RouteBlocking || !canonical.ValidDigest(effectKey) || !validFindingDecision(decision) {
		return "", ErrInvalidRequest
	}
	var b strings.Builder
	b.WriteString(MarkerForFinding(effectKey))
	b.WriteString("\n### Agents Gateway blocking finding\n\n")
	finding := decision.Finding
	fmt.Fprintf(&b, "**Finding:** `%s`\n\n", markdownCode(finding.ID))
	fmt.Fprintf(&b, "**Rule:** `%s`\n\n", markdownCode(finding.RuleID))
	fmt.Fprintf(&b, "**Location:** `%s:%d-%d`\n\n", markdownCode(finding.Path), finding.Location.StartLine, finding.Location.EndLine)
	fmt.Fprintf(&b, "%s\n\n", markdownText(finding.Message))
	b.WriteString("**Corroboration:** independently validated deterministic evidence\n\n")
	for _, evidence := range decision.Evidence {
		if evidence.Route != findingcorroboration.RouteBlocking {
			continue
		}
		fmt.Fprintf(&b, "- `%s` `%s`\n", markdownCode(string(evidence.Class)), markdownCode(evidence.Digest))
	}
	body := b.String()
	if len(body) > MaxFindingReviewBodyBytes {
		return "", ErrBodyTooLarge
	}
	return body, nil
}

// RenderAdvisorySection returns an AGW-owned PR-body section. An empty string
// means there are no advisory findings; callers must then perform no section
// mutation. Model-only findings are always advisory by the corroboration
// contract and are rendered here, never as review comments.
func RenderAdvisorySection(request FindingsRequest, result findingcorroboration.CorroborationResult, requestDigest string) (string, error) {
	if !canonical.ValidDigest(requestDigest) {
		return "", ErrInvalidRequest
	}
	decisions := append([]findingcorroboration.FindingDecision(nil), result.Findings...)
	sort.Slice(decisions, func(left, right int) bool { return decisions[left].Finding.ID < decisions[right].Finding.ID })
	var b strings.Builder
	for _, decision := range decisions {
		if decision.Route != findingcorroboration.RouteAdvisory {
			continue
		}
		if !validFindingDecision(decision) {
			return "", ErrInvalidRequest
		}
		if b.Len() == 0 {
			b.WriteString(advisoryMarkerStart)
			b.WriteString("\n## Agents Gateway advisory findings\n\n")
			b.WriteString("These findings are informational only. They do not request changes and are not an authoritative review decision.\n\n")
			fmt.Fprintf(&b, "Finding artifact: `%s`\n\n", markdownCode(request.ArtifactDigest))
		}
		finding := decision.Finding
		fmt.Fprintf(&b, "### `%s` — `%s:%d-%d`\n\n", markdownCode(finding.ID), markdownCode(finding.Path), finding.Location.StartLine, finding.Location.EndLine)
		fmt.Fprintf(&b, "**Rule:** `%s`\n\n", markdownCode(finding.RuleID))
		fmt.Fprintf(&b, "%s\n\n", markdownText(finding.Message))
		fmt.Fprintf(&b, "**Why advisory:** %s\n\n", markdownText(decision.Reason))
	}
	if b.Len() == 0 {
		return "", nil
	}
	b.WriteString(advisoryMarkerEnd)
	section := b.String()
	if len(section) > MaxAdvisorySectionBytes {
		return "", ErrBodyTooLarge
	}
	return section, nil
}

// MergeAdvisorySection preserves all user-owned PR-body bytes and replaces or
// appends exactly one AGW-owned marker section. A mismatched existing section
// is a conflict; silently overwriting it could erase a human review note.
func MergeAdvisorySection(body, section string) (string, error) {
	if len(body) > MaxPullRequestBodyBytes || !utf8.ValidString(body) || strings.ContainsAny(body, "\x00") || len(section) > MaxAdvisorySectionBytes || !utf8.ValidString(section) {
		return "", ErrInvalidRequest
	}
	if section == "" {
		return body, nil
	}
	start := strings.Index(body, advisoryMarkerStart)
	endMarkerIndex := strings.Index(body, advisoryMarkerEnd)
	if start >= 0 || endMarkerIndex >= 0 {
		if start < 0 || endMarkerIndex < start {
			return "", ErrGitHubConflict
		}
		end := endMarkerIndex + len(advisoryMarkerEnd)
		if strings.TrimSpace(body[start:end]) != strings.TrimSpace(section) {
			return "", ErrGitHubConflict
		}
		return body, nil
	}
	separator := "\n\n"
	if strings.TrimSpace(body) == "" {
		separator = ""
	}
	merged := body + separator + section
	if len(merged) > MaxPullRequestBodyBytes {
		return "", ErrBodyTooLarge
	}
	return merged, nil
}

func validFindingDecision(decision findingcorroboration.FindingDecision) bool {
	canonicalBytes, err := findingcorroboration.CanonicalResultBytes(findingcorroboration.CorroborationResult{SchemaVersion: findingcorroboration.SchemaVersion, Findings: []findingcorroboration.FindingDecision{decision}, Counts: findingcorroboration.Counts{TotalFindings: 1, BlockingFindings: boolInt(decision.Route == findingcorroboration.RouteBlocking), AdvisoryFindings: boolInt(decision.Route == findingcorroboration.RouteAdvisory), DeterministicEvidence: countDeterministic(decision), CorroboratingEvidence: countCorroborating(decision), ModelAssertions: countModels(decision)}})
	return err == nil && len(canonicalBytes) > 0
}

func countDeterministic(decision findingcorroboration.FindingDecision) int {
	count := 0
	for _, evidence := range decision.Evidence {
		if evidence.Class != findingcorroboration.EvidenceClassModelAssertion && evidence.Deterministic && evidence.Independent {
			count++
		}
	}
	return count
}

func countCorroborating(decision findingcorroboration.FindingDecision) int {
	count := 0
	for _, evidence := range decision.Evidence {
		if evidence.Class != findingcorroboration.EvidenceClassModelAssertion && evidence.Deterministic && evidence.Independent && evidence.Corroborates {
			count++
		}
	}
	return count
}

func countModels(decision findingcorroboration.FindingDecision) int {
	count := 0
	for _, evidence := range decision.Evidence {
		if evidence.Class == findingcorroboration.EvidenceClassModelAssertion {
			count++
		}
	}
	return count
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func markdownCode(value string) string {
	value = strings.ReplaceAll(value, "`", "'")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}

func markdownText(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	value = strings.ReplaceAll(value, "\x00", "")
	return value
}
