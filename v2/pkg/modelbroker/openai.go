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

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const maxProviderResponse = 8 << 20

type OpenAICompatible struct {
	endpoint *url.URL
	client   *http.Client
}

func NewOpenAICompatible(endpoint string, client *http.Client) (*OpenAICompatible, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")) {
		return nil, errors.New("provider endpoint must use HTTPS (HTTP is allowed only for loopback tests)")
	}
	if parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("provider endpoint contains forbidden URL components")
	}
	if client == nil {
		client = secureHTTPClient(2 * time.Minute)
	}
	return &OpenAICompatible{endpoint: parsed, client: client}, nil
}

func secureHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: otelhttp.NewTransport(transport),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("provider redirects are forbidden")
		},
	}
}

func (p *OpenAICompatible) Invoke(ctx context.Context, request Request, credential []byte) (Response, error) {
	if len(credential) == 0 {
		return Response{}, errors.New("empty provider credential")
	}
	messages := make([]map[string]string, 0, len(request.Messages))
	for _, m := range request.Messages {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
	}
	payload := map[string]any{"model": request.Model, "messages": messages}
	if request.MaxTokens > 0 {
		payload["max_tokens"] = request.MaxTokens
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, err
	}
	endpoint := *p.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/chat/completions"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+string(credential))
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "agents-gateway-v2")
	httpResponse, err := p.client.Do(httpRequest)
	if err != nil {
		return Response{}, err
	}
	defer httpResponse.Body.Close()
	limited := io.LimitReader(httpResponse.Body, maxProviderResponse+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return Response{}, err
	}
	if len(responseBody) > maxProviderResponse {
		return Response{}, errors.New("provider response exceeded size limit")
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{}, fmt.Errorf("provider returned HTTP %d", httpResponse.StatusCode)
	}
	var decoded struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return Response{}, errors.New("provider returned invalid JSON")
	}
	if len(decoded.Choices) == 0 {
		return Response{}, errors.New("provider returned no choices")
	}
	return Response{Model: decoded.Model, Content: decoded.Choices[0].Message.Content, FinishReason: decoded.Choices[0].FinishReason, InputTokens: decoded.Usage.Prompt, OutputTokens: decoded.Usage.Completion}, nil
}
