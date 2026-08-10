package toolbroker

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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v2/pkg/toolpolicy"
)

// The adapter deliberately implements the synchronous subset of MCP
// Streamable HTTP. It does not keep sessions or emit SSE: every request gets
// one JSON response, and notifications get an empty 202 response.
const (
	MCPProtocolVersion = "2025-06-18"

	mcpMaxRequestBody = 1 << 20
	mcpMaxResponse    = 1 << 20
	mcpMaxTools       = 256
	mcpMaxToolName    = 128
	mcpMaxDescription = 4096
	mcpMaxField       = 512
	mcpMaxCallID      = 128
	mcpMaxSchema      = 64 << 10
)

var (
	ErrMCPInvalidConfig = errors.New("invalid MCP adapter configuration")
)

// ExposedTool is the immutable, gateway-owned description of a tool visible
// through one handler. Server, resource, and effect are deliberately not
// accepted from a tools/call request.
type ExposedTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Server      string
	Resource    string
	Effect      toolpolicy.Effect
}

// MCPHandlerConfig binds one per-run MCP endpoint to a tenant, principal, and
// exact tool catalog. The constructor copies and validates all values before
// serving requests; callers cannot mutate the handler's policy by retaining a
// slice or RawMessage from this structure.
type MCPHandlerConfig struct {
	OrganizationID string
	ProjectID      string
	UserID         string
	RunID          string
	ServerName     string
	ServerVersion  string
	Tools          []ExposedTool
}

// MCPTool and MCPConfig are concise aliases for integrations that use the
// protocol name rather than the internal broker terminology.
type MCPTool = ExposedTool
type MCPConfig = MCPHandlerConfig

// MCPHandler serves the supported MCP JSON-RPC methods for one run.
type MCPHandler struct {
	broker *Broker
	config mcpConfig
}

type mcpConfig struct {
	organizationID string
	projectID      string
	userID         string
	runID          string
	serverName     string
	serverVersion  string
	tools          []mcpTool
	byName         map[string]mcpTool
}

type mcpTool struct {
	name        string
	description string
	schema      json.RawMessage
	server      string
	resource    string
	effect      toolpolicy.Effect
}

// NewMCPHandler constructs an immutable per-run MCP handler.
func NewMCPHandler(broker *Broker, config MCPHandlerConfig) (*MCPHandler, error) {
	if broker == nil {
		return nil, fmt.Errorf("%w: broker is required", ErrMCPInvalidConfig)
	}
	if config.ServerName == "" {
		config.ServerName = "agents-gateway"
	}
	if config.ServerVersion == "" {
		config.ServerVersion = "v2"
	}
	if err := validateMCPBinding(config); err != nil {
		return nil, err
	}

	tools := make([]mcpTool, len(config.Tools))
	byName := make(map[string]mcpTool, len(config.Tools))
	for i, tool := range config.Tools {
		copiedSchema := append(json.RawMessage(nil), tool.InputSchema...)
		m := mcpTool{
			name:        tool.Name,
			description: tool.Description,
			schema:      copiedSchema,
			server:      tool.Server,
			resource:    tool.Resource,
			effect:      tool.Effect,
		}
		tools[i] = m
		byName[m.name] = m
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].name < tools[j].name })
	return &MCPHandler{
		broker: broker,
		config: mcpConfig{
			organizationID: config.OrganizationID,
			projectID:      config.ProjectID,
			userID:         config.UserID,
			runID:          config.RunID,
			serverName:     config.ServerName,
			serverVersion:  config.ServerVersion,
			tools:          tools,
			byName:         byName,
		},
	}, nil
}

// NewMCPServer is an alias kept for callers that name an MCP HTTP endpoint a
// server. It has the same immutable per-run semantics as NewMCPHandler.
func NewMCPServer(broker *Broker, config MCPHandlerConfig) (*MCPHandler, error) {
	return NewMCPHandler(broker, config)
}

