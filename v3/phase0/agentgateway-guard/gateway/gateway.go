// Package gateway is a provider-free data-plane double for the Phase 0
// agentgateway composition. It models only the properties that AGW owns the
// right to verify at this seam: a fixed MCP backend, trusted credential
// injection, bounded MCP forwarding, and no generic proxy surface.
//
// This is a test fixture, not a replacement for agentgateway and not a
// production credential broker. The real workload remains disabled until the
// exact agentgateway image/configuration passes the separate capability gate.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	MCPPath       = "/mcp"
	HealthPath    = "/healthz"
	BackendURL    = "http://127.0.0.1:9090/mcp"
	BackendCanary = "phase0-only-canary"
	canaryHeader  = "x-phase0-canary"
	mcpVersion    = "2025-06-18"
	maxSessionID  = 256
)

var (
	ErrInvalidConfig  = errors.New("gateway: invalid configuration")
	ErrInvalidRequest = errors.New("gateway: invalid MCP request")
	ErrUpstream       = errors.New("gateway: upstream request failed")
	ErrCredentialLeak = errors.New("gateway: upstream response contained the credential")
)

// Config is trusted process configuration. Credential is intentionally not an
// HTTP input and BackendURL is restricted to the fixed loopback recorder
// target used by this fixture.
type Config struct {
	BackendURL string
	Credential string
	HTTPClient *http.Client
}

// Evidence is a safe projection of forwarding behavior. It contains counts
// and a digest only; the credential and request bodies are never returned.
type Evidence struct {
	ForwardedRequests  int `json:"forwardedRequests"`
	ToolCalls          int `json:"toolCalls"`
	CredentialInjected int `json:"credentialInjected"`
}

type Gateway struct {
	backendURL string
	credential []byte
	client     *http.Client

	mu                 sync.RWMutex
	closed             bool
	forwardedRequests  int
	toolCalls          int
	credentialInjected int
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func New(config Config) (*Gateway, error) {
	backend := config.BackendURL
	if backend == "" {
		backend = BackendURL
	}
	if !validBackendURL(backend) || !validCredential(config.Credential) {
		return nil, ErrInvalidConfig
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	} else {
		copy := *client
		if copy.Timeout <= 0 {
			copy.Timeout = 15 * time.Second
		}
		client = &copy
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Gateway{
		backendURL: backend,
		credential: []byte(config.Credential),
		client:     client,
	}, nil
}

// Close wipes the in-memory fixture credential. It is useful to make tests
// exercise the same lifecycle expectation as a per-run sidecar.
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	for i := range g.credential {
		g.credential[i] = 0
	}
	g.credential = nil
	g.closed = true
}

func (g *Gateway) Evidence() Evidence {
	if g == nil {
		return Evidence{}
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return Evidence{
		ForwardedRequests:  g.forwardedRequests,
		ToolCalls:          g.toolCalls,
		CredentialInjected: g.credentialInjected,
	}
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRequest(r) || r.Method != http.MethodGet || r.URL.Path != HealthPath || r.URL.RawQuery != "" {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc(MCPPath, g.handleMCP)
	return mux
}

func (g *Gateway) handleMCP(w http.ResponseWriter, r *http.Request) {
	if g == nil || r == nil || !isLoopbackRequest(r) || r.URL.Path != MCPPath {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || !contentTypeIs(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, strictjson.MaxDocumentBytes+1))
	if err != nil || len(body) > strictjson.MaxDocumentBytes {
		writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	request, err := decodeRequest(body)
	if err != nil {
		writeRPCError(w, nil, -32600, "invalid request")
		return
	}
	if session := r.Header.Get("Mcp-Session-Id"); session != "" && !validSessionID(session) {
		writeRPCError(w, request.ID, -32600, "invalid session")
		return
	}
	if version := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")); version != "" && version != mcpVersion {
		writeRPCError(w, request.ID, -32602, "unsupported protocol version")
		return
	}
	if err := validateMethod(request); err != nil {
		writeRPCError(w, request.ID, -32602, "unsupported MCP request")
		return
	}

	status, responseBody, responseHeaders, err := g.forward(r.Context(), body, r.Header.Get("Mcp-Session-Id"), request.Method == "tools/call")
	if err != nil {
		writeRPCError(w, request.ID, -32001, "upstream unavailable")
		return
	}
	if len(responseBody) == 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("MCP-Protocol-Version", mcpVersion)
	if session := responseHeaders.Get("Mcp-Session-Id"); session != "" {
		if !validSessionID(session) {
			writeRPCError(w, request.ID, -32001, "upstream unavailable")
			return
		}
		w.Header().Set("Mcp-Session-Id", session)
	}
	w.WriteHeader(status)
	_, _ = w.Write(responseBody)
}

func (g *Gateway) forward(ctx context.Context, body []byte, session string, toolCall bool) (int, []byte, http.Header, error) {
	g.mu.RLock()
	if g.closed || len(g.credential) == 0 || g.client == nil {
		g.mu.RUnlock()
		return 0, nil, nil, ErrInvalidConfig
	}
	credential := append([]byte(nil), g.credential...)
	client := g.client
	endpoint := g.backendURL
	g.mu.RUnlock()
	defer zeroBytes(credential)

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, ErrUpstream
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("MCP-Protocol-Version", mcpVersion)
	request.Header.Set(canaryHeader, string(credential))
	if session != "" {
		request.Header.Set("Mcp-Session-Id", session)
	}
	g.mu.Lock()
	g.forwardedRequests++
	g.credentialInjected++
	if toolCall {
		g.toolCalls++
	}
	g.mu.Unlock()

	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, ErrUpstream
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, strictjson.MaxDocumentBytes+1))
	if err != nil || len(responseBody) > strictjson.MaxDocumentBytes {
		return 0, nil, nil, ErrUpstream
	}
	if bytes.Contains(responseBody, credential) {
		return 0, nil, nil, ErrCredentialLeak
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, nil, nil, ErrUpstream
	}
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return response.StatusCode, nil, response.Header, nil
	}
	if !contentTypeIs(response.Header.Get("Content-Type"), "application/json") || strictjson.ValidateObject(responseBody) != nil {
		return 0, nil, nil, ErrUpstream
	}
	return response.StatusCode, responseBody, response.Header, nil
}

