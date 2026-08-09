package modelbroker

// This file contains the raw Responses API boundary used by a host-side
// broker. It intentionally does not share the Chat Completions request model:
// Codex and other Responses clients may use fields, tools, and streaming
// events that must pass through without lossy translation.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	responsesPath = "/v1/responses"

	defaultResponsesMaxHeaderBytes = 32 << 10
	defaultResponsesMaxBodyBytes   = 8 << 20
	defaultResponsesMaxResponse    = 32 << 20
	defaultResponsesTimeout        = 30 * time.Second
	maxResponsesErrorBodyBytes     = 16 << 10
)

var (
	errResponsesInvalidConfig = errors.New("invalid Responses proxy configuration")
	errResponsesBadRequest    = errors.New("invalid Responses request")
	errResponsesUpstream      = errors.New("Responses provider request failed")
)

// CredentialSource returns a fresh credential owned by the proxy. The proxy
// uses it only while constructing and sending the final upstream request and
// clears the returned byte slice after the request completes.
//
// The value returned here must be an upstream provider credential, never the
// caller's bearer token. A per-run broker should authenticate its caller
// before invoking the handler and resolve this credential at this boundary.
type CredentialSource func(context.Context) ([]byte, error)

// ResponsesProxyConfig configures one host-side raw Responses API proxy. A
// proxy is intentionally narrow: it accepts only POST /v1/responses and only
// models explicitly listed in AllowedModels.
type ResponsesProxyConfig struct {
	// UpstreamURL is the exact provider Responses endpoint, for example
	// https://api.openai.com/v1/responses. Userinfo, query, and fragment
	// components are rejected. Plain HTTP is accepted only for loopback test
	// servers.
	UpstreamURL string

	AllowedModels []string
	Credential    CredentialSource

	MaxHeaderBytes      int
	MaxRequestBodyBytes int64
	MaxResponseBytes    int64
	UpstreamTimeout     time.Duration
}

// ResponsesProxy is an http.Handler suitable for mounting at a per-run
// broker endpoint. It does not implement authentication itself; the enclosing
// per-run broker must authenticate and authorize the request before routing it
// here, or construct one proxy per already-authorized run session.
type ResponsesProxy struct {
	upstream       *url.URL
	allowedModels  map[string]struct{}
	credential     CredentialSource
	maxHeaderBytes int
	maxRequestBody int64
	maxResponse    int64
	client         *http.Client
}

// NewResponsesProxy constructs a raw, streaming Responses API proxy.
func NewResponsesProxy(config ResponsesProxyConfig) (*ResponsesProxy, error) {
	upstream, err := validateResponsesEndpoint(config.UpstreamURL)
	if err != nil {
		return nil, err
	}
	if config.Credential == nil || len(config.AllowedModels) == 0 {
		return nil, errResponsesInvalidConfig
	}

	allowedModels := make(map[string]struct{}, len(config.AllowedModels))
	for _, model := range config.AllowedModels {
		if model == "" || model != strings.TrimSpace(model) {
			return nil, errResponsesInvalidConfig
		}
		allowedModels[model] = struct{}{}
	}

	maxHeaderBytes := config.MaxHeaderBytes
	if maxHeaderBytes <= 0 {
		maxHeaderBytes = defaultResponsesMaxHeaderBytes
	}
	maxRequestBody := config.MaxRequestBodyBytes
	if maxRequestBody <= 0 {
		maxRequestBody = defaultResponsesMaxBodyBytes
	}
	maxResponse := config.MaxResponseBytes
	if maxResponse <= 0 {
		maxResponse = defaultResponsesMaxResponse
	}
	timeout := config.UpstreamTimeout
	if timeout <= 0 {
		timeout = defaultResponsesTimeout
	}
	if maxHeaderBytes < 1024 || maxRequestBody <= 0 || maxResponse <= 0 {
		return nil, errResponsesInvalidConfig
	}

	return &ResponsesProxy{
		upstream:       upstream,
		allowedModels:  allowedModels,
		credential:     config.Credential,
		maxHeaderBytes: maxHeaderBytes,
		maxRequestBody: maxRequestBody,
		maxResponse:    maxResponse,
		client:         newResponsesHTTPClient(timeout),
	}, nil
}

func newResponsesHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	// A Responses stream can legitimately run much longer than the time it
	// takes the provider to return headers. Bound connection/header setup here
	// and let the per-run context/TTL bound the streamed response lifetime.
	transport.ResponseHeaderTimeout = timeout
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		if transport.TLSClientConfig.MinVersion < tls.VersionTLS13 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS13
		}
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errResponsesUpstream
		},
	}
}

