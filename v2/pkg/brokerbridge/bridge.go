// Package brokerbridge exposes a tiny loopback HTTP surface inside one
// sandbox and forwards only approved routes to that run's private Unix broker.
// It never opens a host TCP port and never provides direct internet access.
package brokerbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runbroker"
)

const (
	DefaultClientConfigPath = "/run/agw/client.json"
	DefaultSocketPath       = "/run/agw/broker.sock"
	ResponsesPath           = "/v1/responses"
	MCPPath                 = "/mcp"
	ArtifactPath            = "/v1/artifacts/output"
	ArtifactCreatePath      = "/v1/artifacts/create"
	maxClientConfigBytes    = 64 << 10
)

type clientConfig struct {
	SessionID       string `json:"session_id"`
	BearerToken     string `json:"bearer_token"`
	PolicyDigest    string `json:"policy_digest"`
	AllowedModel    string `json:"allowed_model"`
	ModelURL        string `json:"model_url"`
	ToolsURL        string `json:"tools_url"`
	ArtifactURL     string `json:"artifact_url,omitempty"`
	ToolsEnabled    bool   `json:"tools_enabled"`
	ArtifactEnabled bool   `json:"artifact_enabled"`
}

// Config names only sandbox-local paths and limits. The listener always binds
// an ephemeral IPv4 loopback port, regardless of configuration.
type Config struct {
	ClientConfigPath  string
	SocketPath        string
	MaxHeaderBytes    int
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
}

// Endpoints are the only URLs an in-sandbox harness may use.
type Endpoints struct {
	ResponsesBaseURL  string
	MCPURL            string
	ArtifactURL       string
	ArtifactCreateURL string
	AllowedModel      string
	ToolsEnabled      bool
	ArtifactEnabled   bool
}

// Bridge owns one sandbox-local TCP listener and one Unix-only HTTP client.
type Bridge struct {
	listener net.Listener
	server   *http.Server
	client   *http.Client
	config   clientConfig
	baseURL  string

	closeOnce sync.Once
	closeErr  error
}

// Start validates the mounted run capability, binds loopback, and begins
// serving. The returned endpoint base is intentionally derived from the
// kernel-selected listener rather than accepted from client.json.
func Start(cfg Config) (*Bridge, Endpoints, error) {
	if cfg.ClientConfigPath == "" {
		cfg.ClientConfigPath = DefaultClientConfigPath
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath
	}
	clientCfg, err := readClientConfig(cfg.ClientConfigPath)
	if err != nil {
		return nil, Endpoints{}, err
	}
	if err := validateUnixSocket(cfg.SocketPath); err != nil {
		return nil, Endpoints{}, err
	}
	if cfg.MaxHeaderBytes == 0 {
		cfg.MaxHeaderBytes = 32 << 10
	}
	if cfg.MaxHeaderBytes < 1024 || cfg.MaxHeaderBytes > 1<<20 {
		return nil, Endpoints{}, errors.New("broker bridge header limit is outside the supported range")
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 5 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.IdleTimeout <= 0 {
		return nil, Endpoints{}, errors.New("broker bridge timeouts must be positive")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", cfg.SocketPath)
	}
	transport.DisableKeepAlives = false
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		transport.CloseIdleConnections()
		return nil, Endpoints{}, fmt.Errorf("bind broker bridge loopback: %w", err)
	}
	bridge := &Bridge{listener: listener, client: client, config: clientCfg}
	bridge.baseURL = "http://" + listener.Addr().String()
	bridge.server = &http.Server{
		Handler:           bridge,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       cfg.IdleTimeout,
	}
	go func() { _ = bridge.server.Serve(listener) }()
	return bridge, Endpoints{
		ResponsesBaseURL:  bridge.baseURL + "/v1",
		MCPURL:            bridge.baseURL + MCPPath,
		ArtifactURL:       bridge.baseURL + ArtifactPath,
		ArtifactCreateURL: bridge.baseURL + ArtifactCreatePath,
		AllowedModel:      clientCfg.AllowedModel,
		ToolsEnabled:      clientCfg.ToolsEnabled,
		ArtifactEnabled:   clientCfg.ArtifactEnabled,
	}, nil
}

