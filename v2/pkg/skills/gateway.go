package skills

// This file is deliberately independent of materialize.go.  The gateway is a
// host-side fetcher for the Skills MCP Gateway; it never passes its credential
// or HTTP client into a sandbox.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	canonicalSkillDomain = "agents-gateway/skill-content/v1\x00"
	mcpProtocolVersion   = "2025-06-18"

	defaultGatewayMaxFileBytes     int64 = 4 << 20
	defaultGatewayMaxTotalBytes    int64 = 32 << 20
	defaultGatewayMaxFiles               = 2048
	defaultGatewayMaxResponseBytes int64 = 32 << 20
	defaultGatewayMaxRequestBytes  int64 = 64 << 10
	maxGatewayPathBytes                  = 4096
	maxGatewayErrorBytes                 = 512
)

// CredentialProvider is called only while constructing an outbound HTTP
// request. The returned credential is never stored in a client, materialized
// file, result, or sandbox contract.
type CredentialProvider func(context.Context) (string, error)

// GatewayLimits bound every untrusted value received from the gateway.
type GatewayLimits struct {
	MaxFileBytes     int64
	MaxTotalBytes    int64
	MaxFiles         int
	MaxResponseBytes int64
	MaxRequestBytes  int64
}

func (l GatewayLimits) withDefaults() GatewayLimits {
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = defaultGatewayMaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = defaultGatewayMaxTotalBytes
	}
	if l.MaxFiles <= 0 {
		l.MaxFiles = defaultGatewayMaxFiles
	}
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = defaultGatewayMaxResponseBytes
	}
	if l.MaxRequestBytes <= 0 {
		l.MaxRequestBytes = defaultGatewayMaxRequestBytes
	}
	return l
}

// GatewayClient is the host-side MCP client. Endpoint must be the gateway's
// /mcp URL. HTTP is accepted only for loopback endpoints, which is useful for
// local tests and local standalone deployments.
type GatewayClient struct {
	Endpoint   string
	Credential CredentialProvider
	HTTPClient *http.Client
	Limits     GatewayLimits

	requestID atomic.Uint64
}

// SkillsGatewayClient is a descriptive alias for callers that prefer the
// product name in their own wiring.
type SkillsGatewayClient = GatewayClient

// NewGatewayClient validates the endpoint before any network or credential
// access occurs.
func NewGatewayClient(endpoint string, credential CredentialProvider) (*GatewayClient, error) {
	c := &GatewayClient{Endpoint: endpoint, Credential: credential}
	if _, err := c.endpointURL(); err != nil {
		return nil, err
	}
	return c, nil
}

// MaterializedSkill contains only non-secret run metadata. SourceRevision is
// catalog metadata supplied by the gateway; ContentDigest is independently
// computed from the bytes read by this client and must not be treated as a
// source commit or revision.
type MaterializedSkill struct {
	SkillID        string
	ContentDigest  string
	SourceRevision map[string]any
}

// ResolvedSkill is the non-secret result of resolving the current catalog
// content at plan/apply time. Runtime materialization still requires this exact
// digest and fails closed if the gateway content changes.
type ResolvedSkill = MaterializedSkill

// GatewayMaterializer is a small adapter useful when the materializer is
// injected separately from the gateway client.
type GatewayMaterializer struct {
	Client *GatewayClient
}

// Materialize fetches an exact skill ID into a newly-created private staging
// directory, verifies its canonical content digest, and makes the result
// read-only. A failed operation removes the newly-created destination.
func (c *GatewayClient) Materialize(ctx context.Context, skillID, expectedDigest, destination string) (MaterializedSkill, error) {
	return (GatewayMaterializer{Client: c}).Materialize(ctx, skillID, expectedDigest, destination)
}

// MaterializeSkill is an explicit synonym for Materialize.
func (c *GatewayClient) MaterializeSkill(ctx context.Context, skillID, expectedDigest, destination string) (MaterializedSkill, error) {
	return c.Materialize(ctx, skillID, expectedDigest, destination)
}

