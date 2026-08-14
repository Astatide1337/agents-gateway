package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

// ModelResponse is the bounded result of one OpenAI-compatible Responses
// request. Body is copied and contains no provider credential.
type ModelResponse struct {
	Body         []byte
	ContentType  string
	Provider     string
	Model        string
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
}

type modelReservation struct {
	provider       compiledProvider
	inputEstimate  int64
	outputEstimate int64
	costEstimate   int64
}

type modelWire uint8

const (
	modelWireResponses modelWire = iota + 1
	modelWireAnthropicMessages
)

// InvokeModel forwards a validated OpenAI Responses request to the selected
// allowlisted provider. It is the direct seam used by tests and host-side
// integrations; ServeHTTP exposes the same operation on loopback /v1.
func (b *Broker) InvokeModel(ctx context.Context, body []byte) (ModelResponse, error) {
	return b.invokeModel(ctx, body, nil)
}

func (b *Broker) invokeModel(ctx context.Context, body []byte, incoming http.Header) (ModelResponse, error) {
	return b.invokeModelWire(ctx, body, incoming, modelWireResponses)
}

// InvokeAnthropicModel forwards one validated Anthropic Messages request. The
// method is intentionally separate from InvokeModel so a caller cannot
// accidentally send a Claude request through the Responses route or make a
// provider kind silently change wire protocols.
func (b *Broker) InvokeAnthropicModel(ctx context.Context, body []byte) (ModelResponse, error) {
	return b.invokeModelWire(ctx, body, nil, modelWireAnthropicMessages)
}

func (b *Broker) invokeAnthropicModel(ctx context.Context, body []byte, incoming http.Header) (ModelResponse, error) {
	return b.invokeModelWire(ctx, body, incoming, modelWireAnthropicMessages)
}

func (b *Broker) invokeModelWire(ctx context.Context, body []byte, incoming http.Header, wire modelWire) (ModelResponse, error) {
	var zero ModelResponse
	if b == nil || ctx == nil || len(body) == 0 || int64(len(body)) > b.limits.maxModelRequest || strictjson.ValidateObject(body) != nil {
		return zero, ErrInvalidRequest
	}
	model, maxOutput, err := requestForWire(body, wire)
	if err != nil {
		return zero, ErrInvalidRequest
	}
	provider, ok := b.providerForModel(model)
	if !ok || !providerSupportsWire(provider, wire) {
		return zero, ErrDenied
	}
	upstreamBody := body
	var flattened flattenedTools
	reservation, err := b.reserveModel(ctx, provider, body, maxOutput)
	if err != nil {
		return zero, err
	}
	// Harnesses commonly send a very large output allowance (or omit it),
	// while the broker owns the actual per-run token ceiling. Bound the
	// provider request to the reservation so a client-side default cannot make
	// every otherwise-valid request fail the budget check or bypass the cap.
	upstreamBody, err = constrainModelOutput(body, wire, reservation.outputEstimate)
	if err != nil {
		return zero, ErrInvalidRequest
	}
	if wire == modelWireResponses && provider.kind == "openrouter-responses" {
		upstreamBody, flattened, err = flattenOpenRouterResponsesRequest(upstreamBody)
		if err != nil {
			return zero, err
		}
	}
	finalize := true
	defer func() {
		if finalize {
			b.finishModel(reservation, 0, 0, 0)
		}
	}()
	credential, err := b.resolveCredential(ctx, provider.credentialRef)
	if err != nil {
		return zero, err
	}
	defer zeroBytes(credential)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.endpoint, bytes.NewReader(upstreamBody))
	if err != nil {
		return zero, ErrUpstreamUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("User-Agent", "agents-gateway-v3")
	if wire == modelWireAnthropicMessages {
		copyAnthropicRequestHeaders(request.Header, incoming)
		if provider.kind == "anthropic-messages" {
			if request.Header.Get("anthropic-version") == "" {
				request.Header.Set("anthropic-version", "2023-06-01")
			}
			if len(credential) > 0 {
				request.Header.Set("x-api-key", string(credential))
			}
		} else if len(credential) > 0 {
			request.Header.Set("Authorization", "Bearer "+string(credential))
		}
	} else {
		copyModelRequestHeaders(request.Header, incoming)
		if len(credential) > 0 {
			request.Header.Set("Authorization", "Bearer "+string(credential))
		}
	}
	response, err := b.client.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, ErrUpstreamUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return zero, ErrUpstreamUnavailable
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]))
	if !contentTypeIs(response.Header.Get("Content-Type"), "application/json", "text/event-stream") {
		return zero, ErrUpstreamInvalid
	}
	result, err := readBounded(response.Body, b.limits.maxModelResponse)
	if err != nil || !utf8Bytes(result) {
		if err == nil {
			err = ErrUpstreamInvalid
		}
		return zero, err
	}
	if contentType == "application/json" && strictjson.ValidateObject(result) != nil {
		return zero, ErrUpstreamInvalid
	}
	if wire == modelWireResponses && provider.kind == "openrouter-responses" {
		result, err = restoreOpenRouterResponses(result, contentType, flattened)
		if err != nil {
			return zero, err
		}
	}
	inputTokens, outputTokens, inputFound, outputFound := extractUsageForProvider(result, contentType, provider.kind)
	if !inputFound {
		inputTokens = reservation.inputEstimate
	}
	if !outputFound {
		outputTokens = tokenEstimate(len(result))
	}
	if inputTokens < 0 || outputTokens < 0 || inputTokens > DefaultMaxModelTokens*1024 || outputTokens > DefaultMaxModelTokens*1024 {
		return zero, ErrUpstreamInvalid
	}
	cost, costErr := b.estimateCost(ctx, provider, inputTokens, outputTokens)
	if costErr != nil {
		return zero, ErrBudgetExceeded
	}
	if !b.finishModel(reservation, inputTokens, outputTokens, cost) {
		finalize = false
		return zero, ErrBudgetExceeded
	}
	finalize = false
	return ModelResponse{Body: append([]byte(nil), result...), ContentType: contentType, Provider: provider.name, Model: model, InputTokens: inputTokens, OutputTokens: outputTokens, CostMicros: cost}, nil
}

