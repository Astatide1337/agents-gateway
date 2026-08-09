package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/artifact"
	"github.com/Astatide1337/agents-gateway/v2/pkg/artifactcatalog"
	"github.com/Astatide1337/agents-gateway/v2/pkg/authz"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
)

// testAuthenticator intentionally lives in a test file. Production callers
// must provide a real Authenticator and the server must fail closed if they do
// not configure one.
type testAuthenticator struct {
	principal authz.Principal
	err       error
}

func (a testAuthenticator) Authenticate(*http.Request) (authz.Principal, error) {
	if a.err != nil {
		return authz.Principal{}, a.err
	}
	return a.principal, nil
}

type auditRecordingStore struct {
	store.Store
	mu     sync.Mutex
	audits []store.AuditEvent
}

type recordingRunStarter struct {
	mu       sync.Mutex
	requests []RunStartRequest
	err      error
}

type recordingRunSignaler struct {
	mu      sync.Mutex
	signals []RunSignal
	err     error
}

type blockingRunSignaler struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (s *blockingRunSignaler) SignalRun(_ context.Context, _ store.Scope, _ string, _ RunSignal) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}

func (s *blockingRunSignaler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *recordingRunSignaler) SignalRun(_ context.Context, _ store.Scope, _ string, signal RunSignal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.signals = append(s.signals, signal)
	return nil
}

func (s *recordingRunSignaler) snapshot() []RunSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunSignal(nil), s.signals...)
}

func (s *recordingRunStarter) StartRun(_ context.Context, request RunStartRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return s.err
}

func (s *recordingRunStarter) snapshot() []RunStartRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RunStartRequest(nil), s.requests...)
}

func (s *auditRecordingStore) AppendAudit(_ context.Context, event store.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, event)
	return nil
}

func (s *auditRecordingStore) auditSnapshot() []store.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.AuditEvent(nil), s.audits...)
}

func editor() testAuthenticator {
	return testAuthenticator{principal: authz.Principal{
		ID: "user-a", Type: authz.PrincipalHuman,
		ProjectRoles: map[string]authz.Role{"org-a/project-a": authz.RoleProjectEditor},
	}}
}

func viewer() testAuthenticator {
	return testAuthenticator{principal: authz.Principal{
		ID: "viewer-a", Type: authz.PrincipalHuman,
		ProjectRoles: map[string]authz.Role{"org-a/project-a": authz.RoleViewer},
	}}
}

func newTestHandler(storage store.Store, authenticator Authenticator) *Handler {
	return New(storage, authenticator, Options{
		MaxBodyBytes: 1024,
		Now:          func() time.Time { return time.Unix(100, 0).UTC() },
		RunStarter:   &recordingRunStarter{},
		RunSignaler:  &recordingRunSignaler{},
	})
}

func approver() testAuthenticator {
	return testAuthenticator{principal: authz.Principal{
		ID: "approver-a", Type: authz.PrincipalHuman,
		ProjectRoles: map[string]authz.Role{"org-a/project-a": authz.RoleApprover},
	}}
}