// Resolve computes the gateway's canonical content digest without writing any
// files. Callers persist the returned digest in an immutable Agent/SkillSet
// revision before a run is admitted.
func (c *GatewayClient) Resolve(ctx context.Context, skillID string) (ResolvedSkill, error) {
	var result ResolvedSkill
	if c == nil {
		return result, errors.New("skills gateway client is required")
	}
	if c.Credential == nil {
		return result, errors.New("skills gateway credential provider is required")
	}
	if err := validateSkillID(skillID); err != nil {
		return result, err
	}
	inspection, _, digest, err := (GatewayMaterializer{Client: c}).fetch(ctx, skillID)
	if err != nil {
		return result, err
	}
	return ResolvedSkill{SkillID: skillID, ContentDigest: digest, SourceRevision: inspection.sourceRevision}, nil
}

// ResolveSkill is an explicit synonym for Resolve.
func (c *GatewayClient) ResolveSkill(ctx context.Context, skillID string) (ResolvedSkill, error) {
	return c.Resolve(ctx, skillID)
}

func (m GatewayMaterializer) Materialize(ctx context.Context, skillID, expectedDigest, destination string) (MaterializedSkill, error) {
	var result MaterializedSkill
	if m.Client == nil {
		return result, errors.New("skills gateway client is required")
	}
	if m.Client.Credential == nil {
		return result, errors.New("skills gateway credential provider is required")
	}
	if err := validateSkillID(skillID); err != nil {
		return result, err
	}
	if !validGatewayDigest(expectedDigest) {
		return result, errors.New("expected canonical sha256 digest is required")
	}
	if destination == "" {
		return result, errors.New("skill staging destination is required")
	}
	if _, err := os.Lstat(destination); err == nil {
		return result, errors.New("skill staging destination already exists")
	} else if !os.IsNotExist(err) {
		return result, fmt.Errorf("inspect skill staging destination: %w", err)
	}

	inspection, contents, actualDigest, err := m.fetch(ctx, skillID)
	if err != nil {
		return result, err
	}
	if actualDigest != expectedDigest {
		return result, fmt.Errorf("skill content digest mismatch: expected %s, got %s", expectedDigest, actualDigest)
	}
	if err := writePrivateSkill(destination, contents); err != nil {
		return result, err
	}
	return MaterializedSkill{
		SkillID:        skillID,
		ContentDigest:  actualDigest,
		SourceRevision: inspection.sourceRevision,
	}, nil
}

func (m GatewayMaterializer) fetch(ctx context.Context, skillID string) (gatewayInspection, map[string][]byte, string, error) {
	var empty gatewayInspection
	limits := m.Client.Limits.withDefaults()
	session := m.Client.newSession()
	if _, err := session.initialize(ctx); err != nil {
		return empty, nil, "", err
	}
	inspection, err := session.inspect(ctx, skillID)
	if err != nil {
		return empty, nil, "", err
	}
	if inspection.catalogID != skillID {
		return empty, nil, "", fmt.Errorf("skills gateway returned catalog ID %q for requested skill %q", inspection.catalogID, skillID)
	}
	if len(inspection.files) == 0 {
		return empty, nil, "", errors.New("skills gateway returned no skill files")
	}
	if len(inspection.files) > limits.MaxFiles {
		return empty, nil, "", errors.New("skill exceeds file-count limit")
	}
	contents := make(map[string][]byte, len(inspection.files))
	var total int64
	for _, relativePath := range inspection.files {
		if err := contextError(ctx); err != nil {
			return empty, nil, "", err
		}
		gatewayPath, err := joinGatewaySkillPath(skillID, relativePath)
		if err != nil {
			return empty, nil, "", err
		}
		text, err := session.read(ctx, gatewayPath, limits.MaxFileBytes, limits.MaxResponseBytes)
		if err != nil {
			return empty, nil, "", fmt.Errorf("read skill file %q: %w", relativePath, err)
		}
		if !utf8.ValidString(text) {
			return empty, nil, "", fmt.Errorf("skill file %q is not valid UTF-8", relativePath)
		}
		if int64(len(text)) > limits.MaxFileBytes {
			return empty, nil, "", fmt.Errorf("skill file %q exceeds size limit", relativePath)
		}
		if int64(len(text)) > limits.MaxTotalBytes-total {
			return empty, nil, "", errors.New("skill exceeds total-size limit")
		}
		total += int64(len(text))
		contents[relativePath] = []byte(text)
	}
	digest, err := CanonicalDigest(contents)
	if err != nil {
		return empty, nil, "", fmt.Errorf("compute skill content digest: %w", err)
	}
	return inspection, contents, digest, nil
}

