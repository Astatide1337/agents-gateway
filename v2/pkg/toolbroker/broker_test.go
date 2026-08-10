package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

type policySource []toolpolicy.Grant

func (p policySource) Grants(context.Context, string, string, string) ([]toolpolicy.Grant, error) {
	return p, nil
}

type credentialSource string

func (c credentialSource) ResolveCredential(context.Context, string, string) ([]byte, error) {
	return []byte(c), nil
}

type serverSource Server

func (s serverSource) Server(context.Context, string, string, string) (Server, error) {
	return Server(s), nil
}

type memoryEffects struct {
	claims map[string]string
	states map[string]string
}

type memoryAudit struct {
	mu      sync.Mutex
	records []AuditRecord
	err     error
}

func (m *memoryAudit) AppendAudit(_ context.Context, record AuditRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.records = append(m.records, record)
	return nil
}

func (m *memoryAudit) snapshot() []AuditRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuditRecord(nil), m.records...)
}

func (m *memoryEffects) Claim(_ context.Context, _, _, run, key, digest string) (bool, error) {
	if m.claims == nil {
		m.claims = map[string]string{}
		m.states = map[string]string{}
	}
	id := run + "/" + key
	if _, ok := m.claims[id]; ok {
		return false, nil
	}
	m.claims[id] = digest
	m.states[id] = "claimed"
	return true, nil
}
func (m *memoryEffects) Complete(_ context.Context, _, _, run, key, state string, _ []byte) error {
	m.states[run+"/"+key] = state
	return nil
}

func TestWriteRequiresApprovalAndIsClaimedOnce(t *testing.T) {
	calls := 0
	server := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		id, _ := r.Context().Value(mcpTestUpstreamRequestID{}).(string)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, id)
	})))
	defer server.Close()
	effects := &memoryEffects{}
	broker, _ := New(policySource{{Server: "github", Tool: "create_pr", Resources: []string{"github:repo:owner/repo"}, Effect: toolpolicy.EffectWrite}}, credentialSource("token"), serverSource(Server{Name: "github", Endpoint: server.URL, CredentialRef: "cred"}), effects, &memoryAudit{}, nil)
	request := Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "github", Tool: "create_pr", Resource: "github:repo:owner/repo", EffectKey: "effect", Arguments: json.RawMessage(`{"title":"change"}`)}
	if _, err := broker.Call(context.Background(), request); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("expected approval, got %v", err)
	}
	request.Approved = true
	if _, err := broker.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Call(context.Background(), request); !errors.Is(err, ErrEffectAlreadyClaimed) {
		t.Fatalf("expected duplicate claim denial, got %v", err)
	}
	if calls != 1 || effects.states["run/effect"] != "succeeded" {
		t.Fatalf("calls=%d states=%v", calls, effects.states)
	}
}

func TestInitializationTransportFailureMarksFailedAndDoesNotLeakSecret(t *testing.T) {
	effects := &memoryEffects{}
	broker, _ := New(policySource{{Server: "x", Tool: "write", Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow}}, credentialSource("never-log-this"), serverSource(Server{Name: "x", Endpoint: "http://127.0.0.1:1", CredentialRef: "cred"}), effects, &memoryAudit{}, nil)
	_, err := broker.Call(context.Background(), Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "x", Tool: "write", EffectKey: "effect", Arguments: json.RawMessage(`{}`)})
	if err == nil || effects.states["run/effect"] != "failed" {
		t.Fatalf("error=%v states=%v", err, effects.states)
	}
}

func TestResourceOutsideGrantDenied(t *testing.T) {
	broker, _ := New(policySource{{Server: "github", Tool: "read", Resources: []string{"github:repo:owner/allowed"}, Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow}}, credentialSource(""), serverSource(Server{}), &memoryEffects{}, &memoryAudit{}, nil)
	_, err := broker.Call(context.Background(), Request{OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "github", Tool: "read", Resource: "github:repo:owner/other", Arguments: json.RawMessage(`{}`)})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expected denied, got %v", err)
	}
}

func TestAuditIsMandatoryAndPrivacyBounded(t *testing.T) {
	var upstreamCalls int
	var upstreamRequestID string
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		id, _ := r.Context().Value(mcpTestUpstreamRequestID{}).(string)
		upstreamRequestID = id
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"response-secret"}]}}`, id)
	})))
	defer upstream.Close()
	audit := &memoryAudit{}
	effects := &memoryEffects{}
	broker, err := New(
		policySource{{Server: "github", Tool: "write", Resources: []string{"github:repo:owner/repo"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow}},
		credentialSource("credential-secret"),
		serverSource(Server{Name: "github", Endpoint: upstream.URL, CredentialRef: "credential"}),
		effects, audit, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "github", Tool: "write",
		Resource: "github:repo:owner/repo", EffectKey: "raw-effect-key", Approved: true,
		Arguments: json.RawMessage(`{"secret":"argument-secret"}`),
	}
	if _, err := broker.Call(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	records := audit.snapshot()
	if len(records) != 2 || records[0].Phase != "authorization" || records[1].Outcome != "succeeded" {
		t.Fatalf("unexpected audit records: %#v", records)
	}
	encoded, _ := json.Marshal(records)
	for _, forbidden := range []string{"credential-secret", "argument-secret", "raw-effect-key", "response-secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("audit contained forbidden value %q: %s", forbidden, encoded)
		}
	}
	if records[1].EffectDigest == "" || records[1].EffectDigest == "raw-effect-key" || records[1].RequestDigest == "" {
		t.Fatalf("audit did not use bounded digests: %#v", records[1])
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls=%d, want 1", upstreamCalls)
	}
	if strings.Contains(upstreamRequestID, "raw-effect-key") || !strings.Contains(upstreamRequestID, "mcp-write:sha256:") {
		t.Fatalf("upstream request ID crossed raw effect boundary: %q", upstreamRequestID)
	}
}

func TestAuditAuthorizationFailsClosedBeforeUpstream(t *testing.T) {
	audit := &memoryAudit{err: errors.New("audit store offline")}
	var upstreamCalls int
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
	})))
	defer upstream.Close()
	broker, err := New(
		policySource{{Server: "github", Tool: "read", Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow}},
		credentialSource("credential"), serverSource(Server{Name: "github", Endpoint: upstream.URL, CredentialRef: "credential"}),
		&memoryEffects{}, audit, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.Call(context.Background(), Request{
		OrganizationID: "org", ProjectID: "project", UserID: "user", RunID: "run", Server: "github", Tool: "read", Arguments: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrAuditUnavailable) || upstreamCalls != 0 {
		t.Fatalf("audit failure did not fail closed: error=%v upstreamCalls=%d", err, upstreamCalls)
	}
}

func sessionMCPHandler(tool http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch request.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "test-session")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`, request.ID)
		case "notifications/initialized":
			if r.Header.Get("Mcp-Session-Id") != "test-session" {
				http.Error(w, "missing session", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "test-session" {
				http.Error(w, "missing session", http.StatusBadRequest)
				return
			}
			tool.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpTestUpstreamRequestID{}, string(request.ID))))
		default:
			http.Error(w, "unsupported method", http.StatusBadRequest)
		}
	})
}

type mcpTestUpstreamRequestID struct{}