func decodeRequest(body []byte) (rpcRequest, error) {
	var request rpcRequest
	if strictjson.ValidateObject(body) != nil {
		return request, ErrInvalidRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return rpcRequest{}, ErrInvalidRequest
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return rpcRequest{}, ErrInvalidRequest
	}
	if request.JSONRPC != "2.0" || request.Method == "" || len(request.Method) > 128 || !safeName(request.Method) {
		return rpcRequest{}, ErrInvalidRequest
	}
	if len(request.ID) > 0 && !validRPCID(request.ID) {
		return rpcRequest{}, ErrInvalidRequest
	}
	if len(request.Params) == 0 {
		request.Params = json.RawMessage(`{}`)
	}
	if strictjson.ValidateObject(request.Params) != nil {
		return rpcRequest{}, ErrInvalidRequest
	}
	return request, nil
}

func validateMethod(request rpcRequest) error {
	needsID := request.Method != "notifications/initialized"
	if needsID && len(request.ID) == 0 {
		return ErrInvalidRequest
	}
	if !needsID && len(request.ID) != 0 {
		return ErrInvalidRequest
	}
	switch request.Method {
	case "initialize", "notifications/initialized", "ping", "tools/list":
		return nil
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := decodeStrictObject(request.Params, &params); err != nil || params.Name != "record" || strictjson.ValidateObject(params.Arguments) != nil {
			return ErrInvalidRequest
		}
		return nil
	default:
		return ErrInvalidRequest
	}
}

func validBackendURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" || parsed.Path != MCPPath || parsed.Port() != "9090" || parsed.String() != value {
		return false
	}
	return parsed.Host == "127.0.0.1:9090" || parsed.Host == "[::1]:9090"
}

func validCredential(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validSessionID(value string) bool {
	if value == "" || len(value) > maxSessionID || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validRPCID(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 256 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || strictjson.Validate(raw) != nil {
		return false
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return safeName(value)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(&number) == nil && number.String() != ""
}

func safeName(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-/:", r) {
			continue
		}
		return false
	}
	return true
}

func decodeStrictObject(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func isLoopbackRequest(r *http.Request) bool {
	if r == nil || r.RemoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func contentTypeIs(value, expected string) bool {
	return strings.EqualFold(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]), expected)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = []byte("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(id),
		"error": map[string]any{"code": code, "message": message},
	})
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
