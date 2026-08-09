package brokerfactory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerbridge"
	"github.com/Astatide1337/agents-gateway/v2/pkg/brokerdispatch"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolbroker"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
	"github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

const (
	testOrg       = "org"
	testProject   = "project"
	testUser      = "user"
	testRun       = "run"
	testRouteName = "openai"
	testModel     = "gpt-5"
	testSecret    = "sk-test-secret-that-must-not-leak"
)

type factoryFixture struct {
	store         *store.Memory
	artifacts     *artifact.Store
	factory       *Factory
	binding       runbroker.SessionBinding
	modelRef      workflow.RevisionRef
	toolRef       *workflow.RevisionRef
	credential    *spec.Credential
	credentialRef workflow.RevisionRef
}

func newFactoryFixture(t *testing.T, upstreamURL string) *factoryFixture {
	t.Helper()
	control := store.NewMemory()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	objects, err := artifact.NewLocalObjectClient(root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifact.New(objects, artifact.Config{
		Bucket:      "artifacts",
		Prefix:      "agw",
		MaxBytes:    1 << 20,
		DownloadTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &factoryFixture{
		store:     control,
		artifacts: artifacts,
		binding: runbroker.SessionBinding{
			OrgID: testOrg, ProjectID: testProject, UserID: testUser, RunID: testRun,
		},
	}
	if _, err := control.CreateRun(context.Background(), store.Run{
		Scope: store.Scope{OrganizationID: testOrg, ProjectID: testProject},
		ID:    testRun, Kind: spec.KindAgentRun, DefinitionDigest: "sha256:" + strings.Repeat("f", 64), RequestedBy: testUser,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.factory = &Factory{
		Store:     control,
		Artifacts: artifacts,
		Effects:   control,
		OpenAIURL: upstreamURL,
		LookupEnv: func(name string) (string, bool) {
			if name == "OPENAI_API_KEY" || name == "OPENROUTER_API_KEY" {
				return testSecret, true
			}
			return "", false
		},
	}
	fixture.credential = &spec.Credential{
		ResourceMeta: resourceMeta(spec.KindCredential, "openai-credential"),
		Spec: spec.CredentialSpec{
			Kind:      "openai-api-key",
			SecretRef: "env://OPENAI_API_KEY",
		},
	}
	fixture.credentialRef = applyResource(t, control, fixture.binding, fixture.credential)
	fixture.modelRef = applyResource(t, control, fixture.binding, &spec.ModelRoute{
		ResourceMeta: resourceMeta(spec.KindModelRoute, testRouteName),
		Spec: spec.ModelRouteSpec{Providers: []spec.ModelProvider{{
			Name: testRouteName, Kind: "openai-responses", Model: testModel, Credential: fixture.credentialRef.Name,
		}}},
	})
	return fixture
}

func resourceMeta(kind, name string) spec.ResourceMeta {
	return spec.ResourceMeta{
		TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: kind},
		Metadata: spec.ObjectMeta{Name: name},
	}
}

func applyResource(t *testing.T, source *store.Memory, binding runbroker.SessionBinding, resource spec.Resource) workflow.RevisionRef {
	t.Helper()
	digest, err := spec.RevisionDigest(resource)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(resource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.ApplyResource(context.Background(), store.Resource{
		Scope:    store.Scope{OrganizationID: binding.OrgID, ProjectID: binding.ProjectID},
		Kind:     resource.Meta().Kind,
		Name:     resource.Meta().Metadata.Name,
		Digest:   digest,
		Document: document,
	}); err != nil {
		t.Fatal(err)
	}
	return workflow.RevisionRef{Kind: resource.Meta().Kind, Name: resource.Meta().Metadata.Name, Digest: digest}
}

func newHandlerRequest(fixture *factoryFixture) brokerdispatch.HandlerRequest {
	return brokerdispatch.HandlerRequest{
		Binding: fixture.binding,
		Input: workflow.ScheduleRunnerTaskInput{
			OrganizationID: fixture.binding.OrgID,
			ProjectID:      fixture.binding.ProjectID,
			RunID:          fixture.binding.RunID,
			Contract:       workflow.ExecutionContract{ModelRoute: &fixture.modelRef},
		},
	}
}

func TestFactoryRejectsImmutableDigestMismatch(t *testing.T) {
	fixture := newFactoryFixture(t, "")
	request := newHandlerRequest(fixture)
	request.Input.Contract.ModelRoute.Digest = strings.Repeat("sha256:", 1) + strings.Repeat("0", 64)

	_, err := fixture.factory.NewHandler(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), string(brokerdispatch.HandlerSetupModelRoute)) {
		t.Fatalf("expected secret-safe model-route failure, got %v", err)
	}
}

func TestFactoryRequiresExactlyOneOpenAIResponsesProvider(t *testing.T) {
	tests := []struct {
		name      string
		providers []spec.ModelProvider
	}{
		{name: "none"},
		{name: "two", providers: []spec.ModelProvider{
			{Name: "one", Kind: "openai-responses", Model: testModel, Credential: "openai-credential"},
			{Name: "two", Kind: "openai-responses", Model: "gpt-5-mini", Credential: "openai-credential"},
		}},
		{name: "non-openai", providers: []spec.ModelProvider{{Name: "anthropic", Kind: "anthropic", Model: "claude", Credential: "openai-credential"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFactoryFixture(t, "")
			route := &spec.ModelRoute{ResourceMeta: resourceMeta(spec.KindModelRoute, "route-"+test.name), Spec: spec.ModelRouteSpec{Providers: test.providers}}
			ref := applyResource(t, fixture.store, fixture.binding, route)
			request := newHandlerRequest(fixture)
			request.Input.Contract.ModelRoute = &ref
			if _, err := fixture.factory.NewHandler(context.Background(), request); err == nil {
				t.Fatal("invalid provider cardinality or kind was accepted")
			}
		})
	}
}

func TestFactoryModelProxyInjectsCredentialAtLoopbackUpstream(t *testing.T) {
	var mu sync.Mutex
	var gotAuthorization, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuthorization = r.Header.Get("Authorization")
		gotBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response"}`)
	}))
	defer upstream.Close()

	fixture := newFactoryFixture(t, upstream.URL+"/v1/responses")
	handler, err := fixture.factory.NewHandler(context.Background(), newHandlerRequest(fixture))
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"model":"gpt-5","input":"hello"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer caller-token-must-not-forward")
	recorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"id":"resp_test","object":"response"}` {
		t.Fatalf("unexpected model proxy response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	authorization, body := gotAuthorization, gotBody
	mu.Unlock()
	if authorization != "Bearer "+testSecret {
		t.Fatalf("upstream did not receive injected credential: %q", authorization)
	}
	if strings.Contains(authorization, "caller-token-must-not-forward") || body != payload {
		t.Fatalf("caller credential or request body was mishandled: auth=%q body=%q", authorization, body)
	}
}

func TestFactoryRoutesExplicitOpenRouterResponsesProvider(t *testing.T) {
	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"openrouter-response","object":"response"}`)
	}))
	defer upstream.Close()

	fixture := newFactoryFixture(t, "http://127.0.0.1:1/v1/responses")
	fixture.factory.OpenRouterURL = upstream.URL + "/v1/responses"
	credentialRef := applyResource(t, fixture.store, fixture.binding, &spec.Credential{
		ResourceMeta: resourceMeta(spec.KindCredential, "openrouter-credential"),
		Spec:         spec.CredentialSpec{Kind: "openrouter-api-key", SecretRef: "env://OPENROUTER_API_KEY"},
	})
	routeRef := applyResource(t, fixture.store, fixture.binding, &spec.ModelRoute{
		ResourceMeta: resourceMeta(spec.KindModelRoute, "openrouter"),
		Spec: spec.ModelRouteSpec{Providers: []spec.ModelProvider{{
			Name: "openrouter", Kind: "openrouter-responses", Model: testModel, Credential: credentialRef.Name,
		}}},
	})
	request := newHandlerRequest(fixture)
	request.Input.Contract.ModelRoute = &routeRef
	handler, err := fixture.factory.NewHandler(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	proxyRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello"}`))
	proxyRequest.Header.Set("Content-Type", "application/json")
	handler.Handler.ServeHTTP(recorder, proxyRequest)
	if recorder.Code != http.StatusOK || gotAuthorization != "Bearer "+testSecret {
		t.Fatalf("OpenRouter Responses route failed: status=%d authorization=%q", recorder.Code, gotAuthorization)
	}
}

