package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/runtimeevents"
	"github.com/Astatide1337/agents-gateway/v3/pkg/proto"
	"github.com/Astatide1337/agents-gateway/v3/pkg/runtimeproto"
)

const (
	testRunUID     = "run-uid"
	testSpecDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type testCredentials struct {
	mu       sync.Mutex
	values   map[string][]byte
	err      error
	requests []string
}

func (c *testCredentials) ResolveToken(ctx context.Context, ref string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.requests = append(c.requests, ref)
	value := append([]byte(nil), c.values[ref]...)
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, errors.New("credential is absent")
	}
	return value, nil
}

type testLedger struct {
	mu         sync.Mutex
	claimed    map[string]bool
	claims     []effects.Claim
	outcomes   []effects.Outcome
	unknown    bool
	commitErr  error
	claimError error
}

func newTestLedger() *testLedger {
	return &testLedger{claimed: map[string]bool{}}
}

func (l *testLedger) Claim(ctx context.Context, claim effects.Claim) (effects.ClaimDecision, error) {
	if err := ctx.Err(); err != nil {
		return effects.ClaimDecision{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claimError != nil {
		return effects.ClaimDecision{}, l.claimError
	}
	if l.unknown {
		return effects.ClaimDecision{Unknown: true, Claim: claim}, effects.ErrUnknown
	}
	if l.claimed[claim.EffectKey] {
		return effects.ClaimDecision{Claim: claim}, nil
	}
	l.claimed[claim.EffectKey] = true
	l.claims = append(l.claims, claim)
	return effects.ClaimDecision{Execute: true, Claim: claim}, nil
}

func (l *testLedger) Commit(ctx context.Context, outcome effects.Outcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outcomes = append(l.outcomes, outcome)
	return l.commitErr
}

func (l *testLedger) snapshot() (claims []effects.Claim, outcomes []effects.Outcome) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]effects.Claim(nil), l.claims...), append([]effects.Outcome(nil), l.outcomes...)
}

type testObjectStore struct {
	mu       sync.Mutex
	objects  map[string][]byte
	contents map[string]string
	uri      string
}

func newTestObjectStore() *testObjectStore {
	return &testObjectStore{objects: map[string][]byte{}, contents: map[string]string{}, uri: "s3://test-bucket"}
}

