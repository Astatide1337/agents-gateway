package githubpublish

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

const (
	maxReviewCommentCount = 100
	maxReviewBodyBytes    = publish.MaxFindingReviewBodyBytes
)

var _ publish.ReviewClient = (*Client)(nil)

type reviewPullRequestPayload struct {
	Number  int64  `json:"number"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type reviewPayload struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	State    string `json:"state"`
	CommitID string `json:"commit_id"`
}

type reviewCommentPayload struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      *int   `json:"line"`
	StartLine *int   `json:"start_line"`
	Side      string `json:"side"`
	StartSide string `json:"start_side"`
	CommitID  string `json:"commit_id"`
}

// GetPullRequest reads the immutable target identity used by findings output.
// It does not create or update any GitHub object.
func (c *Client) GetPullRequest(ctx context.Context, repo githubapp.Repository, number int64) (publish.PullRequestTarget, error) {
	if number <= 0 || number > publish.MaxPullRequestNumber {
		return publish.PullRequestTarget{}, publish.ErrInvalidRequest
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.PullRequestTarget{}, err
	}
	return c.readPullRequestTarget(ctx, current, repo, number)
}

func (c *Client) readPullRequestTarget(ctx context.Context, current session, repo githubapp.Repository, number int64) (publish.PullRequestTarget, error) {
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/pulls/"+strconv.FormatInt(number, 10), nil, nil)
	if err != nil {
		return publish.PullRequestTarget{}, err
	}
	if response.status == http.StatusNotFound {
		return publish.PullRequestTarget{}, publish.ErrGitHubNotFound
	}
	if response.status != http.StatusOK {
		return publish.PullRequestTarget{}, ErrUnclassified
	}
	var decoded reviewPullRequestPayload
	if decodeJSON(response.body, &decoded) != nil || !validReviewPullRequest(decoded, repo, number) {
		return publish.PullRequestTarget{}, ErrUnclassified
	}
	return publish.PullRequestTarget{
		Number: decoded.Number, URL: decoded.HTMLURL, BaseRef: decoded.Base.Ref,
		HeadBranch: decoded.Head.Ref, HeadSHA: decoded.Head.SHA, Body: decoded.Body,
	}, nil
}

func validReviewPullRequest(value reviewPullRequestPayload, repo githubapp.Repository, number int64) bool {
	return value.Number == number && validPullURL(value.HTMLURL, repo, number) && validBaseRefName(value.Head.Ref) && validObjectSHA(value.Head.SHA) && validBaseRefName(value.Base.Ref) && len(value.Body) <= publish.MaxPullRequestBodyBytes && utf8.ValidString(value.Body) && !strings.ContainsRune(value.Body, '\x00')
}

// EnsureAdvisorySection idempotently appends the AGW-owned informational
// section to a pull request body. It never creates review state.
func (c *Client) EnsureAdvisorySection(ctx context.Context, repo githubapp.Repository, request publish.AdvisorySectionRequest) (publish.AdvisorySectionResult, error) {
	if err := validateAdvisorySectionRequest(request); err != nil {
		return publish.AdvisorySectionResult{}, err
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.AdvisorySectionResult{}, err
	}
	target, err := c.readPullRequestTarget(ctx, current, repo, request.PullRequestNumber)
	if err != nil {
		return publish.AdvisorySectionResult{}, err
	}
	if target.HeadSHA != request.ExpectedHeadSHA {
		return publish.AdvisorySectionResult{}, publish.ErrGitHubConflict
	}
	merged, err := publish.MergeAdvisorySection(target.Body, request.Section)
	if err != nil {
		return publish.AdvisorySectionResult{}, err
	}
	if merged == target.Body {
		return advisorySectionResult(target, request), nil
	}
	response, err := c.do(ctx, current, http.MethodPatch, repoPath(repo)+"/pulls/"+strconv.FormatInt(request.PullRequestNumber, 10), nil, map[string]string{"body": merged})
	if err != nil || response.status != http.StatusOK {
		// A successful PATCH can be hidden by a timeout. A read proving the
		// exact body is success; anything else stays ambiguous.
		reconciled, reconcileErr := c.readPullRequestTarget(ctx, current, repo, request.PullRequestNumber)
		if reconcileErr == nil && reconciled.HeadSHA == request.ExpectedHeadSHA {
			if mergedBody, mergeErr := publish.MergeAdvisorySection(reconciled.Body, request.Section); mergeErr == nil && mergedBody == reconciled.Body {
				return advisorySectionResult(reconciled, request), nil
			}
		}
		if response.status == http.StatusUnprocessableEntity && err == nil {
			return publish.AdvisorySectionResult{}, publish.ErrGitHubConflict
		}
		return publish.AdvisorySectionResult{}, ErrUnclassified
	}
	var decoded reviewPullRequestPayload
	if decodeJSON(response.body, &decoded) != nil || !validReviewPullRequest(decoded, repo, request.PullRequestNumber) || decoded.Head.SHA != request.ExpectedHeadSHA || decoded.Body != merged {
		return publish.AdvisorySectionResult{}, ErrUnclassified
	}
	return advisorySectionResult(publish.PullRequestTarget{Number: decoded.Number, URL: decoded.HTMLURL, BaseRef: decoded.Base.Ref, HeadBranch: decoded.Head.Ref, HeadSHA: decoded.Head.SHA, Body: decoded.Body}, request), nil
}

func advisorySectionResult(target publish.PullRequestTarget, request publish.AdvisorySectionRequest) publish.AdvisorySectionResult {
	return publish.AdvisorySectionResult{PullRequestNumber: target.Number, URL: target.URL, HeadSHA: target.HeadSHA, EffectKey: request.EffectKey, SectionDigest: request.SectionDigest}
}

func validateAdvisorySectionRequest(request publish.AdvisorySectionRequest) error {
	if request.PullRequestNumber <= 0 || request.PullRequestNumber > publish.MaxPullRequestNumber || !validObjectSHA(request.ExpectedHeadSHA) || !validDigest(request.EffectKey) || !validDigest(request.SectionDigest) || len(request.Section) == 0 || len(request.Section) > publish.MaxAdvisorySectionBytes || !utf8.ValidString(request.Section) || strings.ContainsRune(request.Section, '\x00') || publish.DigestForPatchBytes([]byte(request.Section)) != request.SectionDigest || !strings.HasPrefix(request.Section, "<!-- agw-advisory-findings:v1 -->") || !strings.HasSuffix(request.Section, "<!-- /agw-advisory-findings:v1 -->") {
		return publish.ErrInvalidRequest
	}
	return nil
}

// EnsureBlockingFindingReview creates or proves one REQUEST_CHANGES review
// containing exactly one line-level comment. The effect marker is present in
// both the review body and comment body, so a transport failure can be
// reconciled without creating a duplicate review.
func (c *Client) EnsureBlockingFindingReview(ctx context.Context, repo githubapp.Repository, request publish.BlockingFindingRequest) (publish.BlockingFindingResult, error) {
	if err := validateBlockingFindingRequest(request); err != nil {
		return publish.BlockingFindingResult{}, err
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.BlockingFindingResult{}, err
	}
	target, err := c.readPullRequestTarget(ctx, current, repo, request.PullRequestNumber)
	if err != nil {
		return publish.BlockingFindingResult{}, err
	}
	if target.HeadSHA != request.CommitSHA {
		return publish.BlockingFindingResult{}, publish.ErrGitHubConflict
	}
	existing, found, err := c.findFindingReview(ctx, current, repo, request)
	if err != nil || found {
		return existing, err
	}
	comments := map[string]any{
		"path": request.Path, "line": request.EndLine, "side": "RIGHT", "body": request.Body,
	}
	if request.StartLine != request.EndLine {
		comments["start_line"] = request.StartLine
		comments["start_side"] = "RIGHT"
	}
	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/pulls/"+strconv.FormatInt(request.PullRequestNumber, 10)+"/reviews", nil, map[string]any{
		"body": request.Body, "event": "REQUEST_CHANGES", "commit_id": request.CommitSHA,
		"comments": []any{comments},
	})
	if err != nil || response.status != http.StatusCreated {
		reconciled, found, reconcileErr := c.findFindingReview(ctx, current, repo, request)
		if reconcileErr == nil && found {
			return reconciled, nil
		}
		if response.status == http.StatusUnprocessableEntity && err == nil {
			return publish.BlockingFindingResult{}, publish.ErrGitHubConflict
		}
		return publish.BlockingFindingResult{}, ErrUnclassified
	}
	var created reviewPayload
	if decodeJSON(response.body, &created) != nil || created.ID <= 0 || created.State != "CHANGES_REQUESTED" || created.CommitID != request.CommitSHA || !strings.Contains(created.Body, publish.MarkerForFinding(request.EffectKey)) {
		// The review may already exist even if its response was malformed. Use
		// the marker reconciliation before declaring the outcome unknown.
		reconciled, found, reconcileErr := c.findFindingReview(ctx, current, repo, request)
		if reconcileErr == nil && found {
			return reconciled, nil
		}
		return publish.BlockingFindingResult{}, ErrUnclassified
	}
	reconciled, found, reconcileErr := c.findFindingReview(ctx, current, repo, request)
	if reconcileErr != nil || !found {
		return publish.BlockingFindingResult{}, ErrUnclassified
	}
	return reconciled, nil
}

func validateBlockingFindingRequest(request publish.BlockingFindingRequest) error {
	if request.PullRequestNumber <= 0 || request.PullRequestNumber > publish.MaxPullRequestNumber || !validObjectSHA(request.CommitSHA) || !validDigest(request.EffectKey) || len(request.FindingID) == 0 || len(request.FindingID) > 128 || !utf8.ValidString(request.FindingID) || strings.ContainsAny(request.FindingID, "\x00\r\n") || !validPath(request.Path) || request.StartLine < 1 || request.EndLine < request.StartLine || request.EndLine > publish.MaxReviewLine || request.StartLine > publish.MaxReviewLine || len(request.Body) == 0 || len(request.Body) > maxReviewBodyBytes || !utf8.ValidString(request.Body) || strings.ContainsRune(request.Body, '\x00') {
		return publish.ErrInvalidRequest
	}
	marker := publish.MarkerForFinding(request.EffectKey)
	if strings.Count(request.Body, marker) != 1 {
		return publish.ErrInvalidRequest
	}
	return nil
}

func (c *Client) findFindingReview(ctx context.Context, current session, repo githubapp.Repository, request publish.BlockingFindingRequest) (publish.BlockingFindingResult, bool, error) {
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/pulls/"+strconv.FormatInt(request.PullRequestNumber, 10)+"/reviews", url.Values{"per_page": []string{"100"}}, nil)
	if err != nil {
		return publish.BlockingFindingResult{}, false, err
	}
	if response.status == http.StatusNotFound {
		return publish.BlockingFindingResult{}, false, publish.ErrGitHubNotFound
	}
	if response.status != http.StatusOK {
		return publish.BlockingFindingResult{}, false, ErrUnclassified
	}
	var reviews []reviewPayload
	// A full page does not prove that no older marker exists. Until this
	// adapter implements bounded pagination, fail closed instead of risking a
	// duplicate review effect.
	if decodeJSON(response.body, &reviews) != nil || len(reviews) >= maxReviewCommentCount {
		return publish.BlockingFindingResult{}, false, ErrUnclassified
	}
	marker := publish.MarkerForFinding(request.EffectKey)
	var found publish.BlockingFindingResult
	foundCount := 0
	for _, review := range reviews {
		if review.ID <= 0 || len(review.Body) > maxReviewBodyBytes || !utf8.ValidString(review.Body) {
			return publish.BlockingFindingResult{}, false, ErrUnclassified
		}
		if !strings.Contains(review.Body, marker) {
			continue
		}
		foundCount++
		if foundCount > 1 || review.State != "CHANGES_REQUESTED" || review.CommitID != request.CommitSHA || review.Body != request.Body {
			return publish.BlockingFindingResult{}, false, ErrUnclassified
		}
		comments, commentErr := c.readReviewComments(ctx, current, repo, request.PullRequestNumber, review.ID)
		if commentErr != nil {
			return publish.BlockingFindingResult{}, false, commentErr
		}
		var matching reviewCommentPayload
		matchCount := 0
		for _, comment := range comments {
			if comment.ID <= 0 || len(comment.Body) > maxReviewBodyBytes || !utf8.ValidString(comment.Body) {
				return publish.BlockingFindingResult{}, false, ErrUnclassified
			}
			if !strings.Contains(comment.Body, marker) {
				continue
			}
			matchCount++
			if matchCount > 1 || comment.Body != request.Body || comment.Path != request.Path || comment.CommitID != request.CommitSHA || comment.Side != "RIGHT" || comment.Line == nil || *comment.Line != request.EndLine {
				return publish.BlockingFindingResult{}, false, ErrUnclassified
			}
			if request.StartLine == request.EndLine {
				if comment.StartLine != nil {
					return publish.BlockingFindingResult{}, false, ErrUnclassified
				}
			} else if comment.StartLine == nil || *comment.StartLine != request.StartLine || comment.StartSide != "RIGHT" {
				return publish.BlockingFindingResult{}, false, ErrUnclassified
			}
			matching = comment
		}
		if matchCount != 1 {
			return publish.BlockingFindingResult{}, false, ErrUnclassified
		}
		found = publish.BlockingFindingResult{PullRequestNumber: request.PullRequestNumber, ReviewID: review.ID, CommentID: matching.ID, CommitSHA: request.CommitSHA, EffectKey: request.EffectKey, FindingID: request.FindingID}
	}
	return found, foundCount == 1, nil
}

func (c *Client) readReviewComments(ctx context.Context, current session, repo githubapp.Repository, number, reviewID int64) ([]reviewCommentPayload, error) {
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/pulls/"+strconv.FormatInt(number, 10)+"/reviews/"+strconv.FormatInt(reviewID, 10)+"/comments", url.Values{"per_page": []string{"100"}}, nil)
	if err != nil {
		return nil, err
	}
	if response.status != http.StatusOK {
		return nil, ErrUnclassified
	}
	var comments []reviewCommentPayload
	if decodeJSON(response.body, &comments) != nil || len(comments) >= maxReviewCommentCount {
		return nil, ErrUnclassified
	}
	return comments, nil
}
