package githubpublish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/githubapp"
	"github.com/Astatide1337/agents-gateway/v3/internal/publish"
)

type findingsHTTPRecord struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

type findingsFixture struct {
	mu sync.Mutex

	tokenFixture *fakeGitHub
	requests     []findingsHTTPRecord

	pullBody   string
	pullHead   string
	pullStatus int
	pullMode   string
	pullEdit   func(map[string]any)

	patchStatus int
	patchMode   string
	applyPatch  bool
	closePatch  bool
	patchBodies []string

	postStatus int
	postMode   string
	applyPost  bool
	closePost  bool
	postBodies []map[string]any

	reviews       []reviewPayload
	comments      map[int64][]reviewCommentPayload
	reviewsMode   string
	commentsMode  string
	commentStatus int
	nextReviewID  int64
	nextCommentID int64
}

func newFindingsFixture() *findingsFixture {
	return &findingsFixture{
		tokenFixture:  newFakeGitHub(),
		pullBody:      "Human-owned description.\n",
		pullHead:      testCommitSHA,
		applyPatch:    true,
		comments:      make(map[int64][]reviewCommentPayload),
		nextReviewID:  900,
		nextCommentID: 1900,
	}
}

func (f *findingsFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	f.mu.Lock()
	f.requests = append(f.requests, findingsHTTPRecord{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Auth:   r.Header.Get("Authorization"),
		Body:   string(bodyBytes),
	})
	f.mu.Unlock()

	if r.URL.Path == "/app/installations/2/access_tokens" && r.Method == http.MethodPost {
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		f.tokenFixture.serveToken(w, r)
		return
	}
	if r.URL.Path != "/repos/acme/demo/pulls/42" && !strings.HasPrefix(r.URL.Path, "/repos/acme/demo/pulls/42/") {
		http.NotFound(w, r)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/demo/pulls/42":
		f.servePull(w)
	case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/demo/pulls/42":
		f.servePatch(w, bodyBytes)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/demo/pulls/42/reviews":
		f.serveReviews(w)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/demo/pulls/42/reviews":
		f.serveReviewPost(w, bodyBytes)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/demo/pulls/42/reviews/") && strings.HasSuffix(r.URL.Path, "/comments"):
		f.serveComments(w)
	default:
		http.NotFound(w, r)
	}
}

func (f *findingsFixture) servePull(w http.ResponseWriter) {
	f.mu.Lock()
	status, mode, body, head, edit := f.pullStatus, f.pullMode, f.pullBody, f.pullHead, f.pullEdit
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if status != http.StatusOK {
		writeRawFindings(w, status, testToken+" "+testSecretBody)
		return
	}
	if mode == "malformed" {
		writeRawFindings(w, status, "{")
		return
	}
	if mode == "oversized" {
		writeRawFindings(w, status, strings.Repeat("x", 2048))
		return
	}
	payload := findingsPullJSON(body, head)
	if edit != nil {
		edit(payload)
	}
	writeJSON(w, status, payload)
}

func (f *findingsFixture) servePatch(w http.ResponseWriter, bodyBytes []byte) {
	var request map[string]any
	if err := json.Unmarshal(bodyBytes, &request); err != nil {
		writeRawFindings(w, http.StatusBadRequest, "bad patch")
		return
	}
	f.mu.Lock()
	status, mode, apply, closeAfter := f.patchStatus, f.patchMode, f.applyPatch, f.closePatch
	if body, ok := request["body"].(string); ok {
		f.patchBodies = append(f.patchBodies, body)
		if apply {
			f.pullBody = body
		}
	}
	body, head := f.pullBody, f.pullHead
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	if closeAfter {
		panic(http.ErrAbortHandler)
	}
	if mode == "malformed" {
		writeRawFindings(w, status, "not-json")
		return
	}
	if mode == "oversized" {
		writeRawFindings(w, status, strings.Repeat("x", 2048))
		return
	}
	if status != http.StatusOK {
		writeRawFindings(w, status, "unprocessable")
		return
	}
	writeJSON(w, status, findingsPullJSON(body, head))
}