func createSignalRun(t *testing.T, storage store.Store, status string) {
	t.Helper()
	_, err := storage.CreateRun(context.Background(), store.Run{
		Scope: store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, ID: "run-signal",
		Kind: "AgentRun", DefinitionDigest: "sha256:" + strings.Repeat("a", 64), RequestedBy: "user-a", Status: status,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func request(t *testing.T, handler http.Handler, method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Request-ID", "test-request-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func agentDocument(t *testing.T, name, secret string) string {
	t.Helper()
	return `{"apiVersion":"agents.astatide.com/v1alpha1","kind":"Agent","metadata":{"name":"` + name + `","namespace":"project-a"},"spec":{"runtime":{"harness":"codex","image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"instructions":{"inline":"fix issue"},"environment":[{"name":"API_TOKEN","value":"` + secret + `"}]}}`
}

func safeAgentDocument(name string) string {
	return `{"apiVersion":"agents.astatide.com/v1alpha1","kind":"Agent","metadata":{"name":"` + name + `","namespace":"project-a"},"spec":{"runtime":{"harness":"codex","image":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"instructions":{"inline":"fix issue"},"sandboxProfileRef":"coding-medium","environment":[{"name":"API_TOKEN","valueFrom":"credential://github-token"}]}}`
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("invalid response JSON: %v; body=%s", err, response.Body.String())
	}
	return value
}

func TestHealthReadinessRequestIDAndFailClosedAuth(t *testing.T) {
	storage := store.NewMemory()
	handler := newTestHandler(storage, nil)
	health := request(t, handler, http.MethodGet, "/healthz", "", "")
	if health.Code != http.StatusOK || health.Header().Get("X-Request-ID") != "test-request-1" {
		t.Fatalf("health status=%d headers=%v body=%s", health.Code, health.Header(), health.Body.String())
	}
	ready := request(t, handler, http.MethodGet, "/readyz", "", "")
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness without auth=%d", ready.Code)
	}
	protected := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-1", "", "")
	if protected.Code != http.StatusServiceUnavailable {
		t.Fatalf("protected request without auth=%d", protected.Code)
	}
	if strings.Contains(protected.Body.String(), "authenticator unavailable") {
		t.Fatal("internal authenticator detail leaked")
	}
}

func TestReadinessFailsClosedWithoutLeakingDependencyError(t *testing.T) {
	handler := New(store.NewMemory(), editor(), Options{RunStarter: &recordingRunStarter{}, ReadyCheck: func(context.Context) error {
		return errors.New("postgres password secret-value rejected")
	}})
	response := request(t, handler, http.MethodGet, "/ready", "", "")
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret-value") {
		t.Fatalf("readiness status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResourceApplyGetAndSecretRedaction(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, editor())
	secret := "super-secret-value"
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/resources/Agent/fixer"
	response := request(t, handler, http.MethodPut, path, "application/json", agentDocument(t, "fixer", secret))
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("literal secret rejection status=%d body=%s", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodPut, path, "application/json", safeAgentDocument("fixer"))
	if response.Code != http.StatusCreated {
		t.Fatalf("safe apply status=%d body=%s", response.Code, response.Body.String())
	}
	get := request(t, handler, http.MethodGet, path, "", "")
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), secret) {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
	body := decodeResponse(t, get)
	if body["requestId"] != "test-request-1" {
		t.Fatalf("request id missing: %#v", body)
	}
	audits := storage.auditSnapshot()
	if len(audits) != 3 || audits[0].Decision != "allowed" || audits[0].Action != string(authz.ActionApply) || audits[1].Action != string(authz.ActionApply) || audits[2].Action != string(authz.ActionRead) {
		t.Fatalf("unexpected audit records: %#v", audits)
	}
}

func TestCrossTenantDeniedAndAudited(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, editor())
	path := "/api/v1alpha1/organizations/org-b/projects/project-b/resources/Agent/fixer"
	response := request(t, handler, http.MethodPut, path, "application/json", agentDocument(t, "fixer", "do-not-return"))
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "do-not-return") {
		t.Fatal("denied request body leaked")
	}
	audits := storage.auditSnapshot()
	if len(audits) != 1 || audits[0].Decision != "denied" || audits[0].OrganizationID != "org-b" || audits[0].PrincipalID != "user-a" {
		t.Fatalf("cross-tenant denial was not audited: %#v", audits)
	}
}

