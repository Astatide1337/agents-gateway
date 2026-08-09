package toolbroker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxMCPSessionHeader = 4096

const maxMCPCredential = 16 << 10

type upstreamSession struct {
	client     *http.Client
	endpoint   *url.URL
	credential []byte
	sessionID  string
}

func openUpstreamSession(ctx context.Context, client *http.Client, endpoint *url.URL, credential []byte, requestID string) (*upstreamSession, error) {
	if client == nil || endpoint == nil {
		return nil, errors.New("MCP session client and endpoint are required")
	}
	session := &upstreamSession{client: client, endpoint: endpoint, credential: credential}
	cleanlyOpened := false
	defer func() {
		if !cleanlyOpened {
			session.close()
		}
	}()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      "initialize:" + requestID,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": MCPProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]string{
				"name":    "agents-gateway",
				"version": "2",
			},
		},
	})
	response, err := session.do(ctx, http.MethodPost, payload)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("initialize MCP session: %w", err)
	}
	defer response.Body.Close()
	body, err := readMCPBody(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read MCP initialization: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("initialize MCP session: HTTP %d", response.StatusCode)
	}
	sessionHeaders := response.Header.Values("Mcp-Session-Id")
	if len(sessionHeaders) > 1 {
		return nil, errors.New("MCP server returned multiple session IDs")
	}
	if len(sessionHeaders) == 1 && sessionHeaders[0] == "" {
		return nil, errors.New("MCP server returned an empty session ID")
	}
	if err := validateSessionHeader(response.Header.Get("Mcp-Session-Id")); err != nil {
		return nil, err
	}
	session.sessionID = response.Header.Get("Mcp-Session-Id")
	if !validMCPResponseContentType(response.Header.Get("Content-Type")) {
		return nil, errors.New("MCP initialization returned an unsupported content type")
	}
	decoded, err := decodeMCPWireBodyForRequest(body, response.Header.Get("Content-Type"), "initialize:"+requestID)
	if err != nil {
		return nil, fmt.Errorf("decode MCP initialization: %w", err)
	}
	envelope, err := decodeUpstreamMCPResponse(decoded, "initialize:"+requestID)
	if err != nil || envelope.Error != nil {
		return nil, errors.New("MCP server returned an invalid initialization result")
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(envelope.Result, &result) != nil || result.ProtocolVersion != MCPProtocolVersion {
		return nil, errors.New("MCP server returned an invalid initialization result")
	}
	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	notificationResponse, err := session.do(ctx, http.MethodPost, notification)
	if err != nil {
		return nil, fmt.Errorf("complete MCP initialization: %w", err)
	}
	defer notificationResponse.Body.Close()
	if notificationResponse.StatusCode < 200 || notificationResponse.StatusCode >= 300 {
		return nil, fmt.Errorf("complete MCP initialization: HTTP %d", notificationResponse.StatusCode)
	}
	notificationBody, err := readMCPBody(notificationResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read MCP initialization notification: %w", err)
	}
	if len(bytes.TrimSpace(notificationBody)) != 0 {
		return nil, errors.New("MCP initialization notification returned an unexpected body")
	}
	cleanlyOpened = true
	return session, nil
}

func (s *upstreamSession) post(ctx context.Context, payload []byte) (*http.Response, error) {
	return s.do(ctx, http.MethodPost, payload)
}

func (s *upstreamSession) do(ctx context.Context, method string, payload []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, s.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", MCPProtocolVersion)
	request.Header.Set("User-Agent", "agents-gateway-v2")
	if len(s.credential) > 0 {
		request.Header.Set("Authorization", "Bearer "+string(s.credential))
	}
	if s.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", s.sessionID)
	}
	return s.client.Do(request)
}

func (s *upstreamSession) close() {
	if s == nil || s.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	response, err := s.do(ctx, http.MethodDelete, nil)
	if err == nil {
		_ = response.Body.Close()
	} else if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func validateSessionHeader(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxMCPSessionHeader || value != strings.TrimSpace(value) || strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return errors.New("MCP server returned an invalid session ID")
	}
	return nil
}

func validateMCPCredential(value []byte) error {
	if len(value) > maxMCPCredential || strings.IndexFunc(string(value), func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return errors.New("MCP credential contains invalid header characters")
	}
	return nil
}

func readMCPBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxMCPResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMCPResponse {
		return nil, errors.New("MCP response exceeds size limit")
	}
	if !utf8.Valid(body) {
		return nil, errors.New("MCP response is not valid UTF-8")
	}
	return body, nil
}

func decodeMCPWireBody(body []byte) ([]byte, error) {
	return decodeMCPWireBodyForRequest(body, "", "")
}