func (s *testObjectStore) Put(_ context.Context, key string, body []byte, contentType string) (bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[key]; ok {
		return false, s.uri + "/" + key, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	s.contents[key] = contentType
	return true, s.uri + "/" + key, nil
}

func (s *testObjectStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, runtimeevents.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func (s *testObjectStore) Create(ctx context.Context, key string, body []byte, contentType string) (bool, error) {
	created, _, err := s.Put(ctx, key, body, contentType)
	return created, err
}

type testTransport struct {
	mu             sync.Mutex
	requests       []*http.Request
	bodies         [][]byte
	mcpCalls       int
	modelCalls     int
	failMCP        bool
	failModel      bool
	modelContent   string
	modelBody      string
	modelResponder func(int, []byte) (string, string)
	mcpResult      string
	mcpEncoding    string
	invalidMCP     bool
	checkSessionID bool
}

func (t *testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.requests = append(t.requests, request)
	t.bodies = append(t.bodies, append([]byte(nil), body...))
	if request.URL.Path == "/mcp" {
		t.mcpCalls++
	}
	if request.URL.Path == "/v1/responses" || request.URL.Path == "/api/v1/responses" {
		t.modelCalls++
	}
	failMCP, failModel, invalidMCP, checkSession := t.failMCP, t.failModel, t.invalidMCP, t.checkSessionID
	modelContent, modelBody, modelResponder, modelCall, mcpResult, mcpEncoding := t.modelContent, t.modelBody, t.modelResponder, t.modelCalls, t.mcpResult, t.mcpEncoding
	t.mu.Unlock()

	if request.URL.Path == "/mcp" {
		if failMCP {
			return nil, errors.New("upstream failed with bearer=super-secret")
		}
		var payload struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		if checkSession && payload.Method != "initialize" && request.Header.Get("Mcp-Session-Id") != "session-1" {
			return nil, errors.New("missing session")
		}
		switch payload.Method {
		case "initialize":
			if invalidMCP {
				return jsonResponse(http.StatusOK, `{"jsonrpc":"2.0","id":"wrong","result":{}}`, "application/json", "session-1"), nil
			}
			return jsonResponse(http.StatusOK, `{"jsonrpc":"2.0","id":"`+payload.ID+`","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`, "application/json", "session-1"), nil
		case "notifications/initialized":
			return emptyResponse(http.StatusAccepted), nil
		case "tools/call":
			result := mcpResult
			if result == "" {
				result = `{"content":[{"type":"text","text":"ok"}],"isError":false}`
			}
			response := jsonResponse(http.StatusOK, `{"jsonrpc":"2.0","id":"`+payload.ID+`","result":`+result+`}`, "application/json", "session-1")
			if mcpEncoding != "" {
				response.Header.Set("Content-Encoding", mcpEncoding)
			}
			return response, nil
		default:
			return nil, errors.New("unexpected MCP method")
		}
	}
	if request.URL.Path == "/v1/responses" || request.URL.Path == "/api/v1/responses" {
		if failModel {
			return nil, errors.New("model failed with bearer=super-secret")
		}
		if modelResponder != nil {
			contentType, responseBody := modelResponder(modelCall, body)
			return jsonResponse(http.StatusOK, responseBody, contentType, ""), nil
		}
		if modelContent == "" {
			modelContent = "application/json"
		}
		if modelBody == "" {
			modelBody = `{"id":"resp-1","object":"response","usage":{"input_tokens":3,"output_tokens":2},"output":[]}`
		}
		return jsonResponse(http.StatusOK, modelBody, modelContent, ""), nil
	}
	if request.URL.Path == "/v1/messages" || request.URL.Path == "/api/v1/messages" {
		if failModel {
			return nil, errors.New("model failed with bearer=super-secret")
		}
		if modelContent == "" {
			modelContent = "application/json"
		}
		if modelBody == "" {
			modelBody = `{"id":"msg-1","type":"message","role":"assistant","model":"test-model","content":[],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`
		}
		return jsonResponse(http.StatusOK, modelBody, modelContent, ""), nil
	}
	return nil, errors.New("unexpected upstream endpoint")
}

func jsonResponse(status int, body, contentType, sessionID string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	if contentType != "" {
		response.Header.Set("Content-Type", contentType)
	}
	if sessionID != "" {
		response.Header.Set("Mcp-Session-Id", sessionID)
	}
	return response
}

func emptyResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
}

type testBrokerOptions struct {
	transport           *testTransport
	credentials         *testCredentials
	ledger              *testLedger
	artifacts           *testObjectStore
	requireApproval     bool
	maxToolCalls        int64
	maxModelRequests    int64
	maxModelTokens      int64
	providerKind        string
	providerEndpoint    string
	maxCostUSD          string
	withToolCredential  bool
	withModelCredential bool
	withoutPricing      bool
}

func newTestConfig(options testBrokerOptions) Config {
	if options.transport == nil {
		options.transport = &testTransport{}
	}
	if options.credentials == nil {
		options.credentials = &testCredentials{values: map[string][]byte{}}
	}
	if options.ledger == nil {
		options.ledger = newTestLedger()
	}
	if options.artifacts == nil {
		options.artifacts = newTestObjectStore()
	}
	if options.providerKind == "" {
		options.providerKind = "openai-responses"
	}
	if options.providerEndpoint == "" {
		options.providerEndpoint = "https://api.example.test/v1/responses"
	}
	if options.maxCostUSD == "" {
		options.maxCostUSD = "1.000000"
	}
	toolCredential := ""
	modelCredential := ""
	if options.withToolCredential {
		toolCredential = "mcp-secret"
		options.credentials.values[toolCredential] = []byte("mcp-token")
	}
	if options.withModelCredential {
		modelCredential = "model-secret"
		options.credentials.values[modelCredential] = []byte("model-token")
	}
	toolSet := v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{
		Name: "github", Ref: "https://mcp.example.test/mcp", CredentialsRef: toolCredential,
		Tools: []v1alpha1.ToolDefinition{
			{Name: "read_issue", Effect: v1alpha1.EffectRead, ExactArguments: map[string]apix.JSON{"issue": {Raw: []byte(`"427"`)}}},
			{Name: "read_status", Effect: v1alpha1.EffectRead},
			{Name: "write_issue", Effect: v1alpha1.EffectWrite, ExactArguments: map[string]apix.JSON{
				"issue": {Raw: []byte(`"427"`)}, "force": {Raw: []byte(`true`)},
			}},
		},
	}}, Profiles: []v1alpha1.ToolProfile{
		{Name: v1alpha1.ToolProfileExplore, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "read_issue"}, {Server: "github", Tool: "read_status"}}},
		{Name: v1alpha1.ToolProfileEdit, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "read_issue"}, {Server: "github", Tool: "write_issue"}}},
		{Name: v1alpha1.ToolProfileVerify, Tools: []v1alpha1.ToolRef{{Server: "github", Tool: "read_issue"}}},
	}, MaxToolsPerPhase: 3}
	route := v1alpha1.ModelRouteSpec{Providers: []v1alpha1.ModelProvider{{
		Name: "openai", Kind: options.providerKind, Model: "test-model", CredentialRef: modelCredential, Priority: 1,
	}}, Budget: v1alpha1.ModelBudget{MaxCostUSD: options.maxCostUSD, MaxTokens: 1000, MaxRequests: 10}}
	config := Config{
		RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA,
		ToolSet: toolSet, ModelRoute: route, Credentials: options.credentials,
		Effects: options.ledger, Artifacts: options.artifacts, ProviderEndpoints: map[string]string{"openai": options.providerEndpoint},
		WorkspaceRoot: "/workspace",
		Pricing:       PricingTable{"openai": {InputMicrosPerToken: 1, OutputMicrosPerToken: 2}},
		MaxToolCalls:  options.maxToolCalls, MaxModelRequests: options.maxModelRequests, MaxModelTokens: options.maxModelTokens,
		MaxCostUSD:                  options.maxCostUSD,
		RequireApprovalForMutations: options.requireApproval, transport: options.transport,
		PhaseTransitionAuthorizer: PhaseTransitionAuthorizerFunc(func(context.Context) error { return nil }),
	}
	if options.withoutPricing {
		config.Pricing = nil
	}
	return config
}

