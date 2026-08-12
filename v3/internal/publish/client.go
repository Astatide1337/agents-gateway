// Package publish contains the controller-owned GitHub publication boundary.
//
// The Client interface is deliberately credential-free. An implementation is
// constructed by the operator with a short-lived GitHub App installation
// token, but the Publisher never receives, stores, logs, or forwards that
// token. The Ensure methods are proof-bearing operations: they must return
// success only after the requested GitHub state has been read back and
// verified. An implementation that cannot prove the state must return an
// error; the Publisher will then enter UnknownEffect.
package publish

import (
	"context"
	"errors"

	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
)

var (
	// ErrInvalidRequest means the publisher or its caller supplied an invalid
	// immutable contract. It is safe to return because it contains no input.
	ErrInvalidRequest = errors.New("publish: invalid request")
	// ErrGitHubConflict means the injected client proved that the requested
	// state conflicts with existing GitHub state without applying the effect.
	ErrGitHubConflict = errors.New("publish: GitHub state conflict")
	// ErrGitHubNotFound means a required GitHub object was proven absent.
	ErrGitHubNotFound = errors.New("publish: GitHub object not found")
	// ErrMutationAmbiguous is an optional client classification for an API
	// response that cannot prove whether a mutation happened. The Publisher
	// treats all unclassified client errors the same way.
	ErrMutationAmbiguous = errors.New("publish: GitHub mutation outcome is ambiguous")
	// ErrUnknownEffect is returned when a claim, GitHub mutation, or ledger
	// outcome cannot be proven. Callers must surface this as terminal
	// UnknownEffect and require human reconciliation.
	ErrUnknownEffect = errors.New("publish: external effect is unknown")
	// ErrBaseMismatch is a deterministic, pre-mutation failure: the named base
	// ref no longer points at the immutable base SHA in the run contract.
	ErrBaseMismatch = errors.New("publish: base ref does not match base SHA")
	// ErrPreviouslyFailed reports a replay of a durably recorded failed
	// publication. It is intentionally static and bounded.
	ErrPreviouslyFailed = errors.New("publish: publication was previously rejected")
	// ErrBodyTooLarge is returned if the evidence body exceeds the bounded
	// GitHub request contract.
	ErrBodyTooLarge = errors.New("publish: evidence body exceeds bound")
)

// Client is the only authenticated boundary used by Publisher. It contains
// no token parameter by design. An implementation should use the GitHub Git
// data API for branches, trees, and commits and the pull-request API for the
// PR and labels.
type Client interface {
	// GetRef reads a fully qualified ref such as refs/heads/main. It is a
	// read-only call and is used immediately after the ledger claim to prove
	// that the requested base has not moved.
	GetRef(context.Context, githubapp.Repository, string) (Reference, error)
	// EnsureBranch creates refs/heads/Name at BaseSHA, or proves that the
	// existing deterministic branch already has exactly that SHA.
	EnsureBranch(context.Context, githubapp.Repository, BranchRequest) (BranchResult, error)
	// EnsureCommit creates or proves the exact commit represented by Patch on
	// BranchName with BaseSHA as its parent. The implementation must use the
	// fixed author in the request and verify the patch digest before success.
	EnsureCommit(context.Context, githubapp.Repository, CommitRequest) (CommitResult, error)
	// EnsurePullRequest opens or proves one PR for the exact head/base/effect
	// marker. It must never merge or otherwise modify the base branch.
	EnsurePullRequest(context.Context, githubapp.Repository, PullRequestRequest) (PullRequestResult, error)
	// EnsureLabels applies the complete desired label set and proves that all
	// labels are present on the PR. Partial or uncertain application is an
	// error and therefore terminal UnknownEffect to the Publisher.
	EnsureLabels(context.Context, githubapp.Repository, LabelRequest) error
}

// Reference is the minimal read-only Git ref proof.
type Reference struct {
	Name string
	SHA  string
}

// BranchRequest is the exact branch state the client must create or prove.
type BranchRequest struct {
	Name    string
	BaseSHA string
}

// BranchResult is a proof returned by EnsureBranch.
type BranchResult struct {
	Name string
	SHA  string
}

// CommitRequest is the exact immutable commit operation. The Publisher fixes
// Author to the unexported AGW bot identity; callers cannot supply a
// credential or an alternate identity through the Publisher.
type CommitRequest struct {
	BranchName  string
	BaseSHA     string
	Patch       Patch
	PatchDigest string
	// ManifestDigest independently binds the normalized file operations used
	// by a GitHub Git-data client. PatchDigest remains the digest of the exact
	// captured patch.diff bytes and is the effect-ledger key input.
	ManifestDigest string
	Message        string
	Author         CommitAuthor
	EffectKey      string
}

// CommitAuthor is intentionally a value type containing public identity only.
type CommitAuthor struct {
	Name  string
	Email string
}

// agwBotAuthor is the sole identity used for controller-owned commits. Keep
// this value private so an integration cannot mutate the publisher's author
// through a package-level variable.
var agwBotAuthor = CommitAuthor{
	Name:  "agw-bot",
	Email: "agw-bot@users.noreply.github.com",
}

// CommitResult is a proof returned by EnsureCommit. The echoed fields bind
// the returned commit to the operation that the Publisher requested.
type CommitResult struct {
	SHA            string
	BranchName     string
	BaseSHA        string
	PatchDigest    string
	ManifestDigest string
	Author         CommitAuthor
}

// PullRequestRequest is the exact PR mutation. EffectKey is included in the
// request and body as the durable idempotency marker. No merge operation is
// represented by this interface.
type PullRequestRequest struct {
	BaseRef    string
	HeadBranch string
	Title      string
	Body       string
	EffectKey  string
}

// PullRequestResult is a proof returned by EnsurePullRequest.
type PullRequestResult struct {
	Number     int64
	URL        string
	BaseRef    string
	HeadBranch string
	EffectKey  string
}

// LabelRequest is a complete desired label set, not a single append. Labels
// are sorted and deduplicated by the Publisher before they cross this seam.
type LabelRequest struct {
	Number int64
	Labels []string
}