func validateMCPBinding(config MCPHandlerConfig) error {
	for field, value := range map[string]string{
		"organization":   config.OrganizationID,
		"project":        config.ProjectID,
		"user":           config.UserID,
		"run":            config.RunID,
		"server name":    config.ServerName,
		"server version": config.ServerVersion,
	} {
		if err := boundedText(value, mcpMaxField); err != nil {
			return fmt.Errorf("%w: %s %v", ErrMCPInvalidConfig, field, err)
		}
	}
	if len(config.Tools) == 0 || len(config.Tools) > mcpMaxTools {
		return fmt.Errorf("%w: tool catalog must contain 1-%d tools", ErrMCPInvalidConfig, mcpMaxTools)
	}
	seen := make(map[string]struct{}, len(config.Tools))
	for _, tool := range config.Tools {
		if err := boundedText(tool.Name, mcpMaxToolName); err != nil || !validToolName(tool.Name) {
			return fmt.Errorf("%w: invalid tool name", ErrMCPInvalidConfig)
		}
		if _, ok := seen[tool.Name]; ok {
			return fmt.Errorf("%w: duplicate tool name", ErrMCPInvalidConfig)
		}
		seen[tool.Name] = struct{}{}
		if len(tool.Description) > mcpMaxDescription || !utf8.ValidString(tool.Description) {
			return fmt.Errorf("%w: tool %q description is too long or invalid", ErrMCPInvalidConfig, tool.Name)
		}
		if len(tool.InputSchema) == 0 || len(tool.InputSchema) > mcpMaxSchema || strictjson.Validate(tool.InputSchema) != nil {
			return fmt.Errorf("%w: tool %q has an invalid input schema", ErrMCPInvalidConfig, tool.Name)
		}
		if err := validateJSONValue(tool.InputSchema, 0); err != nil {
			return fmt.Errorf("%w: tool %q schema: %v", ErrMCPInvalidConfig, tool.Name, err)
		}
		var schema map[string]json.RawMessage
		if err := decodeStrictObject(tool.InputSchema, &schema); err != nil {
			return fmt.Errorf("%w: tool %q schema must be an object", ErrMCPInvalidConfig, tool.Name)
		}
		if err := boundedText(tool.Server, mcpMaxField); err != nil {
			return fmt.Errorf("%w: tool %q binding is too long", ErrMCPInvalidConfig, tool.Name)
		}
		if tool.Resource != "" && boundedText(tool.Resource, mcpMaxField) != nil {
			return fmt.Errorf("%w: tool %q binding is too long", ErrMCPInvalidConfig, tool.Name)
		}
		if tool.Server == "" {
			return fmt.Errorf("%w: tool %q server is required", ErrMCPInvalidConfig, tool.Name)
		}
		switch tool.Effect {
		case toolpolicy.EffectRead, toolpolicy.EffectWrite, toolpolicy.EffectDestructive:
		default:
			return fmt.Errorf("%w: tool %q has invalid effect", ErrMCPInvalidConfig, tool.Name)
		}
	}
	return nil
}

