// Package githubpublish implements the controller-owned GitHub publication
// boundary for Agents Gateway v3.
//
// The package deliberately keeps authentication behind githubapp.Minter. A
// publish.Client caller never receives a token, and this package never puts a
// token in an error. Each public operation mints one short-lived,
// repository-scoped PermissionPublish installation token and uses it only for
// that operation.
package githubpublish

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

const (
	DefaultRequestTimeout = 15 * time.Second
	MaxRequestTimeout     = 2 * time.Minute
	DefaultMaxResponse    = 8 << 20
	MaxResponseBytes      = 16 << 20
	MaxRequestBytes       = 16 << 20

	maxRefBytes       = 512
	maxBranchBytes    = 255
	maxMessageBytes   = publish.MaxCommitMessageBytes
	maxPullTitleBytes = publish.MaxTitleBytes
	maxPullBodyBytes  = publish.MaxBodyBytes
	maxURLBytes       = publish.MaxPullRequestURLBytes

	agwBotName  = "agw-bot"
	agwBotEmail = "agw-bot@users.noreply.github.com"
	userAgent   = "agents-gateway-v3-publisher"
)

var branchProofDelays = [...]time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}

var (
	// ErrInvalidConfig is returned only for an invalid adapter construction.
	// It contains no configuration values or credentials.
	ErrInvalidConfig = errors.New("githubpublish: invalid configuration")
	// ErrInvalidContext is returned for a nil context. It is static so it is
	// safe to expose to a controller and to tests.
	ErrInvalidContext = errors.New("githubpublish: invalid context")
	// ErrUnclassified means the adapter could not prove the result of an API
	// operation. Publisher maps this to terminal UnknownEffect.
	ErrUnclassified = errors.New("githubpublish: GitHub effect outcome is unclassified")

	errNotFound = errors.New("githubpublish: object not found")
)

// Config constructs an authenticated Client. BaseURL must be the same GitHub
// API origin configured for the supplied Minter. It is normally omitted and
// defaults to api.github.com; a GitHub Enterprise origin may be supplied
// explicitly.
type Config struct {
	Minter *githubapp.Minter

	BaseURL         string
	HTTPClient      *http.Client
	RequestTimeout  time.Duration
	MaxResponseSize int64
}

// Client implements publish.Client. It has no cached installation token: each
// operation mints a fresh, exact-repository PermissionPublish token.
type Client struct {
	minter       *githubapp.Minter
	baseURL      url.URL
	httpClient   *http.Client
	requestLimit time.Duration
	maxResponse  int64
}

var _ publish.Client = (*Client)(nil)

// New constructs a production adapter. Redirects are disabled so a token can
// never be forwarded to a different origin, and response bodies are bounded
// before decoding.
func New(config Config) (*Client, error) {
	if config.Minter == nil {
		return nil, ErrInvalidConfig
	}
	baseURL, err := parseBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := config.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	if timeout <= 0 || timeout > MaxRequestTimeout {
		return nil, ErrInvalidConfig
	}
	maxResponse := config.MaxResponseSize
	if maxResponse == 0 {
		maxResponse = DefaultMaxResponse
	}
	if maxResponse <= 0 || maxResponse > MaxResponseBytes {
		return nil, ErrInvalidConfig
	}
	httpClient, err := cloneHTTPClient(config.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &Client{
		minter:       config.Minter,
		baseURL:      baseURL,
		httpClient:   httpClient,
		requestLimit: timeout,
		maxResponse:  maxResponse,
	}, nil
}

type session struct{ token string }

func (c *Client) session(ctx context.Context, repo githubapp.Repository) (session, error) {
	if ctx == nil {
		return session{}, ErrInvalidContext
	}
	if repo.Validate() != nil {
		return session{}, publish.ErrInvalidRequest
	}
	token, err := c.minter.Mint(ctx, repo, githubapp.PermissionPublish)
	if err != nil {
		// Minter errors are intentionally collapsed. In particular, no future
		// implementation can accidentally return a provider body or token.
		return session{}, ErrUnclassified
	}
	if token.IsZero() || token.Repository() != repo || token.PermissionProfile() != githubapp.PermissionPublish || token.ExpiresAt().IsZero() {
		return session{}, ErrUnclassified
	}
	return session{token: token.Value()}, nil
}

type apiResponse struct {
	status int
	body   []byte
}

func (c *Client) do(ctx context.Context, current session, method, apiPath string, query url.Values, payload any) (apiResponse, error) {
	if ctx == nil {
		return apiResponse{}, ErrInvalidContext
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil || len(encoded) > MaxRequestBytes {
			return apiResponse{}, ErrUnclassified
		}
		body = bytes.NewReader(encoded)
	}
	requestContext, cancel := context.WithTimeout(ctx, c.requestLimit)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, c.endpoint(apiPath, query), body)
	if err != nil {
		return apiResponse{}, ErrUnclassified
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+current.token)
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil || response == nil || response.Body == nil {
		return apiResponse{}, ErrUnclassified
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, c.maxResponse+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil || int64(len(responseBody)) > c.maxResponse {
		return apiResponse{}, ErrUnclassified
	}
	return apiResponse{status: response.StatusCode, body: responseBody}, nil
}

func (c *Client) endpoint(apiPath string, query url.Values) string {
	endpoint := strings.TrimRight(c.baseURL.String(), "/") + "/" + strings.TrimLeft(apiPath, "/")
	if encoded := query.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	return endpoint
}

func parseBaseURL(raw string) (url.URL, error) {
	if raw == "" {
		raw = githubapp.DefaultAPIBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return url.URL{}, ErrInvalidConfig
	}
	if parsed.RawPath != "" || strings.ContainsRune(parsed.Path, '\x00') {
		return url.URL{}, ErrInvalidConfig
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return url.URL{}, ErrInvalidConfig
		}
	}
	return *parsed, nil
}

