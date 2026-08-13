package publish_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/githubpublish"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

// TestGitHubPublishLive is an opt-in, end-to-end publication probe. It creates
// one deterministic test branch, commit, and pull request through the real
// publisher, verifies durable replay through a new ledger instance, and then
// closes/deletes only the exact PR and branch it created.
//
// No test runs this by default: it is an external repository mutation.
func TestGitHubPublishLive(t *testing.T) {
	if os.Getenv("AGW_GITHUB_PUBLISH_LIVE") != "1" {
		t.Skip("set AGW_GITHUB_PUBLISH_LIVE=1 to exercise the real GitHub publication path")
	}

	appID := positiveEnvInt64(t, "AGW_GITHUB_APP_ID")
	installationID := positiveEnvInt64(t, "AGW_GITHUB_INSTALLATION_ID")
	privateKeyPath := requiredEnv(t, "AGW_GITHUB_PRIVATE_KEY_FILE")
	repository := parseRepository(t, requiredEnv(t, "AGW_GITHUB_REPOSITORY"))
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		t.Fatalf("read GitHub App private key: %v", err)
	}
	minter, err := githubapp.New(githubapp.Config{
		AppID:          appID,
		InstallationID: installationID,
		PrivateKeyPEM:  privateKey,
	})
	if err != nil {
		t.Fatalf("construct GitHub App minter: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	baseRef, err := liveDefaultBranch(ctx, minter, repository)
	if err != nil {
		t.Fatalf("resolve default branch: %v", err)
	}
	cloneToken, err := minter.Mint(ctx, repository, githubapp.PermissionCloneRead)
	if err != nil {
		t.Fatalf("mint clone token: %v", err)
	}
	baseSHA, err := minter.ResolveCommit(ctx, cloneToken, repository, baseRef)
	if err != nil {
		t.Fatalf("resolve base commit: %v", err)
	}

	suffix := randomHex(t)
	runUID := "11111111-2222-4333-8444-" + suffix[:12]
	runName := "github-publish-live-" + suffix[:12]
	path := ".agw-live-test-" + suffix[:16] + ".md"
	content := []byte("# Agents Gateway live publication test\n\nThis file is removed with the temporary test branch.\n")
	rawPatch := []byte("diff --git a/" + path + " b/" + path + "\nnew file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1,3 @@\n+# Agents Gateway live publication test\n+\n+This file is removed with the temporary test branch.\n")
	files := []publish.FileChange{{Path: path, Mode: "100644", Content: content}}
	manifestDigest, err := publish.DigestForPatch(files)
	if err != nil {
		t.Fatalf("digest patch manifest: %v", err)
	}
	request := publish.Request{
		RunUID:  runUID,
		RunName: runName,
		Repo:    repository,
		BaseRef: baseRef,
		BaseSHA: baseSHA,
		Patch: publish.Patch{
			Digest:         publish.DigestForPatchBytes(rawPatch),
			Raw:            rawPatch,
			ManifestDigest: manifestDigest,
			Files:          files,
		},
		SpecDigest: sha256Digest("live-spec-" + suffix),
		Gate: publish.GateState{
			Mode:         v1alpha1.GateShadow,
			Verdict:      "Accepted",
			ReportDigest: sha256Digest("live-report-" + suffix),
		},
		RuntimeImageDigest:  "ghcr.io/astatide/agw-runtime@sha256:" + strings.Repeat("1", 64),
		VerifierImageDigest: "ghcr.io/astatide/agw-verifier@sha256:" + strings.Repeat("2", 64),
		PublishMode:         v1alpha1.PublishPullRequest,
		Title:               "AGW live publication probe " + suffix[:8],
		Labels:              []string{"agw/live-test"},
	}

	trace := &liveHTTPTrace{}
	client, err := githubpublish.New(githubpublish.Config{
		Minter:         minter,
		HTTPClient:     &http.Client{Transport: trace},
		RequestTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("construct GitHub publisher client: %v", err)
	}
	store := &liveMemoryStore{objects: make(map[string][]byte)}
	ledger, err := effects.New(store, "live-publish-effects", nil)
	if err != nil {
		t.Fatalf("construct effect ledger: %v", err)
	}
	publisher, err := publish.New(client, ledger)
	if err != nil {
		t.Fatalf("construct publisher: %v", err)
	}

	var first publish.Result
	defer func() {
		if first.BranchName == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if err := cleanupLivePullRequest(cleanupCtx, minter, repository, first.PullRequestNumber, first.BranchName, baseRef); err != nil {
			t.Errorf("cleanup live publication PR #%d / branch %q: %v", first.PullRequestNumber, first.BranchName, err)
		}
	}()

	first, err = publisher.Publish(ctx, request)
	if err != nil || first.State != publish.StateSucceeded {
		t.Logf("publisher API trace: %s", trace.String())
		t.Fatalf("first live publication = %#v, %v", first, err)
	}
	if first.PullRequestURL == "" || first.PullRequestNumber <= 0 || first.BranchName == "" || first.CommitSHA == "" {
		t.Fatalf("first publication returned incomplete proof: %#v", first)
	}

	// A new ledger and Publisher model a controller restart. The second call
	// must replay the recorded outcome and perform no GitHub mutation.
	restartedLedger, err := effects.New(store, "live-publish-effects", nil)
	if err != nil {
		t.Fatalf("construct restarted effect ledger: %v", err)
	}
	restartedPublisher, err := publish.New(client, restartedLedger)
	if err != nil {
		t.Fatalf("construct restarted publisher: %v", err)
	}
	second, err := restartedPublisher.Publish(ctx, request)
	if err != nil || !samePublishResult(first, second) {
		t.Fatalf("restart replay = %#v, %v; first = %#v", second, err, first)
	}
	t.Logf("live GitHub publication succeeded for %s: PR #%d (%s), commit %s; restart replay matched", repository.FullName(), first.PullRequestNumber, first.PullRequestURL, first.CommitSHA)
}

// TestGitHubPublishInspectLive is read-only diagnostics for an interrupted
// publication probe. It is intentionally separate from the mutating test so a
// failed probe can be reconciled before another mutation is attempted.
func TestGitHubPublishInspectLive(t *testing.T) {
	branch := strings.TrimSpace(os.Getenv("AGW_GITHUB_INSPECT_BRANCH"))
	if branch == "" {
		t.Skip("set AGW_GITHUB_INSPECT_BRANCH to inspect one live publication branch")
	}
	appID := positiveEnvInt64(t, "AGW_GITHUB_APP_ID")
	installationID := positiveEnvInt64(t, "AGW_GITHUB_INSTALLATION_ID")
	privateKey, err := os.ReadFile(requiredEnv(t, "AGW_GITHUB_PRIVATE_KEY_FILE"))
	if err != nil {
		t.Fatalf("read GitHub App private key: %v", err)
	}
	repository := parseRepository(t, requiredEnv(t, "AGW_GITHUB_REPOSITORY"))
	minter, err := githubapp.New(githubapp.Config{AppID: appID, InstallationID: installationID, PrivateKeyPEM: privateKey})
	if err != nil {
		t.Fatalf("construct GitHub App minter: %v", err)
	}
	client, err := githubpublish.New(githubpublish.Config{Minter: minter, RequestTimeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("construct GitHub publisher client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ref, err := client.GetRef(ctx, repository, "refs/heads/"+branch)
	if err != nil {
		t.Logf("live branch %q is not readable through publisher (%T)", branch, err)
	} else {
		t.Logf("live branch %q exists at %s", branch, ref.SHA)
	}
	prs, err := listLivePullRequests(ctx, minter, repository, branch)
	if err != nil {
		t.Fatalf("inspect pull requests for branch %q: %v", branch, err)
	}
	for _, pr := range prs {
		t.Logf("live PR #%d state=%s head=%s base=%s url=%s", pr.Number, pr.State, pr.Head.Ref, pr.Base.Ref, pr.HTMLURL)
	}
	if len(prs) == 0 {
		t.Log("no pull request found for the inspected branch")
	}
	if os.Getenv("AGW_GITHUB_INSPECT_CLEANUP") == "1" {
		baseRef, err := liveDefaultBranch(ctx, minter, repository)
		if err != nil {
			t.Fatalf("resolve cleanup base branch: %v", err)
		}
		if err := cleanupLivePullRequest(ctx, minter, repository, 0, branch, baseRef); err != nil {
			t.Fatalf("cleanup inspected branch: %v", err)
		}
		t.Logf("cleaned up inspected branch %q", branch)
	}
}

type livePullRequest struct {
	Number  int64  `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type liveHTTPTrace struct {
	mu      sync.Mutex
	entries []string
}

func (t *liveHTTPTrace) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	entry := request.Method + " " + request.URL.Path
	if err != nil {
		entry += " -> transport-error"
	} else {
		entry += " -> " + strconv.Itoa(response.StatusCode)
	}
	t.mu.Lock()
	t.entries = append(t.entries, entry)
	t.mu.Unlock()
	return response, err
}

func (t *liveHTTPTrace) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(append([]string(nil), t.entries...), ", ")
}

func listLivePullRequests(ctx context.Context, minter *githubapp.Minter, repo githubapp.Repository, branch string) ([]livePullRequest, error) {
	token, err := minter.Mint(ctx, repo, githubapp.PermissionPublish)
	if err != nil {
		return nil, errors.New("mint inspection token")
	}
	query := url.Values{"state": []string{"all"}, "per_page": []string{"100"}}
	endpoint := githubapp.DefaultAPIBaseURL + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("build inspection request")
	}
	setGitHubHeaders(request, token.Value())
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return nil, errors.New("inspect pull requests")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("inspect pull requests returned unexpected status")
	}
	var result []livePullRequest
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, errors.New("decode inspected pull requests")
	}
	return result, nil
}

type liveMemoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *liveMemoryStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, nil
}

func (s *liveMemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, effects.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func liveDefaultBranch(ctx context.Context, minter *githubapp.Minter, repo githubapp.Repository) (string, error) {
	token, err := minter.Mint(ctx, repo, githubapp.PermissionCloneRead)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, githubapp.DefaultAPIBaseURL+"/repos/"+url.PathEscape(repo.Owner)+"/"+url.PathEscape(repo.Name), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token.Value())
	request.Header.Set("User-Agent", "agents-gateway-v3-live-test")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("repository metadata status %d", response.StatusCode)
	}
	var payload struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.DefaultBranch == "" || strings.ContainsAny(payload.DefaultBranch, "/\\\x00\r\n?#") {
		return "", errors.New("invalid default branch")
	}
	return payload.DefaultBranch, nil
}

func cleanupLivePullRequest(ctx context.Context, minter *githubapp.Minter, repo githubapp.Repository, number int64, branch, baseRef string) error {
	token, err := minter.Mint(ctx, repo, githubapp.PermissionPublish)
	if err != nil {
		return errors.New("mint cleanup token")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	baseURL := githubapp.DefaultAPIBaseURL + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name)
	prs, err := listLivePullRequests(ctx, minter, repo, branch)
	if err != nil {
		return errors.New("inspect cleanup pull requests")
	}
	matching := make([]livePullRequest, 0, len(prs))
	if number > 0 {
		pr, err := getLivePullRequest(ctx, minter, repo, number)
		if err != nil {
			return errors.New("read cleanup pull request")
		}
		if pr.Head.Ref != branch || pr.Base.Ref != baseRef {
			return errors.New("cleanup pull request identity did not match")
		}
		matching = append(matching, pr)
	} else {
		for _, pr := range prs {
			if pr.Head.Ref == branch && pr.Base.Ref == baseRef {
				matching = append(matching, pr)
			}
		}
	}
	if number > 0 {
		if len(matching) != 1 || matching[0].Number != number {
			return errors.New("cleanup pull request identity did not match")
		}
	}
	if len(matching) > 1 {
		return errors.New("multiple cleanup pull requests matched")
	}
	if len(matching) == 1 && matching[0].State != "closed" {
		getURL := baseURL + "/pulls/" + strconv.FormatInt(matching[0].Number, 10)
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, getURL, bytes.NewReader([]byte(`{"state":"closed"}`)))
		if err != nil {
			return errors.New("build close request")
		}
		setGitHubHeaders(request, token.Value())
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return errors.New("close cleanup pull request")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return errors.New("close cleanup pull request returned unexpected status")
		}
	}

	deleteURL := baseURL + "/git/refs/heads/" + url.PathEscape(branch)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return errors.New("build branch cleanup request")
	}
	setGitHubHeaders(request, token.Value())
	response, err := client.Do(request)
	if err != nil {
		return errors.New("delete cleanup branch")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return errors.New("delete cleanup branch returned unexpected status")
	}
	return nil
}

func getLivePullRequest(ctx context.Context, minter *githubapp.Minter, repo githubapp.Repository, number int64) (livePullRequest, error) {
	token, err := minter.Mint(ctx, repo, githubapp.PermissionPublish)
	if err != nil {
		return livePullRequest{}, errors.New("mint pull request token")
	}
	endpoint := githubapp.DefaultAPIBaseURL + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls/" + strconv.FormatInt(number, 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return livePullRequest{}, errors.New("build pull request request")
	}
	setGitHubHeaders(request, token.Value())
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return livePullRequest{}, errors.New("read pull request")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return livePullRequest{}, errors.New("pull request returned unexpected status")
	}
	var result livePullRequest
	if err := json.Unmarshal(body, &result); err != nil || result.Number != number {
		return livePullRequest{}, errors.New("decode pull request")
	}
	return result, nil
}

func setGitHubHeaders(request *http.Request, token string) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", "agents-gateway-v3-live-test")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func parseRepository(t *testing.T, value string) githubapp.Repository {
	t.Helper()
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		t.Fatal("AGW_GITHUB_REPOSITORY must be owner/name")
	}
	repo := githubapp.Repository{Owner: parts[0], Name: parts[1]}
	if repo.Validate() != nil {
		t.Fatal("AGW_GITHUB_REPOSITORY is invalid")
	}
	return repo
}

func positiveEnvInt64(t *testing.T, name string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(requiredEnv(t, name), 10, 64)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer", name)
	}
	return value
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}

func randomHex(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generate live test identity: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

func sha256Digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func samePublishResult(left, right publish.Result) bool {
	return left.State == right.State && left.EffectKey == right.EffectKey && left.RequestDigest == right.RequestDigest && left.BranchName == right.BranchName && left.CommitSHA == right.CommitSHA && left.PullRequestNumber == right.PullRequestNumber && left.PullRequestURL == right.PullRequestURL
}