func validToolName(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func boundedText(value string, max int) error {
	if value == "" || !utf8.ValidString(value) || len(value) > max {
		return errors.New("must be non-empty valid UTF-8 within bounds")
	}
	return nil
}

// ServeHTTP accepts only the synchronous Streamable HTTP JSON transport.
func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/mcp" || r.URL.RawQuery != "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !acceptedContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}
	if !acceptedMCPResponse(r.Header.Get("Accept")) {
		http.Error(w, "unsupported accept type", http.StatusNotAcceptable)
		return
	}
	if r.Body == nil {
		h.writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxRequestBody+1))
	if err != nil || len(body) > mcpMaxRequestBody {
		h.writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	request, err := parseRPCRequest(body)
	if err != nil {
		h.writeRPCError(w, nil, rpcParseCode(err), rpcParseMessage(err))
		return
	}
	if request.id == nil {
		if request.method != "notifications/initialized" {
			h.writeRPCError(w, nil, -32600, "invalid request")
			return
		}
		if _, rpcErr := h.dispatch(r.Context(), request); rpcErr != nil {
			h.writeRPCError(w, nil, rpcErr.code, rpcErr.message)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := h.dispatch(r.Context(), request)
	if rpcErr != nil {
		h.writeRPCError(w, request.id, rpcErr.code, rpcErr.message)
		return
	}
	h.writeRPCResult(w, request.id, result)
}

type rpcRequest struct {
	id     json.RawMessage
	method string
	params json.RawMessage
}

type mcpProtocolError struct {
	code    int
	message string
}

func parseRPCRequest(body []byte) (rpcRequest, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] == '[' {
		return rpcRequest{}, errRPCParse
	}
	if err := validateJSONValue(body, 0); err != nil {
		return rpcRequest{}, errRPCInvalid
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(body, &fields); err != nil {
		return rpcRequest{}, errRPCInvalid
	}
	for key := range fields {
		switch key {
		case "jsonrpc", "id", "method", "params":
		default:
			return rpcRequest{}, errRPCInvalid
		}
	}
	var version string
	if err := decodeExact(fields, "jsonrpc", &version); err != nil || version != "2.0" {
		return rpcRequest{}, errRPCInvalid
	}
	var method string
	if err := decodeExact(fields, "method", &method); err != nil || !validMethodSyntax(method) {
		return rpcRequest{}, errRPCInvalid
	}
	request := rpcRequest{method: method}
	if raw, ok := fields["id"]; ok {
		if !validRPCID(raw) {
			return rpcRequest{}, errRPCInvalid
		}
		request.id = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := fields["params"]; ok {
		if string(raw) == "null" {
			return rpcRequest{}, errRPCInvalid
		}
		var params map[string]json.RawMessage
		if err := decodeStrictObject(raw, &params); err != nil {
			return rpcRequest{}, errRPCInvalid
		}
		request.params = append(json.RawMessage(nil), raw...)
	}
	return request, nil
}

func validMethodSyntax(method string) bool {
	return method != "" && len(method) <= mcpMaxToolName && utf8.ValidString(method)
}

func validRPCID(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 256 || string(raw) == "null" {
		return false
	}
	var stringID string
	if json.Unmarshal(raw, &stringID) == nil {
		return boundedText(stringID, 128) == nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(&number) == nil && number.String() != ""
}

func (h *MCPHandler) dispatch(ctx context.Context, request rpcRequest) (any, *mcpProtocolError) {
	if err := ctx.Err(); err != nil {
		return nil, &mcpProtocolError{-32000, "request canceled"}
	}
	switch request.method {
	case "initialize":
		var params struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ClientInfo      json.RawMessage `json:"clientInfo"`
			Meta            json.RawMessage `json:"_meta"`
		}
		if err := decodeParams(request.params, &params, "protocolVersion", "capabilities", "clientInfo", "_meta"); err != nil {
			return nil, invalidParams()
		}
		if err := boundedText(params.ProtocolVersion, 64); err != nil || len(params.Capabilities) == 0 || len(params.ClientInfo) == 0 {
			return nil, invalidParams()
		}
		if err := validateJSONValue(params.Capabilities, 0); err != nil {
			return nil, invalidParams()
		}
		if _, err := decodeMCPMeta(params.Meta); err != nil {
			return nil, invalidParams()
		}
		var clientInfo map[string]json.RawMessage
		if err := decodeStrictObject(params.ClientInfo, &clientInfo); err != nil {
			return nil, invalidParams()
		}
		var clientName, clientVersion string
		if err := decodeExact(clientInfo, "name", &clientName); err != nil {
			return nil, invalidParams()
		}
		if err := boundedText(clientName, mcpMaxField); err != nil {
			return nil, invalidParams()
		}
		if err := decodeExact(clientInfo, "version", &clientVersion); err != nil {
			return nil, invalidParams()
		}
		if err := boundedText(clientVersion, mcpMaxField); err != nil {
			return nil, invalidParams()
		}
		return map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": h.config.serverName, "version": h.config.serverVersion},
		}, nil
	case "notifications/initialized":
		if err := requireMetadataOnlyParams(request.params); err != nil {
			return nil, invalidParams()
		}
		return map[string]any{}, nil
	case "ping":
		if err := requireMetadataOnlyParams(request.params); err != nil {
			return nil, invalidParams()
		}
		return map[string]any{}, nil
	case "tools/list":
		if err := validateListParams(request.params); err != nil {
			return nil, invalidParams()
		}
		tools := make([]map[string]any, 0, len(h.config.tools))
		for _, tool := range h.config.tools {
			entry := map[string]any{
				"name":        tool.name,
				"inputSchema": json.RawMessage(tool.schema),
				"annotations": map[string]bool{
					"readOnlyHint":    tool.effect == toolpolicy.EffectRead,
					"destructiveHint": tool.effect == toolpolicy.EffectDestructive,
				},
			}
			if tool.description != "" {
				entry["description"] = tool.description
			}
			tools = append(tools, entry)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return h.callTool(ctx, request.params, request.id)
	default:
		return nil, &mcpProtocolError{-32601, "method not found"}
	}
}

func (h *MCPHandler) callTool(ctx context.Context, raw, requestID json.RawMessage) (any, *mcpProtocolError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	if err := decodeParams(raw, &params, "name", "arguments", "_meta"); err != nil {
		return nil, invalidParams()
	}
	if err := boundedText(params.Name, mcpMaxToolName); err != nil {
		return nil, invalidParams()
	}
	tool, ok := h.config.byName[params.Name]
	if !ok {
		return nil, &mcpProtocolError{-32602, "tool is not exposed for this run"}
	}
	arguments := params.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if len(arguments) > mcpMaxRequestBody || validateJSONValue(arguments, 0) != nil {
		return nil, invalidParams()
	}
	var argumentObject map[string]json.RawMessage
	if err := decodeStrictObject(arguments, &argumentObject); err != nil {
		return nil, invalidParams()
	}
	callID, err := parseCallID(params.Meta)
	if err != nil {
		return nil, invalidParams()
	}
	if callID == "" && len(requestID) > 0 {
		// Standard MCP clients do not know the optional Agents Gateway metadata
		// extension. The JSON-RPC request identity is stable across transport
		// retries and is therefore a safe default idempotency identity. A model
		// issuing a genuinely new tool call receives a new JSON-RPC identity.
		sum := sha256.Sum256(requestID)
		callID = "jsonrpc:" + hex.EncodeToString(sum[:])
	}
	effectKey := ""
	if tool.effect != toolpolicy.EffectRead {
		if callID == "" {
			return nil, &mcpProtocolError{-32602, "write tool calls require a bounded call identity"}
		}
		effectKey = stableEffectKey(h.config.runID, tool.name, callID)
	}
	result, callErr := h.broker.Call(ctx, Request{
		OrganizationID: h.config.organizationID,
		ProjectID:      h.config.projectID,
		UserID:         h.config.userID,
		RunID:          h.config.runID,
		Server:         tool.server,
		Tool:           tool.name,
		Resource:       tool.resource,
		EffectKey:      effectKey,
		Arguments:      arguments,
	})
	if callErr != nil {
		return nil, mapBrokerError(callErr)
	}
	return normalizeMCPResult(result.Content), nil
}

func parseCallID(raw json.RawMessage) (string, error) {
	fields, err := decodeMCPMeta(raw)
	if err != nil {
		return "", err
	}
	if rawAGW, ok := fields["agw"]; ok {
		var agw map[string]json.RawMessage
		if err := decodeStrictObject(rawAGW, &agw); err != nil {
			return "", err
		}
		for key := range agw {
			if key != "call_id" {
				return "", errors.New("unknown gateway metadata field")
			}
		}
		var callID string
		if err := decodeExact(agw, "call_id", &callID); err != nil || boundedText(callID, mcpMaxCallID) != nil {
			return "", errors.New("invalid call identity")
		}
		return callID, nil
	}
	return "", nil
}

func stableEffectKey(runID, toolName, callID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{runID, toolName, callID}, "\x00")))
	return "mcp-call:" + hex.EncodeToString(sum[:])
}

