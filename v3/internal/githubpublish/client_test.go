package githubpublish

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

const (
	testOwner          = "acme"
	testRepositoryName = "demo"
	testBaseSHA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseTreeSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testTreeSHA        = "cccccccccccccccccccccccccccccccccccccccc"
	testCommitSHA      = "dddddddddddddddddddddddddddddddddddddddd"
	testPRNumber       = int64(42)
	testBranch         = "agw/run-123"
	testEffect         = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testToken          = "ghs_test_token_must_not_escape"
	testSecretBody     = "response body contains ghp_body_secret"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
	testKeyErr  error
)

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	testKeyOnce.Do(func() {
		testKey, testKeyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if testKeyErr != nil {
		t.Fatal(testKeyErr)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
}

func testNow() time.Time {
	return time.Date(2026, time.August, 11, 15, 4, 5, 0, time.UTC)
}

type fakeGitHub struct {
	mu sync.Mutex

	branchExists bool
	branchSHA    string
	blobExists   bool
	blobSHA      string
	blobContent  []byte
	prExists     bool
	prBody       string
	prTitle      string
	labels       []string

	createRefStatus             int
	getRefStatus                int
	createRefBody               any
	malformedRef                bool
	malformedGetRef             bool
	delayCreateRef              bool
	branchNotFoundAfterCreate   int
	branchStaleReadsAfterUpdate int
	apiErrorBody                string

	commitRequest map[string]any
	treeRequest   map[string]any
	pullRequest   map[string]any
	paths         []string
	methods       []string
	scopes        []scopeRequest
}

type scopeRequest struct {
	Repo        string
	Permissions map[string]string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		branchSHA:    testBaseSHA,
		getRefStatus: http.StatusOK,
		labels:       nil,
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.methods = append(f.methods, r.Method)
	f.mu.Unlock()

	if r.URL.Path == "/app/installations/2/access_tokens" && r.Method == http.MethodPost {
		f.serveToken(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/repos/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	if parts[0] != testOwner || parts[1] != testRepositoryName {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	relative := strings.Join(parts[2:], "/")
	f.handleRepository(w, r, relative)
}

func (f *fakeGitHub) serveToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Repositories) != 1 {
		http.Error(w, "bad token request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.scopes = append(f.scopes, scopeRequest{Repo: request.Repositories[0], Permissions: request.Permissions})
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":                testToken,
		"expires_at":           testNow().Add(time.Hour).Format(time.RFC3339),
		"permissions":          map[string]string{"contents": "write", "pull_requests": "write"},
		"repository_selection": "selected",
		// The request accepts repository names; GitHub returns canonical
		// owner/name identities in the response.
		"repositories": []map[string]string{{"full_name": testOwner + "/" + request.Repositories[0]}},
	})
}

func (f *fakeGitHub) handleRepository(w http.ResponseWriter, r *http.Request, relative string) {
	f.mu.Lock()
	getRefStatus := f.getRefStatus
	createRefStatus := f.createRefStatus
	malformedRef := f.malformedRef
	malformedGetRef := f.malformedGetRef
	delayCreateRef := f.delayCreateRef
	apiErrorBody := f.apiErrorBody
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(relative, "git/ref/"):
		if getRefStatus != http.StatusOK {
			http.Error(w, apiErrorBody, getRefStatus)
			return
		}
		ref := strings.TrimPrefix(relative, "git/ref/")
		if ref == "heads/main" {
			if malformedGetRef {
				writeJSON(w, http.StatusOK, map[string]string{"invalid": "body"})
				return
			}
			writeJSON(w, http.StatusOK, refJSON("refs/heads/main", testBaseSHA))
			return
		}
		if ref == "heads/"+testBranch {
			f.mu.Lock()
			exists, sha := f.branchExists, f.branchSHA
			if exists && sha != testBaseSHA && f.branchStaleReadsAfterUpdate > 0 {
				f.branchStaleReadsAfterUpdate--
				sha = testBaseSHA
			}
			if exists && f.branchNotFoundAfterCreate > 0 {
				f.branchNotFoundAfterCreate--
				exists = false
			}
			f.mu.Unlock()
			if !exists {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, http.StatusOK, refJSON("refs/heads/"+testBranch, sha))
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && relative == "git/refs":
		if delayCreateRef {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		if createRefStatus != 0 && createRefStatus != http.StatusCreated {
			http.Error(w, apiErrorBody, createRefStatus)
			return
		}
		var request struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad ref", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.branchExists = true
		f.branchSHA = request.SHA
		f.mu.Unlock()
		if malformedRef {
			writeJSON(w, http.StatusCreated, map[string]string{"malformed": "body"})
			return
		}
		writeJSON(w, http.StatusCreated, refJSON(request.Ref, request.SHA))
	case r.Method == http.MethodPatch && strings.HasPrefix(relative, "git/refs/heads/"):
		var request struct {
			SHA   string `json:"sha"`
			Force bool   `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Force {
			http.Error(w, "bad update", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.branchSHA = request.SHA
		f.branchExists = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, refJSON("refs/heads/"+testBranch, request.SHA))
	case r.Method == http.MethodGet && strings.HasPrefix(relative, "git/blobs/"):
		sha := strings.TrimPrefix(relative, "git/blobs/")
		f.mu.Lock()
		exists, wantSHA, content := f.blobExists, f.blobSHA, append([]byte(nil), f.blobContent...)
		f.mu.Unlock()
		if !exists || sha != wantSHA {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sha":      wantSHA,
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(content),
		})
	case r.Method == http.MethodPost && relative == "git/blobs":
		var request struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Encoding != "base64" {
			http.Error(w, "bad blob", http.StatusBadRequest)
			return
		}
		content, err := base64.StdEncoding.DecodeString(request.Content)
		if err != nil {
			http.Error(w, "bad blob", http.StatusBadRequest)
			return
		}
		sha := gitBlobSHA(content)
		f.mu.Lock()
		f.blobExists, f.blobSHA, f.blobContent = true, sha, append([]byte(nil), content...)
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]string{"sha": sha})
	case r.Method == http.MethodGet && strings.HasPrefix(relative, "git/commits/"):
		sha := strings.TrimPrefix(relative, "git/commits/")
		if sha == testBaseSHA {
			writeJSON(w, http.StatusOK, map[string]any{"sha": testBaseSHA, "tree": map[string]string{"sha": testBaseTreeSHA}})
			return
		}
		if sha == testCommitSHA {
			writeJSON(w, http.StatusOK, commitJSON(testCommitSHA, testTreeSHA, testBaseSHA, "agw: run", agwBotName, agwBotEmail))
			return
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && relative == "git/trees":
		if err := json.NewDecoder(r.Body).Decode(&f.treeRequest); err != nil {
			http.Error(w, "bad tree", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"sha": testTreeSHA})
	case r.Method == http.MethodGet && strings.HasPrefix(relative, "git/trees/"):
		sha := strings.TrimPrefix(relative, "git/trees/")
		if sha != testTreeSHA {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		blobSHA := f.blobSHA
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"sha":       testTreeSHA,
			"truncated": false,
			"tree":      []map[string]string{{"path": "src/main.go", "mode": "100644", "type": "blob", "sha": blobSHA}},
		})
	case r.Method == http.MethodPost && relative == "git/commits":
		if err := json.NewDecoder(r.Body).Decode(&f.commitRequest); err != nil {
			http.Error(w, "bad commit", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusCreated, commitJSON(testCommitSHA, testTreeSHA, testBaseSHA, "agw: run", agwBotName, agwBotEmail))
	case r.Method == http.MethodGet && relative == "pulls":
		f.mu.Lock()
		exists, body, title := f.prExists, f.prBody, f.prTitle
		f.mu.Unlock()
		if !exists {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		writeJSON(w, http.StatusOK, []any{pullJSON(body, title)})
	case r.Method == http.MethodPost && relative == "pulls":
		if err := json.NewDecoder(r.Body).Decode(&f.pullRequest); err != nil {
			http.Error(w, "bad pull", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.prExists = true
		f.prBody, _ = f.pullRequest["body"].(string)
		f.prTitle, _ = f.pullRequest["title"].(string)
		body, title := f.prBody, f.prTitle
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, pullJSON(body, title))
	case r.Method == http.MethodGet && relative == "pulls/42":
		f.mu.Lock()
		body, title := f.prBody, f.prTitle
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, pullJSON(body, title))
	case r.Method == http.MethodGet && relative == "issues/42/labels":
		f.mu.Lock()
		labels := append([]string(nil), f.labels...)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, labelJSON(labels))
	case r.Method == http.MethodPut && relative == "issues/42/labels":
		var request struct {
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad labels", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.labels = append([]string(nil), request.Labels...)
		labels := append([]string(nil), f.labels...)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, labelJSON(labels))
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func refJSON(ref, sha string) map[string]any {
	return map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "commit"}}
}

func commitJSON(sha, tree, parent, message, author, email string) map[string]any {
	return map[string]any{
		"sha":     sha,
		"message": message,
		"author":  map[string]string{"name": author, "email": email},
		"tree":    map[string]string{"sha": tree},
		"parents": []map[string]string{{"sha": parent}},
	}
}

func pullJSON(body, title string) map[string]any {
	return map[string]any{
		"number":   testPRNumber,
		"title":    title,
		"body":     body,
		"html_url": "https://github.com/" + testOwner + "/" + testRepositoryName + "/pull/42",
		"head":     map[string]string{"ref": testBranch},
		"base":     map[string]string{"ref": "main"},
	}
}

func labelJSON(labels []string) []map[string]string {
	result := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		result = append(result, map[string]string{"name": label})
	}
	return result
}

func newTestClient(t *testing.T, server *httptest.Server, timeout time.Duration) *Client {
	t.Helper()
	minter, err := githubapp.NewForTest(githubapp.Config{
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPEM:  testPrivateKeyPEM(t),
		BaseURL:        server.URL,
		HTTPClient:     server.Client(),
		Clock:          testNow,
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Minter: minter, BaseURL: server.URL, HTTPClient: server.Client(), RequestTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testRepo() githubapp.Repository {
	return githubapp.Repository{Owner: testOwner, Name: testRepositoryName}
}

func testPatch(t *testing.T, content []byte) publish.Patch {
	t.Helper()
	files := []publish.FileChange{{Path: "src/main.go", Mode: "100644", Content: content}}
	manifest, err := publish.DigestForPatch(files)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("diff --git a/src/main.go b/src/main.go\n")
	return publish.Patch{Digest: publish.DigestForPatchBytes(raw), Raw: raw, ManifestDigest: manifest, Files: files}
}

func testCommitRequest(t *testing.T) publish.CommitRequest {
	t.Helper()
	patch := testPatch(t, []byte("package main\n"))
	return publish.CommitRequest{
		BranchName:     testBranch,
		BaseSHA:        testBaseSHA,
		Patch:          patch,
		PatchDigest:    patch.Digest,
		ManifestDigest: patch.ManifestDigest,
		Message:        "agw: run",
		Author:         publish.CommitAuthor{Name: agwBotName, Email: agwBotEmail},
		EffectKey:      testEffect,
	}
}

func testPullRequestRequest() publish.PullRequestRequest {
	body := "## Agents Gateway verification\n\n| Effect key | `" + testEffect + "` |\n"
	return publish.PullRequestRequest{BaseRef: "main", HeadBranch: testBranch, Title: "AGW: run", Body: body, EffectKey: testEffect}
}

func TestClientSuccessReplayAndExactScopes(t *testing.T) {
	fake := newFakeGitHub()
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	client := newTestClient(t, server, time.Second)
	repo := testRepo()

	ref, err := client.GetRef(context.Background(), repo, "refs/heads/main")
	if err != nil || ref.SHA != testBaseSHA {
		t.Fatalf("GetRef = %#v, %v", ref, err)
	}
	branch, err := client.EnsureBranch(context.Background(), repo, publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA})
	if err != nil || branch.SHA != testBaseSHA {
		t.Fatalf("EnsureBranch = %#v, %v", branch, err)
	}
	commit, err := client.EnsureCommit(context.Background(), repo, testCommitRequest(t))
	if err != nil || commit.SHA != testCommitSHA {
		t.Fatalf("EnsureCommit = %#v, %v", commit, err)
	}
	pull, err := client.EnsurePullRequest(context.Background(), repo, testPullRequestRequest())
	if err != nil || pull.Number != testPRNumber || pull.URL != "https://github.com/acme/demo/pull/42" {
		t.Fatalf("EnsurePullRequest = %#v, %v", pull, err)
	}
	labels := publish.LabelRequest{Number: testPRNumber, Labels: []string{"agw/shadow", "agw/accepted"}}
	if err := client.EnsureLabels(context.Background(), repo, labels); err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}

	fake.mu.Lock()
	pathsBeforeReplay := len(fake.paths)
	methodsBeforeReplay := len(fake.methods)
	fake.mu.Unlock()
	replayedPull, err := client.EnsurePullRequest(context.Background(), repo, testPullRequestRequest())
	if err != nil || replayedPull != pull {
		t.Fatalf("PR replay = %#v, %v; first=%#v", replayedPull, err, pull)
	}
	if err := client.EnsureLabels(context.Background(), repo, labels); err != nil {
		t.Fatalf("label replay: %v", err)
	}
	fake.mu.Lock()
	pathsAfterReplay := len(fake.paths)
	methodsAfterReplay := len(fake.methods)
	scopes := append([]scopeRequest(nil), fake.scopes...)
	commitRequest := fake.commitRequest
	treeRequest := fake.treeRequest
	for index, path := range fake.paths {
		if strings.Contains(path, "/merge") {
			t.Fatalf("merge endpoint was called at index %d: %q", index, path)
		}
	}
	fake.mu.Unlock()
	if pathsAfterReplay <= pathsBeforeReplay || methodsAfterReplay <= methodsBeforeReplay {
		t.Fatal("replay did not perform proof reads")
	}
	if len(scopes) == 0 {
		t.Fatal("no installation-token scope requests recorded")
	}
	for _, scope := range scopes {
		if scope.Repo != "demo" || scope.Permissions["contents"] != "write" || scope.Permissions["pull_requests"] != "write" {
			t.Fatalf("token scope was broader or incomplete: %#v", scope)
		}
	}
	if commitRequest["tree"] != testTreeSHA || fmt.Sprint(commitRequest["parents"]) != "["+testBaseSHA+"]" {
		t.Fatalf("commit request was not bound to base/tree: %#v", commitRequest)
	}
	if treeRequest["base_tree"] != testBaseTreeSHA {
		t.Fatalf("tree request base_tree=%v, want %s", treeRequest["base_tree"], testBaseTreeSHA)
	}
}

func TestEnsureBranchConflictAndMalformedSuccessAreNotRetried(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		fake := newFakeGitHub()
		fake.branchExists = true
		fake.branchSHA = strings.Repeat("e", 40)
		server := httptest.NewTLSServer(fake)
		defer server.Close()
		client := newTestClient(t, server, time.Second)
		_, err := client.EnsureBranch(context.Background(), testRepo(), publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA})
		if !errors.Is(err, publish.ErrGitHubConflict) {
			t.Fatalf("conflict error = %v", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, path := range fake.paths {
			if path == "/repos/acme/demo/git/refs" {
				t.Fatal("conflicting branch attempted a create")
			}
		}
	})

	t.Run("malformed 2xx", func(t *testing.T) {
		fake := newFakeGitHub()
		fake.malformedRef = true
		server := httptest.NewTLSServer(fake)
		defer server.Close()
		client := newTestClient(t, server, time.Second)
		_, err := client.EnsureBranch(context.Background(), testRepo(), publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA})
		if !errors.Is(err, ErrUnclassified) || errors.Is(err, publish.ErrGitHubConflict) {
			t.Fatalf("malformed success error = %v", err)
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testSecretBody) {
			t.Fatal("malformed response details escaped in error")
		}
	})
}

func TestEnsureBranchRetriesEventualReadAfterSuccessfulCreate(t *testing.T) {
	fake := newFakeGitHub()
	fake.branchNotFoundAfterCreate = 1
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	client := newTestClient(t, server, time.Second)

	branch, err := client.EnsureBranch(context.Background(), testRepo(), publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA})
	if err != nil || branch.Name != testBranch || branch.SHA != testBaseSHA {
		t.Fatalf("eventual branch proof = %#v, %v", branch, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	createCount := 0
	for _, path := range fake.paths {
		if path == "/repos/acme/demo/git/refs" {
			createCount++
		}
	}
	if createCount != 1 {
		t.Fatalf("branch create count = %d, want exactly one mutation", createCount)
	}
}

func TestEnsureCommitRetriesStaleReadAfterBranchUpdate(t *testing.T) {
	fake := newFakeGitHub()
	fake.branchStaleReadsAfterUpdate = 1
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	client := newTestClient(t, server, time.Second)

	if _, err := client.EnsureBranch(context.Background(), testRepo(), publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA}); err != nil {
		t.Fatalf("EnsureBranch = %v", err)
	}
	commit, err := client.EnsureCommit(context.Background(), testRepo(), testCommitRequest(t))
	if err != nil || commit.SHA != testCommitSHA {
		t.Fatalf("stale branch proof commit = %#v, %v", commit, err)
	}
}

func TestEnsurePullRequestConflictAndMalformedResponse(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		fake := newFakeGitHub()
		fake.prExists = true
		fake.prTitle = "other"
		fake.prBody = "same head, different effect"
		server := httptest.NewTLSServer(fake)
		defer server.Close()
		client := newTestClient(t, server, time.Second)
		_, err := client.EnsurePullRequest(context.Background(), testRepo(), testPullRequestRequest())
		if !errors.Is(err, publish.ErrGitHubConflict) {
			t.Fatalf("PR conflict = %v", err)
		}
	})

	t.Run("malformed 2xx", func(t *testing.T) {
		fake := newFakeGitHub()
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/app/installations/2/access_tokens" {
				fake.serveToken(w, r)
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/demo/pulls" {
				writeJSON(w, http.StatusOK, []any{})
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/repos/acme/demo/pulls" {
				writeJSON(w, http.StatusCreated, map[string]string{"not": "a pull"})
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		client := newTestClient(t, server, time.Second)
		_, err := client.EnsurePullRequest(context.Background(), testRepo(), testPullRequestRequest())
		if !errors.Is(err, ErrUnclassified) || strings.Contains(err.Error(), testToken) {
			t.Fatalf("malformed PR response = %v", err)
		}
	})
}

func TestServerErrorRemainsAmbiguousAndTimeoutIsReconciled(t *testing.T) {
	for _, test := range []struct {
		name string
		fake func(*fakeGitHub)
	}{
		{name: "server error", fake: func(fake *fakeGitHub) {
			fake.createRefStatus = http.StatusInternalServerError
			fake.apiErrorBody = testSecretBody
		}},
		{name: "timeout", fake: func(fake *fakeGitHub) { fake.delayCreateRef = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeGitHub()
			test.fake(fake)
			server := httptest.NewTLSServer(fake)
			defer server.Close()
			timeout := time.Second
			if test.name == "timeout" {
				timeout = 20 * time.Millisecond
			}
			client := newTestClient(t, server, timeout)
			branch, err := client.EnsureBranch(context.Background(), testRepo(), publish.BranchRequest{Name: testBranch, BaseSHA: testBaseSHA})
			if test.name == "timeout" {
				if err != nil || branch.Name != testBranch || branch.SHA != testBaseSHA {
					t.Fatalf("timeout with provable branch = %#v, %v", branch, err)
				}
				return
			}
			if !errors.Is(err, ErrUnclassified) || errors.Is(err, publish.ErrGitHubConflict) {
				t.Fatalf("%s error = %v", test.name, err)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testSecretBody) {
				t.Fatal("transport/status detail escaped in error")
			}
		})
	}
}

func TestCommitValidationRejectsDigestBinarySymlinkSubmoduleAndUnsafePath(t *testing.T) {
	tests := []struct {
		name string
		edit func(*publish.CommitRequest)
	}{
		{name: "patch digest", edit: func(request *publish.CommitRequest) { request.PatchDigest = testEffect }},
		{name: "manifest digest", edit: func(request *publish.CommitRequest) { request.ManifestDigest = testEffect }},
		{name: "binary", edit: func(request *publish.CommitRequest) { request.Patch.Files[0].Content = []byte{0xff, 0x00, 0x01} }},
		{name: "symlink mode", edit: func(request *publish.CommitRequest) { request.Patch.Files[0].Mode = "120000" }},
		{name: "submodule mode", edit: func(request *publish.CommitRequest) { request.Patch.Files[0].Mode = "160000" }},
		{name: "unsafe path", edit: func(request *publish.CommitRequest) { request.Patch.Files[0].Path = "../escape" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := testCommitRequest(t)
			test.edit(&request)
			if test.name == "symlink mode" || test.name == "submodule mode" || test.name == "unsafe path" || test.name == "binary" {
				// The adapter must reject the direct contract even when the
				// caller has recomputed its independent manifest digest.
				if manifest, err := publish.DigestForPatch(request.Patch.Files); err == nil {
					request.ManifestDigest = manifest
					request.Patch.ManifestDigest = manifest
				}
			}
			fake := newFakeGitHub()
			server := httptest.NewTLSServer(fake)
			defer server.Close()
			client := newTestClient(t, server, time.Second)
			_, err := client.EnsureCommit(context.Background(), testRepo(), request)
			if !errors.Is(err, publish.ErrInvalidRequest) {
				t.Fatalf("validation error = %v", err)
			}
			fake.mu.Lock()
			if len(fake.scopes) != 0 {
				t.Fatal("invalid commit minted a publish token")
			}
			fake.mu.Unlock()
		})
	}
}

func TestGetRefErrorRedactsTokenAndBody(t *testing.T) {
	fake := newFakeGitHub()
	fake.getRefStatus = http.StatusInternalServerError
	fake.apiErrorBody = testToken + " " + testSecretBody
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	client := newTestClient(t, server, time.Second)
	_, err := client.GetRef(context.Background(), testRepo(), "refs/heads/main")
	if !errors.Is(err, ErrUnclassified) {
		t.Fatalf("GetRef error = %v", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testSecretBody) {
		t.Fatalf("error leaked secret material: %q", err)
	}
}

func TestTokenIsScopedToRequestedRepository(t *testing.T) {
	fake := newFakeGitHub()
	server := httptest.NewTLSServer(fake)
	defer server.Close()
	client := newTestClient(t, server, time.Second)
	_, _ = client.GetRef(context.Background(), githubapp.Repository{Owner: testOwner, Name: "other"}, "refs/heads/main")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.scopes) != 1 || fake.scopes[0].Repo != "other" {
		t.Fatalf("token scope = %#v, want exactly repository name other", fake.scopes)
	}
}

func TestClientImplementsPublishClient(t *testing.T) {
	var _ publish.Client = (*Client)(nil)
}
