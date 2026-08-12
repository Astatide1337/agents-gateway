package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

// MCPErrorCodePhaseDenied is stable for callers that need to distinguish a
// phase-bound tool denial without learning the broker's policy details.
const MCPErrorCodePhaseDenied = -32006

// MCPHandler is the local, synchronous MCP Streamable HTTP surface used by
// Codex. It exposes only the compiled ToolSet; callers cannot name an
// arbitrary upstream server or effect class.
type MCPHandler struct {
	broker  *Broker
	name    string
	version string
}

// NewMCPHandler creates a handler bound to one broker/run.
func NewMCPHandler(b *Broker) (*MCPHandler, error) {
	if b == nil {
		return nil, ErrInvalidConfig
	}
	if b.mode == BrokerModeVerifier {
		return nil, ErrMCPUnavailable
	}
	if len(b.currentTools()) == 0 && b.policyContract.Digest() == "" {
		return nil, ErrInvalidConfig
	}
	return &MCPHandler{broker: b, name: "agents-gateway-v3", version: "3"}, nil
}

// MCPHandler returns the local MCP handler as an http.Handler.
func (b *Broker) MCPHandler() (http.Handler, error) {
	return NewMCPHandler(b)
}

func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.broker == nil || r == nil || !isLoopbackRequest(r) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/mcp" || r.URL.RawQuery != "" {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBrokerHTTPError(w, http.StatusMethodNotAllowed)
		return
	}
	if !contentTypeIs(r.Header.Get("Content-Type"), "application/json") {
		writeBrokerHTTPError(w, http.StatusUnsupportedMediaType)
		return
	}
	body, err := readBounded(r.Body, int64(h.broker.limits.maxMCPRequest))
	if err != nil {
		h.writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	request, err := parseLocalRPC(body)
	if err != nil {
		h.writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	if request.id == nil {
		if request.method != "notifications/initialized" {
			h.writeRPCError(w, nil, -32600, "invalid request")
			return
		}
		if err := h.dispatch(r.Context(), request); err != nil {
			h.writeRPCError(w, nil, rpcErrorCode(err), rpcErrorMessage(err))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, err := h.dispatchResult(r.Context(), request)
	if err != nil {
		h.writeRPCError(w, request.id, rpcErrorCode(err), rpcErrorMessage(err))
		return
	}
	h.writeRPCResult(w, request.id, result)
}

type localRPCRequest struct {
	id     json.RawMessage
	method string
	params json.RawMessage
}

func parseLocalRPC(body []byte) (localRPCRequest, error) {
	var zero localRPCRequest
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || strictJSONObject(trimmed) != nil {
		return zero, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(trimmed, &fields); err != nil {
		return zero, ErrInvalidRequest
	}
	for key := range fields {
		if key != "jsonrpc" && key != "id" && key != "method" && key != "params" {
			return zero, ErrInvalidRequest
		}
	}
	var version, method string
	if err := decodeField(fields, "jsonrpc", &version); err != nil || version != "2.0" || decodeField(fields, "method", &method) != nil || method == "" || len(method) > MaxToolNameBytes || !utf8.ValidString(method) {
		return zero, ErrInvalidRequest
	}
	request := localRPCRequest{method: method}
	if raw, ok := fields["id"]; ok {
		if !validRPCID(raw) {
			return zero, ErrInvalidRequest
		}
		request.id = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := fields["params"]; ok {
		if strictJSONObject(raw) != nil {
			return zero, ErrInvalidRequest
		}
		request.params = append(json.RawMessage(nil), raw...)
	} else {
		request.params = json.RawMessage(`{}`)
	}
	return request, nil
}

func (h *MCPHandler) dispatch(ctx context.Context, request localRPCRequest) error {
	_, err := h.dispatchResult(ctx, request)
	return err
}

func (h *MCPHandler) dispatchResult(ctx context.Context, request localRPCRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch request.method {
	case "initialize":
		if err := validateInitializeParams(request.params); err != nil {
			return nil, ErrInvalidRequest
		}
		return json.RawMessage(`{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"` + h.name + `","version":"` + h.version + `"}}`), nil
	case "notifications/initialized", "ping":
		if strictJSONObject(request.params) != nil {
			return nil, ErrInvalidRequest
		}
		return json.RawMessage(`{}`), nil
	case "tools/list":
		if strictJSONObject(request.params) != nil {
			return nil, ErrInvalidRequest
		}
		return h.toolList(), nil
	case "tools/call":
		return h.callTool(ctx, request.params)
	default:
		return nil, errMethodNotFound
	}
}

var errMethodNotFound = errors.New("broker: method not found")

func validateInitializeParams(raw []byte) error {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      json.RawMessage `json:"clientInfo"`
	}
	if decodeStrictObject(raw, &params) != nil || params.ProtocolVersion == "" || len(params.Capabilities) == 0 || len(params.ClientInfo) == 0 || strictJSONObject(params.Capabilities) != nil || strictJSONObject(params.ClientInfo) != nil {
		return ErrInvalidRequest
	}
	return nil
}

func (h *MCPHandler) toolList() json.RawMessage {
	tools := h.broker.currentTools()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tool := tools[name]
		entries = append(entries, map[string]any{
			"name": name,
			// Do not expose exact argument values. They are policy secrets
			// and the generic object schema remains valid for Codex.
			"inputSchema": map[string]any{"type": "object", "additionalProperties": true},
			"annotations": map[string]bool{
				"readOnlyHint":    tool.effect == v1alpha1.EffectRead,
				"destructiveHint": tool.effect == v1alpha1.EffectPublish,
			},
		})
	}
	if h.broker.policyContract.Digest() != "" {
		entries = append(entries, map[string]any{
			"name":        "policy_check",
			"description": "Run the immutable Policy contract self-checks for this sandbox.",
			"inputSchema": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"ruleIds": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}},
			},
			"annotations": map[string]bool{"readOnlyHint": true, "destructiveHint": false},
		})
	}
	body, _ := json.Marshal(map[string]any{"tools": entries})
	return body
}