// ServeHTTP forwards a valid Responses request while streaming a successful
// upstream response. Upstream response bodies are never buffered in full.
func (p *ResponsesProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != responsesPath || r.URL.RawQuery != "" {
		writeResponsesError(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeResponsesError(w, http.StatusMethodNotAllowed)
		return
	}
	if headerSize(r.Header) > p.maxHeaderBytes {
		writeResponsesError(w, http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	if mediaType := r.Header.Get("Content-Type"); mediaType != "" {
		parsed, _, err := mime.ParseMediaType(mediaType)
		if err != nil || !strings.EqualFold(parsed, "application/json") {
			writeResponsesError(w, http.StatusUnsupportedMediaType)
			return
		}
	} else {
		writeResponsesError(w, http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > p.maxRequestBody {
		writeResponsesError(w, http.StatusRequestEntityTooLarge)
		return
	}

	body, err := readResponsesBody(r, p.maxRequestBody)
	if err != nil {
		writeResponsesError(w, http.StatusRequestEntityTooLarge)
		return
	}
	model, err := requestModel(body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest)
		return
	}
	if _, ok := p.allowedModels[model]; !ok {
		writeResponsesError(w, http.StatusForbidden)
		return
	}

	credential, err := p.credential(r.Context())
	if err != nil || len(credential) == 0 {
		zero(credential)
		writeResponsesError(w, http.StatusBadGateway)
		return
	}
	defer zero(credential)

	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream.String(), bytes.NewReader(body))
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway)
		return
	}
	copyAllowedRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Authorization", "Bearer "+string(credential))

	upstreamResponse, err := p.client.Do(upstreamRequest)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeResponsesError(w, http.StatusBadGateway)
		return
	}
	defer upstreamResponse.Body.Close()

	if upstreamResponse.StatusCode < http.StatusOK || upstreamResponse.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(upstreamResponse.Body, maxResponsesErrorBodyBytes))
		writeResponsesError(w, upstreamResponse.StatusCode)
		return
	}

	copyAllowedResponseHeaders(w.Header(), upstreamResponse.Header)
	w.WriteHeader(upstreamResponse.StatusCode)
	streamResponsesBody(r.Context(), w, upstreamResponse.Body, p.maxResponse)
}

func validateResponsesEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errResponsesInvalidConfig
	}
	if parsed.Path != responsesPath || parsed.RawPath != "" {
		return nil, errResponsesInvalidConfig
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errResponsesInvalidConfig
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func headerSize(header http.Header) int {
	size := 0
	for name, values := range header {
		for _, value := range values {
			size += len(name) + len(value) + 4
		}
	}
	return size
}

func readResponsesBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, errResponsesBadRequest
	}
	return body, nil
}

func requestModel(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errResponsesBadRequest
	}
	var model string
	modelSeen := false
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok {
			return "", errResponsesBadRequest
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", errResponsesBadRequest
		}
		if key != "model" {
			continue
		}
		if modelSeen {
			return "", errResponsesBadRequest
		}
		modelSeen = true
		if err := json.Unmarshal(raw, &model); err != nil || model == "" || model != strings.TrimSpace(model) {
			return "", errResponsesBadRequest
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !modelSeen {
		return "", errResponsesBadRequest
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", errResponsesBadRequest
	}
	return model, nil
}

var allowedRequestHeaders = [...]string{
	"Accept",
	"Idempotency-Key",
	"OpenAI-Beta",
	"X-Client-Request-Id",
}

func copyAllowedRequestHeaders(dst, src http.Header) {
	for _, name := range allowedRequestHeaders {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

var allowedResponseHeaders = [...]string{
	"Cache-Control",
	"Content-Type",
	"OpenAI-Request-Id",
	"Retry-After",
	"X-Request-Id",
}

func copyAllowedResponseHeaders(dst, src http.Header) {
	total := 0
	for _, name := range allowedResponseHeaders {
		for _, value := range src.Values(name) {
			size := len(name) + len(value) + 4
			if size <= defaultResponsesMaxHeaderBytes && total+size <= defaultResponsesMaxHeaderBytes {
				dst.Add(name, value)
				total += size
			}
		}
	}
}

func streamResponsesBody(ctx context.Context, dst http.ResponseWriter, src io.Reader, limit int64) {
	buffer := make([]byte, 32<<10)
	var total int64
	flusher, canFlush := dst.(http.Flusher)
	for {
		if ctx.Err() != nil {
			return
		}
		count, err := src.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > limit {
				return
			}
			if _, writeErr := dst.Write(buffer[:count]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
	}
}

func writeResponsesError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"type":"responses_proxy_error","message":"model provider request failed"}}`)
}