func cloneHTTPClient(input *http.Client) (*http.Client, error) {
	client := &http.Client{}
	if input != nil {
		*client = *input
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Jar = nil
	transport := client.Transport
	if transport == nil {
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || defaultTransport == nil {
			return nil, ErrInvalidConfig
		}
		transport = defaultTransport.Clone()
	}
	if standard, ok := transport.(*http.Transport); ok {
		standard = standard.Clone()
		standard.Proxy = nil
		client.Transport = standard
	} else {
		client.Transport = transport
	}
	return client, nil
}

func decodeJSON(body []byte, target any) error {
	if len(body) == 0 {
		return ErrUnclassified
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return ErrUnclassified
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrUnclassified
	}
	return nil
}

func repoPath(repo githubapp.Repository) string {
	return "repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
}

func refAPIPath(fullRef string) (string, error) {
	if !validFullRef(fullRef) {
		return "", publish.ErrInvalidRequest
	}
	parts := strings.Split(strings.TrimPrefix(fullRef, "refs/"), "/")
	return strings.Join(append([]string{"git", "ref"}, escapeParts(parts)...), "/"), nil
}

func escapeParts(parts []string) []string {
	escaped := make([]string, len(parts))
	for index, part := range parts {
		escaped[index] = url.PathEscape(part)
	}
	return escaped
}

type refPayload struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

func (c *Client) readRef(ctx context.Context, current session, repo githubapp.Repository, fullRef string) (publish.Reference, error) {
	apiRef, err := refAPIPath(fullRef)
	if err != nil {
		return publish.Reference{}, err
	}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/"+apiRef, nil, nil)
	if err != nil {
		return publish.Reference{}, err
	}
	if response.status == http.StatusNotFound {
		return publish.Reference{}, errNotFound
	}
	if response.status != http.StatusOK {
		return publish.Reference{}, ErrUnclassified
	}
	var decoded refPayload
	if decodeJSON(response.body, &decoded) != nil || decoded.Ref != fullRef || decoded.Object.Type != "commit" || !validObjectSHA(decoded.Object.SHA) {
		return publish.Reference{}, ErrUnclassified
	}
	return publish.Reference{Name: decoded.Ref, SHA: decoded.Object.SHA}, nil
}

// GetRef reads and proves a fully qualified GitHub ref.
func (c *Client) GetRef(ctx context.Context, repo githubapp.Repository, ref string) (publish.Reference, error) {
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.Reference{}, err
	}
	observed, err := c.readRef(ctx, current, repo, ref)
	if errors.Is(err, errNotFound) {
		return publish.Reference{}, publish.ErrGitHubNotFound
	}
	if err != nil {
		return publish.Reference{}, err
	}
	return observed, nil
}