func normalizeMCPResult(content json.RawMessage) any {
	if len(content) == 0 || strictjson.Validate(content) != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "tool returned no content"}}}
	}
	var value any
	if json.Unmarshal(content, &value) != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "tool returned invalid content"}}}
	}
	if items, ok := value.([]any); ok {
		return map[string]any{"content": items}
	}
	if object, ok := value.(map[string]any); ok {
		if items, ok := object["content"].([]any); ok {
			result := map[string]any{"content": items}
			if isError, ok := object["isError"].(bool); ok {
				result["isError"] = isError
			}
			if structured, ok := object["structuredContent"]; ok {
				result["structuredContent"] = structured
			}
			return result
		}
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(content)}}}
}

func mapBrokerError(err error) *mcpProtocolError {
	switch {
	case errors.Is(err, ErrDenied):
		return &mcpProtocolError{-32001, "tool call denied by policy"}
	case errors.Is(err, ErrApprovalRequired):
		return &mcpProtocolError{-32002, "tool call requires approval"}
	case errors.Is(err, ErrEffectAlreadyClaimed):
		return &mcpProtocolError{-32003, "tool call effect was already claimed"}
	case errors.Is(err, ErrOutcomeUnknown):
		return &mcpProtocolError{-32004, "tool call outcome is unknown"}
	default:
		return &mcpProtocolError{-32000, "tool call failed"}
	}
}

