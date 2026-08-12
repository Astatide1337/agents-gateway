// Package guard is the small AGW-owned boundary used by the Phase 0
// agentgateway composition fixture. It is intentionally not a production
// broker: it exists to prove the ordering and isolation invariants before a
// real runtime is wired to agentgateway.
package guard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

const (
	SchemaVersion = 1
	CallPath      = "/call"
	HealthPath    = "/healthz"
	DefaultTool   = "record"
)

var (
	ErrBinding       = errors.New("run or resolved-spec binding failed")
	ErrPolicy        = errors.New("tool policy denied the call")
	ErrInvalidCall   = errors.New("invalid guard call")
	ErrUnknownEffect = errors.New("external effect outcome is unknown")
)

// Call is the only operation exposed by the fixture. The guard deliberately
// does not expose a generic URL-forwarding or arbitrary MCP method endpoint.
type Call struct {
	RunUID         string            `json:"runUID"`
	SpecDigest     string            `json:"specDigest"`
	Server         string            `json:"server"`
	Tool           string            `json:"tool"`
	Resource       string            `json:"resource,omitempty"`
	Arguments      json.RawMessage   `json:"arguments"`
	DeclaredEffect toolpolicy.Effect `json:"declaredEffect"`
	Effect         *EffectRequest    `json:"effect,omitempty"`
}

// EffectRequest contains the immutable inputs used by effects.EffectKey. A
// write cannot proceed without all of them; a display name is never enough.
type EffectRequest struct {
	BaseSHA     string `json:"baseSHA"`
	PatchDigest string `json:"patchDigest"`
	Operation   string `json:"operation"`
}

type DownstreamCall struct {
	Server    string
	Tool      string
	Resource  string
	Arguments json.RawMessage
}

// Downstream is intentionally narrow. The real composition uses
// HTTPDownstream to speak MCP to agentgateway; unit tests use a counting
// implementation to prove denied calls never reach upstream.
type Downstream interface {
	Call(context.Context, DownstreamCall) (json.RawMessage, error)
}

// ApprovalVerifier is trusted configuration, never request input. An
// implementation must bind its decision to the immutable effect key and
// request digest supplied by the guard. The Phase 0 fixture may approve one
// precomputed key in a unit test; the HTTP body cannot assert approval.
type ApprovalVerifier interface {
	Verify(context.Context, Call, string, string) (bool, error)
}

type ApprovalVerifierFunc func(context.Context, Call, string, string) (bool, error)

func (f ApprovalVerifierFunc) Verify(ctx context.Context, call Call, effectKey, requestDigest string) (bool, error) {
	return f(ctx, call, effectKey, requestDigest)
}

type Config struct {
	RunUID     string
	SpecDigest string
	BaseSHA    string
	Grants     []toolpolicy.Grant
	Ledger     *effects.Ledger
	Downstream Downstream
	Approvals  ApprovalVerifier
}

type Guard struct {
	runUID     string
	specDigest string
	baseSHA    string
	grants     []toolpolicy.Grant
	ledger     *effects.Ledger
	downstream Downstream
	approvals  ApprovalVerifier
}