func decodeMCPWireBodyForRequest(body []byte, contentType, expectedID string) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("MCP server returned an empty response")
	}
	isSSE, err := mcpResponseIsSSE(contentType, trimmed)
	if err != nil {
		return nil, err
	}
	if !isSSE {
		if !json.Valid(trimmed) {
			return nil, errors.New("MCP server returned invalid JSON")
		}
		return trimmed, nil
	}

	var event []byte
	var eventData bytes.Buffer
	haveData := false
	eventCount := 0
	finishEvent := func() error {
		if !haveData {
			return nil
		}
		eventCount++
		if eventCount > 1024 {
			return errors.New("MCP event stream contains too many events")
		}
		candidate := bytes.TrimSpace(eventData.Bytes())
		if len(candidate) == 0 || bytes.Equal(candidate, []byte("[DONE]")) {
			eventData.Reset()
			haveData = false
			return nil
		}
		if !json.Valid(candidate) {
			return errors.New("MCP server returned an invalid event stream")
		}
		kind, id, err := classifyMCPEvent(candidate)
		if err != nil {
			return err
		}
		if kind == "response" {
			if expectedID != "" && !bytes.Equal(id, mustJSONID(expectedID)) {
				return errors.New("MCP response ID did not match the request")
			}
			if event != nil {
				return errors.New("MCP response stream contained multiple responses")
			}
			event = append([]byte(nil), candidate...)
		} else if kind == "request" {
			return errors.New("MCP server sent an unsupported request over the response stream")
		}
		eventData.Reset()
		haveData = false
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 4096), maxMCPResponse)
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			if err := finishEvent(); err != nil {
				return nil, err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := splitSSEField(line)
		if field != "data" {
			continue
		}
		if haveData {
			eventData.WriteByte('\n')
		}
		eventData.WriteString(value)
		if eventData.Len() > maxMCPResponse {
			return nil, errors.New("MCP event stream exceeds limits")
		}
		haveData = true
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("MCP event stream exceeds limits")
	}
	if err := finishEvent(); err != nil {
		return nil, err
	}
	if len(event) == 0 || !json.Valid(event) {
		return nil, errors.New("MCP server returned an invalid event stream")
	}
	return event, nil
}

func mcpResponseIsSSE(contentType string, body []byte) (bool, error) {
	if contentType == "" {
		// Keep the unexported compatibility helper useful in unit tests; all
		// production HTTP responses are required to provide a recognized type.
		return body[0] != '{', nil
	}
	media := strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	switch strings.ToLower(media) {
	case "application/json":
		return false, nil
	case "text/event-stream":
		return true, nil
	default:
		return false, errors.New("MCP server returned an unsupported content type")
	}
}

func splitSSEField(line []byte) (string, string) {
	parts := bytes.SplitN(line, []byte{':'}, 2)
	field := string(parts[0])
	if len(parts) == 1 {
		return field, ""
	}
	value := string(parts[1])
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	return field, value
}

func classifyMCPEvent(raw []byte) (kind string, id []byte, err error) {
	fields, err := decodeMCPTopLevelObject(raw)
	if err != nil {
		return "", nil, errors.New("MCP event is not a JSON object")
	}
	if _, ok := fields["result"]; ok {
		return "response", append([]byte(nil), fields["id"]...), nil
	}
	if _, ok := fields["error"]; ok {
		return "response", append([]byte(nil), fields["id"]...), nil
	}
	if _, ok := fields["method"]; ok {
		if rawID, hasID := fields["id"]; hasID {
			return "request", rawID, nil
		}
		return "notification", nil, nil
	}
	return "unknown", nil, errors.New("MCP event is neither a response nor a notification")
}

type mcpResponse struct {
	Result json.RawMessage
	Error  *mcpRPCError
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func decodeUpstreamMCPResponse(raw []byte, expectedID string) (mcpResponse, error) {
	fields, err := decodeMCPTopLevelObject(raw)
	if err != nil {
		return mcpResponse{}, errors.New("MCP response is not a JSON object")
	}
	var version string
	if err := decodeExact(fields, "jsonrpc", &version); err != nil || version != "2.0" {
		return mcpResponse{}, errors.New("MCP response has an invalid JSON-RPC version")
	}
	for key := range fields {
		if key != "jsonrpc" && key != "id" && key != "result" && key != "error" {
			return mcpResponse{}, errors.New("MCP response contains an unsupported field")
		}
	}
	rawID, ok := fields["id"]
	if !ok || !validRPCID(rawID) || (expectedID != "" && !bytes.Equal(rawID, mustJSONID(expectedID))) {
		return mcpResponse{}, errors.New("MCP response ID did not match the request")
	}
	result, hasResult := fields["result"]
	rawError, hasError := fields["error"]
	if hasResult == hasError {
		return mcpResponse{}, errors.New("MCP response must contain exactly one result or error")
	}
	if hasResult {
		if !json.Valid(result) {
			return mcpResponse{}, errors.New("MCP response result is invalid JSON")
		}
		return mcpResponse{Result: append(json.RawMessage(nil), result...)}, nil
	}
	errorFields, err := decodeMCPTopLevelObject(rawError)
	if err != nil {
		return mcpResponse{}, errors.New("MCP response error is invalid")
	}
	for key := range errorFields {
		if key != "code" && key != "message" && key != "data" {
			return mcpResponse{}, errors.New("MCP response error contains an unsupported field")
		}
	}
	var rpcError mcpRPCError
	if err := decodeExact(errorFields, "code", &rpcError.Code); err != nil {
		return mcpResponse{}, errors.New("MCP response error is invalid")
	}
	if err := decodeExact(errorFields, "message", &rpcError.Message); err != nil || rpcError.Message == "" {
		return mcpResponse{}, errors.New("MCP response error is invalid")
	}
	return mcpResponse{Error: &rpcError}, nil
}

func decodeMCPTopLevelObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("object required")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("object key required")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("object terminator required")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return fields, nil
}

func mustJSONID(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

func validMCPResponseContentType(value string) bool {
	if value == "" {
		return false
	}
	media := strings.TrimSpace(strings.SplitN(value, ";", 2)[0])
	return strings.EqualFold(media, "application/json") || strings.EqualFold(media, "text/event-stream")
}