func TestFactoryMissingAndInvalidEnvCredentialAreSanitized(t *testing.T) {
	tests := []struct {
		name      string
		secretRef string
		lookup    func(string) (string, bool)
	}{
		{name: "missing environment", secretRef: "env://MISSING_OPENAI_KEY", lookup: func(string) (string, bool) { return "", false }},
		{name: "invalid environment reference", secretRef: "env://not-a-valid-name", lookup: func(string) (string, bool) { return testSecret, true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCalls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			fixture := newFactoryFixture(t, upstream.URL+"/v1/responses")
			credential := &spec.Credential{ResourceMeta: resourceMeta(spec.KindCredential, "bad-credential"), Spec: spec.CredentialSpec{Kind: "openai-api-key", SecretRef: test.secretRef}}
			credentialRef := applyResource(t, fixture.store, fixture.binding, credential)
			route := &spec.ModelRoute{ResourceMeta: resourceMeta(spec.KindModelRoute, "bad-route"), Spec: spec.ModelRouteSpec{Providers: []spec.ModelProvider{{Name: "bad", Kind: "openai-responses", Model: testModel, Credential: credentialRef.Name}}}}
			routeRef := applyResource(t, fixture.store, fixture.binding, route)
			fixture.factory.LookupEnv = test.lookup
			request := newHandlerRequest(fixture)
			request.Input.Contract.ModelRoute = &routeRef
			handler, err := fixture.factory.NewHandler(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			modelRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
			modelRequest.Header.Set("Content-Type", "application/json")
			handler.Handler.ServeHTTP(recorder, modelRequest)
			if recorder.Code != http.StatusBadGateway || upstreamCalls != 0 {
				t.Fatalf("credential failure reached upstream or returned wrong status: status=%d calls=%d body=%s", recorder.Code, upstreamCalls, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), testSecret) || strings.Contains(recorder.Body.String(), test.secretRef) {
				t.Fatalf("credential details leaked in response: %s", recorder.Body.String())
			}
		})
	}
}

func TestFactoryMCPCatalogDeniesUnlistedToolsAndApprovesWritesByDefault(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"upstream","result":{"content":[]}}`)
	}))
	defer upstream.Close()
	fixture := newFactoryFixture(t, "")
	toolSet := &spec.ToolSet{
		ResourceMeta: resourceMeta(spec.KindToolSet, "catalog"),
		Spec: spec.ToolSetSpec{Servers: []spec.MCPServer{{
			Name: "local", Ref: upstream.URL, Tools: []spec.ToolGrant{{
				Name: "write_file", Effect: string(toolpolicy.EffectWrite),
				InputSchema: map[string]any{"type": "object"},
			}},
		}}},
	}
	toolRef := applyResource(t, fixture.store, fixture.binding, toolSet)
	request := newHandlerRequest(fixture)
	request.Input.Contract.ToolSet = &toolRef
	handler, err := fixture.factory.NewHandler(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	list := mcpFactoryRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	listRecorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), `"name":"write_file"`) || strings.Contains(listRecorder.Body.String(), "local") {
		t.Fatalf("catalog was not exact and sanitized: status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	unknown := mcpFactoryRequest(http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"shell","arguments":{}}}`)
	unknownRecorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(unknownRecorder, unknown)
	if !strings.Contains(unknownRecorder.Body.String(), `"code":-32602`) || upstreamCalls != 0 {
		t.Fatalf("unlisted tool was not denied before upstream: status=%d body=%s calls=%d", unknownRecorder.Code, unknownRecorder.Body.String(), upstreamCalls)
	}

	write := mcpFactoryRequest(http.MethodPost, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_file","arguments":{},"_meta":{"agw":{"call_id":"write-1"}}}}`)
	writeRecorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(writeRecorder, write)
	if !strings.Contains(writeRecorder.Body.String(), `"code":-32002`) || upstreamCalls != 0 {
		t.Fatalf("write tool did not default to approval-required: status=%d body=%s calls=%d", writeRecorder.Code, writeRecorder.Body.String(), upstreamCalls)
	}
}

