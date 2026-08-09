// Package toolbroker is the only path from a normal agent sandbox to MCP.
// It applies capability policy before resolving upstream credentials and uses
// an effect ledger to prevent unsafe write retries.
package toolbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const maxMCPResponse = 8 << 20

const maxMCPRequestTimeout = 2 * time.Minute

var (
	ErrDenied               = errors.New("tool call denied")
	ErrApprovalRequired     = errors.New("tool call requires approval")
	ErrEffectAlreadyClaimed = errors.New("effect has already been claimed")
	// ErrOutcomeUnknown marks a request that may have reached an upstream MCP
	// server but whose result could not be established safely. Callers must not
	// retry a mutating call with a new effect identity.
	ErrOutcomeUnknown = errors.New("tool call outcome is unknown")
)

type Server struct{ Name, Endpoint, CredentialRef string }

type Request struct {
	OrganizationID, ProjectID, UserID, RunID string
	Server, Tool, Resource, EffectKey        string
	Arguments                                json.RawMessage
	Approved                                 bool
}

type Result struct {
	Content     json.RawMessage
	EffectState string
}

type PolicySource interface {
	Grants(context.Context, string, string, string) ([]toolpolicy.Grant, error)
}
type CredentialResolver interface {
	ResolveCredential(context.Context, string, string) ([]byte, error)
}
type ServerSource interface {
	Server(context.Context, string, string, string) (Server, error)
}

type EffectLedger interface {
	Claim(context.Context, string, string, string, string, string) (bool, error)
	Complete(context.Context, string, string, string, string, string, []byte) error
}

type Broker struct {
	policy      PolicySource
	credentials CredentialResolver
	servers     ServerSource
	effects     EffectLedger
	audit       AuditSink
	client      *http.Client
}

func New(policy PolicySource, credentials CredentialResolver, servers ServerSource, effects EffectLedger, audit AuditSink, client *http.Client) (*Broker, error) {
	if policy == nil || credentials == nil || servers == nil || effects == nil || audit == nil {
		return nil, errors.New("policy, credentials, servers, effect ledger, and audit sink are required")
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		client = &http.Client{
			Transport: otelhttp.NewTransport(transport),
			Timeout:   maxMCPRequestTimeout,
		}
	} else {
		// A caller-supplied client must not be able to silently weaken the
		// upstream credential boundary. In particular, net/http follows
		// redirects by default and may carry Authorization to a same-host
		// redirect. Clone so New does not mutate a client owned by its caller.
		copy := *client
		copy.CheckRedirect = rejectMCPRedirects
		if copy.Timeout == 0 || copy.Timeout > maxMCPRequestTimeout {
			copy.Timeout = maxMCPRequestTimeout
		}
		client = &copy
	}
	return &Broker{policy: policy, credentials: credentials, servers: servers, effects: effects, audit: audit, client: client}, nil
}