func newTestBroker(t *testing.T, options testBrokerOptions) *Broker {
	t.Helper()
	broker, err := New(newTestConfig(options))
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func TestNewRejectsUnsafeAndUnsupportedResolvedContracts(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want error
	}{
		{name: "private MCP endpoint", edit: func(config *Config) { config.ToolSet.Servers[0].Ref = "https://127.0.0.1/mcp" }, want: ErrInvalidConfig},
		{name: "MCP query", edit: func(config *Config) { config.ToolSet.Servers[0].Ref = "https://mcp.example.test/mcp?token=secret" }, want: ErrInvalidConfig},
		{name: "unsupported provider", edit: func(config *Config) { config.ModelRoute.Providers[0].Kind = "unknown-wire" }, want: ErrUnsupportedProvider},
		{name: "missing pricing", edit: func(config *Config) { config.Pricing = nil }, want: ErrInvalidConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
			test.edit(&config)
			_, err := New(config)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}

	config := newTestConfig(testBrokerOptions{})
	config.ModelRoute.Budget.MaxCostUSD = "0"
	config.Pricing = nil
	if _, err := New(config); err != nil {
		t.Fatalf("zero-cost route should not require a pricing table: %v", err)
	}

	config = newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
	config.MaxCostUSD = "1.500000"
	config.ModelRoute.Budget.MaxCostUSD = "2.00"
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if broker.budget.maxCostMicros != 1_500_000 {
		t.Fatalf("effective run/route cost budget=%d, want 1500000", broker.budget.maxCostMicros)
	}

	config = newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
	config.MaxCostUSD = "2.00"
	config.ModelRoute.Budget.MaxCostUSD = "0"
	broker, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if broker.budget.maxCostMicros != 0 {
		t.Fatalf("zero route budget was weakened to %d", broker.budget.maxCostMicros)
	}

	config = newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
	config.MaxCostUSD = "0"
	config.ModelRoute.Budget.MaxCostUSD = "2.00"
	broker, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if broker.budget.maxCostMicros != 0 {
		t.Fatalf("zero run budget was weakened to %d", broker.budget.maxCostMicros)
	}

	config = newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
	config.ToolSet.Servers[0].CredentialsRef = "../secret"
	if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unsafe credential reference error=%v", err)
	}
	config = newTestConfig(testBrokerOptions{withToolCredential: true, withModelCredential: true})
	config.ModelRoute.Budget.MaxRequests = 1
	config.ModelRoute.Budget.MaxTokens = 100
	config.MaxModelRequests = 100
	config.MaxModelTokens = 1000
	broker, err = New(config)
	if err != nil {
		t.Fatal(err)
	}
	if broker.budget.maxRequests != 1 || broker.budget.maxTokens != 100 {
		t.Fatalf("host limits relaxed resolved budget: requests=%d tokens=%d", broker.budget.maxRequests, broker.budget.maxTokens)
	}
}