// CanonicalDigest computes the stable, domain-separated content digest used
// by the gateway materializer. Paths are sorted by their canonical UTF-8 byte
// representation and each path and byte payload is length-delimited to avoid
// concatenation ambiguity. The returned form is sha256:<lowercase-hex>.
func CanonicalDigest(files map[string][]byte) (string, error) {
	paths := make([]string, 0, len(files))
	seenFolded := make(map[string]string, len(files))
	for path := range files {
		if err := validateRelativeSkillPath(path); err != nil {
			return "", err
		}
		folded := strings.ToLower(path)
		if previous, ok := seenFolded[folded]; ok && previous != path {
			return "", fmt.Errorf("skill contains case-colliding paths %q and %q", previous, path)
		}
		seenFolded[folded] = path
		paths = append(paths, path)
	}
	sort.Strings(paths)

	hash := sha256.New()
	_, _ = hash.Write([]byte(canonicalSkillDomain))
	for _, path := range paths {
		_, _ = hash.Write([]byte("file\x00"))
		writeLength(hash, uint64(len(path)))
		_, _ = hash.Write([]byte(path))
		writeLength(hash, uint64(len(files[path])))
		_, _ = hash.Write(files[path])
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// ComputeCanonicalDigest is an intentionally verbose alias for integrations
// that prefer a verb-like exported API.
func ComputeCanonicalDigest(files map[string][]byte) (string, error) {
	return CanonicalDigest(files)
}

func writeLength(w io.Writer, length uint64) {
	var encoded [10]byte
	n := 0
	for length >= 0x80 {
		encoded[n] = byte(length) | 0x80
		length >>= 7
		n++
	}
	encoded[n] = byte(length)
	n++
	_, _ = w.Write(encoded[:n])
}

type gatewayInspection struct {
	catalogID      string
	files          []string
	sourceRevision map[string]any
}

type mcpSession struct {
	client    *GatewayClient
	sessionID string
}

func (c *GatewayClient) newSession() *mcpSession { return &mcpSession{client: c} }

func (s *mcpSession) initialize(ctx context.Context) (json.RawMessage, error) {
	result, err := s.request(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "agents-gateway",
			"version": "2",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize skills gateway MCP session: %w", err)
	}
	if err := requireJSONObject(result, "initialize result"); err != nil {
		return nil, err
	}
	if err := s.notification(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("complete skills gateway MCP initialization: %w", err)
	}
	return result, nil
}

func (s *mcpSession) inspect(ctx context.Context, skillID string) (gatewayInspection, error) {
	var inspection gatewayInspection
	result, err := s.request(ctx, "tools/call", map[string]any{
		"name":      "skills_inspect",
		"arguments": map[string]string{"name": skillID},
	})
	if err != nil {
		return inspection, fmt.Errorf("call skills_inspect: %w", err)
	}
	payload, err := toolPayload(result)
	if err != nil {
		return inspection, fmt.Errorf("decode skills_inspect result: %w", err)
	}
	if err := rejectUnknownKeys(payload, map[string]bool{"metadata": true, "catalog": true, "files": true, "error": true}); err != nil {
		return inspection, fmt.Errorf("invalid skills_inspect payload: %w", err)
	}
	if rawError, ok := payload["error"]; ok {
		var message string
		if err := json.Unmarshal(rawError, &message); err != nil || message == "" {
			return inspection, errors.New("invalid skills_inspect error payload")
		}
		return inspection, fmt.Errorf("skills gateway rejected skill inspection: %s", boundedText(message, maxGatewayErrorBytes))
	}
	rawCatalog, ok := payload["catalog"]
	if !ok {
		return inspection, errors.New("skills_inspect result omitted catalog")
	}
	var catalog map[string]json.RawMessage
	if err := unmarshalObject(rawCatalog, &catalog); err != nil {
		return inspection, fmt.Errorf("invalid skills_inspect catalog: %w", err)
	}
	var catalogID string
	if err := unmarshalRequiredString(catalog, "id", &catalogID); err != nil {
		return inspection, fmt.Errorf("invalid skills_inspect catalog ID: %w", err)
	}
	if err := validateSkillID(catalogID); err != nil {
		return inspection, fmt.Errorf("invalid skills_inspect catalog ID: %w", err)
	}
	rawFiles, ok := payload["files"]
	if !ok {
		return inspection, errors.New("skills_inspect result omitted files")
	}
	var listed []string
	if err := unmarshalStringArray(rawFiles, &listed); err != nil {
		return inspection, fmt.Errorf("invalid skills_inspect file list: %w", err)
	}
	if err := validateListedFiles(listed); err != nil {
		return inspection, err
	}

	var sourceRevision map[string]any
	if rawSource, ok := catalog["source"]; ok && string(rawSource) != "null" {
		if err := unmarshalAnyObject(rawSource, &sourceRevision); err != nil {
			return inspection, fmt.Errorf("invalid skills_inspect source metadata: %w", err)
		}
	}
	return gatewayInspection{catalogID: catalogID, files: listed, sourceRevision: sourceRevision}, nil
}

func (s *mcpSession) read(ctx context.Context, path string, maxFileBytes, maxResponseBytes int64) (string, error) {
	result, err := s.requestWithResponseLimit(ctx, "tools/call", map[string]any{
		"name":      "skill_read",
		"arguments": map[string]string{"path": path},
	}, maxResponseBytes)
	if err != nil {
		return "", err
	}
	text, err := toolText(result)
	if err != nil {
		return "", err
	}
	if int64(len(text)) > maxFileBytes {
		return "", errors.New("skill file exceeds size limit")
	}
	return text, nil
}

func (s *mcpSession) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.requestWithResponseLimit(ctx, method, params, s.client.Limits.withDefaults().MaxResponseBytes)
}

func (s *mcpSession) requestWithResponseLimit(ctx context.Context, method string, params any, responseLimit int64) (json.RawMessage, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	id := "agw-" + strconv.FormatUint(s.client.requestID.Add(1), 10)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("encode MCP request: %w", err)
	}
	limits := s.client.Limits.withDefaults()
	if int64(len(body)) > limits.MaxRequestBytes {
		return nil, errors.New("MCP request exceeds size limit")
	}
	responseBody, responseHeaders, status, err := s.doHTTP(ctx, body, responseLimit)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("skills gateway returned HTTP status %d", status)
	}
	if value := responseHeaders.Get("Mcp-Session-Id"); value != "" {
		if err := validateHeaderValue(value); err != nil {
			return nil, fmt.Errorf("invalid skills gateway session header: %w", err)
		}
		s.sessionID = value
	}
	return parseRPCResponse(responseBody, id)
}