func (f *findingsFixture) serveReviews(w http.ResponseWriter) {
	f.mu.Lock()
	mode := f.reviewsMode
	reviews := append([]reviewPayload(nil), f.reviews...)
	f.mu.Unlock()
	switch mode {
	case "malformed":
		writeRawFindings(w, http.StatusOK, "[")
		return
	case "oversized":
		writeRawFindings(w, http.StatusOK, strings.Repeat("x", 2048))
		return
	case "full":
		reviews = make([]reviewPayload, maxReviewCommentCount)
		for index := range reviews {
			reviews[index] = reviewPayload{ID: int64(index + 1), Body: fmt.Sprintf("unrelated review %d", index), State: "COMMENTED", CommitID: testCommitSHA}
		}
	}
	writeJSON(w, http.StatusOK, reviews)
}

func (f *findingsFixture) serveReviewPost(w http.ResponseWriter, bodyBytes []byte) {
	var request map[string]any
	if err := json.Unmarshal(bodyBytes, &request); err != nil {
		writeRawFindings(w, http.StatusBadRequest, "bad review")
		return
	}
	f.mu.Lock()
	status, mode, apply, closeAfter := f.postStatus, f.postMode, f.applyPost, f.closePost
	f.postBodies = append(f.postBodies, request)
	reviewID := f.nextReviewID
	commentID := f.nextCommentID
	f.nextReviewID++
	f.nextCommentID++
	if apply {
		body, _ := request["body"].(string)
		commitID, _ := request["commit_id"].(string)
		f.reviews = append(f.reviews, reviewPayload{ID: reviewID, Body: body, State: "CHANGES_REQUESTED", CommitID: commitID})
		var commentItems []map[string]any
		encoded, _ := json.Marshal(request["comments"])
		_ = json.Unmarshal(encoded, &commentItems)
		for _, item := range commentItems {
			line, _ := item["line"].(float64)
			startLine, hasStart := item["start_line"].(float64)
			comment := reviewCommentPayload{ID: commentID, Body: body, Path: stringValue(item["path"]), Side: stringValue(item["side"]), CommitID: commitID, Line: intPointer(int(line))}
			if hasStart {
				comment.StartLine = intPointer(int(startLine))
				comment.StartSide = stringValue(item["start_side"])
			}
			f.comments[reviewID] = append(f.comments[reviewID], comment)
		}
	}
	f.mu.Unlock()
	if closeAfter {
		panic(http.ErrAbortHandler)
	}
	if status == 0 {
		status = http.StatusCreated
	}
	if mode == "malformed" {
		writeRawFindings(w, status, "not-json")
		return
	}
	if mode == "oversized" {
		writeRawFindings(w, status, strings.Repeat("x", 2048))
		return
	}
	if status != http.StatusCreated {
		writeRawFindings(w, status, "unprocessable")
		return
	}
	f.mu.Lock()
	requestBody := f.postBodies[len(f.postBodies)-1]
	body, _ := requestBody["body"].(string)
	commitID, _ := requestBody["commit_id"].(string)
	f.mu.Unlock()
	writeJSON(w, status, reviewPayload{ID: reviewID, Body: body, State: "CHANGES_REQUESTED", CommitID: commitID})
}

func (f *findingsFixture) serveComments(w http.ResponseWriter) {
	f.mu.Lock()
	mode := f.commentsMode
	var comments []reviewCommentPayload
	for _, current := range f.comments {
		comments = append(comments, current...)
	}
	f.mu.Unlock()
	if f.commentStatus != 0 {
		writeRawFindings(w, f.commentStatus, "comments error")
		return
	}
	if mode == "malformed" {
		writeRawFindings(w, http.StatusOK, "[")
		return
	}
	if mode == "oversized" {
		writeRawFindings(w, http.StatusOK, strings.Repeat("x", 2048))
		return
	}
	if mode == "full" {
		comments = make([]reviewCommentPayload, maxReviewCommentCount)
		for index := range comments {
			line := index + 1
			comments[index] = reviewCommentPayload{ID: int64(index + 1), Body: fmt.Sprintf("unrelated comment %d", index), Path: "src/main.go", Line: &line, Side: "RIGHT", CommitID: testCommitSHA}
		}
	}
	writeJSON(w, http.StatusOK, comments)
}