func TestInvalidPayloadsAndBodyLimit(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, editor())
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/resources/Agent/fixer"
	unknown := request(t, handler, http.MethodPut, path, "application/json", `{"apiVersion":"agents.astatide.com/v1alpha1","kind":"Agent","metadata":{"name":"fixer"},"spec":{},"password":"super-secret"}`)
	if unknown.Code != http.StatusBadRequest || strings.Contains(unknown.Body.String(), "super-secret") {
		t.Fatalf("unknown field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	wrongType := request(t, handler, http.MethodPut, path, "text/plain", "secret body")
	if wrongType.Code != http.StatusUnsupportedMediaType || strings.Contains(wrongType.Body.String(), "secret body") {
		t.Fatalf("wrong content type status=%d body=%s", wrongType.Code, wrongType.Body.String())
	}
	overLimit := request(t, handler, http.MethodPut, path, "application/json", `{"padding":"`+strings.Repeat("x", 1200)+`"}`)
	if overLimit.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", overLimit.Code, overLimit.Body.String())
	}
	if len(storage.auditSnapshot()) != 3 {
		t.Fatalf("expected each authorized request to be audited")
	}
}

func TestToolSetArgumentsNullRejectedAndOmissionReadBackUnconstrained(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, editor())
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/resources/ToolSet/github-tools"
	prefix := `{"apiVersion":"agents.astatide.com/v1alpha1","kind":"ToolSet","metadata":{"name":"github-tools"},"spec":{"servers":[{"name":"gateway","ref":"https://mcp.example.test/mcp","tools":[{"name":"get_me","effect":"read"`
	suffix := `}]}]}}`

	explicitNull := request(t, handler, http.MethodPut, path, "application/json", prefix+`,"arguments":null`+suffix)
	if explicitNull.Code != http.StatusBadRequest || strings.Contains(explicitNull.Body.String(), "get_me") {
		t.Fatalf("explicit null status=%d body=%s", explicitNull.Code, explicitNull.Body.String())
	}

	omitted := request(t, handler, http.MethodPut, path, "application/json", prefix+suffix)
	if omitted.Code != http.StatusCreated {
		t.Fatalf("omitted arguments apply status=%d body=%s", omitted.Code, omitted.Body.String())
	}
	get := request(t, handler, http.MethodGet, path, "", "")
	if get.Code != http.StatusOK {
		t.Fatalf("omitted arguments readback status=%d body=%s", get.Code, get.Body.String())
	}
	response := decodeResponse(t, get)
	data := response["data"].(map[string]any)
	document := data["document"].(map[string]any)
	specification := document["spec"].(map[string]any)
	servers := specification["servers"].([]any)
	tools := servers[0].(map[string]any)["tools"].([]any)
	if _, present := tools[0].(map[string]any)["arguments"]; present {
		t.Fatalf("omitted arguments appeared in API readback: %#v", tools[0])
	}
}