func (s *mcpSession) notification(ctx context.Context, method string, params any) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("encode MCP notification: %w", err)
	}
	limits := s.client.Limits.withDefaults()
	if int64(len(body)) > limits.MaxRequestBytes {
		return errors.New("MCP notification exceeds size limit")
	}
	responseBody, responseHeaders, status, err := s.doHTTP(ctx, body, limits.MaxResponseBytes)
	if err != nil {
		return err
	}
	if value := responseHeaders.Get("Mcp-Session-Id"); value != "" {
		if err := validateHeaderValue(value); err != nil {
			return fmt.Errorf("invalid skills gateway session header: %w", err)
		}
		s.sessionID = value
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("skills gateway returned HTTP status %d", status)
	}
	if len(bytes.TrimSpace(responseBody)) != 0 {
		return errors.New("MCP notification unexpectedly returned a response")
	}
	return nil
}

func (s *mcpSession) doHTTP(ctx context.Context, body []byte, responseLimit int64) ([]byte, http.Header, int, error) {
	endpoint, err := s.client.endpointURL()
	if err != nil {
		return nil, nil, 0, err
	}
	if responseLimit <= 0 {
		responseLimit = defaultGatewayMaxResponseBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("create skills gateway request: %w", err)
	}
	credential := ""
	if s.client.Credential == nil {
		return nil, nil, 0, errors.New("skills gateway credential provider is required")
	}
	credential, err = s.client.Credential(ctx)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("obtain skills gateway credential: %w", err)
	}
	if credential == "" || len(credential) > 4096 || credential != strings.TrimSpace(credential) || strings.IndexFunc(credential, unicodeSpace) >= 0 || strings.IndexFunc(credential, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return nil, nil, 0, errors.New("skills gateway credential is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	if s.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	client := s.client.httpClient()
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("skills gateway request failed: %w", err)
	}
	defer response.Body.Close()
	payload, readErr := readGatewayBody(response.Body, responseLimit)
	if readErr != nil {
		return nil, nil, response.StatusCode, readErr
	}
	if !utf8.Valid(payload) {
		return nil, nil, response.StatusCode, errors.New("skills gateway returned invalid UTF-8")
	}
	return payload, response.Header.Clone(), response.StatusCode, nil
}