func (f *findingsFixture) recordsSnapshot() []findingsHTTPRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]findingsHTTPRecord(nil), f.requests...)
}

func (f *findingsFixture) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.postBodies)
}

func (f *findingsFixture) patchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.patchBodies)
}

func newFindingsTestClient(t *testing.T, server *httptest.Server, timeout time.Duration, maxResponse int64) *Client {
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
	client, err := New(Config{Minter: minter, BaseURL: server.URL, HTTPClient: server.Client(), RequestTimeout: timeout, MaxResponseSize: maxResponse})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newFindingsTestServer(t *testing.T, fixture *findingsFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(fixture)
	t.Cleanup(server.Close)
	return server
}

func findingsPullJSON(body, head string) map[string]any {
	return map[string]any{
		"number":   testPRNumber,
		"body":     body,
		"html_url": "https://github.com/acme/demo/pull/42",
		"head":     map[string]string{"ref": testBranch, "sha": head},
		"base":     map[string]string{"ref": "main"},
	}
}

func writeRawFindings(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func intPointer(value int) *int { return &value }

func testBlockingFindingRequest(t *testing.T) publish.BlockingFindingRequest {
	t.Helper()
	body := publish.MarkerForFinding(testEffect) + "\n### Finding\n\nThe checked value is unsafe."
	return publish.BlockingFindingRequest{
		PullRequestNumber: testPRNumber,
		CommitSHA:         testCommitSHA,
		EffectKey:         testEffect,
		FindingID:         "finding-1",
		Path:              "src/main.go",
		StartLine:         10,
		EndLine:           12,
		Body:              body,
	}
}

func testAdvisorySectionRequest(t *testing.T) publish.AdvisorySectionRequest {
	t.Helper()
	section := "<!-- agw-advisory-findings:v1 -->\n## Advisory findings\n\nInformational note.\n<!-- /agw-advisory-findings:v1 -->"
	return publish.AdvisorySectionRequest{
		PullRequestNumber: testPRNumber,
		ExpectedHeadSHA:   testCommitSHA,
		EffectKey:         testEffect,
		SectionDigest:     publish.DigestForPatchBytes([]byte(section)),
		Section:           section,
	}
}

func TestFindingsAuthSessionIsControllerOwnedAndNeverLeaksToken(t *testing.T) {
	fixture := newFindingsFixture()
	fixture.pullStatus = http.StatusInternalServerError
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)

	_, err := client.GetPullRequest(context.Background(), testRepo(), testPRNumber)
	if !errors.Is(err, ErrUnclassified) {
		t.Fatalf("GetPullRequest error = %v, want unclassified", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testSecretBody) {
		t.Fatalf("error leaked controller-owned credential or response body: %q", err)
	}
	for _, record := range fixture.recordsSnapshot() {
		if record.Path == "/app/installations/2/access_tokens" {
			continue
		}
		if record.Auth != "Bearer "+testToken {
			t.Fatalf("API request authorization = %q, want controller-minted bearer", record.Auth)
		}
		if strings.Contains(record.Path, testToken) || strings.Contains(record.Query, testToken) || strings.Contains(record.Body, testToken) {
			t.Fatalf("installation token leaked into request data: %#v", record)
		}
	}
}

func TestGetPullRequestValidatesImmutableIdentity(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "number", edit: func(value map[string]any) { value["number"] = 43 }},
		{name: "url", edit: func(value map[string]any) { value["html_url"] = "https://evil.example/acme/demo/pull/42" }},
		{name: "head sha", edit: func(value map[string]any) {
			value["head"] = map[string]string{"ref": testBranch, "sha": "not-a-sha"}
		}},
		{name: "base ref", edit: func(value map[string]any) { value["base"] = map[string]string{"ref": "../main"} }},
		{name: "body nul", edit: func(value map[string]any) { value["body"] = "bad\x00body" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFindingsFixture()
			server := newFindingsTestServer(t, fixture)
			fixture.pullEdit = test.edit
			client := newFindingsTestClient(t, server, time.Second, 1024)
			_, err := client.GetPullRequest(context.Background(), testRepo(), testPRNumber)
			if !errors.Is(err, ErrUnclassified) {
				t.Fatalf("invalid pull identity error = %v", err)
			}
		})
	}

	fixture := newFindingsFixture()
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)
	valid, err := client.GetPullRequest(context.Background(), testRepo(), testPRNumber)
	if err != nil || valid.Number != testPRNumber || valid.HeadSHA != testCommitSHA || valid.URL != "https://github.com/acme/demo/pull/42" {
		t.Fatalf("valid pull identity = %#v, %v", valid, err)
	}
}

