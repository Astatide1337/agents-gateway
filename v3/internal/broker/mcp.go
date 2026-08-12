package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

type mcpCallResponse struct {
	content json.RawMessage
	isError bool
}

func (b *Broker) invokeMCP(ctx context.Context, endpoint, server, tool string, arguments, credential []byte) (mcpCallResponse, error) {
	if b == nil || b.client == nil || ctx == nil || endpoint == "" || len(arguments) == 0 {
		return mcpCallResponse{}, ErrInvalidRequest
	}
	requestID := "agw-" + strings.TrimPrefix(requestDigest(server, tool, arguments), "sha256:")[:32]
	initPayload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "initialize-" + requestID, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "agents-gateway-v3", "version": "3"},
		},
	})
	if err != nil {
		return mcpCallResponse{}, ErrInvalidRequest
	}
	initResponse, err := b.mcpRequest(ctx, endpoint, initPayload, credential, "")
	if err != nil {
		return mcpCallResponse{}, err
	}
	initResult, _, err := decodeMCPResponse(initResponse.body, initResponse.contentType, "initialize-"+requestID)
	sessionID := initResponse.sessionID
	if err != nil || !validInitializeResult(initResult) {
		return mcpCallResponse{}, ErrUpstreamInvalid
	}
	notification := []byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
	notificationResponse, err := b.mcpRequest(ctx, endpoint, notification, credential, sessionID)
	if err != nil {
		return mcpCallResponse{}, err
	}
	if notificationResponse.status != http.StatusOK && notificationResponse.status != http.StatusAccepted && notificationResponse.status != http.StatusNoContent {
		return mcpCallResponse{}, ErrUpstreamInvalid
	}
	if len(bytes.TrimSpace(notificationResponse.body)) != 0 {
		return mcpCallResponse{}, ErrUpstreamInvalid
	}
	callPayload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": requestID, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": json.RawMessage(arguments)},
	})
	if err != nil || len(callPayload) > b.limits.maxMCPRequest {
		return mcpCallResponse{}, ErrInvalidRequest
	}
	callResponse, err := b.mcpRequest(ctx, endpoint, callPayload, credential, sessionID)
	if err != nil {
		return mcpCallResponse{}, err
	}
	result, _, err := decodeMCPResponse(callResponse.body, callResponse.contentType, requestID)
	if err != nil || strictjson.ValidateObject(result) != nil {
		return mcpCallResponse{}, ErrUpstreamInvalid
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(result, &fields); err != nil {
		return mcpCallResponse{}, ErrUpstreamInvalid
	}
	isError := false
	if raw, ok := fields["isError"]; ok {
		if err := json.Unmarshal(raw, &isError); err != nil {
			return mcpCallResponse{}, ErrUpstreamInvalid
		}
	}
	return mcpCallResponse{content: append(json.RawMessage(nil), result...), isError: isError}, nil
}

type mcpHTTPResponse struct {
	status      int
	body        []byte
	contentType string
	sessionID   string
}

func (b *Broker) mcpRequest(ctx context.Context, endpoint string, payload, credential []byte, sessionID string) (mcpHTTPResponse, error) {
	if len(payload) == 0 || len(payload) > b.limits.maxMCPRequest || !utf8.Valid(payload) || strictjson.Validate(payload) != nil {
		return mcpHTTPResponse{}, ErrInvalidRequest
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return mcpHTTPResponse{}, ErrUpstreamUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	request.Header.Set("User-Agent", "agents-gateway-v3")
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	if len(credential) > 0 {
		request.Header.Set("Authorization", "Bearer "+string(credential))
	}
	response, err := b.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return mcpHTTPResponse{}, ctxErr
		}
		return mcpHTTPResponse{}, ErrUpstreamUnavailable
	}
	defer response.Body.Close()
	body, readErr := readBounded(response.Body, int64(b.limits.maxMCPResponse))
	if readErr != nil {
		return mcpHTTPResponse{}, readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return mcpHTTPResponse{}, ErrUpstreamUnavailable
	}
	if encoding := strings.TrimSpace(strings.ToLower(response.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
		return mcpHTTPResponse{}, ErrUpstreamInvalid
	}
	values := response.Header.Values("Mcp-Session-Id")
	if len(values) > 1 || (len(values) == 1 && !validSessionID(values[0])) {
		return mcpHTTPResponse{}, ErrUpstreamInvalid
	}
	returnedSessionID := ""
	if len(values) == 1 {
		returnedSessionID = values[0]
	}
	media := strings.ToLower(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]))
	if len(bytes.TrimSpace(body)) == 0 {
		return mcpHTTPResponse{status: response.StatusCode, body: body, contentType: media, sessionID: returnedSessionID}, nil
	}
	if !contentTypeIs(response.Header.Get("Content-Type"), "application/json", "text/event-stream") {
		return mcpHTTPResponse{}, ErrUpstreamInvalid
	}
	return mcpHTTPResponse{status: response.StatusCode, body: body, contentType: media, sessionID: returnedSessionID}, nil
}