func TestRunCreationIsIdempotentAndScoped(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, editor())
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/runs"
	body := `{"kind":"AgentRun","definitionDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","idempotencyKey":"issue-42","agentRef":"fixer","inputRef":"s3://org-a/project-a/issues/42.json"}`
	first := request(t, handler, http.MethodPost, path, "application/json", body)
	second := request(t, handler, http.MethodPost, path, "application/json", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("idempotency statuses=%d,%d bodies=%s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	firstJSON, secondJSON := decodeResponse(t, first), decodeResponse(t, second)
	firstData := firstJSON["data"].(map[string]any)
	secondData := secondJSON["data"].(map[string]any)
	if firstData["id"] != secondData["id"] {
		t.Fatalf("idempotency returned different IDs: %v vs %v", firstData["id"], secondData["id"])
	}
	get := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/"+firstData["id"].(string), "", "")
	if get.Code != http.StatusOK {
		t.Fatalf("get run status=%d body=%s", get.Code, get.Body.String())
	}
	if len(storage.auditSnapshot()) != 3 {
		t.Fatalf("expected run create replay and get audits")
	}
}

func TestTenantScopedCollectionEndpointsPaginateAndFilter(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	for index, kind := range []string{"Agent", "Workflow", "Runner", "Entitlement", "ModelRoute", "Approval"} {
		if _, err := storage.ApplyResource(context.Background(), store.Resource{
			Scope: scope, Kind: kind, Name: fmt.Sprintf("resource-%d", index),
			Digest: fmt.Sprintf("sha256:%064d", index+10), AppliedBy: "tester", Document: []byte(`{"kind":"` + kind + `"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 2; index++ {
		if _, err := storage.CreateRun(context.Background(), store.Run{
			Scope: scope, ID: fmt.Sprintf("run-%d", index), Kind: "AgentRun",
			DefinitionDigest: fmt.Sprintf("sha256:%064d", index+30), RequestedBy: "tester",
		}); err != nil {
			t.Fatal(err)
		}
	}
	handler := newTestHandler(storage, viewer())
	base := "/api/v1alpha1/organizations/org-a/projects/project-a/"
	resources := request(t, handler, http.MethodGet, base+"resources?kind=Agent,Workflow&limit=1", "", "")
	if resources.Code != http.StatusOK {
		t.Fatalf("resource collection status=%d body=%s", resources.Code, resources.Body.String())
	}
	resourceData := decodeResponse(t, resources)["data"].(map[string]any)
	if len(resourceData["items"].([]any)) != 1 || resourceData["hasMore"] != true || resourceData["nextOffset"].(float64) != 1 {
		t.Fatalf("unexpected resource page: %#v", resourceData)
	}
	for _, path := range []string{"definitions", "approvals", "runners", "entitlements", "model-routes"} {
		response := request(t, handler, http.MethodGet, base+path+"?limit=10", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s collection status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	runs := request(t, handler, http.MethodGet, base+"runs?limit=1", "", "")
	if runs.Code != http.StatusOK || len(decodeResponse(t, runs)["data"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatalf("run collection status=%d body=%s", runs.Code, runs.Body.String())
	}
	invalid := request(t, handler, http.MethodGet, base+"runs?limit=501", "", "")
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "invalid_pagination") {
		t.Fatalf("invalid pagination status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestUsageQuotaAuditEndpointsAreAuthenticatedAndTenantScoped(t *testing.T) {
	storage := store.NewMemory()
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	if _, err := storage.ApplyResource(context.Background(), store.Resource{
		Scope: scope, Kind: "Project", Name: "project-a", Digest: "sha256:" + strings.Repeat("p", 64), AppliedBy: "tester",
		Document: []byte(`{"apiVersion":"agents.astatide.com/v1alpha1","kind":"Project","metadata":{"name":"project-a"},"spec":{"organizationRef":"org-a","quotas":{"concurrentRuns":4}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateRun(context.Background(), store.Run{Scope: scope, ID: "run-usage", Kind: "AgentRun", DefinitionDigest: "sha256:" + strings.Repeat("r", 64), RequestedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler(storage, viewer())
	base := "/api/v1alpha1/organizations/org-a/projects/project-a/"
	quotas := request(t, handler, http.MethodGet, base+"quotas", "", "")
	if quotas.Code != http.StatusOK || !strings.Contains(quotas.Body.String(), "concurrentRuns") || !strings.Contains(quotas.Body.String(), "activeRuns") {
		t.Fatalf("quota response status=%d body=%s", quotas.Code, quotas.Body.String())
	}
	usage := request(t, handler, http.MethodGet, base+"usage", "", "")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), "totalRuns") {
		t.Fatalf("usage response status=%d body=%s", usage.Code, usage.Body.String())
	}
	audit := request(t, handler, http.MethodGet, base+"audit?limit=2", "", "")
	if audit.Code != http.StatusOK || !strings.Contains(audit.Body.String(), "principalId") {
		t.Fatalf("audit response status=%d body=%s", audit.Code, audit.Body.String())
	}
	unauthorized := request(t, New(store.NewMemory(), nil, Options{}), http.MethodGet, base+"audit", "", "")
	if unauthorized.Code != http.StatusServiceUnavailable {
		t.Fatalf("unauthenticated audit status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	otherTenant := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-b/projects/project-b/audit", "", "")
	if otherTenant.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant audit status=%d body=%s", otherTenant.Code, otherTenant.Body.String())
	}
}

func TestRunCreationDispatchesDurablyAndFailsClosed(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	starter := &recordingRunStarter{}
	handler := New(storage, editor(), Options{MaxBodyBytes: 2048, RunStarter: starter})
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/runs"
	body := `{"kind":"WorkflowRun","definitionDigest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","idempotencyKey":"release-7","workflowRef":"release","inputRef":"s3://org-a/project-a/input.json"}`
	response := request(t, handler, http.MethodPost, path, "application/json", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("dispatch status=%d body=%s", response.Code, response.Body.String())
	}
	requests := starter.snapshot()
	if len(requests) != 1 || requests[0].WorkflowRef != "release" || requests[0].InputRef != "s3://org-a/project-a/input.json" {
		t.Fatalf("unexpected durable dispatch: %#v", requests)
	}

	failedStore := &auditRecordingStore{Store: store.NewMemory()}
	failed := &recordingRunStarter{err: errors.New("temporal unavailable: secret endpoint")}
	failedHandler := New(failedStore, editor(), Options{MaxBodyBytes: 2048, RunStarter: failed})
	failedResponse := request(t, failedHandler, http.MethodPost, path, "application/json", body)
	if failedResponse.Code != http.StatusServiceUnavailable || strings.Contains(failedResponse.Body.String(), "secret endpoint") {
		t.Fatalf("failed dispatch status=%d body=%s", failedResponse.Code, failedResponse.Body.String())
	}
	events, err := failedStore.ListEvents(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, deterministicRunID(store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "release-7"), 0)
	if err != nil || len(events) != 1 || events[0].Type != "run.dispatch_failed" {
		t.Fatalf("dispatch failure event=%#v err=%v", events, err)
	}

	noStarter := New(store.NewMemory(), editor(), Options{MaxBodyBytes: 2048})
	noStarterResponse := request(t, noStarter, http.MethodPost, path, "application/json", body)
	if noStarterResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing starter must fail closed: %d %s", noStarterResponse.Code, noStarterResponse.Body.String())
	}
}

func TestEventListingAndSSEStream(t *testing.T) {
	storage := store.NewMemory()
	handler := newTestHandler(storage, viewer())
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	created, err := storage.CreateRun(context.Background(), store.Run{Scope: scope, ID: "run-1", Kind: "AgentRun", DefinitionDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", RequestedBy: "user-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.AppendEvent(context.Background(), store.Event{Scope: scope, RunID: created.ID, Type: "run.started", Payload: []byte(`{"token":"secret-event"}`)}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-1/events"
	list := request(t, handler, http.MethodGet, path, "", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "run.started") || strings.Contains(list.Body.String(), "secret-event") {
		t.Fatalf("event list status=%d body=%s", list.Code, list.Body.String())
	}
	streamReq := httptest.NewRequest(http.MethodGet, path+"?stream=true&follow=false", nil)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set("X-Request-ID", "stream-request")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, streamReq)
	if stream.Code != http.StatusOK || !strings.HasPrefix(stream.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(stream.Body.String(), "event: run.started") {
		t.Fatalf("SSE status=%d headers=%v body=%s", stream.Code, stream.Header(), stream.Body.String())
	}
}

func TestAuthenticatorErrorMapsWithoutLeakage(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	handler := newTestHandler(storage, testAuthenticator{err: errors.New("backend credentials: super-secret")})
	response := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-1", "", "")
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "super-secret") {
		t.Fatalf("auth error status=%d body=%s", response.Code, response.Body.String())
	}
	if len(storage.auditSnapshot()) != 1 || storage.auditSnapshot()[0].Decision != "denied" {
		t.Fatalf("authentication denial was not audited")
	}
}

func TestPrincipalContextIsAvailableToAuthenticatorBackedHandler(t *testing.T) {
	principal := authz.Principal{ID: "context-user", ProjectRoles: map[string]authz.Role{"org-a/project-a": authz.RoleViewer}}
	authenticator := &contextCheckingAuthenticator{principal: principal}
	handler := newTestHandler(store.NewMemory(), authenticator)
	response := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-1", "", "")
	if response.Code != http.StatusNotFound || !authenticator.sawRequest {
		t.Fatalf("expected authenticated not-found request, status=%d saw=%v", response.Code, authenticator.sawRequest)
	}
}

type contextCheckingAuthenticator struct {
	principal  authz.Principal
	sawRequest bool
}

func (a *contextCheckingAuthenticator) Authenticate(r *http.Request) (authz.Principal, error) {
	if r.Context() == nil {
		return authz.Principal{}, errors.New("missing request context")
	}
	a.sawRequest = true
	return a.principal, nil
}

func TestRunSignalsAreAuthorizedStrictAndIdempotent(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	signaler := &recordingRunSignaler{}
	handler := New(storage, editor(), Options{RunSignaler: signaler, MaxBodyBytes: 4096})
	createSignalRun(t, storage, "Running")
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/cancel"
	firstRequest := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"operator requested stop"}`))
	firstRequest.Header.Set("Content-Type", "application/json")
	firstRequest.Header.Set("Idempotency-Key", "cancel-1")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, firstRequest)
	if firstResponse.Code != http.StatusAccepted {
		t.Fatalf("cancel status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	if signals := signaler.snapshot(); len(signals) != 1 || signals[0].IdempotencyKey != hashString("cancel-1") {
		t.Fatalf("raw idempotency key crossed delivery boundary: %#v", signals)
	}
	secondRequest := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"operator requested stop"}`))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondRequest.Header.Set("Idempotency-Key", "cancel-1")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusOK || len(signaler.snapshot()) != 1 {
		t.Fatalf("idempotent cancel status=%d signals=%#v body=%s", secondResponse.Code, signaler.snapshot(), secondResponse.Body.String())
	}
	conflictRequest := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"different"}`))
	conflictRequest.Header.Set("Content-Type", "application/json")
	conflictRequest.Header.Set("Idempotency-Key", "cancel-1")
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflictRequest)
	if conflictResponse.Code != http.StatusConflict || !strings.Contains(conflictResponse.Body.String(), "idempotency_conflict") {
		t.Fatalf("idempotency conflict status=%d body=%s", conflictResponse.Code, conflictResponse.Body.String())
	}
	unknown := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"stop","prompt":"do not accept"}`))
	unknown.Header.Set("Content-Type", "application/json")
	unknownResponse := httptest.NewRecorder()
	handler.ServeHTTP(unknownResponse, unknown)
	if unknownResponse.Code != http.StatusBadRequest || strings.Contains(unknownResponse.Body.String(), "do not accept") {
		t.Fatalf("strict signal status=%d body=%s", unknownResponse.Code, unknownResponse.Body.String())
	}
	events, err := storage.ListEvents(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "run-signal", 0)
	if err != nil || len(events) != 2 || events[0].Type != "run.signal_requested" || events[1].Type != "run.signal_accepted" {
		t.Fatalf("unexpected signal events=%#v err=%v", events, err)
	}
}

func TestPendingRunSignalIsSafelyRedelivered(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	signaler := &recordingRunSignaler{}
	handler := New(storage, editor(), Options{RunSignaler: signaler, MaxBodyBytes: 4096})
	createSignalRun(t, storage, "Running")
	claim, err := storage.ClaimRunSignal(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "run-signal", hashString("recover-1"), runSignalFingerprint(RunSignal{Kind: signalCancel, Target: "workflow", Reason: "recover delivery"}), hashString("expired-token"), time.Nanosecond)
	if err != nil || !claim.Owner {
		t.Fatalf("seed pending claim=%#v err=%v", claim, err)
	}
	time.Sleep(2 * time.Millisecond)
	req := httptest.NewRequest(http.MethodPost, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/cancel", strings.NewReader(`{"reason":"recover delivery"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "recover-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted || len(signaler.snapshot()) != 1 {
		t.Fatalf("pending retry status=%d signals=%#v body=%s", response.Code, signaler.snapshot(), response.Body.String())
	}
}

func TestConcurrentRunSignalsHaveOneDeliveryOwner(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	signaler := &blockingRunSignaler{started: make(chan struct{}, 1), release: make(chan struct{})}
	handler := New(storage, editor(), Options{RunSignaler: signaler, MaxBodyBytes: 4096})
	createSignalRun(t, storage, "Running")
	path := "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/cancel"
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"one owner"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "concurrent-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		firstDone <- response
	}()
	select {
	case <-signaler.started:
	case <-time.After(time.Second):
		t.Fatal("first signal did not reach delivery boundary")
	}
	for i := 0; i < 8; i++ {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason":"one owner"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "concurrent-1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "signal_in_flight") {
			t.Fatalf("concurrent response=%d body=%s", response.Code, response.Body.String())
		}
	}
	if got := signaler.callCount(); got != 1 {
		t.Fatalf("delivery calls while first is blocked=%d", got)
	}
	close(signaler.release)
	if response := <-firstDone; response.Code != http.StatusAccepted {
		t.Fatalf("first response=%d body=%s", response.Code, response.Body.String())
	}
	if got := signaler.callCount(); got != 1 {
		t.Fatalf("delivery calls=%d, want one", got)
	}
}

func TestApprovalAndReplySignalsUseRoleAndChildTarget(t *testing.T) {
	storage := &auditRecordingStore{Store: store.NewMemory()}
	signaler := &recordingRunSignaler{}
	handler := New(storage, approver(), Options{RunSignaler: signaler, MaxBodyBytes: 4096})
	createSignalRun(t, storage, "WaitingApproval")
	approvalPath := "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/approval"
	approval := request(t, handler, http.MethodPost, approvalPath, "application/json", `{"approvalId":"approval-1","stepId":"release","decision":"approved"}`)
	if approval.Code != http.StatusAccepted {
		t.Fatalf("workflow approval status=%d body=%s", approval.Code, approval.Body.String())
	}
	replyPath := "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/reply"
	replyHandler := New(storage, editor(), Options{RunSignaler: signaler, MaxBodyBytes: 4096})
	reply := request(t, replyHandler, http.MethodPost, replyPath, "application/json", `{"target":"agent","stepId":"test","taskId":"task-1","replyRef":"s3://org-a/project-a/replies/r1.json"}`)
	if reply.Code != http.StatusAccepted {
		t.Fatalf("agent reply status=%d body=%s", reply.Code, reply.Body.String())
	}
	signals := signaler.snapshot()
	if len(signals) != 2 || signals[0].Kind != signalApproval || signals[0].Target != "workflow" || signals[1].Kind != signalReply || signals[1].Target != "agent" || signals[1].StepID != "test" {
		t.Fatalf("unexpected signals=%#v", signals)
	}
	events, err := storage.ListEvents(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "run-signal", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), "s3://org-a/project-a/replies/r1.json") {
			t.Fatal("reply reference should not be copied into signal audit payload")
		}
	}

	denied := New(storage, viewer(), Options{RunSignaler: signaler})
	deniedResponse := request(t, denied, http.MethodPost, approvalPath, "application/json", `{"approvalId":"approval-2","decision":"denied"}`)
	if deniedResponse.Code != http.StatusForbidden || len(signaler.snapshot()) != 2 {
		t.Fatalf("approval authorization status=%d signals=%#v", deniedResponse.Code, signaler.snapshot())
	}
}