func TestEnsureAdvisorySectionMergesExactlyAndReplaysByReadback(t *testing.T) {
	fixture := newFindingsFixture()
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)
	request := testAdvisorySectionRequest(t)

	result, err := client.EnsureAdvisorySection(context.Background(), testRepo(), request)
	if err != nil {
		t.Fatalf("EnsureAdvisorySection = %v", err)
	}
	if result.PullRequestNumber != testPRNumber || result.HeadSHA != testCommitSHA || result.SectionDigest != request.SectionDigest {
		t.Fatalf("advisory result = %#v", result)
	}
	wantBody := fixture.pullBody
	if len(wantBody) == 0 || !strings.HasPrefix(wantBody, "Human-owned description.\n\n") || !strings.HasSuffix(wantBody, request.Section) {
		t.Fatalf("merged body lost user bytes or section: %q", wantBody)
	}
	if fixture.patchCount() != 1 {
		t.Fatalf("PATCH count after first advisory publication = %d, want 1", fixture.patchCount())
	}
	if _, err := client.EnsureAdvisorySection(context.Background(), testRepo(), request); err != nil {
		t.Fatalf("advisory replay = %v", err)
	}
	if fixture.patchCount() != 1 {
		t.Fatalf("advisory replay issued duplicate PATCH, count = %d", fixture.patchCount())
	}
	foundPatch := false
	for _, record := range fixture.recordsSnapshot() {
		if record.Method == http.MethodPatch {
			foundPatch = true
			var payload map[string]string
			if err := json.Unmarshal([]byte(record.Body), &payload); err != nil || payload["body"] != wantBody {
				t.Fatalf("PATCH body = %q, want exact merged body %q", record.Body, wantBody)
			}
		}
	}
	if !foundPatch {
		t.Fatal("fixture did not observe advisory PATCH")
	}
}

func TestEnsureAdvisorySectionReconcilesTransportAmbiguityAnd422IsConflict(t *testing.T) {
	t.Run("transport ambiguity readback", func(t *testing.T) {
		fixture := newFindingsFixture()
		fixture.applyPatch = true
		fixture.closePatch = true
		server := newFindingsTestServer(t, fixture)
		client := newFindingsTestClient(t, server, time.Second, 1024)
		request := testAdvisorySectionRequest(t)
		if _, err := client.EnsureAdvisorySection(context.Background(), testRepo(), request); err != nil {
			t.Fatalf("ambiguous advisory mutation = %v", err)
		}
		if fixture.patchCount() != 1 {
			t.Fatalf("ambiguous advisory mutation issued %d PATCH requests", fixture.patchCount())
		}
	})

	t.Run("deterministic 422", func(t *testing.T) {
		fixture := newFindingsFixture()
		fixture.patchStatus = http.StatusUnprocessableEntity
		fixture.applyPatch = false
		server := newFindingsTestServer(t, fixture)
		client := newFindingsTestClient(t, server, time.Second, 1024)
		_, err := client.EnsureAdvisorySection(context.Background(), testRepo(), testAdvisorySectionRequest(t))
		if !errors.Is(err, publish.ErrGitHubConflict) {
			t.Fatalf("422 advisory error = %v, want conflict", err)
		}
		if fixture.patchCount() != 1 {
			t.Fatalf("422 advisory path issued %d PATCH requests", fixture.patchCount())
		}
	})
}