// EnsureBranch creates the deterministic branch or proves an existing branch
// already points at the exact requested base commit.
func (c *Client) EnsureBranch(ctx context.Context, repo githubapp.Repository, request publish.BranchRequest) (publish.BranchResult, error) {
	if !validBranchName(request.Name) || !validObjectSHA(request.BaseSHA) {
		return publish.BranchResult{}, publish.ErrInvalidRequest
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.BranchResult{}, err
	}
	fullRef := "refs/heads/" + request.Name
	observed, err := c.readRef(ctx, current, repo, fullRef)
	if err == nil {
		if observed.SHA != request.BaseSHA {
			return publish.BranchResult{}, publish.ErrGitHubConflict
		}
		return publish.BranchResult{Name: request.Name, SHA: observed.SHA}, nil
	}
	if !errors.Is(err, errNotFound) {
		return publish.BranchResult{}, err
	}

	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/git/refs", nil, map[string]any{
		"ref": fullRef,
		"sha": request.BaseSHA,
	})
	if err != nil {
		return c.reconcileBranch(ctx, current, repo, request, fullRef)
	}
	if response.status != http.StatusCreated {
		return c.reconcileBranch(ctx, current, repo, request, fullRef)
	}
	var created refPayload
	if decodeJSON(response.body, &created) != nil || created.Ref != fullRef || created.Object.Type != "commit" || created.Object.SHA != request.BaseSHA {
		return publish.BranchResult{}, ErrUnclassified
	}
	return c.reconcileBranch(ctx, current, repo, request, fullRef)
}

func (c *Client) reconcileBranch(ctx context.Context, current session, repo githubapp.Repository, request publish.BranchRequest, fullRef string) (publish.BranchResult, error) {
	// GitHub can return the successful ref-creation response before the new
	// ref is visible to a subsequent GET. Reconcile with bounded read-only
	// retries; never repeat the POST or PATCH mutation.
	for attempt, delay := range branchProofDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return publish.BranchResult{}, ErrUnclassified
			case <-timer.C:
			}
		}
		observed, err := c.readRef(ctx, current, repo, fullRef)
		if err == nil {
			if observed.SHA != request.BaseSHA {
				return publish.BranchResult{}, publish.ErrGitHubConflict
			}
			return publish.BranchResult{Name: request.Name, SHA: observed.SHA}, nil
		}
		if !errors.Is(err, errNotFound) || attempt == len(branchProofDelays)-1 {
			return publish.BranchResult{}, ErrUnclassified
		}
	}
	return publish.BranchResult{}, ErrUnclassified
}

type blobPayload struct {
	SHA      string `json:"sha"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

type treeRequestEntry struct {
	Path string  `json:"path"`
	Mode string  `json:"mode"`
	Type string  `json:"type"`
	SHA  *string `json:"sha"`
}

type treePayload struct {
	SHA       string             `json:"sha"`
	Truncated bool               `json:"truncated"`
	Tree      []treeResponseItem `json:"tree"`
}

type treeResponseItem struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type commitPayload struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Tree    struct {
		SHA string `json:"sha"`
	} `json:"tree"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Author struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
}

type gitCommitRequest struct {
	Message   string       `json:"message"`
	Tree      string       `json:"tree"`
	Parents   []string     `json:"parents"`
	Author    gitSignature `json:"author"`
	Committer gitSignature `json:"committer"`
}

type gitSignature struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func (c *Client) readBlob(ctx context.Context, current session, repo githubapp.Repository, sha string, want []byte) error {
	if !validObjectSHA(sha) {
		return ErrUnclassified
	}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/git/blobs/"+url.PathEscape(sha), nil, nil)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		return errNotFound
	}
	if response.status != http.StatusOK {
		return ErrUnclassified
	}
	var decoded blobPayload
	if decodeJSON(response.body, &decoded) != nil || decoded.SHA != sha || decoded.Encoding != "base64" {
		return ErrUnclassified
	}
	content, ok := decodeBase64(decoded.Content)
	if !ok || !bytes.Equal(content, want) {
		return ErrUnclassified
	}
	return nil
}

func (c *Client) ensureBlob(ctx context.Context, current session, repo githubapp.Repository, content []byte) (string, error) {
	expected := gitBlobSHA(content)
	if err := c.readBlob(ctx, current, repo, expected, content); err == nil {
		return expected, nil
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/git/blobs", nil, map[string]string{
		"content":  encoded,
		"encoding": "base64",
	})
	if err != nil {
		if reconcileErr := c.readBlob(ctx, current, repo, expected, content); reconcileErr == nil {
			return expected, nil
		}
		return "", ErrUnclassified
	}
	if response.status != http.StatusCreated {
		// A blob POST can have succeeded before a transport/status failure. A
		// read can prove an exact object, but absence is still ambiguous and
		// must not be retried by the caller.
		if reconcileErr := c.readBlob(ctx, current, repo, expected, content); reconcileErr == nil {
			return expected, nil
		}
		return "", ErrUnclassified
	}
	var created blobPayload
	if decodeJSON(response.body, &created) != nil || created.SHA != expected {
		return "", ErrUnclassified
	}
	if err := c.readBlob(ctx, current, repo, expected, content); err != nil {
		return "", ErrUnclassified
	}
	return expected, nil
}