func providerSupportsWire(provider compiledProvider, wire modelWire) bool {
	switch wire {
	case modelWireResponses:
		return provider.kind == "openrouter-responses" || provider.kind == "openai-responses"
	case modelWireAnthropicMessages:
		return provider.kind == "openrouter-anthropic-messages" || provider.kind == "anthropic-messages"
	default:
		return false
	}
}

func (b *Broker) providerForModel(model string) (compiledProvider, bool) {
	for _, provider := range b.providers {
		if provider.model == model {
			return provider, true
		}
	}
	return compiledProvider{}, false
}

func (b *Broker) reserveModel(ctx context.Context, provider compiledProvider, body []byte, requestedMaxOutput int64) (modelReservation, error) {
	if err := ctx.Err(); err != nil {
		return modelReservation{}, err
	}
	inputEstimate := tokenEstimate(len(body))
	if inputEstimate < 1 {
		inputEstimate = 1
	}
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	if b.budget.modelRequests >= b.budget.maxRequests {
		return modelReservation{}, ErrBudgetExceeded
	}
	remaining := b.budget.maxTokens - b.budget.modelTokens - b.budget.reservedTokens
	if remaining <= inputEstimate {
		return modelReservation{}, ErrBudgetExceeded
	}
	outputEstimate := remaining - inputEstimate
	if outputEstimate > 128<<10 {
		outputEstimate = 128 << 10
	}
	if requestedMaxOutput > 0 && requestedMaxOutput < outputEstimate {
		outputEstimate = requestedMaxOutput
	}
	if outputEstimate < 1 {
		return modelReservation{}, ErrBudgetExceeded
	}
	costEstimate, err := b.estimateCostLocked(ctx, provider, inputEstimate, outputEstimate)
	if err != nil {
		return modelReservation{}, ErrBudgetExceeded
	}
	if costEstimate > b.budget.maxCostMicros-b.budget.modelCostMicros-b.budget.reservedCost {
		return modelReservation{}, ErrBudgetExceeded
	}
	b.budget.modelRequests++
	b.budget.reservedTokens += inputEstimate + outputEstimate
	b.budget.reservedCost += costEstimate
	return modelReservation{provider: provider, inputEstimate: inputEstimate, outputEstimate: outputEstimate, costEstimate: costEstimate}, nil
}