type Response struct {
	SchemaVersion int             `json:"schemaVersion"`
	Allowed       bool            `json:"allowed"`
	Replayed      bool            `json:"replayed,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type Decision struct {
	Allowed         bool
	NeedsApproval   bool
	Replayed        bool
	EffectiveEffect toolpolicy.Effect
	Reason          string
	EffectKey       string
	RequestDigest   string
}

func New(cfg Config) (*Guard, error) {
	if !validIdentifier(cfg.RunUID) || !validDigest(cfg.SpecDigest) || cfg.Downstream == nil {
		return nil, ErrInvalidCall
	}
	grants := append([]toolpolicy.Grant(nil), cfg.Grants...)
	return &Guard{
		runUID:     cfg.RunUID,
		specDigest: cfg.SpecDigest,
		baseSHA:    cfg.BaseSHA,
		grants:     grants,
		ledger:     cfg.Ledger,
		downstream: cfg.Downstream,
		approvals:  cfg.Approvals,
	}, nil
}

// Authorize validates the strict JSON boundary and immutable identity before
// toolpolicy sees the request. It performs no downstream call.
func (g *Guard) Authorize(call Call) (Decision, error) {
	return g.authorize(context.Background(), call)
}

func (g *Guard) authorize(ctx context.Context, call Call) (Decision, error) {
	if g == nil || !validIdentifier(call.RunUID) || !validDigest(call.SpecDigest) ||
		call.Server == "" || call.Tool == "" || call.DeclaredEffect == toolpolicy.EffectUnknown {
		return Decision{}, ErrInvalidCall
	}
	if call.RunUID != g.runUID || call.SpecDigest != g.specDigest {
		return Decision{Reason: "immutable run/spec binding mismatch"}, ErrBinding
	}
	if err := strictjson.ValidateObject(call.Arguments); err != nil {
		return Decision{Reason: "arguments rejected by strict JSON contract"}, fmt.Errorf("%w: %v", ErrInvalidCall, err)
	}
	request := toolpolicy.Request{
		Server:         call.Server,
		Tool:           call.Tool,
		Resource:       call.Resource,
		Arguments:      call.Arguments,
		DeclaredEffect: call.DeclaredEffect,
	}
	policy := toolpolicy.Evaluate(g.grants, request)
	requestDigest, err := requestDigest(call)
	if err != nil {
		return Decision{}, err
	}
	decision := Decision{
		Allowed:         policy.Allowed,
		NeedsApproval:   policy.NeedsApproval,
		EffectiveEffect: policy.EffectiveEffect,
		Reason:          policy.Reason,
	}
	if policy.NeedsApproval {
		effectKey, err := g.effectKey(call)
		if err != nil {
			return Decision{Reason: "write requires complete effect identity"}, err
		}
		decision.EffectKey = effectKey
		if g.approvals == nil {
			return decision, nil
		}
		approved, err := g.approvals.Verify(ctx, call, effectKey, requestDigest)
		if err != nil {
			return Decision{Reason: "trusted approval verifier failed"}, err
		}
		request.Approved = approved
		policy = toolpolicy.Evaluate(g.grants, request)
		decision.Allowed = policy.Allowed
		decision.NeedsApproval = policy.NeedsApproval
		decision.EffectiveEffect = policy.EffectiveEffect
		decision.Reason = policy.Reason
	}
	if !policy.Allowed {
		if policy.NeedsApproval {
			return decision, nil
		}
		return decision, ErrPolicy
	}
	if call.DeclaredEffect != policy.EffectiveEffect {
		return Decision{Reason: "declared effect does not match the resolved grant"}, ErrPolicy
	}
	if policy.EffectiveEffect != toolpolicy.EffectRead {
		if decision.EffectKey == "" {
			effectKey, err := g.effectKey(call)
			if err != nil {
				return Decision{Reason: "invalid effect identity"}, err
			}
			decision.EffectKey = effectKey
		}
	}
	decision.RequestDigest = requestDigest
	return decision, nil
}

// Execute authorizes and, only after authorization, sends one request to the
// downstream. There is deliberately no retry loop. An uncertain write is
// committed as OutcomeUnknown and is terminal to this fixture.
func (g *Guard) Execute(ctx context.Context, call Call) (Response, error) {
	decision, authErr := g.authorize(ctx, call)
	if authErr != nil || !decision.Allowed {
		if decision.NeedsApproval {
			return Response{SchemaVersion: SchemaVersion, Error: "approval_required"}, ErrPolicy
		}
		if authErr == nil {
			authErr = ErrPolicy
		}
		return Response{SchemaVersion: SchemaVersion, Error: safeError(decision.Reason, authErr)}, authErr
	}

	if decision.EffectiveEffect != toolpolicy.EffectRead {
		claim := effects.Claim{
			EffectKey:     decision.EffectKey,
			RequestDigest: decision.RequestDigest,
			Operation:     call.Effect.Operation,
			RunUID:        g.runUID,
		}
		if g.ledger == nil {
			return Response{SchemaVersion: SchemaVersion, Error: "effect_ledger_required"}, ErrInvalidCall
		}
		claimDecision, err := g.ledger.Claim(ctx, claim)
		if err != nil {
			if errors.Is(err, effects.ErrUnknown) {
				return Response{SchemaVersion: SchemaVersion, Error: "unknown_effect"}, ErrUnknownEffect
			}
			return Response{SchemaVersion: SchemaVersion, Error: "effect_claim_failed"}, err
		}
		if !claimDecision.Execute {
			return Response{SchemaVersion: SchemaVersion, Allowed: true, Replayed: true}, nil
		}
		result, err := g.downstream.Call(ctx, DownstreamCall{Server: call.Server, Tool: call.Tool, Resource: call.Resource, Arguments: append([]byte(nil), call.Arguments...)})
		if err != nil {
			_ = g.ledger.Commit(ctx, effects.Outcome{EffectKey: decision.EffectKey, RequestDigest: decision.RequestDigest, State: effects.OutcomeUnknown})
			return Response{SchemaVersion: SchemaVersion, Error: "unknown_effect"}, ErrUnknownEffect
		}
		if err := validateDownstreamResult(result); err != nil {
			_ = g.ledger.Commit(ctx, effects.Outcome{EffectKey: decision.EffectKey, RequestDigest: decision.RequestDigest, State: effects.OutcomeUnknown})
			return Response{SchemaVersion: SchemaVersion, Error: "unknown_effect"}, ErrUnknownEffect
		}
		resultDigest := digestBytes(result)
		if err := g.ledger.Commit(ctx, effects.Outcome{EffectKey: decision.EffectKey, RequestDigest: decision.RequestDigest, State: effects.OutcomeSucceeded, ResultDigest: resultDigest}); err != nil {
			return Response{SchemaVersion: SchemaVersion, Error: "unknown_effect"}, ErrUnknownEffect
		}
		return Response{SchemaVersion: SchemaVersion, Allowed: true, Result: append([]byte(nil), result...)}, nil
	}

	result, err := g.downstream.Call(ctx, DownstreamCall{Server: call.Server, Tool: call.Tool, Resource: call.Resource, Arguments: append([]byte(nil), call.Arguments...)})
	if err != nil {
		return Response{SchemaVersion: SchemaVersion, Error: "downstream_failed"}, err
	}
	if err := validateDownstreamResult(result); err != nil {
		return Response{SchemaVersion: SchemaVersion, Error: "downstream_failed"}, err
	}
	return Response{SchemaVersion: SchemaVersion, Allowed: true, Result: append([]byte(nil), result...)}, nil
}

func (g *Guard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeResponse(w, http.StatusMethodNotAllowed, Response{SchemaVersion: SchemaVersion, Error: "method_not_allowed"})
			return
		}
		writeResponse(w, http.StatusOK, Response{SchemaVersion: SchemaVersion, Allowed: true})
	})
	mux.HandleFunc(CallPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeResponse(w, http.StatusMethodNotAllowed, Response{SchemaVersion: SchemaVersion, Error: "method_not_allowed"})
			return
		}
		call, err := decodeCall(r)
		if err != nil {
			writeResponse(w, http.StatusBadRequest, Response{SchemaVersion: SchemaVersion, Error: "invalid_call"})
			return
		}
		response, err := g.Execute(r.Context(), call)
		status := http.StatusOK
		switch {
		case errors.Is(err, ErrUnknownEffect):
			status = http.StatusConflict
		case errors.Is(err, ErrPolicy), errors.Is(err, ErrBinding):
			status = http.StatusForbidden
		case errors.Is(err, ErrInvalidCall):
			status = http.StatusBadRequest
		case err != nil:
			status = http.StatusBadGateway
		}
		writeResponse(w, status, response)
	})
	return mux
}

func decodeCall(r *http.Request) (Call, error) {
	if r == nil || r.Body == nil {
		return Call{}, ErrInvalidCall
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, strictjson.MaxDocumentBytes+1))
	if err != nil || len(body) > strictjson.MaxDocumentBytes || strictjson.ValidateObject(body) != nil {
		return Call{}, ErrInvalidCall
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var call Call
	if err := decoder.Decode(&call); err != nil {
		return Call{}, ErrInvalidCall
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Call{}, ErrInvalidCall
	}
	if len(call.Arguments) == 0 {
		return Call{}, ErrInvalidCall
	}
	return call, nil
}

func writeResponse(w http.ResponseWriter, status int, response Response) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}

func requestDigest(call Call) (string, error) {
	normalized, err := strictjson.Normalize(call.Arguments)
	if err != nil {
		return "", err
	}
	payload := struct {
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Resource  string          `json:"resource"`
		Arguments json.RawMessage `json:"arguments"`
	}{call.Server, call.Tool, call.Resource, normalized}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return digestBytes(body), nil
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, r := range strings.TrimPrefix(value, "sha256:") {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/", r) {
			continue
		}
		return false
	}
	return true
}

func safeError(reason string, fallback error) string {
	if reason != "" {
		return reason
	}
	if fallback == nil {
		return "denied"
	}
	return fallback.Error()
}

func validateDownstreamResult(result []byte) error {
	if len(result) == 0 || len(result) > strictjson.MaxDocumentBytes {
		return errors.New("downstream result exceeds strict JSON body limit")
	}
	if err := strictjson.Validate(result); err != nil {
		return errors.New("downstream result violates strict JSON contract")
	}
	return nil
}

// HTTPDownstream is the Phase 0 transport seam. It performs one initialize
// handshake and then one tools/call request per guard call. It does not retry a
// POST; a write uncertainty is owned by the effects ledger above it.
type HTTPDownstream struct {
	endpoint    string
	client      *http.Client
	mu          sync.Mutex
	sessionID   string
	initialized bool
}

func NewHTTPDownstream(endpoint string, client *http.Client) (*HTTPDownstream, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" || parsed.Path != "/mcp" || parsed.Port() != "8082" || parsed.String() != endpoint {
		return nil, fmt.Errorf("downstream must be loopback HTTP: %w", ErrInvalidCall)
	}
	if parsed.Host != "127.0.0.1:8082" && parsed.Host != "[::1]:8082" {
		return nil, fmt.Errorf("downstream host must be exact loopback: %w", ErrInvalidCall)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		// A caller-supplied client is still trusted configuration, but redirects
		// must never turn the exact loopback URL into a second destination. Copy
		// the client so construction cannot mutate a shared client used elsewhere.
		copy := *client
		copy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
		if copy.Timeout <= 0 {
			copy.Timeout = 15 * time.Second
		}
		client = &copy
	}
	return &HTTPDownstream{endpoint: endpoint, client: client}, nil
}

func (g *Guard) effectKey(call Call) (string, error) {
	if call.Effect == nil || call.Effect.BaseSHA == "" || call.Effect.PatchDigest == "" || call.Effect.Operation == "" {
		return "", fmt.Errorf("%w: write requires complete effect identity", ErrInvalidCall)
	}
	// A write must be bound to the immutable base selected for this run. An
	// empty configured base is not a wildcard: allowing it would let the
	// caller choose a fresh effect identity for every retry.
	if g.baseSHA == "" || call.Effect.BaseSHA != g.baseSHA {
		return "", ErrBinding
	}
	effectKey, err := effects.EffectKey(g.runUID, call.Effect.BaseSHA, call.Effect.PatchDigest, call.Effect.Operation)
	if err != nil {
		return "", fmt.Errorf("%w: invalid effect identity", ErrInvalidCall)
	}
	return effectKey, nil
}

func (d *HTTPDownstream) Call(ctx context.Context, call DownstreamCall) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := strictjson.ValidateObject(call.Arguments); err != nil {
		return nil, fmt.Errorf("%w: invalid downstream arguments", ErrInvalidCall)
	}
	if !d.initialized {
		result, sessionID, err := d.rpc(ctx, "phase0-init", "initialize", map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "agw-phase0-guard", "version": "0.1.0"},
		}, "")
		if err != nil {
			return nil, err
		}
		if len(result) == 0 {
			return nil, errors.New("agentgateway initialize returned no result")
		}
		d.sessionID = sessionID
		if _, _, err := d.rpc(ctx, "phase0-initialized", "notifications/initialized", map[string]any{}, d.sessionID); err != nil {
			return nil, err
		}
		d.initialized = true
	}
	// Preserve the strict JSON representation. Decoding through interface{}
	// would convert numbers to float64 and can silently change large integer
	// arguments before they reach the downstream tool.
	result, _, err := d.rpc(ctx, "phase0-call", "tools/call", map[string]any{"name": call.Tool, "arguments": json.RawMessage(call.Arguments)}, d.sessionID)
	return result, err
}

func (d *HTTPDownstream) rpc(ctx context.Context, id, method string, params any, sessionID string) (json.RawMessage, string, error) {
	payload := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	// JSON-RPC notifications have no id. Sending an id on
	// notifications/initialized turns it into a request and makes a strict
	// gateway reject the handshake before the actual tool call. Keep this
	// protocol distinction in the transport seam rather than relying on a
	// permissive upstream to ignore malformed notifications.
	if method != "notifications/initialized" {
		payload["id"] = id
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json, text/event-stream")
	request.Header.Set("mcp-protocol-version", "2025-06-18")
	if sessionID != "" {
		request.Header.Set("mcp-session-id", sessionID)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, strictjson.MaxDocumentBytes+1))
	if err != nil || len(responseBody) > strictjson.MaxDocumentBytes {
		if err == nil {
			err = errors.New("agentgateway response exceeds strict JSON body limit")
		}
		return nil, "", err
	}
	if response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("agentgateway returned HTTP %d", response.StatusCode)
	}
	if method == "notifications/initialized" {
		return nil, response.Header.Get("mcp-session-id"), nil
	}
	responseBody = extractSSEData(responseBody)
	if err := strictjson.ValidateObject(responseBody); err != nil {
		return nil, "", errors.New("invalid JSON-RPC response from agentgateway")
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.JSONRPC != "2.0" {
		return nil, "", errors.New("invalid JSON-RPC response from agentgateway")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", errors.New("invalid JSON-RPC response from agentgateway")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return nil, "", errors.New("agentgateway returned an MCP error")
	}
	if len(envelope.Result) == 0 {
		return nil, "", errors.New("agentgateway response has no result")
	}
	if err := strictjson.Validate(envelope.Result); err != nil {
		return nil, "", errors.New("agentgateway response result violates strict JSON contract")
	}
	return envelope.Result, response.Header.Get("mcp-session-id"), nil
}

func extractSSEData(body []byte) []byte {
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return bytes.TrimSpace(body)
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
	}
	return body
}