func (c *GatewayClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		clone := *c.HTTPClient
		clone.CheckRedirect = rejectRedirects
		if transport, ok := clone.Transport.(*http.Transport); ok && transport != nil {
			transportClone := transport.Clone()
			transportClone.Proxy = nil
			clone.Transport = transportClone
		}
		return &clone
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, CheckRedirect: rejectRedirects}
}

func rejectRedirects(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func (c *GatewayClient) endpointURL() (*url.URL, error) {
	if c == nil || c.Endpoint == "" {
		return nil, errors.New("skills gateway /mcp endpoint is required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("skills gateway endpoint must be an absolute URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("skills gateway endpoint must not contain credentials, query, or fragment")
	}
	if u.Path != "/mcp" && u.Path != "/mcp/" {
		return nil, errors.New("skills gateway endpoint must use /mcp")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, errors.New("skills gateway endpoint must use HTTPS except for loopback HTTP")
	}
	u.Path = "/mcp"
	return u, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readGatewayBody(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("gateway response limit must be positive")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read skills gateway response: %w", err)
	}
	if int64(len(payload)) > limit {
		return nil, errors.New("skills gateway response exceeds size limit")
	}
	return payload, nil
}

func parseRPCResponse(payload []byte, expectedID string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, errors.New("skills gateway returned an empty MCP response")
	}
	var response json.RawMessage
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.Contains(trimmed, []byte("\ndata:")) {
		var err error
		response, err = parseSSEResponse(trimmed)
		if err != nil {
			return nil, err
		}
	} else {
		response = trimmed
	}
	if !utf8.Valid(response) {
		return nil, errors.New("MCP response is not valid UTF-8")
	}
	if err := validateJSONDocument(response); err != nil {
		return nil, fmt.Errorf("invalid MCP response JSON: %w", err)
	}
	var object map[string]json.RawMessage
	if err := unmarshalObject(response, &object); err != nil {
		return nil, fmt.Errorf("MCP response must be an object: %w", err)
	}
	if err := rejectUnknownKeys(object, map[string]bool{"jsonrpc": true, "id": true, "result": true, "error": true}); err != nil {
		return nil, fmt.Errorf("invalid MCP response envelope: %w", err)
	}
	var version string
	if err := json.Unmarshal(object["jsonrpc"], &version); err != nil || version != "2.0" {
		return nil, errors.New("MCP response has invalid jsonrpc version")
	}
	var id string
	if err := json.Unmarshal(object["id"], &id); err != nil || id != expectedID {
		return nil, errors.New("MCP response ID did not match request")
	}
	rawResult, hasResult := object["result"]
	rawError, hasError := object["error"]
	if hasResult == hasError {
		return nil, errors.New("MCP response must contain exactly one of result or error")
	}
	if hasError {
		return nil, formatMCPError(rawError)
	}
	if err := validateJSONDocument(rawResult); err != nil {
		return nil, fmt.Errorf("invalid MCP result JSON: %w", err)
	}
	return rawResult, nil
}

func parseSSEResponse(payload []byte) (json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	scanner.Buffer(make([]byte, 1024), len(payload)+1)
	var data strings.Builder
	var response json.RawMessage
	count := 0
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		candidate := strings.TrimSpace(data.String())
		data.Reset()
		if candidate == "" {
			return nil
		}
		if count != 0 {
			return errors.New("MCP SSE response contained multiple messages")
		}
		response = json.RawMessage(candidate)
		count++
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			data.WriteString(value)
			data.WriteByte('\n')
		case strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:"):
			continue
		default:
			return nil, errors.New("invalid MCP SSE field")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read MCP SSE response: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, errors.New("MCP SSE response did not contain exactly one message")
	}
	return response, nil
}