func TestFactoryThreadsExactToolArgumentsIntoBrokerPolicy(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "factory-test-session")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`, request.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			upstreamCalls++
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[]}}`, request.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, request.ID)
		}
	}))
	defer upstream.Close()

	arguments, err := spec.NewJSONArguments(json.RawMessage(`{"owner":"acme","repo":"gateway","nested":{"enabled":true},"huge":90071992547409931234567890.1234500}`))
	if err != nil {
		t.Fatal(err)
	}
	fixture := newFactoryFixture(t, "")
	toolSet := &spec.ToolSet{
		ResourceMeta: resourceMeta(spec.KindToolSet, "exact-catalog"),
		Spec: spec.ToolSetSpec{Servers: []spec.MCPServer{{
			Name: "local", Ref: upstream.URL, Tools: []spec.ToolGrant{{
				Name: "create_branch", Effect: string(toolpolicy.EffectRead), Approval: string(toolpolicy.ApprovalAllow), Arguments: arguments,
			}},
		}}},
	}
	toolRef := applyResource(t, fixture.store, fixture.binding, toolSet)
	request := newHandlerRequest(fixture)
	request.Input.Contract.ToolSet = &toolRef
	handler, err := fixture.factory.NewHandler(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	matching := mcpFactoryRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_branch","arguments":{"huge":90071992547409931234567890.12345,"nested":{"enabled":true},"repo":"gateway","owner":"acme"}}}`)
	matchingRecorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(matchingRecorder, matching)
	if matchingRecorder.Code != http.StatusOK || strings.Contains(matchingRecorder.Body.String(), `"error"`) || upstreamCalls != 1 {
		t.Fatalf("matching exact arguments failed: status=%d body=%s upstream=%d", matchingRecorder.Code, matchingRecorder.Body.String(), upstreamCalls)
	}

	mismatch := mcpFactoryRequest(http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_branch","arguments":{"huge":90071992547409931234567890.12345,"nested":{"enabled":true},"repo":"gateway","owner":"acme","extra":"no"}}}`)
	mismatchRecorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(mismatchRecorder, mismatch)
	if !strings.Contains(mismatchRecorder.Body.String(), `"code":-32001`) || upstreamCalls != 1 || strings.Contains(mismatchRecorder.Body.String(), "no") {
		t.Fatalf("mismatched exact arguments was not denied before upstream: status=%d body=%s upstream=%d", mismatchRecorder.Code, mismatchRecorder.Body.String(), upstreamCalls)
	}
}

func mcpFactoryRequest(method, body string) *http.Request {
	request := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	return request
}

func TestFactoryArtifactUploadReturnsImmutableLocalReference(t *testing.T) {
	fixture := newFactoryFixture(t, "")
	handler, err := fixture.factory.NewHandler(context.Background(), newHandlerRequest(fixture))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"run_id":"run","status":"completed","message":"ok"}`)
	request := httptest.NewRequest(http.MethodPut, "/v1/artifacts/output", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/vnd.agw.run-output+json")
	recorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("artifact upload failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var reference workflow.ArtifactRef
	decoder := json.NewDecoder(recorder.Body)
	if err := decoder.Decode(&reference); err != nil {
		t.Fatal(err)
	}
	if err := reference.Validate(); err != nil {
		t.Fatalf("factory returned invalid immutable reference: %v (%#v)", err, reference)
	}
	if reference.URI == "" || !strings.HasPrefix(reference.URI, "artifact://catalog/") || reference.SizeBytes != int64(len(body)) {
		t.Fatalf("unexpected artifact reference: %#v", reference)
	}
	versions, err := fixture.store.ListArtifactVersions(context.Background(), store.Scope{OrganizationID: testOrg, ProjectID: testProject}, reference.ID)
	if err != nil || len(versions) != 1 || versions[0].RunID != testRun || strings.Contains(string(versions[0].Document), versions[0].ContentObjectKey) {
		t.Fatalf("catalog versions=%#v err=%v", versions, err)
	}
}

func TestFactoryPublishesAuthoredInteractiveArtifactWithExplicitScriptCapability(t *testing.T) {
	fixture := newFactoryFixture(t, "")
	handler, err := fixture.factory.NewHandler(context.Background(), newHandlerRequest(fixture))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := artifact.PublishDescriptor{
		Schema: "agents-gateway.artifact.v1", Title: "Interactive report",
		Description: "A standalone local interaction.", ContentKind: artifactcatalog.ContentKindInteractive,
		Capabilities: []artifactcatalog.Capability{artifactcatalog.CapabilitySandboxedScripts},
	}
	metadata, err := artifact.EncodePublishDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, brokerbridge.ArtifactCreatePath, strings.NewReader(`<button onclick="document.body.dataset.ok='1'">Run</button>`))
	request.Header.Set("Content-Type", "text/html")
	request.Header.Set(artifact.PublishMetadataHeader, metadata)
	recorder := httptest.NewRecorder()
	handler.Handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("authored artifact upload status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var reference workflow.ArtifactRef
	if err := json.NewDecoder(recorder.Body).Decode(&reference); err != nil {
		t.Fatal(err)
	}
	versions, err := fixture.store.ListArtifactVersions(context.Background(), store.Scope{OrganizationID: testOrg, ProjectID: testProject}, reference.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("catalog versions=%#v err=%v", versions, err)
	}
	var published artifactcatalog.Version
	if err := json.Unmarshal(versions[0].Document, &published); err != nil {
		t.Fatal(err)
	}
	if published.Manifest.ContentKind != artifactcatalog.ContentKindInteractive || !published.Manifest.Security.Allows(artifactcatalog.CapabilitySandboxedScripts) || published.Manifest.Security.Allows(artifactcatalog.CapabilityNetwork) {
		t.Fatalf("unexpected authored artifact policy: %#v", published.Manifest)
	}
}

func TestPolicyDigestIsDeterministicAndContentBound(t *testing.T) {
	contract := workflow.ExecutionContract{
		Agent:          workflow.RevisionRef{Kind: spec.KindAgent, Name: "agent", Digest: "sha256:" + strings.Repeat("a", 64)},
		SandboxProfile: workflow.RevisionRef{Kind: spec.KindSandboxProfile, Name: "sandbox", Digest: "sha256:" + strings.Repeat("b", 64)},
		ModelRoute:     &workflow.RevisionRef{Kind: spec.KindModelRoute, Name: "model", Digest: "sha256:" + strings.Repeat("c", 64)},
		ToolSet:        &workflow.RevisionRef{Kind: spec.KindToolSet, Name: "tools", Digest: "sha256:" + strings.Repeat("d", 64)},
		SkillSet:       &workflow.RevisionRef{Kind: spec.KindSkillSet, Name: "skills", Digest: "sha256:" + strings.Repeat("e", 64)},
	}
	first := policyDigest(contract)
	second := policyDigest(contract)
	if first != second {
		t.Fatalf("policy digest is not deterministic: %q != %q", first, second)
	}
	material := "agents-gateway.broker-policy.v1\x00" + contract.Agent.Digest + "\x00" + contract.SandboxProfile.Digest
	for _, ref := range []*workflow.RevisionRef{contract.ModelRoute, contract.ToolSet, contract.SkillSet} {
		material += "\x00" + ref.Kind + "\x00" + ref.Name + "\x00" + ref.Digest
	}
	wantSum := sha256.Sum256([]byte(material))
	want := "sha256:" + hex.EncodeToString(wantSum[:])
	if first != want {
		t.Fatalf("policy digest contract changed unexpectedly: got=%q want=%q", first, want)
	}
	contract.ToolSet.Digest = "sha256:" + strings.Repeat("f", 64)
	if policyDigest(contract) == first {
		t.Fatal("policy digest did not change when a capability revision changed")
	}
}

func TestStoreAuditSinkPersistsScopedBoundedMCPRecord(t *testing.T) {
	control := store.NewMemory()
	sink := storeAuditSink{storage: control}
	record := toolbroker.AuditRecord{
		OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run",
		Server: "github", Tool: "create_branch", Resource: "github:repo:owner/repo",
		Phase: "outcome", Outcome: "succeeded", Decision: "succeeded",
		RequestDigest: "sha256:" + strings.Repeat("a", 64), EffectDigest: "sha256:" + strings.Repeat("b", 64),
	}
	if err := sink.AppendAudit(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	events, more, err := control.ListAudit(context.Background(), store.Scope{OrganizationID: "org", ProjectID: "project"}, store.Page{Limit: 10})
	if err != nil || more || len(events) != 1 {
		t.Fatalf("durable audit events=%#v more=%v err=%v", events, more, err)
	}
	if events[0].PrincipalID != "user" || events[0].Action != "mcp.tool.outcome" || events[0].ResourceType != "mcp.tool" || events[0].Decision != "succeeded" {
		t.Fatalf("unexpected durable audit scope: %#v", events[0])
	}
	var metadata map[string]any
	if err := json.Unmarshal(events[0].Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"run_id": "run", "server": "github", "tool": "create_branch", "resource": "github:repo:owner/repo", "outcome": "succeeded"} {
		if metadata[key] != want {
			t.Fatalf("metadata[%q]=%v, want %q", key, metadata[key], want)
		}
	}
}
