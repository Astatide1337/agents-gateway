package modelbroker

import (
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
)

type Anthropic struct {
	endpoint *url.URL
	client   *http.Client
}

func NewAnthropic(endpoint string, client *http.Client) (*Anthropic, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("invalid Anthropic endpoint")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return nil, errors.New("Anthropic endpoint must use HTTPS (HTTP loopback is allowed for tests)")
	}
	if parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Anthropic endpoint contains forbidden URL components")
	}
	if client == nil {
		client = secureHTTPClient(2 * time.Minute)
	}
	return &Anthropic{endpoint: parsed, client: client}, nil
}

func (p *Anthropic) Invoke(ctx context.Context, request Request, credential []byte) (Response, error) {
	if len(credential) == 0 {
		return Response{}, errors.New("empty provider credential")
	}
	messages := make([]Message, 0, len(request.Messages))
	var system []string
	for _, message := range request.Messages {
		if message.Role == "system" || message.Role == "developer" {
			system = append(system, message.Content)
			continue
		}
		if message.Role != "user" && message.Role != "assistant" {
			return Response{}, fmt.Errorf("unsupported Anthropic message role %q", message.Role)
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return Response{}, errors.New("Anthropic request requires a user or assistant message")
	}
	maxTokens := request.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	payload := map[string]any{"model": request.Model, "messages": messages, "max_tokens": maxTokens}
	if len(system) > 0 {
		payload["system"] = strings.Join(system, "\n\n")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, errors.New("encode Anthropic request")
	}
	endpoint := *p.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/messages"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("create Anthropic request")
	}
	httpRequest.Header.Set("x-api-key", string(credential))
	httpRequest.Header.Set("anthropic-version", "2023-06-01")
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("user-agent", "agents-gateway-v2")
	httpResponse, err := p.client.Do(httpRequest)
	if err != nil {
		return Response{}, err
	}
	defer httpResponse.Body.Close()
	data, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxProviderResponse+1))
	if err != nil || len(data) > maxProviderResponse {
		return Response{}, errors.New("Anthropic response could not be read safely")
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{}, fmt.Errorf("Anthropic returned HTTP %d", httpResponse.StatusCode)
	}
	var decoded struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&decoded); err != nil {
		return Response{}, errors.New("Anthropic returned invalid JSON")
	}
	var text []string
	for _, block := range decoded.Content {
		if block.Type == "text" {
			text = append(text, block.Text)
		}
	}
	if len(text) == 0 {
		return Response{}, errors.New("Anthropic returned no text content")
	}
	return Response{Model: decoded.Model, Content: strings.Join(text, ""), FinishReason: decoded.StopReason, InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens}, nil
}