// Close releases the loopback listener and all idle Unix connections.
func (b *Bridge) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.closeOnce.Do(func() {
		b.closeErr = b.server.Shutdown(ctx)
		_ = b.listener.Close()
		if transport, ok := b.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	})
	return b.closeErr
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.RawQuery != "" || r.URL.Fragment != "" || !allowedRoute(r.Method, r.URL.Path) {
		writeError(w, http.StatusNotFound)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, "http://agw-run-broker"+r.URL.Path, r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest)
		return
	}
	copyRequestHeaders(request.Header, r.Header, r.URL.Path)
	request.ContentLength = r.ContentLength
	request.Header.Set(runbroker.HeaderAuthorization, "Bearer "+b.config.BearerToken)
	request.Header.Set(runbroker.HeaderSessionID, b.config.SessionID)
	request.Header.Set(runbroker.HeaderModel, b.config.AllowedModel)
	request.Header.Set(runbroker.HeaderPolicyDigest, b.config.PolicyDigest)
	response, err := b.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func allowedRoute(method, path string) bool {
	switch path {
	case ResponsesPath:
		return method == http.MethodPost
	case MCPPath:
		return method == http.MethodPost
	case ArtifactPath:
		return method == http.MethodPut
	case ArtifactCreatePath:
		return method == http.MethodPost
	default:
		return false
	}
}

func readClientConfig(path string) (clientConfig, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return clientConfig{}, errors.New("broker client config path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return clientConfig{}, fmt.Errorf("inspect broker client config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxClientConfigBytes || info.Mode().Perm()&0177 != 0 {
		return clientConfig{}, errors.New("broker client config must be a private bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return clientConfig{}, fmt.Errorf("open broker client config: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxClientConfigBytes+1))
	decoder.DisallowUnknownFields()
	var cfg clientConfig
	if err := decoder.Decode(&cfg); err != nil {
		return clientConfig{}, errors.New("decode broker client config")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return clientConfig{}, errors.New("decode broker client config")
	}
	if err := validateClientConfig(cfg); err != nil {
		return clientConfig{}, err
	}
	return cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateClientConfig(cfg clientConfig) error {
	if !validOpaque(cfg.SessionID, "ags_") || !validOpaque(cfg.BearerToken, "agt_") {
		return errors.New("broker client capability is malformed")
	}
	if !strings.HasPrefix(cfg.PolicyDigest, "sha256:") || len(cfg.PolicyDigest) != len("sha256:")+64 {
		return errors.New("broker policy digest is malformed")
	}
	for _, character := range strings.TrimPrefix(cfg.PolicyDigest, "sha256:") {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return errors.New("broker policy digest is malformed")
		}
	}
	if cfg.AllowedModel == "" || len(cfg.AllowedModel) > 256 || strings.HasPrefix(cfg.AllowedModel, "-") {
		return errors.New("broker model is malformed")
	}
	for _, character := range cfg.AllowedModel {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return errors.New("broker model is malformed")
		}
	}
	// These fields are capability descriptions, not routing inputs. Accept only
	// the fixed documented loopback forms; Start still derives its own port.
	if cfg.ModelURL != "http://127.0.0.1:8787/v1/responses" || cfg.ToolsURL != "http://127.0.0.1:8787/mcp" {
		return errors.New("broker loopback capability description is malformed")
	}
	if cfg.ArtifactURL != "http://127.0.0.1:8787/v1/artifacts/output" {
		return errors.New("broker artifact capability description is malformed")
	}
	if !cfg.ArtifactEnabled {
		return errors.New("broker artifact upload capability is required")
	}
	return nil
}

func validOpaque(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) < len(prefix)+32 || len(value) > len(prefix)+96 {
		return false
	}
	for _, character := range strings.TrimPrefix(value, prefix) {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func validateUnixSocket(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("broker socket path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect broker socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return errors.New("broker socket must be a private Unix socket")
	}
	return nil
}

func copyRequestHeaders(destination, source http.Header, path string) {
	names := []string{"Content-Type", "Accept"}
	if path == ArtifactCreatePath {
		names = append(names, "X-AGW-Artifact-Metadata")
	}
	if path == MCPPath {
		names = append(names, "MCP-Protocol-Version", "MCP-Session-Id")
	}
	for _, name := range names {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func copyResponseHeaders(destination, source http.Header) {
	for _, name := range []string{"Content-Type", "Content-Length", "MCP-Protocol-Version", "MCP-Session-Id", "Retry-After", "OpenAI-Request-Id"} {
		for _, value := range source.Values(name) {
			destination.Add(name, value)
		}
	}
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":`+strconv.Quote(http.StatusText(status))+`}`)
}

// Keep bufio imported in a deliberate compile-time assertion: responses may
// be streamed and are never buffered by this bridge.
var _ io.Reader = (*bufio.Reader)(nil)
