package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

const (
	testRun  = "phase0-run-uid"
	testSpec = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBase = "phase0-base-sha"
)

type countingDownstream struct {
	mu       sync.Mutex
	calls    int
	result   json.RawMessage
	err      error
	requests []DownstreamCall
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (d *countingDownstream) Call(_ context.Context, call DownstreamCall) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.requests = append(d.requests, call)
	if d.err != nil {
		return nil, d.err
	}
	return append([]byte(nil), d.result...), nil
}

func (d *countingDownstream) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func fixture(t *testing.T, downstream Downstream) *Guard {
	t.Helper()
	g, err := New(Config{
		RunUID:     testRun,
		SpecDigest: testSpec,
		BaseSHA:    testBase,
		Downstream: downstream,
		Grants: []toolpolicy.Grant{
			{Server: "recording", Tool: "record", Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow, Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`)},
			{Server: "recording", Tool: "record", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalRequired, Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`)},
			{Server: "recording", Tool: "record", Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow, Arguments: json.RawMessage(`{"count":1}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func call(arguments string) Call {
	return Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(arguments), DeclaredEffect: toolpolicy.EffectRead}
}

func TestArgumentMatrixAndDeniedCallsNeverReachDownstream(t *testing.T) {
	downstream := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, downstream)
	server := httptest.NewServer(g.Handler())
	defer server.Close()

	tests := []struct {
		name       string
		call       Call
		wantStatus int
		wantCalls  int
	}{
		{name: "exact", call: call(`{"message":"hello","mode":"safe"}`), wantStatus: http.StatusOK, wantCalls: 1},
		{name: "object order is insignificant", call: call(`{"mode":"safe","message":"hello"}`), wantStatus: http.StatusOK, wantCalls: 2},
		{name: "duplicate top-level member", call: call(`{"message":"hello","message":"hello","mode":"safe"}`), wantStatus: http.StatusBadRequest, wantCalls: 2},
		{name: "duplicate nested member", call: call(`{"message":"hello","mode":"safe","nested":{"x":1,"x":1}}`), wantStatus: http.StatusBadRequest, wantCalls: 2},
		{name: "null", call: Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`null`), DeclaredEffect: toolpolicy.EffectRead}, wantStatus: http.StatusBadRequest, wantCalls: 2},
		{name: "extra member", call: call(`{"message":"hello","mode":"safe","extra":true}`), wantStatus: http.StatusForbidden, wantCalls: 2},
		{name: "nested mismatch", call: call(`{"message":{"value":"hello"},"mode":"safe"}`), wantStatus: http.StatusForbidden, wantCalls: 2},
		{name: "array", call: Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`[]`), DeclaredEffect: toolpolicy.EffectRead}, wantStatus: http.StatusBadRequest, wantCalls: 2},
		{name: "number mismatch", call: call(`{"message":1,"mode":"safe"}`), wantStatus: http.StatusForbidden, wantCalls: 2},
		{name: "unknown tool", call: Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "unknown", Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectRead}, wantStatus: http.StatusForbidden, wantCalls: 2},
		{name: "denied write before approval", call: Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite}, wantStatus: http.StatusBadRequest, wantCalls: 2},
		{name: "run binding", call: Call{RunUID: "other-run", SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectRead}, wantStatus: http.StatusForbidden, wantCalls: 2},
		{name: "spec binding", call: Call{RunUID: testRun, SpecDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"hello","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectRead}, wantStatus: http.StatusForbidden, wantCalls: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.call)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.Post(server.URL+CallPath, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("status=%d want %d", response.StatusCode, test.wantStatus)
			}
			if got := downstream.count(); got != test.wantCalls {
				t.Fatalf("upstream calls=%d want %d", got, test.wantCalls)
			}
		})
	}
}

func TestAgentCannotSelfApproveWrite(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, d)
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	body := []byte(`{"runUID":"` + testRun + `","specDigest":"` + testSpec + `","server":"recording","tool":"record","arguments":{"message":"write","mode":"safe"},"declaredEffect":"write","approved":true,"effect":{"baseSHA":"` + testBase + `","patchDigest":"` + testDigest("patch") + `","operation":"record-write"}}`)
	response, err := http.Post(server.URL+CallPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || d.count() != 0 {
		t.Fatalf("agent-controlled approval was accepted: status=%d calls=%d", response.StatusCode, d.count())
	}
}

func TestNumbersAreComparedWithoutFloatConversion(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, d)
	decision, err := g.Authorize(Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"count":1.0}`), DeclaredEffect: toolpolicy.EffectRead})
	if err != nil || !decision.Allowed {
		t.Fatalf("numeric equivalent rejected: %#v %v", decision, err)
	}
}

func TestDepthAndSizeLimitsAreRejectedBeforePolicy(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, d)
	deep := `"x"`
	for i := 0; i < 140; i++ {
		deep = `{"x":` + deep + `}`
	}
	for _, value := range []string{deep, `{"message":"` + strings.Repeat("x", 70<<10) + `"}`} {
		if _, err := g.Authorize(call(value)); err == nil {
			t.Fatalf("oversized/deep arguments accepted")
		}
	}
	if d.count() != 0 {
		t.Fatalf("invalid arguments reached downstream: %d calls", d.count())
	}
	if err := strictjson.ValidateObject([]byte(`{"x":` + strings.Repeat(" ", 0) + `1}`)); err != nil {
		t.Fatalf("control strict JSON object rejected: %v", err)
	}
}

