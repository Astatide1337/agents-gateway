package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

func TestBrokerRejectsRecursiveDuplicateArgumentsAtBoundary(t *testing.T) {
	policy := &countingMCPPolicy{grants: []toolpolicy.Grant{{Server: "github", Tool: "create_branch", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow}}}
	servers := &countingMCPServerSource{}
	effects := &memoryEffects{}
	audit := &memoryAudit{}
	broker, err := New(policy, credentialSource("credential-secret"), servers, effects, audit, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, arguments := range []string{
		`{"owner":"first-secret","owner":"second-secret"}`,
		`{"owner":"acme","nested":{"branch":"first-secret","branch":"second-secret"}}`,
		`{"owner":"acme","nested":[{"branch":"first-secret","branch":"second-secret"}]}`,
	} {
		_, err := broker.Call(context.Background(), Request{
			OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run",
			Server: "github", Tool: "create_branch", EffectKey: fmt.Sprintf("duplicate-%d", index),
			Arguments: json.RawMessage(arguments),
		})
		if err == nil || strings.Contains(err.Error(), "first-secret") || strings.Contains(err.Error(), "second-secret") {
			t.Fatalf("duplicate arguments did not fail with a bounded error: %v", err)
		}
	}
	if policy.count() != 0 || servers.count() != 0 || len(effects.claims) != 0 || len(audit.snapshot()) != 0 {
		t.Fatalf("duplicate arguments crossed the broker boundary: policy=%d servers=%d effects=%v audit=%v", policy.count(), servers.count(), effects.claims, audit.snapshot())
	}
}

func TestExactArgumentsFailClosedBeforeExternalEffect(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":"upstream","result":{"content":[]}}`)
	})))
	defer upstream.Close()

	audit := &memoryAudit{}
	effects := &memoryEffects{}
	expected := json.RawMessage(`{"owner":"constraint-secret-owner","repo":"gateway","nested":{"enabled":true,"labels":["one",2]}}`)
	broker, err := New(
		policySource{{Server: "github", Tool: "create_branch", Resources: []string{"github:repo:acme/gateway"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow, Arguments: expected}},
		credentialSource("credential-secret"),
		serverSource(Server{Name: "github", Endpoint: upstream.URL, CredentialRef: "credential"}),
		effects, audit, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	for index, arguments := range []string{
		`{"owner":"constraint-secret-owner","repo":"gateway","nested":{"enabled":true}}`,
		`{"owner":"constraint-secret-owner","repo":"gateway","nested":{"enabled":true,"labels":["one",2]},"extra":"secret-argument"}`,
		`{"owner":"constraint-secret-owner","repo":"gateway","nested":{"enabled":false,"labels":["one",2]}}`,
	} {
		_, err := broker.Call(context.Background(), Request{
			OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run",
			Server: "github", Tool: "create_branch", Resource: "github:repo:acme/gateway",
			EffectKey: fmt.Sprintf("effect-%d", index), Approved: true, Arguments: json.RawMessage(arguments),
		})
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("case %d error=%v, want ErrDenied", index, err)
		}
		if strings.Contains(err.Error(), "constraint-secret-owner") || strings.Contains(err.Error(), "secret-argument") {
			t.Fatalf("case %d leaked constrained arguments in error: %v", index, err)
		}
	}
	if upstreamCalls != 0 {
		t.Fatalf("mismatched exact arguments reached upstream %d time(s)", upstreamCalls)
	}
	if len(effects.claims) != 0 {
		t.Fatalf("mismatched exact arguments claimed external effects: %#v", effects.claims)
	}
	records := audit.snapshot()
	if len(records) != 6 {
		t.Fatalf("audit records=%d, want one authorization and one outcome per denial: %#v", len(records), records)
	}
	encoded, _ := json.Marshal(records)
	if strings.Contains(string(encoded), "constraint-secret-owner") || strings.Contains(string(encoded), "secret-argument") || strings.Contains(string(encoded), "credential-secret") {
		t.Fatalf("secret payload crossed audit boundary: %s", encoded)
	}
	for _, record := range records {
		if record.RequestDigest == "" || record.EffectDigest == "" && record.Phase == "outcome" {
			t.Fatalf("audit record was not bounded: %#v", record)
		}
	}
}

func TestExactArgumentsMatchForwardsAndClaimsOnce(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		id, _ := r.Context().Value(mcpTestUpstreamRequestID{}).(string)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[]}}`, id)
	})))
	defer upstream.Close()
	effects := &memoryEffects{}
	expected := json.RawMessage(`{"owner":"acme","repo":"gateway","nested":{"enabled":true,"labels":["one",2]}}`)
	broker, err := New(
		policySource{{Server: "github", Tool: "create_branch", Resources: []string{"github:repo:acme/gateway"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow, Arguments: expected}},
		credentialSource("credential"), serverSource(Server{Name: "github", Endpoint: upstream.URL, CredentialRef: "credential"}), effects, &memoryAudit{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.Call(context.Background(), Request{
		OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "github", Tool: "create_branch",
		Resource: "github:repo:acme/gateway", EffectKey: "effect", Approved: true,
		Arguments: json.RawMessage(`{"nested":{"labels":["one",2.0],"enabled":true},"repo":"gateway","owner":"acme"}`),
	})
	if err != nil || upstreamCalls != 1 || effects.states["run/effect"] != "succeeded" {
		t.Fatalf("matching exact arguments failed: err=%v upstream=%d effects=%v", err, upstreamCalls, effects.states)
	}
}