func toolPayload(result json.RawMessage) (map[string]json.RawMessage, error) {
	object, err := rawResultObject(result)
	if err != nil {
		return nil, err
	}
	if raw, ok := object["isError"]; ok {
		var isError bool
		if err := json.Unmarshal(raw, &isError); err != nil {
			return nil, errors.New("MCP tool isError was not boolean")
		}
		if isError {
			return nil, errors.New("MCP tool returned an error result")
		}
	}
	if raw, ok := object["structuredContent"]; ok {
		var structured map[string]json.RawMessage
		if err := unmarshalObject(raw, &structured); err == nil {
			return structured, nil
		}
	}
	content, ok := object["content"]
	if !ok {
		return nil, errors.New("MCP tool result omitted structuredContent and content")
	}
	text, err := textContent(content)
	if err != nil {
		return nil, err
	}
	var structured map[string]json.RawMessage
	if err := unmarshalObject([]byte(text), &structured); err != nil {
		return nil, errors.New("MCP tool content was not a JSON object")
	}
	return structured, nil
}

func toolText(result json.RawMessage) (string, error) {
	object, err := rawResultObject(result)
	if err != nil {
		return "", err
	}
	if raw, ok := object["isError"]; ok {
		var isError bool
		if err := json.Unmarshal(raw, &isError); err != nil {
			return "", errors.New("MCP tool isError was not boolean")
		}
		if isError {
			return "", errors.New("MCP tool returned an error result")
		}
	}
	if raw, ok := object["structuredContent"]; ok {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return checkGatewayText(text)
		}
	}
	content, ok := object["content"]
	if !ok {
		if text, err := jsonString(result); err == nil {
			return checkGatewayText(text)
		}
		return "", errors.New("MCP tool result omitted content")
	}
	text, err := textContent(content)
	if err != nil {
		return "", err
	}
	return checkGatewayText(text)
}

func rawResultObject(result json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := unmarshalObject(result, &object); err != nil {
		return nil, fmt.Errorf("MCP tool result must be an object: %w", err)
	}
	if err := rejectUnknownKeys(object, map[string]bool{"content": true, "structuredContent": true, "isError": true, "_meta": true}); err != nil {
		return nil, fmt.Errorf("invalid MCP tool result: %w", err)
	}
	return object, nil
}

func textContent(raw json.RawMessage) (string, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		return "", errors.New("MCP tool content must contain exactly one item")
	}
	if err := rejectUnknownKeys(items[0], map[string]bool{"type": true, "text": true, "annotations": true, "_meta": true}); err != nil {
		return "", fmt.Errorf("invalid MCP text content: %w", err)
	}
	var kind string
	if err := unmarshalRequiredString(items[0], "type", &kind); err != nil || kind != "text" {
		return "", errors.New("MCP tool content item is not text")
	}
	var text string
	if err := unmarshalRequiredString(items[0], "text", &text); err != nil {
		return "", fmt.Errorf("invalid MCP text content: %w", err)
	}
	return text, nil
}

func checkGatewayText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("MCP tool returned invalid UTF-8")
	}
	if strings.HasPrefix(text, "ERROR: skill file not found") {
		return "", errors.New("skills gateway could not read requested file")
	}
	return text, nil
}