func (h *MCPHandler) callTool(ctx context.Context, raw []byte) (json.RawMessage, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if decodeStrictObject(raw, &params) != nil || !safeName(params.Name, MaxToolNameBytes) {
		return nil, ErrInvalidRequest
	}
	tool, lookup := h.broker.lookupToolByName(params.Name)
	if lookup == toolLookupPhaseDenied {
		return nil, ErrPhaseDenied
	}
	arguments := params.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if strictJSONObject(arguments) != nil {
		return nil, ErrInvalidRequest
	}
	if params.Name == "policy_check" {
		var request struct {
			RuleIDs []string `json:"ruleIds,omitempty"`
		}
		if decodeStrictObject(arguments, &request) != nil || len(request.RuleIDs) > 64 {
			return nil, ErrInvalidRequest
		}
		for _, id := range request.RuleIDs {
			if !safeName(id, MaxToolNameBytes) {
				return nil, ErrInvalidRequest
			}
		}
		observations, err := h.broker.PolicyCheck(ctx, request.RuleIDs)
		if err != nil {
			return nil, err
		}
		return policyCheckToolResult(observations)
	}
	if lookup != toolLookupAllowed {
		return nil, ErrDenied
	}
	result, err := h.broker.CallTool(ctx, ToolCallRequest{Server: tool.server, Tool: params.Name, Arguments: arguments})
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), result.Content...), nil
}

func (h *MCPHandler) writeRPCResult(w http.ResponseWriter, id json.RawMessage, result json.RawMessage) {
	if len(result) == 0 || strictJSONValue(result) != nil {
		h.writeRPCError(w, id, -32603, "internal error")
		return
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": json.RawMessage(result)})
	if err != nil || len(body) > h.broker.limits.maxMCPResponse {
		h.writeRPCError(w, id, -32603, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *MCPHandler) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = []byte("null")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", mcpProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func validRPCID(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 256 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || strictJSONValue(raw) != nil {
		return false
	}
	var stringID string
	if json.Unmarshal(raw, &stringID) == nil {
		return safeName(stringID, 128)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(&number) == nil && number.String() != ""
}

func strictJSONObject(raw []byte) error {
	return strictjson.ValidateObject(raw)
}

func strictJSONValue(raw []byte) error {
	return strictjson.Validate(raw)
}

func rpcErrorCode(err error) int {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		return -32602
	case errors.Is(err, ErrPhaseDenied):
		return MCPErrorCodePhaseDenied
	case errors.Is(err, ErrDenied):
		return -32001
	case errors.Is(err, ErrApprovalRequired):
		return -32002
	case errors.Is(err, ErrEffectAlreadyClaimed):
		return -32003
	case errors.Is(err, ErrUnknownEffect):
		return -32004
	case errors.Is(err, ErrBudgetExceeded):
		return -32005
	default:
		return -32000
	}
}

func rpcErrorMessage(err error) string {
	switch {
	case errors.Is(err, errMethodNotFound):
		return "method not found"
	case errors.Is(err, ErrPhaseDenied):
		return "tool call denied"
	case errors.Is(err, ErrDenied):
		return "tool call denied by policy"
	case errors.Is(err, ErrApprovalRequired):
		return "tool call requires approval"
	case errors.Is(err, ErrEffectAlreadyClaimed):
		return "tool call effect was already claimed"
	case errors.Is(err, ErrUnknownEffect):
		return "tool call outcome is unknown"
	case errors.Is(err, ErrBudgetExceeded):
		return "configured budget exceeded"
	default:
		return "request failed"
	}
}