// constrainModelOutput rewrites only the wire-specific output limit. The
// broker must enforce its reservation even when a harness sends an excessive
// max_output_tokens/max_tokens value or omits the field entirely.
func constrainModelOutput(body []byte, wire modelWire, limit int64) ([]byte, error) {
	if len(body) == 0 || limit < 1 || strictjson.ValidateObject(body) != nil {
		return nil, ErrInvalidRequest
	}
	field := "max_output_tokens"
	if wire == modelWireAnthropicMessages {
		field = "max_tokens"
	} else if wire != modelWireResponses {
		return nil, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(body, &fields); err != nil {
		return nil, ErrInvalidRequest
	}
	var requested int64
	if raw, ok := fields[field]; ok {
		if err := json.Unmarshal(raw, &requested); err != nil || requested < 1 {
			return nil, ErrInvalidRequest
		}
		if requested <= limit {
			return body, nil
		}
	}
	fields[field] = json.RawMessage(strconv.FormatInt(limit, 10))
	return json.Marshal(fields)
}

func (b *Broker) finishModel(reservation modelReservation, inputTokens, outputTokens, cost int64) bool {
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	reservedTokens := reservation.inputEstimate + reservation.outputEstimate
	if b.budget.reservedTokens >= reservedTokens {
		b.budget.reservedTokens -= reservedTokens
	} else {
		b.budget.reservedTokens = 0
	}
	if b.budget.reservedCost >= reservation.costEstimate {
		b.budget.reservedCost -= reservation.costEstimate
	} else {
		b.budget.reservedCost = 0
	}
	if inputTokens < 0 || outputTokens < 0 || cost < 0 || inputTokens > (1<<62)-outputTokens || b.budget.modelTokens > (1<<62)-inputTokens-outputTokens || b.budget.modelCostMicros > (1<<62)-cost {
		return false
	}
	b.budget.modelTokens += inputTokens + outputTokens
	b.budget.modelCostMicros += cost
	return b.budget.modelTokens <= b.budget.maxTokens && b.budget.modelCostMicros <= b.budget.maxCostMicros
}

func (b *Broker) estimateCost(ctx context.Context, provider compiledProvider, inputTokens, outputTokens int64) (int64, error) {
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	return b.estimateCostLocked(ctx, provider, inputTokens, outputTokens)
}

func (b *Broker) estimateCostLocked(ctx context.Context, provider compiledProvider, inputTokens, outputTokens int64) (int64, error) {
	if inputTokens < 0 || outputTokens < 0 {
		return 0, nil
	}
	// A zero-dollar route is an explicit free-provider contract. Pricing is
	// optional for that contract, so token accounting must not turn every
	// non-empty request into a budget failure before it reaches the provider.
	// Paid routes require a positive cap and a pricing table at construction.
	if b.budget.maxCostMicros == 0 {
		return 0, nil
	}
	if b.pricing == nil {
		return 0, ErrInvalidConfig
	}
	value, err := b.pricing.EstimateCost(ctx, provider.name, provider.model, inputTokens, outputTokens)
	if err != nil || value < 0 || value > (1<<62) {
		return 0, ErrBudgetExceeded
	}
	return value, nil
}

func requestModel(body []byte) (string, int64, error) {
	if strictjson.ValidateObject(body) != nil {
		return "", 0, ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := decodeStrictObject(body, &fields); err != nil {
		return "", 0, ErrInvalidRequest
	}
	var model string
	if err := decodeField(fields, "model", &model); err != nil || !safeModel(model) {
		return "", 0, ErrInvalidRequest
	}
	var maxOutput int64
	if raw, ok := fields["max_output_tokens"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &maxOutput) != nil || maxOutput < 0 || maxOutput > MaxConfiguredModelTokens {
			return "", 0, ErrInvalidRequest
		}
	}
	return model, maxOutput, nil
}

func requestForWire(body []byte, wire modelWire) (string, int64, error) {
	switch wire {
	case modelWireResponses:
		return requestModel(body)
	case modelWireAnthropicMessages:
		return requestAnthropicModel(body)
	default:
		return "", 0, ErrInvalidRequest
	}
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

type anthropicMessagesRequest struct {
	Model             string             `json:"model"`
	Messages          []anthropicMessage `json:"messages"`
	MaxTokens         int64              `json:"max_tokens"`
	System            json.RawMessage    `json:"system,omitempty"`
	StopSequences     []string           `json:"stop_sequences,omitempty"`
	Stream            bool               `json:"stream,omitempty"`
	Temperature       *float64           `json:"temperature,omitempty"`
	TopK              *int64             `json:"top_k,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	Metadata          json.RawMessage    `json:"metadata,omitempty"`
	Tools             []json.RawMessage  `json:"tools,omitempty"`
	ToolChoice        json.RawMessage    `json:"tool_choice,omitempty"`
	Thinking          json.RawMessage    `json:"thinking,omitempty"`
	Container         json.RawMessage    `json:"container,omitempty"`
	ContextManagement json.RawMessage    `json:"context_management,omitempty"`
	OutputConfig      json.RawMessage    `json:"output_config,omitempty"`
	ServiceTier       string             `json:"service_tier,omitempty"`
}

func requestAnthropicModel(body []byte) (string, int64, error) {
	if strictjson.ValidateObject(body) != nil {
		return "", 0, ErrInvalidRequest
	}
	var request anthropicMessagesRequest
	if err := decodeStrictObject(body, &request); err != nil || !safeModel(request.Model) || request.MaxTokens < 1 || request.MaxTokens > MaxConfiguredModelTokens || len(request.Messages) == 0 || len(request.Messages) > 1024 {
		return "", 0, ErrInvalidRequest
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 1) {
		return "", 0, ErrInvalidRequest
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return "", 0, ErrInvalidRequest
	}
	if request.TopK != nil && (*request.TopK < 0 || *request.TopK > MaxConfiguredModelTokens) {
		return "", 0, ErrInvalidRequest
	}
	if len(request.StopSequences) > 16 {
		return "", 0, ErrInvalidRequest
	}
	for _, stop := range request.StopSequences {
		if len(stop) > 4096 || !utf8.ValidString(stop) || strings.ContainsAny(stop, "\x00\r\n") {
			return "", 0, ErrInvalidRequest
		}
	}
	for _, message := range request.Messages {
		if message.Role != "user" && message.Role != "assistant" || len(message.Name) > 128 || !utf8.ValidString(message.Name) || strings.ContainsAny(message.Name, "\x00\r\n") || !validAnthropicContent(message.Content) {
			return "", 0, ErrInvalidRequest
		}
	}
	if len(request.System) > 0 && !validAnthropicContent(request.System) {
		return "", 0, ErrInvalidRequest
	}
	if len(request.Metadata) > 0 && !validAnthropicObject(request.Metadata) {
		return "", 0, ErrInvalidRequest
	}
	for _, raw := range []json.RawMessage{request.ToolChoice, request.Thinking, request.Container, request.ContextManagement, request.OutputConfig} {
		if len(raw) > 0 && !validAnthropicObjectOrString(raw) {
			return "", 0, ErrInvalidRequest
		}
	}
	if len(request.Tools) > 256 {
		return "", 0, ErrInvalidRequest
	}
	for _, tool := range request.Tools {
		if len(tool) == 0 || strictjson.ValidateObject(tool) != nil || len(tool) > 64<<10 {
			return "", 0, ErrInvalidRequest
		}
	}
	if len(request.System) > 256<<10 || len(request.Metadata) > 256<<10 || len(request.ToolChoice) > 64<<10 || len(request.Thinking) > 64<<10 || len(request.Container) > 64<<10 || len(request.ContextManagement) > 64<<10 || len(request.OutputConfig) > 64<<10 {
		return "", 0, ErrInvalidRequest
	}
	return request.Model, request.MaxTokens, nil
}

func validAnthropicContent(raw json.RawMessage) bool {
	if len(raw) == 0 || strictjson.Validate(raw) != nil {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return utf8.ValidString(text) && len(text) <= 256<<10 && !strings.ContainsRune(text, '\x00')
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 || len(blocks) > 1024 {
		return false
	}
	for _, block := range blocks {
		if len(block) == 0 || len(block) > 256<<10 || strictjson.ValidateObject(block) != nil {
			return false
		}
	}
	return true
}

func validAnthropicObject(raw json.RawMessage) bool {
	return len(raw) > 0 && strictjson.ValidateObject(raw) == nil
}

func validAnthropicObjectOrString(raw json.RawMessage) bool {
	if len(raw) == 0 || strictjson.Validate(raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	if validAnthropicObject(raw) {
		return true
	}
	var text string
	return json.Unmarshal(raw, &text) == nil && utf8.ValidString(text) && len(text) <= 64<<10
}

func tokenEstimate(bytesCount int) int64 {
	if bytesCount <= 0 {
		return 0
	}
	return int64((bytesCount + 3) / 4)
}

func extractUsage(body []byte, contentType string) (int64, int64, bool) {
	if contentType == "application/json" {
		return usageFromJSON(body)
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), DefaultMaxModelResponseBytes)
	var input, output int64
	var found bool
	var data bytes.Buffer
	consume := func() {
		candidate := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(candidate) == 0 || bytes.Equal(candidate, []byte("[DONE]")) || strictjson.ValidateObject(candidate) != nil {
			return
		}
		in, out, ok := usageFromJSON(candidate)
		if !ok {
			var event struct {
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(candidate, &event) == nil && len(event.Response) > 0 {
				in, out, ok = usageFromJSON(event.Response)
			}
		}
		if ok {
			input, output, found = in, out, true
		}
	}
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			consume()
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			_, _ = data.Write(value)
		}
	}
	consume()
	return input, output, found && input >= 0 && output >= 0
}

func extractUsageForProvider(body []byte, contentType, providerKind string) (int64, int64, bool, bool) {
	if providerKind != "anthropic-messages" && providerKind != "openrouter-anthropic-messages" {
		input, output, found := extractUsage(body, contentType)
		return input, output, found, found
	}
	if contentType == "application/json" {
		return anthropicUsageFromJSON(body)
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), DefaultMaxModelResponseBytes)
	var input, output int64
	var inputFound, outputFound bool
	var data bytes.Buffer
	consume := func() {
		candidate := bytes.TrimSpace(data.Bytes())
		data.Reset()
		if len(candidate) == 0 || bytes.Equal(candidate, []byte("[DONE]")) {
			return
		}
		in, out, inOK, outOK := anthropicUsageFromEvent(candidate)
		if inOK {
			input, inputFound = in, true
		}
		if outOK {
			output, outputFound = out, true
		}
	}
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			consume()
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			_, _ = data.Write(value)
		}
	}
	consume()
	return input, output, inputFound, outputFound
}

func anthropicUsageFromJSON(body []byte) (int64, int64, bool, bool) {
	var event map[string]json.RawMessage
	if json.Unmarshal(body, &event) != nil {
		return 0, 0, false, false
	}
	return anthropicUsageFromFields(event)
}

func anthropicUsageFromEvent(body []byte) (int64, int64, bool, bool) {
	var event map[string]json.RawMessage
	if json.Unmarshal(body, &event) != nil {
		return 0, 0, false, false
	}
	input, output, inputFound, outputFound := anthropicUsageFromFields(event)
	if raw, ok := event["message"]; ok && strictjson.ValidateObject(raw) == nil {
		var message map[string]json.RawMessage
		if json.Unmarshal(raw, &message) == nil {
			in, out, inOK, outOK := anthropicUsageFromFields(message)
			if inOK {
				input, inputFound = in, true
			}
			if outOK {
				output, outputFound = out, true
			}
		}
	}
	return input, output, inputFound, outputFound
}

func anthropicUsageFromFields(fields map[string]json.RawMessage) (int64, int64, bool, bool) {
	raw, ok := fields["usage"]
	if !ok || strictjson.ValidateObject(raw) != nil {
		return 0, 0, false, false
	}
	var usage struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return 0, 0, false, false
	}
	inputOK := usage.InputTokens != nil && *usage.InputTokens >= 0
	outputOK := usage.OutputTokens != nil && *usage.OutputTokens >= 0
	var input, output int64
	if inputOK {
		input = *usage.InputTokens
	}
	if outputOK {
		output = *usage.OutputTokens
	}
	return input, output, inputOK, outputOK
}

func usageFromJSON(body []byte) (int64, int64, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return 0, 0, false
	}
	raw, ok := envelope["usage"]
	if !ok {
		return 0, 0, false
	}
	var usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &usage) != nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return 0, 0, false
	}
	return usage.InputTokens, usage.OutputTokens, true
}

func copyModelRequestHeaders(dst, src http.Header) {
	if src == nil {
		return
	}
	for _, name := range []string{"Accept", "Idempotency-Key", "OpenAI-Beta", "X-Client-Request-Id"} {
		for _, value := range src.Values(name) {
			if len(value) <= 1024 && !strings.ContainsAny(value, "\r\n") {
				dst.Add(name, value)
			}
		}
	}
}

func copyAnthropicRequestHeaders(dst, src http.Header) {
	if src == nil {
		return
	}
	for _, name := range []string{"Accept", "Idempotency-Key", "anthropic-version", "anthropic-beta", "X-Client-Request-Id"} {
		for _, value := range src.Values(name) {
			if len(value) <= 1024 && !strings.ContainsAny(value, "\r\n") {
				dst.Add(name, value)
			}
		}
	}
}

// ServeHTTP exposes only the Codex-compatible Responses route. It is safe to
// mount on a listener only when the listener itself is loopback; the request
// check below adds a second defense against accidental publication.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if b == nil || r == nil || !isLoopbackRequest(r) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" {
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
	body, err := readBounded(r.Body, b.limits.maxModelRequest)
	if err != nil {
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	result, err := b.invokeModel(r.Context(), body, r.Header)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeBrokerHTTPError(w, modelHTTPStatus(err))
		return
	}
	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Body)
}

// ServeAnthropicHTTP exposes the native Anthropic Messages path used by
// Claude Code. It is mounted below /api by the command router for OpenRouter
// routes and at /v1/messages for direct Anthropic routes.
func (b *Broker) ServeAnthropicHTTP(w http.ResponseWriter, r *http.Request) {
	if b == nil || r == nil || !isLoopbackRequest(r) {
		writeBrokerHTTPError(w, http.StatusNotFound)
		return
	}
	if (r.URL.Path != "/v1/messages" && r.URL.Path != "/api/v1/messages") || r.URL.RawQuery != "" {
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
	body, err := readBounded(r.Body, b.limits.maxModelRequest)
	if err != nil {
		writeBrokerHTTPError(w, http.StatusRequestEntityTooLarge)
		return
	}
	if err := b.validateAnthropicPath(body, r.URL.Path); err != nil {
		writeBrokerHTTPError(w, modelHTTPStatus(err))
		return
	}
	result, err := b.invokeAnthropicModel(r.Context(), body, r.Header)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		writeBrokerHTTPError(w, modelHTTPStatus(err))
		return
	}
	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Body)
}

func (b *Broker) validateAnthropicPath(body []byte, path string) error {
	model, _, err := requestAnthropicModel(body)
	if err != nil {
		return ErrInvalidRequest
	}
	provider, ok := b.providerForModel(model)
	if !ok || !providerSupportsWire(provider, modelWireAnthropicMessages) {
		return ErrDenied
	}
	if path == "/api/v1/messages" && provider.kind != "openrouter-anthropic-messages" {
		return ErrDenied
	}
	if path == "/v1/messages" && provider.kind != "anthropic-messages" {
		return ErrDenied
	}
	return nil
}

func modelHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		return http.StatusBadRequest
	case errors.Is(err, ErrDenied), errors.Is(err, ErrBudgetExceeded), errors.Is(err, ErrCredentialUnavailable):
		return http.StatusForbidden
	default:
		return http.StatusBadGateway
	}
}

func utf8Bytes(body []byte) bool {
	return utf8.Valid(body)
}

func isLoopbackRequest(r *http.Request) bool {
	if r == nil || r.RemoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeBrokerHTTPError(w http.ResponseWriter, status int) {
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"request rejected"}`)
}
