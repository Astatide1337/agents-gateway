package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/store"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

// TestFaultInjectionAmbiguousMCPWriteMarksUnknownAndCannotBeRetried proves
// the important failure boundary: the upstream accepts the write, the client
// loses the response, and a replay with the same effect identity is refused.
func TestFaultInjectionAmbiguousMCPWriteMarksUnknownAndCannotBeRetried(t *testing.T) {
	var upstreamWrites atomic.Int32
	upstream := httptest.NewServer(sessionMCPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamWrites.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		connection, _, err := hijacker.Hijack()
		if err == nil {
			// The request has reached the external system, but no response can
			// establish whether its side effect was committed.
			_ = connection.Close()
		}
	})))
	defer upstream.Close()

	scope := store.Scope{OrganizationID: "mcp-fault-org", ProjectID: "mcp-fault-project"}
	effects := store.NewMemory()
	if _, err := effects.CreateRun(context.Background(), store.Run{
		Scope: scope, ID: "mcp-fault-run", Kind: "AgentRun",
		DefinitionDigest: "sha256:" + repeatFaultHex('a'), RequestedBy: "fault-test",
	}); err != nil {
		t.Fatal(err)
	}
	broker, err := New(
		policySource{{Server: "external", Tool: "write", Resources: []string{"external:resource:one"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow}},
		credentialSource("fault-test-credential"),
		serverSource(Server{Name: "external", Endpoint: upstream.URL, CredentialRef: "credential"}),
		effects,
		&memoryAudit{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		OrganizationID: scope.OrganizationID, ProjectID: scope.ProjectID, UserID: "fault-user", RunID: "mcp-fault-run",
		Server: "external", Tool: "write", Resource: "external:resource:one", EffectKey: "effect-ambiguous",
		Arguments: json.RawMessage(`{"value":"one"}`), Approved: true,
	}
	if _, err := broker.Call(context.Background(), request); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("ambiguous MCP write error=%v, want ErrOutcomeUnknown", err)
	}
	if got := upstreamWrites.Load(); got != 1 {
		t.Fatalf("upstream write count after first attempt=%d, want 1", got)
	}
	if _, err := broker.Call(context.Background(), request); !errors.Is(err, ErrEffectAlreadyClaimed) {
		t.Fatalf("ambiguous MCP replay error=%v, want ErrEffectAlreadyClaimed", err)
	}
	if got := upstreamWrites.Load(); got != 1 {
		t.Fatalf("ambiguous replay duplicated upstream write: count=%d", got)
	}
}

func repeatFaultHex(value byte) string {
	result := make([]byte, 64)
	for i := range result {
		result[i] = value
	}
	return string(result)
}