func TestCallToolEnforcesAllowlistExactArgumentsAndFixedCredential(t *testing.T) {
	credentials := &testCredentials{values: map[string][]byte{"mcp-secret": []byte("mcp-token")}}
	transport := &testTransport{checkSessionID: true}
	broker := newTestBroker(t, testBrokerOptions{credentials: credentials, transport: transport, withToolCredential: true})

	result, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if err != nil || result.EffectState != "read" || !bytes.Contains(result.Content, []byte(`"isError":false`)) {
		t.Fatalf("read result=%s error=%v", result.Content, err)
	}
	if _, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"428"}`)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong read arguments error=%v", err)
	}
	if _, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "missing", Arguments: []byte(`{}`)}); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown tool error=%v", err)
	}
	if got := broker.Usage().ToolCalls; got != 3 {
		t.Fatalf("tool counter=%d want 3", got)
	}

	credentials.mu.Lock()
	requests := append([]string(nil), credentials.requests...)
	credentials.mu.Unlock()
	if len(requests) != 1 || requests[0] != "mcp-secret" {
		t.Fatalf("credential resolver requests=%v", requests)
	}
	transport.mu.Lock()
	for _, request := range transport.requests {
		if request.Header.Get("Authorization") != "Bearer mcp-token" {
			t.Fatalf("upstream authorization header=%q", request.Header.Get("Authorization"))
		}
	}
	transport.mu.Unlock()
}

func TestMutatingToolRequiresApprovalAndUsesEffectLedger(t *testing.T) {
	ledger := newTestLedger()
	transport := &testTransport{}
	broker := newTestBroker(t, testBrokerOptions{ledger: ledger, transport: transport, requireApproval: true, withToolCredential: true})
	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := ToolCallRequest{Server: "github", Tool: "write_issue", Arguments: []byte(`{"force":true,"issue":"427"}`)}
	if _, err := broker.CallTool(context.Background(), request); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unapproved mutation error=%v", err)
	}
	claims, outcomes := ledger.snapshot()
	if len(claims) != 0 || len(outcomes) != 0 {
		t.Fatalf("unapproved mutation touched ledger: claims=%d outcomes=%d", len(claims), len(outcomes))
	}

	request.Approved = true
	result, err := broker.CallTool(context.Background(), request)
	if err != nil || result.EffectState != "succeeded" || result.EffectKey == "" {
		t.Fatalf("approved mutation result=%#v error=%v", result, err)
	}
	claims, outcomes = ledger.snapshot()
	if len(claims) != 1 || len(outcomes) != 1 || outcomes[0].State != effects.OutcomeSucceeded || outcomes[0].EffectKey != result.EffectKey {
		t.Fatalf("ledger claims=%#v outcomes=%#v", claims, outcomes)
	}
	if _, err := broker.CallTool(context.Background(), request); !errors.Is(err, ErrEffectAlreadyClaimed) {
		t.Fatalf("replayed mutation error=%v", err)
	}
	transport.mu.Lock()
	mcpCalls := transport.mcpCalls
	transport.mu.Unlock()
	if mcpCalls != 3 {
		t.Fatalf("replayed mutation reached upstream: %d MCP requests", mcpCalls)
	}
}

func TestMutatingUncertaintyIsTerminalAndCredentialErrorsAreRedacted(t *testing.T) {
	ledger := newTestLedger()
	transport := &testTransport{failMCP: true}
	broker := newTestBroker(t, testBrokerOptions{ledger: ledger, transport: transport, withToolCredential: true})
	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "write_issue", Approved: true, Arguments: []byte(`{"force":true,"issue":"427"}`)})
	if !errors.Is(err, ErrUnknownEffect) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("uncertain mutation error=%v", err)
	}
	_, outcomes := ledger.snapshot()
	if len(outcomes) != 1 || outcomes[0].State != effects.OutcomeUnknown {
		t.Fatalf("uncertain mutation outcomes=%#v", outcomes)
	}

	credentials := &testCredentials{values: map[string][]byte{"mcp-secret": []byte("mcp-token")}, err: errors.New("token=super-secret")}
	readBroker := newTestBroker(t, testBrokerOptions{credentials: credentials, transport: &testTransport{}, withToolCredential: true})
	_, err = readBroker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`)})
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("credential error=%v", err)
	}
}

