package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

func TestBrokerCompilesExplicitProfilesAndDefensivelyCopiesConfig(t *testing.T) {
	config := newTestConfig(testBrokerOptions{})
	original := config.ToolSet.Servers[0].Tools[0].ExactArguments["issue"]
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if len(broker.profiles) != 3 {
		t.Fatalf("compiled profiles=%d, want 3", len(broker.profiles))
	}
	if _, ok := broker.profiles[v1alpha1.ToolProfileExplore]["read_issue"]; !ok {
		t.Fatal("read_issue was not compiled into explore")
	}
	if _, ok := broker.profiles[v1alpha1.ToolProfileEdit]["read_issue"]; !ok {
		t.Fatal("read_issue was not compiled into edit")
	}
	write := broker.profiles[v1alpha1.ToolProfileEdit]["write_issue"]
	if write.effect != v1alpha1.EffectWrite || !strictjson.EqualObjects(write.exactArgs, []byte(`{"force":true,"issue":"427"}`)) {
		t.Fatalf("write_issue policy was not preserved: effect=%q exactArgs=%s", write.effect, write.exactArgs)
	}

	config.ToolSet.Profiles[0].Tools[0].Tool = "write_issue"
	config.ToolSet.Servers[0].Tools[0].ExactArguments["issue"] = apix.JSON{Raw: []byte(`"999"`)}
	if _, ok := broker.currentTools()["read_issue"]; !ok {
		t.Fatal("mutating the source profile changed the compiled profile")
	}
	if _, err := broker.CallTool(context.Background(), ToolCallRequest{
		Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`),
	}); err != nil {
		t.Fatalf("compiled exact arguments changed with source config: %v", err)
	}
	if !bytes.Equal([]byte(original.Raw), []byte(`"427"`)) {
		t.Fatalf("test fixture exact argument unexpectedly changed: %s", original.Raw)
	}
}

func TestCompileToolSetRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "omitted profile", mutate: func(config *Config) {
			config.ToolSet.Profiles = config.ToolSet.Profiles[:2]
		}},
		{name: "max above API limit", mutate: func(config *Config) {
			config.ToolSet.MaxToolsPerPhase = v1alpha1.MaxToolsPerPhase + 1
		}},
		{name: "zero max", mutate: func(config *Config) {
			config.ToolSet.MaxToolsPerPhase = 0
		}},
		{name: "profile exceeds configured max", mutate: func(config *Config) {
			config.ToolSet.MaxToolsPerPhase = 1
		}},
		{name: "duplicate profile ref", mutate: func(config *Config) {
			config.ToolSet.Profiles[0].Tools = append(config.ToolSet.Profiles[0].Tools, v1alpha1.ToolRef{Server: "github", Tool: "read_issue"})
		}},
		{name: "unknown server ref", mutate: func(config *Config) {
			config.ToolSet.Profiles[0].Tools[0].Server = "unknown"
		}},
		{name: "unknown tool ref", mutate: func(config *Config) {
			config.ToolSet.Profiles[0].Tools[0].Tool = "unknown"
		}},
		{name: "unsafe ref name", mutate: func(config *Config) {
			config.ToolSet.Profiles[0].Tools[0].Tool = "bad/name"
		}},
		{name: "duplicate profile name", mutate: func(config *Config) {
			config.ToolSet.Profiles[1].Name = config.ToolSet.Profiles[0].Name
		}},
		{name: "unknown profile name", mutate: func(config *Config) {
			config.ToolSet.Profiles[0].Name = v1alpha1.ToolProfileName("admin")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := newTestConfig(testBrokerOptions{})
			test.mutate(&config)
			if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New error=%v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestBrokerPhaseTransitionIsOneWayAndAuthorizationBound(t *testing.T) {
	broker := newTestBroker(t, testBrokerOptions{})
	if got := broker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("initial phase=%q, want explore", got)
	}
	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatalf("authorized transition error=%v", err)
	}
	if got := broker.CurrentPhase(); got != v1alpha1.ToolProfileEdit {
		t.Fatalf("phase after transition=%q, want edit", got)
	}
	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatalf("idempotent transition error=%v", err)
	}

	missing := newTestConfig(testBrokerOptions{})
	missing.PhaseTransitionAuthorizer = nil
	missingBroker, err := New(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := missingBroker.TransitionToEdit(context.Background()); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("missing authorizer error=%v, want ErrPhaseTransitionDenied", err)
	}
	if got := missingBroker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("missing-authorizer phase=%q, want explore", got)
	}

	var authorizerCalls atomic.Int32
	unauthorized := newTestConfig(testBrokerOptions{})
	unauthorized.PhaseTransitionAuthorizer = PhaseTransitionAuthorizerFunc(func(context.Context) error {
		authorizerCalls.Add(1)
		return errors.New("not trusted")
	})
	unauthorizedBroker, err := New(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	if err := unauthorizedBroker.TransitionToEdit(context.Background()); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("unauthorized transition error=%v, want ErrPhaseTransitionDenied", err)
	}
	if got := unauthorizedBroker.CurrentPhase(); got != v1alpha1.ToolProfileExplore {
		t.Fatalf("unauthorized phase=%q, want explore", got)
	}
	if got := authorizerCalls.Load(); got != 1 {
		t.Fatalf("authorizer calls=%d, want 1", got)
	}
}

func TestBrokerPhaseTransitionReplayStillRequiresAuthority(t *testing.T) {
	var allow atomic.Bool
	allow.Store(true)
	config := newTestConfig(testBrokerOptions{})
	config.PhaseTransitionAuthorizer = PhaseTransitionAuthorizerFunc(func(context.Context) error {
		if !allow.Load() {
			return ErrPhaseTransitionDenied
		}
		return nil
	})
	b, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.TransitionToEdit(context.Background()); err != nil {
		t.Fatalf("initial transition error=%v", err)
	}
	allow.Store(false)
	if err := b.TransitionToEdit(context.Background()); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("unauthorized idempotent replay error=%v, want ErrPhaseTransitionDenied", err)
	}
	if got := b.CurrentPhase(); got != v1alpha1.ToolProfileEdit {
		t.Fatalf("phase after denied replay=%q, want edit", got)
	}
}

func TestPhaseSupervisorCapabilityIsPairedAndNotContextForged(t *testing.T) {
	first := NewPhaseSupervisor()
	second := NewPhaseSupervisor()
	authorizer := first.Authorizer()
	if authorizer == nil {
		t.Fatal("phase supervisor returned nil authorizer")
	}
	if err := authorizer.AuthorizeExploreToEdit(context.Background()); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("background context error=%v, want ErrPhaseTransitionDenied", err)
	}
	if err := authorizer.AuthorizeExploreToEdit(second.withAuthority(context.Background())); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("cross-supervisor context error=%v, want ErrPhaseTransitionDenied", err)
	}
	if err := authorizer.AuthorizeExploreToEdit(first.withAuthority(context.Background())); err != nil {
		t.Fatalf("paired supervisor context error=%v", err)
	}
}

func TestMCPListFollowsExploreAndEditProfiles(t *testing.T) {
	broker := newTestBroker(t, testBrokerOptions{})
	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	explore := mustMCPList(t, handler)
	if !sameStrings(explore, []string{"read_issue", "read_status"}) {
		t.Fatalf("explore tools=%v", explore)
	}
	if err := broker.TransitionToEdit(context.Background()); err != nil {
		t.Fatal(err)
	}
	edit := mustMCPList(t, handler)
	if !sameStrings(edit, []string{"read_issue", "write_issue"}) {
		t.Fatalf("edit tools=%v", edit)
	}
}

func TestMCPToolCallAcceptsClientMetadataWithoutForwardingIt(t *testing.T) {
	transport := &testTransport{}
	broker := newTestBroker(t, testBrokerOptions{transport: transport})
	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := performMCP(handler, "tools/call", map[string]any{
		"name":      "read_issue",
		"arguments": map[string]any{"issue": "427"},
		"_meta":     map[string]any{"progressToken": 1, "client": "codex"},
	})
	if err != nil || status != http.StatusOK || bytes.Contains(body, []byte(`"error"`)) {
		t.Fatalf("metadata tool call status=%d error=%v body=%s", status, err, body)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for index, request := range transport.requests {
		if request.URL.Path == "/mcp" && bytes.Contains(transport.bodies[index], []byte(`"method":"tools/call"`)) && bytes.Contains(transport.bodies[index], []byte(`"_meta"`)) {
			t.Fatalf("client metadata leaked into upstream MCP request: %s", transport.bodies[index])
		}
	}
}

func TestVerifierOnlyProfileIsNotAgentSelectable(t *testing.T) {
	config := newTestConfig(testBrokerOptions{})
	config.ToolSet.Servers[0].Tools = append(config.ToolSet.Servers[0].Tools, v1alpha1.ToolDefinition{
		Name: "verify_only", Effect: v1alpha1.EffectRead,
	})
	config.ToolSet.Profiles[2].Tools = append(config.ToolSet.Profiles[2].Tools, v1alpha1.ToolRef{
		Server: "github", Tool: "verify_only",
	})
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range mustMCPList(t, handler) {
		if tool == "verify_only" {
			t.Fatalf("agent tools/list exposed verifier-only tool")
		}
	}
	status, body, err := performMCP(handler, "tools/call", map[string]any{
		"name": "verify_only", "arguments": map[string]any{},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("verifier-only MCP call status=%d error=%v body=%s", status, err, body)
	}
	var response struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != MCPErrorCodePhaseDenied {
		t.Fatalf("verifier-only MCP call error=%d, want phase denial", response.Error.Code)
	}
}

func TestPhaseDeniedCallDoesNotReserveOrReachPolicyDependencies(t *testing.T) {
	ledger := newTestLedger()
	transport := &testTransport{}
	credentials := &testCredentials{values: map[string][]byte{"mcp-secret": []byte("mcp-token")}}
	broker := newTestBroker(t, testBrokerOptions{
		ledger: ledger, transport: transport, credentials: credentials, withToolCredential: true,
	})

	_, err := broker.CallTool(context.Background(), ToolCallRequest{
		Server: "github", Tool: "write_issue", Approved: true, Arguments: []byte(`{"issue":"wrong"}`),
	})
	if !errors.Is(err, ErrPhaseDenied) {
		t.Fatalf("direct phase-denied error=%v", err)
	}
	if usage := broker.Usage(); usage.ToolCalls != 0 {
		t.Fatalf("phase-denied direct call usage=%#v, want zero tool calls", usage)
	}
	claims, outcomes := ledger.snapshot()
	if len(claims) != 0 || len(outcomes) != 0 {
		t.Fatalf("phase-denied direct call touched ledger: claims=%d outcomes=%d", len(claims), len(outcomes))
	}
	credentials.mu.Lock()
	credentialRequests := len(credentials.requests)
	credentials.mu.Unlock()
	transport.mu.Lock()
	mcpCalls := transport.mcpCalls
	transport.mu.Unlock()
	if credentialRequests != 0 || mcpCalls != 0 {
		t.Fatalf("phase-denied direct call reached credential/upstream: credentials=%d MCP=%d", credentialRequests, mcpCalls)
	}

	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := performMCP(handler, "tools/call", map[string]any{
		"name": "write_issue", "arguments": map[string]any{"issue": "427", "force": true},
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("MCP phase-denied status=%d error=%v body=%s", status, err, body)
	}
	var response struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != MCPErrorCodePhaseDenied || response.Error.Message != "tool call denied" || strings.Contains(response.Error.Message, "edit") {
		t.Fatalf("phase-denied MCP error=%#v", response.Error)
	}
	if usage := broker.Usage(); usage.ToolCalls != 0 {
		t.Fatalf("phase-denied MCP call usage=%#v, want zero tool calls", usage)
	}
	credentials.mu.Lock()
	credentialRequests = len(credentials.requests)
	credentials.mu.Unlock()
	transport.mu.Lock()
	mcpCalls = transport.mcpCalls
	transport.mu.Unlock()
	if credentialRequests != 0 || mcpCalls != 0 {
		t.Fatalf("phase-denied MCP call reached credential/upstream: credentials=%d MCP=%d", credentialRequests, mcpCalls)
	}
}

func TestVerifierModeStartsInVerifyAndHasNoAgentMCP(t *testing.T) {
	config := newTestConfig(testBrokerOptions{transport: &testTransport{}})
	config.Mode = BrokerModeVerifier
	config.PhaseTransitionAuthorizer = nil
	broker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if got := broker.CurrentPhase(); got != v1alpha1.ToolProfileVerify {
		t.Fatalf("verifier initial phase=%q, want verify", got)
	}
	if _, err := broker.MCPHandler(); !errors.Is(err, ErrMCPUnavailable) {
		t.Fatalf("verifier MCP handler error=%v, want ErrMCPUnavailable", err)
	}
	if err := broker.TransitionToEdit(context.Background()); !errors.Is(err, ErrPhaseTransitionDenied) {
		t.Fatalf("verifier transition error=%v, want ErrPhaseTransitionDenied", err)
	}
	result, err := broker.CallTool(context.Background(), ToolCallRequest{
		Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`),
	})
	if err != nil || result.EffectState != "read" {
		t.Fatalf("verifier host call result=%#v error=%v", result, err)
	}
}