func TestRunSignalFailsClosedWhenWorkflowSignalerMissingOrRunTerminal(t *testing.T) {
	storage := store.NewMemory()
	createSignalRun(t, storage, "Succeeded")
	handler := New(storage, editor(), Options{})
	terminal := request(t, handler, http.MethodPost, "/api/v1alpha1/organizations/org-a/projects/project-a/runs/run-signal/cancel", "application/json", `{}`)
	if terminal.Code != http.StatusConflict {
		t.Fatalf("terminal signal status=%d body=%s", terminal.Code, terminal.Body.String())
	}
	_, err := storage.SetRunStatus(context.Background(), store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}, "run-signal", "Cancelled", "")
	if err == nil {
		t.Fatal("terminal run should not transition")
	}
}

func TestArtifactCatalogAndContentAreAuthenticatedScopedAndDownloadOnly(t *testing.T) {
	storage := store.NewMemory()
	scope := store.Scope{OrganizationID: "org-a", ProjectID: "project-a"}
	if _, err := storage.CreateRun(context.Background(), store.Run{
		Scope: scope, ID: "run-artifact", Kind: "AgentRun",
		DefinitionDigest: "sha256:" + strings.Repeat("a", 64), RequestedBy: "user-1",
	}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	objects, err := artifact.NewLocalObjectClient(root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(objects, artifact.Config{Bucket: "local", Prefix: "runs", MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := artifacts.Put(context.Background(), artifact.PutRequest{
		OrganizationID: scope.OrganizationID, ProjectID: scope.ProjectID, RunID: "run-artifact",
		Name: "report.txt", MediaType: "text/plain", Body: strings.NewReader("safe artifact content"),
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := artifactcatalog.NewRunOutput(artifactcatalog.PublishInput{
		ArtifactID: metadata.ID, VersionID: metadata.ID, Title: metadata.Name,
		URI:    "artifact://catalog/" + metadata.ID + "/" + metadata.ID,
		Digest: metadata.Digest, MediaType: metadata.MediaType,
		SizeBytes: metadata.SizeBytes, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.PutArtifactVersion(context.Background(), store.ArtifactVersion{
		Scope: scope, ArtifactID: contract.ArtifactID, VersionID: contract.VersionID,
		RunID: "run-artifact", VersionNumber: 1, Document: document,
		ContentObjectKey: metadata.ObjectKey, SourceObjectKey: metadata.ObjectKey,
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(storage, viewer(), Options{ArtifactStore: artifacts})
	base := "/api/v1alpha1/organizations/org-a/projects/project-a/artifacts"

	list := request(t, handler, http.MethodGet, base, "", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), metadata.ID) || strings.Contains(list.Body.String(), metadata.ObjectKey) {
		t.Fatalf("artifact list status=%d body=%s", list.Code, list.Body.String())
	}
	detail := request(t, handler, http.MethodGet, base+"/"+metadata.ID, "", "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "report.txt") {
		t.Fatalf("artifact detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	content := request(t, handler, http.MethodGet, base+"/"+metadata.ID+"/versions/"+metadata.ID+"/content", "", "")
	if content.Code != http.StatusOK || content.Body.String() != "safe artifact content" {
		t.Fatalf("artifact content status=%d body=%q", content.Code, content.Body.String())
	}
	if got := content.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("content disposition=%q", got)
	}
	if got := content.Header().Get("Content-Security-Policy"); got != "default-src 'none'; sandbox" {
		t.Fatalf("content CSP=%q", got)
	}
	if got := content.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff=%q", got)
	}

	crossTenant := request(t, handler, http.MethodGet, "/api/v1alpha1/organizations/org-b/projects/project-b/artifacts/"+metadata.ID, "", "")
	if crossTenant.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant artifact status=%d body=%s", crossTenant.Code, crossTenant.Body.String())
	}
}

func TestArtifactContentIntegrityIsVerifiedBeforeResponse(t *testing.T) {
	content := "stored bytes"
	wrongDigest := "sha256:" + strings.Repeat("0", 64)
	verified, err := stageVerifiedArtifact(strings.NewReader(content), int64(len(content)), wrongDigest)
	if err == nil {
		name := verified.Name()
		_ = verified.Close()
		_ = os.Remove(name)
		t.Fatal("expected digest mismatch")
	}

	actual := sha256.Sum256([]byte(content))
	verified, err = stageVerifiedArtifact(strings.NewReader(content), int64(len(content)), "sha256:"+hex.EncodeToString(actual[:]))
	if err != nil {
		t.Fatal(err)
	}
	name := verified.Name()
	defer os.Remove(name)
	defer verified.Close()
	read, err := io.ReadAll(verified)
	if err != nil || string(read) != content {
		t.Fatalf("verified content=%q err=%v", read, err)
	}
}