func TestCallToolBudgetIsAtomicUnderConcurrency(t *testing.T) {
	const calls = 24
	broker := newTestBroker(t, testBrokerOptions{transport: &testTransport{}, maxToolCalls: calls})
	var wait sync.WaitGroup
	results := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := broker.CallTool(context.Background(), ToolCallRequest{Server: "github", Tool: "read_status", Arguments: []byte(`{}`)})
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	var succeeded, budgetErrors int
	for err := range results {
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrBudgetExceeded) {
			budgetErrors++
		} else {
			t.Fatalf("unexpected concurrent call error=%v", err)
		}
	}
	if succeeded != calls || budgetErrors != 0 || broker.Usage().ToolCalls != calls {
		t.Fatalf("succeeded=%d budgetErrors=%d usage=%#v", succeeded, budgetErrors, broker.Usage())
	}
}

func TestModelProxyAllowlistBudgetAndContentType(t *testing.T) {
	credentials := &testCredentials{values: map[string][]byte{"model-secret": []byte("model-token")}}
	transport := &testTransport{}
	broker := newTestBroker(t, testBrokerOptions{credentials: credentials, transport: transport, withModelCredential: true, maxModelRequests: 1})
	body := []byte(`{"model":"test-model","input":"hello","max_output_tokens":16}`)
	result, err := broker.InvokeModel(context.Background(), body)
	if err != nil || result.Provider != "openai" || result.Model != "test-model" || result.ContentType != "application/json" || result.InputTokens != 3 || result.OutputTokens != 2 || result.CostMicros != 7 {
		t.Fatalf("model result=%#v error=%v", result, err)
	}
	if _, err := broker.InvokeModel(context.Background(), body); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second model request error=%v", err)
	}
	if _, err := broker.InvokeModel(context.Background(), []byte(`{"model":"not-allowed","input":"hello"}`)); !errors.Is(err, ErrDenied) {
		t.Fatalf("unallowlisted model error=%v", err)
	}
	transport.mu.Lock()
	modelCalls := transport.modelCalls
	var authorization string
	for _, request := range transport.requests {
		if request.URL.Path == "/v1/responses" {
			authorization = request.Header.Get("Authorization")
		}
	}
	transport.mu.Unlock()
	if modelCalls != 1 || authorization != "Bearer model-token" {
		t.Fatalf("model calls=%d authorization=%q", modelCalls, authorization)
	}
	usage := broker.Usage()
	if usage.ModelRequests != 1 || usage.ModelTokens != 5 || usage.ModelCostMicros != 7 {
		t.Fatalf("model usage=%#v", usage)
	}
	invalidTransport := &testTransport{modelBody: `[]`}
	invalidBroker := newTestBroker(t, testBrokerOptions{transport: invalidTransport})
	if _, err := invalidBroker.InvokeModel(context.Background(), body); !errors.Is(err, ErrUpstreamInvalid) {
		t.Fatalf("invalid JSON model response error=%v", err)
	}
	streamBroker := newTestBroker(t, testBrokerOptions{transport: &testTransport{
		modelContent: "text/event-stream",
		modelBody:    "data: {\"usage\":{\"input_tokens\":4,\"output_tokens\":6}}\n\ndata: [DONE]\n\n",
	}})
	streamResult, err := streamBroker.InvokeModel(context.Background(), body)
	if err != nil || streamResult.ContentType != "text/event-stream" || streamResult.InputTokens != 4 || streamResult.OutputTokens != 6 {
		t.Fatalf("stream model result=%#v error=%v", streamResult, err)
	}
}