func TestHTTPDecoderRejectsUnknownAndTrailingFields(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, d)
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	body := []byte(`{"runUID":"` + testRun + `","specDigest":"` + testSpec + `","server":"recording","tool":"record","arguments":{"message":"hello","mode":"safe"},"declaredEffect":"read","unexpected":true}`)
	response, err := http.Post(server.URL+CallPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || d.count() != 0 {
		t.Fatalf("unknown field was not rejected: status=%d calls=%d", response.StatusCode, d.count())
	}
}

func TestHTTPDecoderBoundsBodyWithoutNilResponseWriter(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	g := fixture(t, d)
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	body := append([]byte(`{"runUID":"`+testRun+`","specDigest":"`+testSpec+`","server":"recording","tool":"record","arguments":{"message":"`), bytes.Repeat([]byte("x"), strictjson.MaxDocumentBytes)...)
	body = append(body, []byte(`"},"declaredEffect":"read"}`)...)
	response, err := http.Post(server.URL+CallPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || d.count() != 0 {
		t.Fatalf("oversized body was not rejected safely: status=%d calls=%d", response.StatusCode, d.count())
	}
}

func TestHTTPDownstreamRequiresExactLoopbackEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://127.0.0.1:8082/mcp",
		"http://[::1]:8082/mcp",
	} {
		if _, err := NewHTTPDownstream(endpoint, nil); err != nil {
			t.Fatalf("valid endpoint %q rejected: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"https://127.0.0.1:8082/mcp",
		"http://user:pass@127.0.0.1:8082/mcp",
		"http://127.0.0.1.evil:8082/mcp",
		"http://127.0.0.1:8081/mcp",
		"http://127.0.0.1:8082/other",
		"http://127.0.0.1:8082/mcp/",
		"http://127.0.0.1:8082/mcp?next=http://evil",
		"http://127.0.0.1:8082/mcp#fragment",
		"http://127.0.0.1:8082/mcp#",
		"http://127.0.0.1:8082/mcp%2f..%2fmcp",
	} {
		if _, err := NewHTTPDownstream(endpoint, nil); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}

func TestHTTPDownstreamPreservesStrictJSONAndRejectsRedirects(t *testing.T) {
	const largeNumber = "123456789012345678901234567890"
	var requests [][]byte
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		response := func(status int, body string) *http.Response {
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}
		}
		switch len(requests) {
		case 1:
			response := response(http.StatusOK, `{"jsonrpc":"2.0","id":"phase0-init","result":{"ok":true}}`)
			response.Header.Set("mcp-session-id", "phase0-session")
			return response, nil
		case 2:
			return response(http.StatusAccepted, ""), nil
		case 3:
			return response(http.StatusOK, `{"jsonrpc":"2.0","id":"phase0-call","result":{"value":`+largeNumber+`}}`), nil
		default:
			return nil, errors.New("unexpected downstream request")
		}
	})}
	downstream, err := NewHTTPDownstream("http://127.0.0.1:8082/mcp", client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := downstream.Call(context.Background(), DownstreamCall{
		Server:    "recording",
		Tool:      "record",
		Arguments: json.RawMessage(`{"value":` + largeNumber + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(requests[2], []byte(largeNumber)) {
		t.Fatalf("large JSON number was changed before forwarding: %s", requests[2])
	}
	if bytes.Contains(requests[1], []byte(`"id"`)) {
		t.Fatalf("notifications/initialized was serialized as a request: %s", requests[1])
	}
	if string(result) != `{"value":`+largeNumber+`}` {
		t.Fatalf("result changed: %s", result)
	}

	redirectCalls := 0
	redirectClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		redirectCalls++
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     http.Header{"Location": []string{"https://public.example.invalid/"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}
	redirectDownstream, err := NewHTTPDownstream("http://127.0.0.1:8082/mcp", redirectClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redirectDownstream.Call(context.Background(), DownstreamCall{Arguments: json.RawMessage(`{"value":1}`)}); err == nil || redirectCalls != 1 {
		t.Fatalf("redirect was followed or accepted: err=%v calls=%d", err, redirectCalls)
	}
}

func TestHTTPDownstreamRejectsOversizedResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", strictjson.MaxDocumentBytes+1))),
			Request:    request,
		}, nil
	})}
	downstream, err := NewHTTPDownstream("http://127.0.0.1:8082/mcp", client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downstream.Call(context.Background(), DownstreamCall{Arguments: json.RawMessage(`{"value":1}`)}); err == nil {
		t.Fatal("oversized downstream response was accepted")
	}
}

func TestWriteBaseMustMatchImmutableRunBase(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	write := Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite, Effect: &EffectRequest{BaseSHA: "different-base", PatchDigest: testDigest("patch"), Operation: "record-write"}}
	g, err := New(Config{
		RunUID: testRun, SpecDigest: testSpec, BaseSHA: testBase, Downstream: d,
		Approvals: trustedApprovalFor(t, write),
		Grants:    []toolpolicy.Grant{{Server: "recording", Tool: "record", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalRequired, Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(context.Background(), write); !errors.Is(err, ErrBinding) || d.count() != 0 {
		t.Fatalf("write with mutable base was not rejected: err=%v calls=%d", err, d.count())
	}
}

func TestAmbiguousWriteCommitsUnknownAndNeverRetries(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	clock := func() time.Time { return time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC) }
	ledger, err := effects.New(store, "phase0/effects", clock)
	if err != nil {
		t.Fatal(err)
	}
	d := &countingDownstream{err: errors.New("connection ended after write")}
	write := Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite, Effect: &EffectRequest{BaseSHA: testBase, PatchDigest: testDigest("patch"), Operation: "record-write"}}
	approvals := trustedApprovalFor(t, write)
	g, err := New(Config{
		RunUID: testRun, SpecDigest: testSpec, BaseSHA: testBase, Ledger: ledger, Downstream: d,
		Approvals: approvals,
		Grants:    []toolpolicy.Grant{{Server: "recording", Tool: "record", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalRequired, Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := g.Authorize(write)
	if err != nil {
		t.Fatal(err)
	}
	response, err := g.Execute(context.Background(), write)
	if !errors.Is(err, ErrUnknownEffect) || response.Error != "unknown_effect" || d.count() != 1 {
		t.Fatalf("first ambiguous write = %#v %v calls=%d", response, err, d.count())
	}
	response, err = g.Execute(context.Background(), write)
	if !errors.Is(err, ErrUnknownEffect) || response.Error != "unknown_effect" || d.count() != 1 {
		t.Fatalf("replayed ambiguous write retried: %#v %v calls=%d", response, err, d.count())
	}
	outcome, err := ledger.ReadOutcome(context.Background(), decision.EffectKey, decision.RequestDigest)
	if err != nil || outcome.State != effects.OutcomeUnknown {
		t.Fatalf("unknown outcome not durable: %#v %v", outcome, err)
	}
}

func TestAmbiguousClaimNeverCallsDownstream(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}, fail: errors.New("ambiguous object-store create")}
	ledger, _ := effects.New(store, "phase0/effects", time.Now)
	d := &countingDownstream{result: json.RawMessage(`{"ok":true}`)}
	write := Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite, Effect: &EffectRequest{BaseSHA: testBase, PatchDigest: testDigest("patch"), Operation: "record-write"}}
	g, err := New(Config{
		RunUID: testRun, SpecDigest: testSpec, BaseSHA: testBase, Ledger: ledger, Downstream: d,
		Approvals: trustedApprovalFor(t, write),
		Grants:    []toolpolicy.Grant{{Server: "recording", Tool: "record", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalRequired, Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(context.Background(), write); err == nil || d.count() != 0 {
		t.Fatalf("ambiguous claim reached downstream: err=%v calls=%d", err, d.count())
	}
}

func TestMalformedWriteResultIsTerminal(t *testing.T) {
	store := &memoryStore{objects: map[string][]byte{}}
	ledger, err := effects.New(store, "phase0/effects", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	d := &countingDownstream{result: json.RawMessage(`not-json`)}
	write := Call{RunUID: testRun, SpecDigest: testSpec, Server: "recording", Tool: "record", Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`), DeclaredEffect: toolpolicy.EffectWrite, Effect: &EffectRequest{BaseSHA: testBase, PatchDigest: testDigest("malformed-result"), Operation: "record-write"}}
	g, err := New(Config{
		RunUID: testRun, SpecDigest: testSpec, BaseSHA: testBase, Ledger: ledger, Downstream: d,
		Approvals: trustedApprovalFor(t, write),
		Grants:    []toolpolicy.Grant{{Server: "recording", Tool: "record", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalRequired, Arguments: json.RawMessage(`{"message":"write","mode":"safe"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := g.Execute(context.Background(), write)
	if !errors.Is(err, ErrUnknownEffect) || response.Error != "unknown_effect" || d.count() != 1 {
		t.Fatalf("malformed write result was not fenced: %#v %v calls=%d", response, err, d.count())
	}
	response, err = g.Execute(context.Background(), write)
	if !errors.Is(err, ErrUnknownEffect) || response.Error != "unknown_effect" || d.count() != 1 {
		t.Fatalf("malformed write result was retried: %#v %v calls=%d", response, err, d.count())
	}
}

func trustedApprovalFor(t *testing.T, call Call) ApprovalVerifier {
	t.Helper()
	effectKey, err := effects.EffectKey(testRun, call.Effect.BaseSHA, call.Effect.PatchDigest, call.Effect.Operation)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := requestDigest(call)
	if err != nil {
		t.Fatal(err)
	}
	return ApprovalVerifierFunc(func(_ context.Context, _ Call, gotEffectKey, gotDigest string) (bool, error) {
		return gotEffectKey == effectKey && gotDigest == digest, nil
	})
}

func TestOutputDoesNotContainCredentialCanary(t *testing.T) {
	d := &countingDownstream{result: json.RawMessage(`{"content":[{"type":"text","text":"recorded"}]}`)}
	g := fixture(t, d)
	response, err := g.Execute(context.Background(), call(`{"message":"hello","mode":"safe"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(response)
	if bytes.Contains(body, []byte("phase0-only-canary")) {
		t.Fatal("credential canary leaked through guard response")
	}
}

type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	fail    error
}

func (s *memoryStore) Create(_ context.Context, key string, body []byte, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return false, s.fail
	}
	if _, exists := s.objects[key]; exists {
		return false, nil
	}
	s.objects[key] = append([]byte(nil), body...)
	return true, nil
}

func (s *memoryStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, effects.ErrNotFound
	}
	return append([]byte(nil), body...), nil
}

func testDigest(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(digest[:])
}