func TestEnsureBlockingFindingReviewPayloadAndReadback(t *testing.T) {
	fixture := newFindingsFixture()
	fixture.applyPost = true
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)
	request := testBlockingFindingRequest(t)

	result, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), request)
	if err != nil {
		t.Fatalf("EnsureBlockingFindingReview = %v", err)
	}
	if result.PullRequestNumber != testPRNumber || result.CommitSHA != request.CommitSHA || result.EffectKey != request.EffectKey || result.ReviewID <= 0 || result.CommentID <= 0 {
		t.Fatalf("blocking result = %#v", result)
	}
	if fixture.postCount() != 1 {
		t.Fatalf("review POST count = %d, want 1", fixture.postCount())
	}
	records := fixture.recordsSnapshot()
	var post findingsHTTPRecord
	for _, record := range records {
		if record.Method == http.MethodPost && strings.HasSuffix(record.Path, "/reviews") {
			post = record
		}
	}
	if post.Path == "" {
		t.Fatal("fixture did not observe review POST")
	}
	var payload struct {
		Body     string `json:"body"`
		Event    string `json:"event"`
		CommitID string `json:"commit_id"`
		Comments []struct {
			Path      string `json:"path"`
			Line      int    `json:"line"`
			StartLine int    `json:"start_line"`
			Side      string `json:"side"`
			StartSide string `json:"start_side"`
			Body      string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(post.Body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Event != "REQUEST_CHANGES" || payload.CommitID != request.CommitSHA || payload.Body != request.Body || len(payload.Comments) != 1 {
		t.Fatalf("review payload = %#v", payload)
	}
	comment := payload.Comments[0]
	if comment.Path != request.Path || comment.Line != request.EndLine || comment.StartLine != request.StartLine || comment.Side != "RIGHT" || comment.StartSide != "RIGHT" || comment.Body != request.Body {
		t.Fatalf("line comment payload = %#v", comment)
	}

	if _, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), request); err != nil {
		t.Fatalf("blocking review replay = %v", err)
	}
	if fixture.postCount() != 1 {
		t.Fatalf("blocking replay issued duplicate POST, count = %d", fixture.postCount())
	}
}

func TestEnsureBlockingFindingReviewReconcilesMarkerAfterTransportAmbiguity(t *testing.T) {
	fixture := newFindingsFixture()
	fixture.applyPost = true
	fixture.closePost = true
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)

	result, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), testBlockingFindingRequest(t))
	if err != nil {
		t.Fatalf("ambiguous review mutation = %v", err)
	}
	if result.ReviewID <= 0 || result.CommentID <= 0 || fixture.postCount() != 1 {
		t.Fatalf("ambiguous review result = %#v, POST count = %d", result, fixture.postCount())
	}
}

func TestEnsureBlockingFindingReviewRejectsMarkerMismatchAndDuplicates(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*findingsFixture, publish.BlockingFindingRequest)
	}{
		{name: "marker body mismatch", configure: func(f *findingsFixture, request publish.BlockingFindingRequest) {
			f.reviews = []reviewPayload{{ID: 901, Body: publish.MarkerForFinding(request.EffectKey) + "\nother body", State: "CHANGES_REQUESTED", CommitID: request.CommitSHA}}
		}},
		{name: "duplicate reviews", configure: func(f *findingsFixture, request publish.BlockingFindingRequest) {
			f.reviews = []reviewPayload{
				{ID: 901, Body: request.Body, State: "CHANGES_REQUESTED", CommitID: request.CommitSHA},
				{ID: 902, Body: request.Body, State: "CHANGES_REQUESTED", CommitID: request.CommitSHA},
			}
			line := request.EndLine
			f.comments[901] = []reviewCommentPayload{{ID: 1901, Body: request.Body, Path: request.Path, Line: &line, Side: "RIGHT", CommitID: request.CommitSHA}}
			f.comments[902] = []reviewCommentPayload{{ID: 1902, Body: request.Body, Path: request.Path, Line: &line, Side: "RIGHT", CommitID: request.CommitSHA}}
		}},
		{name: "duplicate marker comments", configure: func(f *findingsFixture, request publish.BlockingFindingRequest) {
			f.reviews = []reviewPayload{{ID: 901, Body: request.Body, State: "CHANGES_REQUESTED", CommitID: request.CommitSHA}}
			line := request.EndLine
			f.comments[901] = []reviewCommentPayload{
				{ID: 1901, Body: request.Body, Path: request.Path, Line: &line, Side: "RIGHT", CommitID: request.CommitSHA},
				{ID: 1902, Body: request.Body, Path: request.Path, Line: &line, Side: "RIGHT", CommitID: request.CommitSHA},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFindingsFixture()
			request := testBlockingFindingRequest(t)
			test.configure(fixture, request)
			server := newFindingsTestServer(t, fixture)
			client := newFindingsTestClient(t, server, time.Second, 1024)
			_, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), request)
			if !errors.Is(err, ErrUnclassified) {
				t.Fatalf("error = %v, want unclassified", err)
			}
			if fixture.postCount() != 0 {
				t.Fatalf("rejected marker state issued %d duplicate POSTs", fixture.postCount())
			}
		})
	}
}