func (c *Client) readCommit(ctx context.Context, current session, repo githubapp.Repository, sha string) (commitPayload, error) {
	if !validObjectSHA(sha) {
		return commitPayload{}, ErrUnclassified
	}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/git/commits/"+url.PathEscape(sha), nil, nil)
	if err != nil {
		return commitPayload{}, err
	}
	if response.status == http.StatusNotFound {
		return commitPayload{}, errNotFound
	}
	if response.status != http.StatusOK {
		return commitPayload{}, ErrUnclassified
	}
	var decoded commitPayload
	if decodeJSON(response.body, &decoded) != nil || decoded.SHA != sha || !validObjectSHA(decoded.Tree.SHA) {
		return commitPayload{}, ErrUnclassified
	}
	return decoded, nil
}

func (c *Client) readTree(ctx context.Context, current session, repo githubapp.Repository, sha string, files []publish.FileChange) error {
	if !validObjectSHA(sha) {
		return ErrUnclassified
	}
	query := url.Values{"recursive": []string{"1"}}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/git/trees/"+url.PathEscape(sha), query, nil)
	if err != nil {
		return err
	}
	if response.status == http.StatusNotFound {
		return errNotFound
	}
	if response.status != http.StatusOK {
		return ErrUnclassified
	}
	var decoded treePayload
	if decodeJSON(response.body, &decoded) != nil || decoded.SHA != sha || decoded.Truncated {
		return ErrUnclassified
	}
	observed := make(map[string]treeResponseItem, len(decoded.Tree))
	for _, entry := range decoded.Tree {
		if !validPath(entry.Path) || !validObjectSHA(entry.SHA) || (entry.Type != "blob" && entry.Type != "tree" && entry.Type != "commit") {
			return ErrUnclassified
		}
		if _, exists := observed[entry.Path]; exists {
			return ErrUnclassified
		}
		observed[entry.Path] = entry
	}
	for _, file := range files {
		entry, exists := observed[file.Path]
		if file.Delete {
			if exists {
				return ErrUnclassified
			}
			continue
		}
		if !exists || entry.Mode != file.Mode || entry.Type != "blob" || entry.SHA != gitBlobSHA(file.Content) {
			return ErrUnclassified
		}
	}
	return nil
}

func (c *Client) createTree(ctx context.Context, current session, repo githubapp.Repository, baseTree string, files []publish.FileChange, blobs map[string]string) (string, error) {
	entries := make([]treeRequestEntry, 0, len(files))
	for _, file := range files {
		entry := treeRequestEntry{Path: file.Path, Mode: file.Mode, Type: "blob"}
		if file.Delete {
			entry.SHA = nil
		} else {
			sha := blobs[file.Path]
			if !validObjectSHA(sha) {
				return "", ErrUnclassified
			}
			entry.SHA = &sha
		}
		entries = append(entries, entry)
	}
	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/git/trees", nil, map[string]any{
		"base_tree": baseTree,
		"tree":      entries,
	})
	if err != nil {
		return "", err
	}
	if response.status != http.StatusCreated {
		return "", ErrUnclassified
	}
	var created treePayload
	if decodeJSON(response.body, &created) != nil || !validObjectSHA(created.SHA) {
		return "", ErrUnclassified
	}
	return created.SHA, nil
}

func (c *Client) createCommit(ctx context.Context, current session, repo githubapp.Repository, request publish.CommitRequest, treeSHA string) (string, error) {
	payload := gitCommitRequest{
		Message:   request.Message,
		Tree:      treeSHA,
		Parents:   []string{request.BaseSHA},
		Author:    gitSignature{Name: agwBotName, Email: agwBotEmail},
		Committer: gitSignature{Name: agwBotName, Email: agwBotEmail},
	}
	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/git/commits", nil, payload)
	if err != nil {
		return "", err
	}
	if response.status != http.StatusCreated {
		return "", ErrUnclassified
	}
	var created commitPayload
	if decodeJSON(response.body, &created) != nil || !validObjectSHA(created.SHA) || created.Tree.SHA != treeSHA || len(created.Parents) != 1 || created.Parents[0].SHA != request.BaseSHA || created.Message != request.Message || created.Author.Name != agwBotName || created.Author.Email != agwBotEmail {
		return "", ErrUnclassified
	}
	return created.SHA, nil
}