func (b *Broker) Call(ctx context.Context, request Request) (Result, error) {
	if request.OrganizationID == "" || request.ProjectID == "" || request.UserID == "" || request.RunID == "" || request.Server == "" || request.Tool == "" || len(request.Arguments) == 0 {
		return Result{}, errors.New("complete tenant, principal, run, server, tool, and arguments are required")
	}
	if len(request.Arguments) > mcpMaxRequestBody || strictjson.ValidateObject(request.Arguments) != nil {
		return Result{}, errors.New("tool arguments must be a JSON object without duplicate keys")
	}
	if len(request.EffectKey) > mcpMaxCallID {
		return Result{}, errors.New("effect key exceeds size limit")
	}
	grants, err := b.policy.Grants(ctx, request.OrganizationID, request.ProjectID, request.RunID)
	if err != nil {
		return Result{}, fmt.Errorf("load capability policy: %w", err)
	}
	decision := toolpolicy.Evaluate(grants, toolpolicy.Request{Server: request.Server, Tool: request.Tool, Resource: request.Resource, Arguments: request.Arguments, Approved: request.Approved})
	requestDigest := digest(request.Server, request.Tool, request.Resource, request.Arguments)
	mutating := decision.EffectiveEffect != toolpolicy.EffectRead
	policyDecision := "denied"
	if decision.NeedsApproval {
		policyDecision = "approval_required"
	} else if decision.Allowed {
		policyDecision = "allowed"
	}
	if err := b.recordAudit(ctx, request, "authorization", policyDecision, policyDecision, requestDigest); err != nil {
		return Result{}, err
	}
	if decision.NeedsApproval {
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", ErrApprovalRequired)
	}
	if !decision.Allowed {
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", ErrDenied)
	}
	if mutating {
		if request.EffectKey == "" {
			return Result{}, b.terminalError(ctx, request, requestDigest, "failed", errors.New("mutating tool call requires an effect key"))
		}
		claimed, err := b.effects.Claim(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, requestDigest)
		if err != nil {
			return Result{}, b.terminalError(ctx, request, requestDigest, "failed", fmt.Errorf("claim effect: %w", err))
		}
		if !claimed {
			return Result{}, b.terminalError(ctx, request, requestDigest, "duplicate", ErrEffectAlreadyClaimed)
		}
	}
	server, err := b.servers.Server(ctx, request.OrganizationID, request.ProjectID, request.Server)
	if err != nil {
		if mutating {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil)
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", fmt.Errorf("resolve MCP server: %w", err))
	}
	endpoint, err := validateEndpoint(server.Endpoint)
	if err != nil {
		if mutating {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil)
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", err)
	}
	credential, err := b.credentials.ResolveCredential(ctx, request.OrganizationID, server.CredentialRef)
	if err != nil {
		if mutating {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil)
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", fmt.Errorf("resolve MCP credential: %w", err))
	}
	defer zero(credential)
	if err := validateMCPCredential(credential); err != nil {
		if mutating {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil)
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", err)
	}
	// Never forward the caller's effect key as a JSON-RPC identifier: upstream
	// servers may log request IDs. A digest is sufficient for correlation and
	// preserves the no-raw-effect-key boundary.
	rpcID := "mcp-read:" + requestDigest
	if mutating {
		rpcID = "mcp-write:" + effectDigest(request.EffectKey)
	}
	session, err := openUpstreamSession(ctx, b.client, endpoint, credential, rpcID)
	if err != nil {
		if mutating {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil)
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", err)
	}
	defer func() { session.close() }()
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpcID, "method": "tools/call", "params": map[string]any{"name": request.Tool, "arguments": json.RawMessage(request.Arguments)}})
	httpResponse, err := session.post(ctx, payload)
	if err != nil {
		if httpResponse != nil && httpResponse.Body != nil {
			_ = httpResponse.Body.Close()
		}
		return b.unknownFailure(ctx, request, mutating, requestDigest, err)
	}
	if httpResponse.StatusCode == http.StatusNotFound && session.sessionID != "" && !mutating {
		// MCP requires clients to establish a new session after a 404 for a
		// session-bound request. Reads are safe to replay; writes are never
		// replayed because a 404 does not prove the upstream did not act.
		_ = httpResponse.Body.Close()
		session.close()
		session, err = openUpstreamSession(ctx, b.client, endpoint, credential, rpcID)
		if err != nil {
			return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", err)
		}
		httpResponse, err = session.post(ctx, payload)
		if err != nil {
			if httpResponse != nil && httpResponse.Body != nil {
				_ = httpResponse.Body.Close()
			}
			return b.unknownFailure(ctx, request, false, requestDigest, err)
		}
	}
	defer httpResponse.Body.Close()
	body, err := readMCPBody(httpResponse.Body)
	if err != nil {
		return b.unknownFailure(ctx, request, mutating, requestDigest, err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return b.unknownFailure(ctx, request, mutating, requestDigest, fmt.Errorf("MCP server returned HTTP %d", httpResponse.StatusCode))
	}
	if !validMCPResponseContentType(httpResponse.Header.Get("Content-Type")) {
		return b.unknownFailure(ctx, request, mutating, requestDigest, errors.New("MCP server returned an unsupported content type"))
	}
	wirePayload, err := decodeMCPWireBodyForRequest(body, httpResponse.Header.Get("Content-Type"), rpcID)
	if err != nil {
		return b.unknownFailure(ctx, request, mutating, requestDigest, err)
	}
	decoded, err := decodeUpstreamMCPResponse(wirePayload, rpcID)
	if err != nil {
		return b.unknownFailure(ctx, request, mutating, requestDigest, err)
	}
	if decoded.Error != nil {
		if mutating {
			if err := b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "failed", nil); err != nil {
				_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "unknown", nil)
				return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", fmt.Errorf("%w: persist failed effect result", ErrOutcomeUnknown))
			}
		}
		return Result{}, b.terminalError(ctx, request, requestDigest, "failed", fmt.Errorf("MCP tool failed with code %d", decoded.Error.Code))
	}
	toolFields, err := decodeMCPTopLevelObject(decoded.Result)
	if err != nil {
		return b.unknownFailure(ctx, request, mutating, requestDigest, err)
	}
	toolIsError := false
	if rawIsError, ok := toolFields["isError"]; ok {
		trimmedIsError := bytes.TrimSpace(rawIsError)
		if !bytes.Equal(trimmedIsError, []byte("true")) && !bytes.Equal(trimmedIsError, []byte("false")) {
			return b.unknownFailure(ctx, request, mutating, requestDigest, errors.New("MCP tool isError was not boolean"))
		}
		toolIsError = bytes.Equal(trimmedIsError, []byte("true"))
	}
	if toolIsError {
		if mutating {
			if err := b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "unknown", nil); err != nil {
				_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "unknown", nil)
				return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", fmt.Errorf("%w: persist unknown effect result", ErrOutcomeUnknown))
			}
			return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", ErrOutcomeUnknown)
		}
		// isError is a valid MCP CallToolResult, not a JSON-RPC transport
		// failure. Preserve it for read callers so the MCP adapter can expose
		// the upstream content and its isError marker.
		return b.finish(ctx, request, requestDigest, Result{Content: decoded.Result, EffectState: "read"}, "read")
	}
	if mutating {
		if err := b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "succeeded", decoded.Result); err != nil {
			_ = b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "unknown", nil)
			return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", fmt.Errorf("%w: persist effect result", ErrOutcomeUnknown))
		}
	}
	return b.finish(ctx, request, requestDigest, Result{Content: decoded.Result, EffectState: map[bool]string{true: "succeeded", false: "read"}[mutating]}, map[bool]string{true: "succeeded", false: "read"}[mutating])
}

