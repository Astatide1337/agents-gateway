package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/phase0/agentgateway-guard/guard"
	"github.com/Astatide1337/agents-gateway/v3/phase0/agentgateway-guard/recording"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

const (
	testRunUID     = "phase0-run-uid"
	testSpecDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBaseSHA    = "phase0-base-sha"
)

// rewriteTransport keeps the production URL contract under test while
// routing the provider-free test to ephemeral httptest listeners. The
// production constructors still accept only their exact loopback ports.
type rewriteTransport struct {
	host   string
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if request.URL.Host != t.host {
		return base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	urlCopy := *clone.URL
	urlCopy.Scheme = t.target.Scheme
	urlCopy.Host = t.target.Host
	clone.URL = &urlCopy
	clone.Host = t.target.Host
	clone.RequestURI = ""
	return base.RoundTrip(clone)
}

func TestProviderFreeGuardGatewayMCPRecordingChain(t *testing.T) {
	const canary = BackendCanary
	recorder, err := recording.New(canary)
	if err != nil {
		t.Fatal(err)
	}
	recordingServer := httptest.NewServer(recorder.Handler())
	defer recordingServer.Close()
	recordingTarget, err := url.Parse(recordingServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	gatewayFixture, err := New(Config{
		BackendURL: BackendURL,
		Credential: canary,
		HTTPClient: &http.Client{Transport: rewriteTransport{host: "127.0.0.1:9090", target: recordingTarget}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gatewayFixture.Close()
	gatewayServer := httptest.NewServer(gatewayFixture.Handler())
	defer gatewayServer.Close()
	gatewayTarget, err := url.Parse(gatewayServer.URL)
	if err != nil {
		t.Fatal(err)
	}

	downstream, err := guard.NewHTTPDownstream("http://127.0.0.1:8082/mcp", &http.Client{
		Transport: rewriteTransport{host: "127.0.0.1:8082", target: gatewayTarget},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := guard.New(guard.Config{
		RunUID:     testRunUID,
		SpecDigest: testSpecDigest,
		BaseSHA:    testBaseSHA,
		Downstream: downstream,
		Grants: []toolpolicy.Grant{{
			Server: "recording", Tool: "record", Effect: toolpolicy.EffectRead,
			Approval:  toolpolicy.ApprovalAllow,
			Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	guardServer := httptest.NewServer(fixture.Handler())
	defer guardServer.Close()

	validCall := guard.Call{
		RunUID: testRunUID, SpecDigest: testSpecDigest, Server: "recording", Tool: "record",
		Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectRead,
	}
	response := postGuardCall(t, guardServer.Client(), guardServer.URL+guard.CallPath, validCall)
	if !response.Allowed || response.Error != "" || bytes.Contains(response.Result, []byte(canary)) {
		t.Fatalf("valid guarded call=%#v; credential leaked or call was denied", response)
	}

	recordingEvidence := recorder.Evidence()
	if recordingEvidence.Calls != 1 || recordingEvidence.CanaryValid != 1 {
		t.Fatalf("recording evidence=%#v, want one credentialed tool call", recordingEvidence)
	}
	gatewayEvidence := gatewayFixture.Evidence()
	if gatewayEvidence.ForwardedRequests != 3 || gatewayEvidence.ToolCalls != 1 || gatewayEvidence.CredentialInjected != 3 {
		t.Fatalf("gateway evidence=%#v, want initialize/notification/call with injection each time", gatewayEvidence)
	}
	// A single downstream session performs the handshake once. The second
	// guarded read must not create a second initialize/notification pair.
	if second := postGuardCall(t, guardServer.Client(), guardServer.URL+guard.CallPath, validCall); !second.Allowed || second.Error != "" {
		t.Fatalf("second guarded read=%#v", second)
	}
	if evidence := recorder.Evidence(); evidence.Calls != 2 || evidence.CanaryValid != 2 {
		t.Fatalf("second guarded read evidence=%#v", evidence)
	}
	if evidence := gatewayFixture.Evidence(); evidence.ForwardedRequests != 4 || evidence.ToolCalls != 2 || evidence.CredentialInjected != 4 {
		t.Fatalf("second read reopened MCP handshake: %#v", evidence)
	}

	denied := validCall
	denied.Tool = "not-allowlisted"
	if got := postGuardStatus(t, guardServer.Client(), guardServer.URL+guard.CallPath, denied); got != http.StatusForbidden {
		t.Fatalf("unknown tool status=%d, want %d", got, http.StatusForbidden)
	}

	nonExact := validCall
	nonExact.Arguments = json.RawMessage(`{"message":"hello","mode":"safe","extra":true}`)
	if got := postGuardStatus(t, guardServer.Client(), guardServer.URL+guard.CallPath, nonExact); got != http.StatusForbidden {
		t.Fatalf("non-exact arguments status=%d, want %d", got, http.StatusForbidden)
	}

	write := guard.Call{
		RunUID: testRunUID, SpecDigest: testSpecDigest, Server: "recording", Tool: "record",
		Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite,
		Effect: &guard.EffectRequest{BaseSHA: testBaseSHA, PatchDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Operation: "record-write"},
	}
	if got := postGuardStatus(t, guardServer.Client(), guardServer.URL+guard.CallPath, write); got != http.StatusForbidden {
		t.Fatalf("unapproved write status=%d, want %d", got, http.StatusForbidden)
	}

	if evidence := recorder.Evidence(); evidence.Calls != 2 || evidence.CanaryValid != 2 {
		t.Fatalf("denied calls reached recording gateway: %#v", evidence)
	}
	if evidence := gatewayFixture.Evidence(); evidence.ForwardedRequests != 4 || evidence.ToolCalls != 2 {
		t.Fatalf("denied calls reached gateway: %#v", evidence)
	}
}

func TestGatewayRejectsGenericMCPAndUnsafeConfiguration(t *testing.T) {
	for _, config := range []Config{
		{BackendURL: "https://127.0.0.1:9090/mcp", Credential: BackendCanary},
		{BackendURL: "http://127.0.0.2:9090/mcp", Credential: BackendCanary},
		{BackendURL: "http://127.0.0.1:9090/mcp?next=https://public.invalid", Credential: BackendCanary},
		{BackendURL: BackendURL, Credential: "contains\nheader"},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("unsafe gateway configuration error=%v, want ErrInvalidConfig", err)
		}
	}

	recorder, err := recording.New(BackendCanary)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(recorder.Handler())
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := New(Config{
		BackendURL: BackendURL, Credential: BackendCanary,
		HTTPClient: &http.Client{Transport: rewriteTransport{host: "127.0.0.1:9090", target: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	proxy := httptest.NewServer(fixture.Handler())
	defer proxy.Close()

	request, err := http.NewRequest(http.MethodPost, proxy.URL+MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"resources/read","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := proxy.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("unsupported MCP request")) {
		t.Fatalf("generic MCP method response status=%d body=%s", response.StatusCode, body)
	}
	if evidence := recorder.Evidence(); evidence.Calls != 0 || evidence.CanaryValid != 0 || fixture.Evidence().ForwardedRequests != 0 {
		t.Fatalf("generic method reached backend: recorder=%#v gateway=%#v", evidence, fixture.Evidence())
	}
}

func postGuardCall(t *testing.T, client *http.Client, endpoint string, call guard.Call) guard.Response {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var value guard.Response
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("guard status=%d body=%#v", response.StatusCode, value)
	}
	return value
}

func postGuardStatus(t *testing.T, client *http.Client, endpoint string, call guard.Call) int {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response.StatusCode
}