func formatMCPError(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := unmarshalObject(raw, &object); err != nil {
		return errors.New("skills gateway returned an invalid MCP error")
	}
	if err := rejectUnknownKeys(object, map[string]bool{"code": true, "message": true, "data": true}); err != nil {
		return fmt.Errorf("invalid skills gateway MCP error: %w", err)
	}
	var code json.Number
	var message string
	if err := json.Unmarshal(object["code"], &code); err != nil || code.String() == "" {
		return errors.New("skills gateway MCP error has invalid code")
	}
	if err := json.Unmarshal(object["message"], &message); err != nil {
		return errors.New("skills gateway MCP error has invalid message")
	}
	return fmt.Errorf("skills gateway MCP error %s: %s", code.String(), boundedText(message, maxGatewayErrorBytes))
}

func validateListedFiles(files []string) error {
	seen := make(map[string]struct{}, len(files))
	seenFolded := make(map[string]string, len(files))
	foundManifest := false
	for _, path := range files {
		if err := validateRelativeSkillPath(path); err != nil {
			return err
		}
		if _, ok := seen[path]; ok {
			return fmt.Errorf("skills gateway returned duplicate file %q", path)
		}
		seen[path] = struct{}{}
		folded := strings.ToLower(path)
		if previous, ok := seenFolded[folded]; ok && previous != path {
			return fmt.Errorf("skills gateway returned case-colliding files %q and %q", previous, path)
		}
		seenFolded[folded] = path
		if path == "SKILL.md" {
			foundManifest = true
		}
	}
	if !foundManifest {
		return errors.New("skills gateway file list does not contain root SKILL.md")
	}
	return nil
}

func validateSkillID(skillID string) error {
	if err := validateRelativeSkillPath(skillID); err != nil {
		return fmt.Errorf("invalid skill ID: %w", err)
	}
	return nil
}

func joinGatewaySkillPath(skillID, relativePath string) (string, error) {
	if err := validateSkillID(skillID); err != nil {
		return "", err
	}
	if err := validateRelativeSkillPath(relativePath); err != nil {
		return "", err
	}
	joined := skillID + "/" + relativePath
	if err := validateRelativeSkillPath(joined); err != nil {
		return "", fmt.Errorf("invalid gateway skill path: %w", err)
	}
	return joined, nil
}

func validateRelativeSkillPath(path string) error {
	if path == "" || len(path) > maxGatewayPathBytes || !utf8.ValidString(path) {
		return errors.New("skill path is empty, invalid UTF-8, or too long")
	}
	if strings.HasPrefix(path, "/") || strings.ContainsRune(path, '\x00') || strings.Contains(path, "\\") || filepath.IsAbs(path) {
		return fmt.Errorf("unsafe skill path %q", path)
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("skill path contains a special component: %q", path)
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return fmt.Errorf("skill path contains a special name: %q", path)
		}
		for _, r := range part {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("skill path contains a control character: %q", path)
			}
		}
		if isWindowsReservedName(part) {
			return fmt.Errorf("skill path contains a reserved name: %q", path)
		}
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) != path {
		return fmt.Errorf("skill path is not canonical: %q", path)
	}
	return nil
}

func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	default:
		return false
	}
}

func writePrivateSkill(destination string, contents map[string][]byte) (err error) {
	if err := os.Mkdir(destination, 0700); err != nil {
		return fmt.Errorf("create private skill staging directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(destination)
		}
	}()
	createdDirs := map[string]struct{}{destination: {}}
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, relativePath := range paths {
		if err := validateRelativeSkillPath(relativePath); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(relativePath))
		if !within(destination, target) {
			return errors.New("skill staging path escaped destination")
		}
		parent := filepath.Dir(target)
		if err := makeFreshDirectories(destination, parent, createdDirs); err != nil {
			return err
		}
		file, openErr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if openErr != nil {
			return fmt.Errorf("create staged skill file %q: %w", relativePath, openErr)
		}
		_, writeErr := file.Write(contents[relativePath])
		closeErr := file.Close()
		if writeErr != nil {
			return fmt.Errorf("write staged skill file %q: %w", relativePath, writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close staged skill file %q: %w", relativePath, closeErr)
		}
		info, statErr := os.Lstat(target)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("staged skill file %q is not a regular file", relativePath)
		}
		if err := os.Chmod(target, 0444); err != nil {
			return fmt.Errorf("make staged skill file read-only: %w", err)
		}
	}
	for _, directory := range sortedDirectories(createdDirs) {
		info, statErr := os.Lstat(directory)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("skill staging contains a symlink or non-directory")
		}
		if err := os.Chmod(directory, 0555); err != nil {
			return fmt.Errorf("make staged skill directory read-only: %w", err)
		}
	}
	keep = true
	return nil
}