func (b *Broker) unknownFailure(ctx context.Context, request Request, mutating bool, requestDigest string, cause error) (Result, error) {
	if !mutating {
		if auditErr := b.recordAudit(ctx, request, "outcome", "failed", "upstream_failed", requestDigest); auditErr != nil {
			return Result{}, auditErr
		}
		if cause == nil {
			return Result{}, errors.New("MCP upstream request failed")
		}
		return Result{}, fmt.Errorf("MCP upstream request failed: %w", cause)
	}
	if err := b.effects.Complete(ctx, request.OrganizationID, request.ProjectID, request.RunID, request.EffectKey, "unknown", nil); err != nil {
		return Result{}, b.terminalError(ctx, request, requestDigest, "unknown", fmt.Errorf("%w: persist unknown effect result", ErrOutcomeUnknown))
	}
	if auditErr := b.recordAudit(ctx, request, "outcome", "unknown", "upstream_failed", requestDigest); auditErr != nil {
		return Result{}, auditErr
	}
	if cause == nil {
		return Result{}, ErrOutcomeUnknown
	}
	return Result{}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, cause)
}

func (b *Broker) recordAudit(ctx context.Context, request Request, phase, outcome, decision, requestDigest string) error {
	record := AuditRecord{
		OrganizationID: request.OrganizationID, ProjectID: request.ProjectID, UserID: request.UserID, RunID: request.RunID,
		Server: request.Server, Tool: request.Tool, Resource: request.Resource, Phase: phase, Outcome: outcome, Decision: decision,
		RequestDigest: requestDigest, EffectDigest: effectDigest(request.EffectKey),
	}
	if err := record.validate(); err != nil {
		return fmt.Errorf("%w: invalid audit record", ErrAuditUnavailable)
	}
	if err := b.audit.AppendAudit(ctx, record); err != nil {
		return fmt.Errorf("%w: %s", ErrAuditUnavailable, phase)
	}
	return nil
}

func (b *Broker) terminalError(ctx context.Context, request Request, requestDigest, outcome string, cause error) error {
	if err := b.recordAudit(ctx, request, "outcome", outcome, outcome, requestDigest); err != nil {
		return err
	}
	return cause
}

func (b *Broker) finish(ctx context.Context, request Request, requestDigest string, result Result, outcome string) (Result, error) {
	if err := b.recordAudit(ctx, request, "outcome", outcome, outcome, requestDigest); err != nil {
		return Result{}, err
	}
	return result, nil
}

func rejectMCPRedirects(*http.Request, []*http.Request) error {
	return errors.New("MCP redirects are forbidden")
}

func validateEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid MCP endpoint")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return nil, errors.New("MCP endpoint must use HTTPS (HTTP loopback is allowed for tests)")
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Opaque != "" {
		return nil, errors.New("MCP endpoint contains forbidden URL components")
	}
	return parsed, nil
}
func digest(server, tool, resource string, args []byte) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{server, tool, resource, string(args)}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