func validSessionID(value string) bool {
	return value != "" && len(value) <= maxSessionIDBytes && strings.TrimSpace(value) == value && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return r < 0x21 || r > 0x7e }) < 0
}

func decodeMCPResponse(body []byte, contentType, expectedID string) (json.RawMessage, string, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, "", ErrUpstreamInvalid
	}
	if contentType == "text/event-stream" {
		return decodeMCPSSE(trimmed, expectedID)
	}
	if contentType != "application/json" || strictjson.ValidateObject(trimmed) != nil {
		return nil, "", ErrUpstreamInvalid
	}
	return decodeMCPJSONRPC(trimmed, expectedID)
}

func decodeMCPSSE(body []byte, expectedID string) (json.RawMessage, string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), DefaultMaxMCPResponseBytes)
	var data bytes.Buffer
	var result json.RawMessage
	var session string
	finish := func() error {
		if data.Len() == 0 {
			return nil
		}
		candidate := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if bytes.Equal(candidate, []byte("[DONE]")) {
			return nil
		}
		decoded, decodedSession, err := decodeMCPJSONRPC(candidate, expectedID)
		if err != nil {
			return err
		}
		if result != nil {
			return ErrUpstreamInvalid
		}
		result = decoded
		if session == "" {
			session = decodedSession
		}
		return nil
	}
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			if err := finish(); err != nil {
				return nil, "", err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		parts := bytes.SplitN(line, []byte{':'}, 2)
		if !bytes.Equal(parts[0], []byte("data")) {
			continue
		}
		if data.Len() > 0 {
			data.WriteByte('\n')
		}
		if len(parts) == 2 {
			value := parts[1]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			_, _ = data.Write(value)
		}
		if data.Len() > DefaultMaxMCPResponseBytes {
			return nil, "", ErrUpstreamInvalid
		}
	}
	if scanner.Err() != nil {
		return nil, "", ErrUpstreamInvalid
	}
	if err := finish(); err != nil || result == nil {
		return nil, "", ErrUpstreamInvalid
	}
	return result, session, nil
}

func decodeMCPJSONRPC(body []byte, expectedID string) (json.RawMessage, string, error) {
	if len(body) == 0 || strictjson.ValidateObject(body) != nil {
		return nil, "", ErrUpstreamInvalid
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(body, &fields); err != nil {
		return nil, "", ErrUpstreamInvalid
	}
	for key := range fields {
		if key != "jsonrpc" && key != "id" && key != "result" && key != "error" {
			return nil, "", ErrUpstreamInvalid
		}
	}
	var version string
	if err := decodeField(fields, "jsonrpc", &version); err != nil || version != "2.0" {
		return nil, "", ErrUpstreamInvalid
	}
	var id string
	if err := decodeField(fields, "id", &id); err != nil || id != expectedID || !safeName(id, 256) {
		return nil, "", ErrUpstreamInvalid
	}
	result, hasResult := fields["result"]
	rawError, hasError := fields["error"]
	if hasResult == hasError || (hasResult && strictjson.Validate(result) != nil) {
		return nil, "", ErrUpstreamInvalid
	}
	if hasError && (strictjson.ValidateObject(rawError) != nil || len(rawError) > 16<<10) {
		return nil, "", ErrUpstreamInvalid
	}
	if hasError {
		return nil, "", ErrUpstreamInvalid
	}
	return append(json.RawMessage(nil), result...), "", nil
}

func validInitializeResult(raw []byte) bool {
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	return json.Unmarshal(raw, &result) == nil && result.ProtocolVersion == mcpProtocolVersion
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

func decodeField(fields map[string]json.RawMessage, key string, target any) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ErrUpstreamInvalid
	}
	return json.Unmarshal(raw, target)
}