func (c *Client) updateBranch(ctx context.Context, current session, repo githubapp.Repository, branch, baseSHA, commitSHA string) error {
	fullRef := "refs/heads/" + branch
	apiRef := strings.Join(append([]string{"git", "refs", "heads"}, escapeParts(strings.Split(branch, "/"))...), "/")
	response, err := c.do(ctx, current, http.MethodPatch, repoPath(repo)+"/"+apiRef, nil, map[string]any{
		"sha":   commitSHA,
		"force": false,
	})
	if err != nil {
		return c.reconcileUpdatedBranch(ctx, current, repo, fullRef, baseSHA, commitSHA)
	}
	if response.status != http.StatusOK {
		return c.reconcileUpdatedBranch(ctx, current, repo, fullRef, baseSHA, commitSHA)
	}
	var updated refPayload
	if decodeJSON(response.body, &updated) != nil || updated.Ref != fullRef || updated.Object.Type != "commit" || updated.Object.SHA != commitSHA {
		return ErrUnclassified
	}
	return c.reconcileUpdatedBranch(ctx, current, repo, fullRef, baseSHA, commitSHA)
}

func (c *Client) reconcileUpdatedBranch(ctx context.Context, current session, repo githubapp.Repository, fullRef, baseSHA, commitSHA string) error {
	for attempt, delay := range branchProofDelays {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ErrUnclassified
			case <-timer.C:
			}
		}
		observed, err := c.readRef(ctx, current, repo, fullRef)
		if err == nil {
			switch observed.SHA {
			case commitSHA:
				return nil
			case baseSHA:
				// A successful GitHub ref update can be briefly hidden by a
				// stale read. Continue with proof-only retries.
			default:
				return publish.ErrGitHubConflict
			}
		} else if !errors.Is(err, errNotFound) {
			return ErrUnclassified
		}
		if attempt == len(branchProofDelays)-1 {
			return ErrUnclassified
		}
	}
	return ErrUnclassified
}

// EnsureCommit turns the verified manifest into Git blobs, a tree, a commit,
// and a non-forced branch update. Every written object is read back before the
// method returns success.
func (c *Client) EnsureCommit(ctx context.Context, repo githubapp.Repository, request publish.CommitRequest) (publish.CommitResult, error) {
	if err := validateCommitRequest(request); err != nil {
		return publish.CommitResult{}, err
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.CommitResult{}, err
	}
	branchRef := "refs/heads/" + request.BranchName
	branch, err := c.readRef(ctx, current, repo, branchRef)
	if errors.Is(err, errNotFound) {
		return publish.CommitResult{}, publish.ErrGitHubNotFound
	}
	if err != nil {
		return publish.CommitResult{}, err
	}
	if branch.SHA != request.BaseSHA {
		return publish.CommitResult{}, publish.ErrGitHubConflict
	}

	baseCommit, err := c.readCommit(ctx, current, repo, request.BaseSHA)
	if errors.Is(err, errNotFound) {
		return publish.CommitResult{}, publish.ErrGitHubNotFound
	}
	if err != nil {
		return publish.CommitResult{}, err
	}
	files := append([]publish.FileChange(nil), request.Patch.Files...)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	blobs := make(map[string]string, len(files))
	for _, file := range files {
		if file.Delete {
			continue
		}
		sha, err := c.ensureBlob(ctx, current, repo, file.Content)
		if err != nil {
			return publish.CommitResult{}, err
		}
		blobs[file.Path] = sha
	}
	treeSHA, err := c.createTree(ctx, current, repo, baseCommit.Tree.SHA, files, blobs)
	if err != nil {
		return publish.CommitResult{}, err
	}
	if err := c.readTree(ctx, current, repo, treeSHA, files); err != nil {
		return publish.CommitResult{}, ErrUnclassified
	}
	commitSHA, err := c.createCommit(ctx, current, repo, request, treeSHA)
	if err != nil {
		return publish.CommitResult{}, err
	}
	committed, err := c.readCommit(ctx, current, repo, commitSHA)
	if err != nil {
		return publish.CommitResult{}, ErrUnclassified
	}
	if !proveCommit(committed, request, treeSHA) {
		return publish.CommitResult{}, ErrUnclassified
	}
	if err := c.readTree(ctx, current, repo, committed.Tree.SHA, files); err != nil {
		return publish.CommitResult{}, ErrUnclassified
	}
	if err := c.updateBranch(ctx, current, repo, request.BranchName, request.BaseSHA, commitSHA); err != nil {
		return publish.CommitResult{}, err
	}
	return publish.CommitResult{
		SHA:            commitSHA,
		BranchName:     request.BranchName,
		BaseSHA:        request.BaseSHA,
		PatchDigest:    request.PatchDigest,
		ManifestDigest: request.ManifestDigest,
		Author:         request.Author,
	}, nil
}

func proveCommit(commit commitPayload, request publish.CommitRequest, treeSHA string) bool {
	return commit.Tree.SHA == treeSHA && len(commit.Parents) == 1 && commit.Parents[0].SHA == request.BaseSHA && commit.Message == request.Message && commit.Author.Name == agwBotName && commit.Author.Email == agwBotEmail
}