func (h *MCPHandler) writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	if err != nil || len(payload) > mcpMaxResponse {
		h.writeRPCError(w, id, -32603, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", MCPProtocolVersion)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (h *MCPHandler) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = []byte("null")
	}
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func acceptedContentType(value string) bool {
	if value == "" {
		return false
	}
	for _, item := range strings.Split(value, ";") {
		if strings.EqualFold(strings.TrimSpace(item), "application/json") {
			return true
		}
	}
	return false
}

func acceptedMCPResponse(value string) bool {
	if value == "" {
		return false
	}
	for _, item := range strings.Split(value, ",") {
		media := strings.TrimSpace(strings.SplitN(item, ";", 2)[0])
		if strings.EqualFold(media, "application/json") || strings.EqualFold(media, "text/event-stream") {
			return true
		}
	}
	return false
}

var (
	errRPCParse   = errors.New("parse error")
	errRPCInvalid = errors.New("invalid request")
)

func rpcParseCode(err error) int {
	if errors.Is(err, errRPCParse) {
		return -32700
	}
	return -32600
}
func rpcParseMessage(err error) string {
	if errors.Is(err, errRPCParse) {
		return "parse error"
	}
	return "invalid request"
}
func invalidParams() *mcpProtocolError { return &mcpProtocolError{-32602, "invalid method parameters"} }

// MCP request metadata is explicitly extensible. It is never authority: the
// broker derives policy, tenant, resource, and effect from its immutable run
// binding. Accept bounded extension metadata for client interoperability and
// read only the namespaced agw.call_id extension where a stable write identity
// is useful.
func decodeMCPMeta(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > mcpMaxSchema || validateJSONValue(raw, 0) != nil {
		return nil, errors.New("invalid metadata")
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(raw, &fields); err != nil {
		return nil, errors.New("metadata must be an object")
	}
	return fields, nil
}

func requireMetadataOnlyParams(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		if key != "_meta" {
			return errors.New("unknown parameter")
		}
	}
	_, err := decodeMCPMeta(fields["_meta"])
	return err
}

func validateListParams(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(raw, &fields); err != nil {
		return err
	}
	for key := range fields {
		if key != "cursor" && key != "_meta" {
			return errors.New("unknown parameter")
		}
	}
	// This endpoint always returns its complete bounded catalog and never
	// advertises nextCursor, so a non-null cursor cannot have originated here.
	if cursor, ok := fields["cursor"]; ok && string(cursor) != "null" {
		return errors.New("pagination is not available")
	}
	_, err := decodeMCPMeta(fields["_meta"])
	return err
}

func decodeParams(raw json.RawMessage, target any, allowed ...string) error {
	if len(raw) == 0 {
		return errors.New("parameters are required")
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(raw, &fields); err != nil {
		return err
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range fields {
		if _, ok := allowedSet[key]; !ok {
			return errors.New("unknown parameter")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func decodeExact(fields map[string]json.RawMessage, name string, target any) error {
	raw, ok := fields[name]
	if !ok {
		return errors.New("required field missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func decodeStrictObject(raw []byte, target any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("object required")
	}
	if err := strictjson.ValidateObject(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validateJSONValue(raw []byte, depth int) error {
	_ = depth
	return strictjson.Validate(raw)
}
