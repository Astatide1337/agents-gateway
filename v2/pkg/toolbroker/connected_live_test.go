package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

// TestConnectedMCPGatewayLive proves that the production broker can negotiate
// Streamable HTTP with the real MCP Gateway, perform one read, record one
// authorized external write, deny its duplicate effect, and clean the test
// annotation through a separately authorized destructive effect.
func TestConnectedMCPGatewayLive(t *testing.T) {
	if os.Getenv("AGW_MCP_LIVE") != "1" {
		t.Skip("set AGW_MCP_LIVE=1 to exercise the connected MCP Gateway")
	}
	targetValue := strings.TrimSpace(os.Getenv("AGW_MCP_LIVE_UPSTREAM"))
	token := strings.TrimSpace(os.Getenv("AGW_MCP_LIVE_TOKEN"))
	if targetValue == "" || token == "" {
		t.Fatal("AGW_MCP_LIVE_UPSTREAM and AGW_MCP_LIVE_TOKEN are required")
	}
	target, err := url.Parse(targetValue)
	if err != nil || target.Scheme == "" || target.Host == "" || target.User != nil {
		t.Fatal("AGW_MCP_LIVE_UPSTREAM must be an absolute URL without credentials")
	}
	target.Path = ""
	target.RawPath = ""
	proxy := httputil.NewSingleHostReverseProxy(target)
	loopback := httptest.NewServer(proxy)
	defer loopback.Close()

	effects := &memoryEffects{}
	audit := &memoryAudit{}
	cleanupToken := strings.TrimSpace(os.Getenv("AGW_MCP_LIVE_GITHUB_TOKEN"))
	if cleanupToken == "" {
		t.Fatal("AGW_MCP_LIVE_GITHUB_TOKEN is required for deterministic branch cleanup")
	}
	grants := policySource{
		{Server: "gateway", Tool: "get_me", Effect: toolpolicy.EffectRead, Approval: toolpolicy.ApprovalAllow},
		{Server: "gateway", Tool: "create_branch", Resources: []string{"github:repo:Astatide1337/agents-gateway"}, Effect: toolpolicy.EffectWrite, Approval: toolpolicy.ApprovalAllow},
	}
	broker, err := New(grants, credentialSource(token), serverSource(Server{Name: "gateway", Endpoint: loopback.URL + "/mcp", CredentialRef: "live-token"}), effects, audit, loopback.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	base := Request{OrganizationID: "live-org", ProjectID: "live-project", UserID: "live-owner", RunID: "live-connected-run", Server: "gateway"}

	read := base
	read.Tool = "get_me"
	read.Arguments = json.RawMessage(`{}`)
	readResult, err := broker.Call(ctx, read)
	if err != nil || len(readResult.Content) == 0 || readResult.EffectState != "read" {
		t.Fatalf("connected MCP read failed: state=%q error=%v", readResult.EffectState, err)
	}

	now := time.Now().UTC()
	branch := "agw-e2e/" + strconv.FormatInt(now.UnixNano(), 10)
	write := base
	write.Tool = "create_branch"
	write.Resource = "github:repo:Astatide1337/agents-gateway"
	write.EffectKey = "live-branch-create-" + strconv.FormatInt(now.UnixNano(), 10)
	write.Approved = true
	write.Arguments = mustLiveJSON(t, map[string]any{
		"owner":       "Astatide1337",
		"repo":        "agents-gateway",
		"branch":      branch,
		"from_branch": "main",
	})
	writeResult, err := broker.Call(ctx, write)
	if err != nil || writeResult.EffectState != "succeeded" {
		t.Fatalf("connected MCP write failed: state=%q error=%v", writeResult.EffectState, err)
	}
	if _, err := broker.Call(ctx, write); !errors.Is(err, ErrEffectAlreadyClaimed) {
		t.Fatalf("duplicate external write was not denied: %v", err)
	}
	if err := deleteLiveBranch(ctx, cleanupToken, branch); err != nil {
		t.Fatalf("clean connected MCP branch %q: %v", branch, err)
	}
	if effects.states[base.RunID+"/"+write.EffectKey] != "succeeded" {
		t.Fatalf("external effects were not durably classified: %#v", effects.states)
	}
	records := audit.snapshot()
	if len(records) < 6 {
		t.Fatalf("expected durable authorization and terminal records for read, write, and duplicate; got %d", len(records))
	}
	if records[len(records)-1].Outcome != "duplicate" || records[len(records)-1].Phase != "outcome" {
		t.Fatalf("duplicate write was not durably audited: %#v", records[len(records)-1])
	}
	var sawSucceeded bool
	for _, record := range records {
		if record.Tool == "create_branch" && record.Outcome == "succeeded" && record.Phase == "outcome" {
			sawSucceeded = true
		}
	}
	if !sawSucceeded {
		t.Fatalf("successful branch write was not durably audited: %#v", records)
	}
}

func mustLiveJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func deleteLiveBranch(ctx context.Context, token, branch string) error {
	endpoint := "https://api.github.com/repos/Astatide1337/agents-gateway/git/refs/heads/" + url.PathEscape(branch)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "agents-gateway-v2-e2e")
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	return nil
}