func validateCommitRequest(request publish.CommitRequest) error {
	if !validBranchName(request.BranchName) || !validObjectSHA(request.BaseSHA) || !validDigest(request.EffectKey) || len(request.Patch.Raw) == 0 || len(request.Patch.Raw) > publish.MaxPatchBytes || !validDigest(request.Patch.Digest) || !validDigest(request.Patch.ManifestDigest) || !validDigest(request.PatchDigest) || request.PatchDigest != publish.DigestForPatchBytes(request.Patch.Raw) || request.Patch.Digest != request.PatchDigest || request.ManifestDigest != request.Patch.ManifestDigest {
		return publish.ErrInvalidRequest
	}
	manifestDigest, err := publish.DigestForPatch(request.Patch.Files)
	if err != nil || manifestDigest != request.ManifestDigest {
		return publish.ErrInvalidRequest
	}
	if request.Author.Name != agwBotName || request.Author.Email != agwBotEmail || !validMessage(request.Message) {
		return publish.ErrInvalidRequest
	}
	if err := validateFiles(request.Patch.Files); err != nil {
		return err
	}
	return nil
}

func validateFiles(files []publish.FileChange) error {
	if len(files) == 0 || len(files) > publish.MaxPatchFiles {
		return publish.ErrInvalidRequest
	}
	copyFiles := append([]publish.FileChange(nil), files...)
	sort.Slice(copyFiles, func(left, right int) bool { return copyFiles[left].Path < copyFiles[right].Path })
	var total int
	for index, file := range copyFiles {
		if !validPath(file.Path) || len(file.Path) > publish.MaxPatchPathBytes || !validMode(file.Mode) {
			return publish.ErrInvalidRequest
		}
		if index > 0 && copyFiles[index-1].Path == file.Path {
			return publish.ErrInvalidRequest
		}
		if file.Delete {
			if file.Content != nil {
				return publish.ErrInvalidRequest
			}
			continue
		}
		if file.Content == nil || !validText(file.Content) {
			return publish.ErrInvalidRequest
		}
		total += len(file.Content)
		if total > publish.MaxPatchBytes {
			return publish.ErrInvalidRequest
		}
	}
	return nil
}

func validText(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, character := range string(content) {
		if unicode.IsControl(character) && character != '\t' && character != '\n' && character != '\r' {
			return false
		}
	}
	return true
}

func validMessage(message string) bool {
	return len(message) > 0 && len(message) <= maxMessageBytes && utf8.ValidString(message) && !strings.ContainsAny(message, "\x00\r\n")
}

func gitBlobSHA(content []byte) string {
	header := []byte("blob " + strconv.Itoa(len(content)) + "\x00")
	hash := sha1.New()
	_, _ = hash.Write(header)
	_, _ = hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

func decodeBase64(value string) ([]byte, bool) {
	value = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, value)
	decoded, err := base64.StdEncoding.DecodeString(value)
	return decoded, err == nil
}