func TestModelProxyEnforcesRunTokenAndCostCaps(t *testing.T) {
	body := []byte(`{"model":"test-model","input":"hello","max_output_tokens":16}`)
	transport := &testTransport{}
	config := newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.MaxModelTokens = 1
	config.ModelRoute.Budget.MaxTokens = 1000
	limitedTokens, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limitedTokens.InvokeModel(context.Background(), body); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run token cap error=%v, want ErrBudgetExceeded", err)
	}
	transport.mu.Lock()
	modelCalls := transport.modelCalls
	transport.mu.Unlock()
	if modelCalls != 0 {
		t.Fatalf("token cap allowed an upstream call: %d", modelCalls)
	}

	transport = &testTransport{}
	config = newTestConfig(testBrokerOptions{transport: transport, withModelCredential: true})
	config.MaxCostUSD = "0.000006"
	config.ModelRoute.Budget.MaxCostUSD = "2.00"
	limitedCost, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limitedCost.InvokeModel(context.Background(), body); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run cost cap error=%v, want ErrBudgetExceeded", err)
	}
	transport.mu.Lock()
	modelCalls = transport.modelCalls
	transport.mu.Unlock()
	if modelCalls != 0 {
		t.Fatalf("cost cap allowed an upstream call: %d", modelCalls)
	}
}