func TestEnsureBlockingFindingReviewReturnsConflictFor422WithoutRetryingPost(t *testing.T) {
	fixture := newFindingsFixture()
	fixture.postStatus = http.StatusUnprocessableEntity
	server := newFindingsTestServer(t, fixture)
	client := newFindingsTestClient(t, server, time.Second, 1024)

	_, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), testBlockingFindingRequest(t))
	if !errors.Is(err, publish.ErrGitHubConflict) {
		t.Fatalf("422 review error = %v, want conflict", err)
	}
	if fixture.postCount() != 1 {
		t.Fatalf("422 review path issued %d POST requests", fixture.postCount())
	}
}

func TestFindingsRejectMalformedAndOversizedResponses(t *testing.T) {
	tests := []struct {
		name string
		mode string
	}{
		{name: "malformed pull", mode: "malformed"},
		{name: "oversized pull", mode: "oversized"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFindingsFixture()
			fixture.pullMode = test.mode
			server := newFindingsTestServer(t, fixture)
			client := newFindingsTestClient(t, server, time.Second, 1024)
			_, err := client.GetPullRequest(context.Background(), testRepo(), testPRNumber)
			if !errors.Is(err, ErrUnclassified) {
				t.Fatalf("pull response error = %v", err)
			}
		})
	}

	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "malformed reviews", mode: "malformed"},
		{name: "oversized reviews", mode: "oversized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFindingsFixture()
			fixture.reviewsMode = test.mode
			server := newFindingsTestServer(t, fixture)
			client := newFindingsTestClient(t, server, time.Second, 1024)
			_, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), testBlockingFindingRequest(t))
			if !errors.Is(err, ErrUnclassified) {
				t.Fatalf("review response error = %v", err)
			}
			if fixture.postCount() != 0 {
				t.Fatalf("invalid review listing issued %d POSTs", fixture.postCount())
			}
		})
	}
}

func TestEnsureBlockingFindingReviewFailsClosedOnFullReviewOrCommentPage(t *testing.T) {
	t.Run("full review page", func(t *testing.T) {
		fixture := newFindingsFixture()
		fixture.reviewsMode = "full"
		server := newFindingsTestServer(t, fixture)
		client := newFindingsTestClient(t, server, time.Second, 1024)
		_, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), testBlockingFindingRequest(t))
		if !errors.Is(err, ErrUnclassified) {
			t.Fatalf("full review page error = %v", err)
		}
		if fixture.postCount() != 0 {
			t.Fatalf("full review page issued %d POSTs", fixture.postCount())
		}
	})

	t.Run("full comment page", func(t *testing.T) {
		fixture := newFindingsFixture()
		request := testBlockingFindingRequest(t)
		fixture.reviews = []reviewPayload{{ID: 901, Body: request.Body, State: "CHANGES_REQUESTED", CommitID: request.CommitSHA}}
		fixture.commentsMode = "full"
		server := newFindingsTestServer(t, fixture)
		client := newFindingsTestClient(t, server, time.Second, 1024)
		_, err := client.EnsureBlockingFindingReview(context.Background(), testRepo(), request)
		if !errors.Is(err, ErrUnclassified) {
			t.Fatalf("full comment page error = %v", err)
		}
		if fixture.postCount() != 0 {
			t.Fatalf("full comment page issued %d POSTs", fixture.postCount())
		}
	})
}