type pullRequestPayload struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// EnsurePullRequest opens or proves the one PR identified by the deterministic
// head/base/effect tuple. It never calls a merge endpoint.
func (c *Client) EnsurePullRequest(ctx context.Context, repo githubapp.Repository, request publish.PullRequestRequest) (publish.PullRequestResult, error) {
	if err := validatePullRequestRequest(request); err != nil {
		return publish.PullRequestResult{}, err
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return publish.PullRequestResult{}, err
	}
	existing, err := c.findPullRequest(ctx, current, repo, request)
	if err != nil {
		return publish.PullRequestResult{}, err
	}
	if existing.Number > 0 {
		return existing, nil
	}
	response, err := c.do(ctx, current, http.MethodPost, repoPath(repo)+"/pulls", nil, map[string]any{
		"title": request.Title,
		"head":  request.HeadBranch,
		"base":  request.BaseRef,
		"body":  request.Body,
	})
	if err != nil {
		reconciled, reconcileErr := c.findPullRequest(ctx, current, repo, request)
		if reconcileErr == nil && reconciled.Number > 0 {
			return reconciled, nil
		}
		if reconcileErr != nil && errors.Is(reconcileErr, publish.ErrGitHubConflict) {
			return publish.PullRequestResult{}, reconcileErr
		}
		return publish.PullRequestResult{}, ErrUnclassified
	}
	if response.status != http.StatusCreated {
		// A timeout or 5xx can happen after GitHub has created the PR. Read
		// the deterministic tuple before declaring the outcome unknown.
		reconciled, reconcileErr := c.findPullRequest(ctx, current, repo, request)
		if reconcileErr == nil && reconciled.Number > 0 {
			return reconciled, nil
		}
		if reconcileErr != nil && errors.Is(reconcileErr, publish.ErrGitHubConflict) {
			return publish.PullRequestResult{}, reconcileErr
		}
		return publish.PullRequestResult{}, ErrUnclassified
	}
	var created pullRequestPayload
	if decodeJSON(response.body, &created) != nil || !validPullPayload(created, request, repo) {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	verified, err := c.readPullRequest(ctx, current, repo, created.Number, request)
	if err != nil {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	return verified, nil
}

func (c *Client) findPullRequest(ctx context.Context, current session, repo githubapp.Repository, request publish.PullRequestRequest) (publish.PullRequestResult, error) {
	query := url.Values{
		"state":    []string{"all"},
		"head":     []string{repo.FullName() + ":" + request.HeadBranch},
		"base":     []string{request.BaseRef},
		"per_page": []string{"100"},
	}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/pulls", query, nil)
	if err != nil {
		return publish.PullRequestResult{}, err
	}
	if response.status == http.StatusNotFound {
		return publish.PullRequestResult{}, publish.ErrGitHubNotFound
	}
	if response.status != http.StatusOK {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	var candidates []pullRequestPayload
	if decodeJSON(response.body, &candidates) != nil || len(candidates) > 100 {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	for _, candidate := range candidates {
		if !validPullIdentity(candidate) {
			return publish.PullRequestResult{}, ErrUnclassified
		}
		if candidate.Head.Ref != request.HeadBranch || candidate.Base.Ref != request.BaseRef {
			continue
		}
		if !containsEffectMarker(candidate.Body, request.EffectKey) {
			return publish.PullRequestResult{}, publish.ErrGitHubConflict
		}
		return c.readPullRequest(ctx, current, repo, candidate.Number, request)
	}
	return publish.PullRequestResult{}, nil
}

func (c *Client) readPullRequest(ctx context.Context, current session, repo githubapp.Repository, number int64, request publish.PullRequestRequest) (publish.PullRequestResult, error) {
	if number <= 0 {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/pulls/"+strconv.FormatInt(number, 10), nil, nil)
	if err != nil {
		return publish.PullRequestResult{}, err
	}
	if response.status == http.StatusNotFound {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	if response.status != http.StatusOK {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	var decoded pullRequestPayload
	if decodeJSON(response.body, &decoded) != nil {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	if decoded.Number != number {
		return publish.PullRequestResult{}, ErrUnclassified
	}
	if !validPullPayload(decoded, request, repo) {
		return publish.PullRequestResult{}, publish.ErrGitHubConflict
	}
	return pullResult(decoded, request), nil
}

func validPullIdentity(value pullRequestPayload) bool {
	return value.Number > 0 && value.Title != "" && value.HTMLURL != "" && value.Head.Ref != "" && value.Base.Ref != ""
}

func validPullPayload(value pullRequestPayload, request publish.PullRequestRequest, repo githubapp.Repository) bool {
	return validPullIdentity(value) && value.Title == request.Title && value.Body == request.Body && value.Head.Ref == request.HeadBranch && value.Base.Ref == request.BaseRef && containsEffectMarker(value.Body, request.EffectKey) && validPullURL(value.HTMLURL, repo, value.Number)
}

func pullResult(value pullRequestPayload, request publish.PullRequestRequest) publish.PullRequestResult {
	return publish.PullRequestResult{
		Number:     value.Number,
		URL:        value.HTMLURL,
		BaseRef:    request.BaseRef,
		HeadBranch: request.HeadBranch,
		EffectKey:  request.EffectKey,
	}
}

func containsEffectMarker(body, effectKey string) bool {
	return strings.Contains(body, "| Effect key | `"+effectKey+"` |")
}

func validatePullRequestRequest(request publish.PullRequestRequest) error {
	if !validBaseRefName(request.BaseRef) || !validBranchName(request.HeadBranch) || !validDigest(request.EffectKey) || len(request.Title) == 0 || len(request.Title) > maxPullTitleBytes || !utf8.ValidString(request.Title) || strings.ContainsAny(request.Title, "\x00\r\n") || len(request.Body) == 0 || len(request.Body) > maxPullBodyBytes || !utf8.ValidString(request.Body) || strings.ContainsAny(request.Body, "\x00") || !containsEffectMarker(request.Body, request.EffectKey) {
		return publish.ErrInvalidRequest
	}
	return nil
}

func validPullURL(value string, repo githubapp.Repository, number int64) bool {
	if len(value) == 0 || len(value) > maxURLBytes || strings.ContainsAny(value, "\x00\r\n\t @?#") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	wantPath := "/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.FormatInt(number, 10)
	return parsed.Path == wantPath
}

type issueLabel struct {
	Name string `json:"name"`
}

func (c *Client) readLabels(ctx context.Context, current session, repo githubapp.Repository, number int64) ([]string, error) {
	query := url.Values{"per_page": []string{"100"}}
	response, err := c.do(ctx, current, http.MethodGet, repoPath(repo)+"/issues/"+strconv.FormatInt(number, 10)+"/labels", query, nil)
	if err != nil {
		return nil, err
	}
	if response.status == http.StatusNotFound {
		return nil, publish.ErrGitHubNotFound
	}
	if response.status != http.StatusOK {
		return nil, ErrUnclassified
	}
	var decoded []issueLabel
	if decodeJSON(response.body, &decoded) != nil || len(decoded) > 100 {
		return nil, ErrUnclassified
	}
	labels := make([]string, 0, len(decoded))
	for _, label := range decoded {
		if !validLabel(label.Name) {
			return nil, ErrUnclassified
		}
		labels = append(labels, label.Name)
	}
	return normalizeLabels(labels), nil
}

// EnsureLabels replaces the PR's complete label set and reads it back. The
// PUT endpoint is intentional: POST would leave stale labels behind and
// would not prove the requested complete state.
func (c *Client) EnsureLabels(ctx context.Context, repo githubapp.Repository, request publish.LabelRequest) error {
	if request.Number <= 0 || len(request.Labels) > publish.MaxLabels {
		return publish.ErrInvalidRequest
	}
	want := normalizeLabels(request.Labels)
	for _, label := range want {
		if !validLabel(label) {
			return publish.ErrInvalidRequest
		}
	}
	current, err := c.session(ctx, repo)
	if err != nil {
		return err
	}
	observed, err := c.readLabels(ctx, current, repo, request.Number)
	if err != nil {
		return err
	}
	if equalStrings(observed, want) {
		return nil
	}
	response, err := c.do(ctx, current, http.MethodPut, repoPath(repo)+"/issues/"+strconv.FormatInt(request.Number, 10)+"/labels", nil, map[string]any{"labels": want})
	if err != nil {
		if reconciled, readErr := c.readLabels(ctx, current, repo, request.Number); readErr == nil && equalStrings(reconciled, want) {
			return nil
		}
		return ErrUnclassified
	}
	if response.status != http.StatusOK {
		if reconciled, readErr := c.readLabels(ctx, current, repo, request.Number); readErr == nil && equalStrings(reconciled, want) {
			return nil
		}
		return ErrUnclassified
	}
	var replaced []issueLabel
	if decodeJSON(response.body, &replaced) != nil || len(replaced) > 100 {
		return ErrUnclassified
	}
	returned := make([]string, 0, len(replaced))
	for _, label := range replaced {
		if !validLabel(label.Name) {
			return ErrUnclassified
		}
		returned = append(returned, label.Name)
	}
	if !equalStrings(normalizeLabels(returned), want) {
		return ErrUnclassified
	}
	verified, err := c.readLabels(ctx, current, repo, request.Number)
	if err != nil || !equalStrings(verified, want) {
		return ErrUnclassified
	}
	return nil
}

func normalizeLabels(labels []string) []string {
	copyLabels := append([]string(nil), labels...)
	sort.Strings(copyLabels)
	result := copyLabels[:0]
	for _, label := range copyLabels {
		if len(result) == 0 || result[len(result)-1] != label {
			result = append(result, label)
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validObjectSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return validLowerHex(value[len("sha256:"):])
}

func validLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validMode(value string) bool {
	// GitHub's 160000 submodule and 120000 symlink modes are deliberately
	// excluded. Only ordinary executable/non-executable blobs are admissible.
	return value == "100644" || value == "100755"
}

func validPath(value string) bool {
	if value == "" || len(value) > publish.MaxPatchPathBytes || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if character < 0x20 || character == 0x7f {
				return false
			}
		}
	}
	return true
}

func validFullRef(value string) bool {
	if len(value) == 0 || len(value) > maxRefBytes || !strings.HasPrefix(value, "refs/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\x5c\x00\r\n ~^:?*[") {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) < 3 || parts[0] != "refs" || (parts[1] != "heads" && parts[1] != "tags") {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "..") {
			return false
		}
	}
	return true
}

func validBranchName(value string) bool {
	if len(value) == 0 || len(value) > maxBranchBytes || !strings.HasPrefix(value, "agw/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\x5c\x00\r\n ~^:?*[") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "..") {
			return false
		}
	}
	return true
}

func validBaseRefName(value string) bool {
	if len(value) == 0 || len(value) > publish.MaxBaseRefBytes || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\x5c\x00\r\n ~^:?*[") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "..") {
			return false
		}
	}
	return true
}

func validLabel(value string) bool {
	return len(value) > 0 && len(value) <= publish.MaxLabelBytes && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