func TestLoopbackHandlersRejectRemoteRequests(t *testing.T) {
	broker := newTestBroker(t, testBrokerOptions{transport: &testTransport{}})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/responses", strings.NewReader(`{"model":"test-model"}`))
	request.RemoteAddr = "203.0.113.10:1234"
	recorder := httptest.NewRecorder()
	broker.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("remote model status=%d", recorder.Code)
	}

	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"tools/list","params":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "127.0.0.1:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"read_issue"`)) {
		t.Fatalf("local MCP status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestArtifactUploadIsImmutableAndAuthoredMetadataIsBounded(t *testing.T) {
	store := newTestObjectStore()
	broker := newTestBroker(t, testBrokerOptions{artifacts: store, transport: &testTransport{}})
	first, err := broker.UploadArtifact(context.Background(), ArtifactUpload{Data: []byte("hello"), MediaType: "text/plain", Kind: "document", Name: "artifact.txt"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.UploadArtifact(context.Background(), ArtifactUpload{Data: []byte("hello"), MediaType: "TEXT/PLAIN", Kind: "document", Name: "artifact.txt"})
	if err != nil || first != second {
		t.Fatalf("idempotent artifact first=%#v second=%#v error=%v", first, second, err)
	}
	if _, err := broker.UploadArtifact(context.Background(), ArtifactUpload{Data: []byte("hello"), MediaType: "text/plain", Name: "title with spaces"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsafe artifact name error=%v", err)
	}

	handler, err := NewArtifactHandler(broker)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(map[string]any{
		"schema": "agents-gateway.artifact.v1", "title": "A useful title with spaces", "description": "plain text output", "content_kind": "document",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/artifacts/create", strings.NewReader("artifact body"))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("X-AGW-Artifact-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated || !bytes.Contains(recorder.Body.Bytes(), []byte(`"media_type":"text/plain"`)) {
		t.Fatalf("artifact handler status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func runtimeLine(t *testing.T, eventType string, seq uint64, terminal bool, data string) []byte {
	t.Helper()
	line, err := runtimeproto.EncodeLine(proto.Envelope{Protocol: proto.ProtocolVersion, Kind: proto.KindEvent, Type: eventType, RunID: testRunUID, Seq: seq, Terminal: terminal, Data: []byte(data)})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestRuntimeSupervisorRequiresTerminalKnownExitAndExactBaseSHA(t *testing.T) {
	store := newTestObjectStore()
	supervisor, err := NewRuntimeSupervisor(RuntimeConfig{RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	stream := bytes.Join([][]byte{
		runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`),
		runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`),
	}, nil)
	record, err := supervisor.Supervise(context.Background(), bytes.NewReader(stream), runtimeevents.ProcessExit{Known: true})
	if err != nil || record.BaseSHA != testBaseSHA || record.SpecDigest != testSpecDigest || record.RunUID != testRunUID || record.TerminalType != runtimeevents.TerminalCompleted {
		t.Fatalf("completion=%#v error=%v", record, err)
	}
	loaded, err := supervisor.LoadCompletion(context.Background())
	if err != nil || loaded.BaseSHA != testBaseSHA {
		t.Fatalf("loaded=%#v error=%v", loaded, err)
	}

	missingTerminal, err := NewRuntimeSupervisor(RuntimeConfig{RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: newTestObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = missingTerminal.Supervise(context.Background(), bytes.NewReader(runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`)), runtimeevents.ProcessExit{Known: true})
	if !errors.Is(err, runtimeevents.ErrNoTerminal) {
		t.Fatalf("missing terminal error=%v", err)
	}

	unknownExit, err := NewRuntimeSupervisor(RuntimeConfig{RunUID: testRunUID, SpecDigest: testSpecDigest, BaseSHA: testBaseSHA, Store: newTestObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = unknownExit.Supervise(context.Background(), bytes.NewReader(bytes.Join([][]byte{
		runtimeLine(t, proto.EventRunStarted, 1, false, `{"agent_id":"agent","sandbox_id":"sandbox"}`),
		runtimeLine(t, proto.EventRunCompleted, 2, true, `{"result":"ok"}`),
	}, nil)), runtimeevents.ProcessExit{})
	if !errors.Is(err, runtimeevents.ErrProcessExitUnknown) {
		t.Fatalf("unknown process exit error=%v", err)
	}
}

type testResolver []net.IPAddr

func (r testResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return append([]net.IPAddr(nil), r...), nil
}

func TestTransportRejectsPrivateDNSAnswersAndSpecialUseIPs(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "169.254.1.1", "::1", "fc00::1", "2001:db8::1"} {
		if isPublicIP(net.ParseIP(ip)) {
			t.Errorf("private/special address considered public: %s", ip)
		}
	}
	if !isPublicIP(net.ParseIP("8.8.8.8")) || !isPublicIP(net.ParseIP("2001:4860:4860::8888")) {
		t.Fatal("public address was rejected")
	}
	dialer := &publicDialer{resolver: testResolver{{IP: net.ParseIP("10.0.0.1")}}, dialer: &net.Dialer{Timeout: time.Millisecond}}
	if _, err := dialer.DialContext(context.Background(), "tcp", "mcp.example.test:443"); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("private DNS answer error=%v", err)
	}
	if err := rejectRedirect(nil, nil); err == nil {
		t.Fatal("redirect was accepted")
	}
	for _, uri := range []string{"s3://", "s3://bucket", "s3://bucket/key?token=secret", "https://user@example.test/key", "file:///tmp/output"} {
		if safeArtifactURI(uri) {
			t.Errorf("unsafe artifact URI accepted: %s", uri)
		}
	}
	if !safeArtifactURI("s3://bucket/runs/run-uid/object") || !safeArtifactURI("artifact://agw/abc") {
		t.Fatal("valid artifact URI rejected")
	}
}