func TestPhaseTransitionListAndCallAreRaceSafe(t *testing.T) {
	broker := newTestBroker(t, testBrokerOptions{maxToolCalls: 1000, transport: &testTransport{}})
	handler, err := broker.MCPHandler()
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsCh := make(chan error, 32)
	report := func(err error) {
		if err != nil {
			errorsCh <- err
		}
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		for i := 0; i < 100; i++ {
			report(broker.TransitionToEdit(context.Background()))
		}
	}()
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 100; j++ {
				_, body, err := performMCP(handler, "tools/list", map[string]any{})
				if err != nil {
					report(err)
					continue
				}
				var response struct {
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal(body, &response); err != nil {
					report(err)
					continue
				}
				var list struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				}
				if err := json.Unmarshal(response.Result, &list); err != nil || len(list.Tools) == 0 {
					if err == nil {
						err = errors.New("empty tools/list result")
					}
					report(err)
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for j := 0; j < 100; j++ {
				_, err := broker.CallTool(context.Background(), ToolCallRequest{
					Server: "github", Tool: "read_issue", Arguments: []byte(`{"issue":"427"}`),
				})
				report(err)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	if got := broker.CurrentPhase(); got != v1alpha1.ToolProfileEdit {
		t.Fatalf("final phase=%q, want edit", got)
	}
}

func mustMCPList(t *testing.T, handler http.Handler) []string {
	t.Helper()
	status, body, err := performMCP(handler, "tools/list", map[string]any{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("tools/list status=%d error=%v body=%s", status, err, body)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(response.Result.Tools))
	for _, tool := range response.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func performMCP(handler http.Handler, method string, params any) (int, []byte, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "test", "method": method, "params": params,
	})
	if err != nil {
		return 0, nil, err
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes(), nil
}

var _ PhaseTransitionAuthorizer = PhaseTransitionAuthorizerFunc(nil)