func makeFreshDirectories(root, target string, created map[string]struct{}) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("skill staging directory escaped destination")
	}
	if relative == "." {
		return nil
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if _, ok := created[current]; ok {
			continue
		}
		if err := os.Mkdir(current, 0700); err != nil {
			return fmt.Errorf("create staged skill directory: %w", err)
		}
		info, statErr := os.Lstat(current)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("staged skill directory is not a real directory")
		}
		created[current] = struct{}{}
	}
	return nil
}

func sortedDirectories(directories map[string]struct{}) []string {
	values := make([]string, 0, len(directories))
	for directory := range directories {
		values = append(values, directory)
	}
	sort.Slice(values, func(i, j int) bool {
		depthI := strings.Count(values[i], string(filepath.Separator))
		depthJ := strings.Count(values[j], string(filepath.Separator))
		if depthI != depthJ {
			return depthI > depthJ
		}
		return values[i] < values[j]
	})
	return values
}

func validateHeaderValue(value string) error {
	if value == "" || len(value) > 512 || strings.IndexFunc(value, func(r rune) bool { return r == '\r' || r == '\n' || r == 0 }) >= 0 {
		return errors.New("session header is empty, too long, or contains control characters")
	}
	return nil
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validGatewayDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func unicodeSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' || r == ' ' || r == '\v' || r == '\f'
}

func unmarshalObject(raw []byte, target *map[string]json.RawMessage) error {
	if err := validateJSONDocument(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil || *target == nil {
		return errors.New("expected a JSON object")
	}
	return nil
}

func requireJSONObject(raw json.RawMessage, name string) error {
	var object map[string]json.RawMessage
	if err := unmarshalObject(raw, &object); err != nil {
		return fmt.Errorf("%s must be a JSON object: %w", name, err)
	}
	return nil
}

func unmarshalAnyObject(raw []byte, target *map[string]any) error {
	if err := validateJSONDocument(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil || *target == nil {
		return errors.New("expected a JSON object")
	}
	return nil
}

func unmarshalRequiredString(object map[string]json.RawMessage, key string, target *string) error {
	raw, ok := object[key]
	if !ok {
		return fmt.Errorf("missing %s", key)
	}
	if err := json.Unmarshal(raw, target); err != nil || !utf8.ValidString(*target) {
		return fmt.Errorf("%s must be a UTF-8 string", key)
	}
	return nil
}

func unmarshalStringArray(raw []byte, target *[]string) error {
	if err := validateJSONDocument(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("expected an array of strings")
	}
	for _, value := range *target {
		if !utf8.ValidString(value) {
			return errors.New("file list contains invalid UTF-8")
		}
	}
	return nil
}

func jsonString(raw []byte) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func rejectUnknownKeys(object map[string]json.RawMessage, allowed map[string]bool) error {
	for key := range object {
		if !allowed[key] {
			return fmt.Errorf("unexpected field %q", key)
		}
	}
	return nil
}

func validateJSONDocument(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, 0); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("JSON contains multiple values")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch delimiter := token.(type) {
	case json.Delim:
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if _, ok := seen[key]; ok {
					return fmt.Errorf("JSON object contains duplicate field %q", key)
				}
				seen[key] = struct{}{}
				if err := validateJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("unterminated JSON object")
			}
		case '[':
			for decoder.More() {
				if err := validateJSONValue(decoder, depth+1); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("unterminated JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	return nil
}
