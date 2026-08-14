package broker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// OpenRouter exposes an OpenAI-compatible Responses endpoint, but it does not
// implement Codex's non-standard Responses "namespace" tool wrapper. Codex
// has an open issue for this provider capability mismatch; keep the adapter at
// the provider boundary so native OpenAI Responses traffic is not changed.
const maxProviderFunctionNameBytes = 64

var providerFunctionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type flattenedTool struct {
	namespace string
	name      string
}

type flattenedTools struct {
	byFlatName map[string]flattenedTool
}

func (m flattenedTools) empty() bool {
	return len(m.byFlatName) == 0
}

func flattenOpenRouterResponsesRequest(body []byte) ([]byte, flattenedTools, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, flattenedTools{}, ErrInvalidRequest
	}

	tools := flattenedTools{byFlatName: make(map[string]flattenedTool)}
	rawTools, ok := envelope["tools"]
	if ok {
		var toolList []json.RawMessage
		if err := json.Unmarshal(rawTools, &toolList); err != nil || len(toolList) > 256 {
			return nil, flattenedTools{}, ErrInvalidRequest
		}
		flattened := make([]json.RawMessage, 0, len(toolList))
		for _, rawTool := range toolList {
			converted, err := flattenOpenRouterTool(rawTool, &tools)
			if err != nil {
				return nil, flattenedTools{}, err
			}
			flattened = append(flattened, converted...)
		}
		envelope["tools"], _ = json.Marshal(flattened)
	}

	if tools.empty() {
		return body, tools, nil
	}
	for key, raw := range envelope {
		converted, err := flattenFunctionCallInput(raw, tools)
		if err != nil {
			return nil, flattenedTools{}, err
		}
		envelope[key] = converted
	}
	converted, err := json.Marshal(envelope)
	if err != nil {
		return nil, flattenedTools{}, ErrInvalidRequest
	}
	return converted, tools, nil
}

func flattenOpenRouterTool(raw json.RawMessage, tools *flattenedTools) ([]json.RawMessage, error) {
	var tool map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tool); err != nil || tool == nil {
		return nil, ErrInvalidRequest
	}
	typeValue, err := jsonStringField(tool, "type")
	if err != nil {
		return nil, ErrInvalidRequest
	}
	if typeValue != "namespace" {
		return []json.RawMessage{raw}, nil
	}
	namespace, err := jsonStringField(tool, "name")
	if err != nil || namespace == "" {
		return nil, ErrInvalidRequest
	}
	var nested []json.RawMessage
	if rawNested, ok := tool["tools"]; !ok || json.Unmarshal(rawNested, &nested) != nil || len(nested) == 0 || len(nested) > 256 {
		return nil, ErrInvalidRequest
	}
	converted := make([]json.RawMessage, 0, len(nested))
	for _, rawNestedTool := range nested {
		flat, err := flattenNamespaceFunction(namespace, rawNestedTool, tools)
		if err != nil {
			return nil, err
		}
		converted = append(converted, flat)
	}
	return converted, nil
}

func flattenNamespaceFunction(namespace string, raw json.RawMessage, tools *flattenedTools) (json.RawMessage, error) {
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
		return nil, ErrInvalidRequest
	}
	if typeValue, err := jsonStringField(nested, "type"); err != nil || typeValue != "function" {
		return nil, ErrInvalidRequest
	}

	// Responses namespace tools use the function fields at the same level as
	// type/name. Accepting the Chat Completions wrapper as well makes the
	// boundary tolerant of a gateway that already normalized MCP once.
	if rawFunction, ok := nested["function"]; ok {
		var functionFields map[string]json.RawMessage
		if err := json.Unmarshal(rawFunction, &functionFields); err != nil || functionFields == nil {
			return nil, ErrInvalidRequest
		}
		for key, value := range functionFields {
			nested[key] = value
		}
		delete(nested, "function")
	}
	name, err := jsonStringField(nested, "name")
	if err != nil || name == "" {
		return nil, ErrInvalidRequest
	}
	flatName := canonicalProviderFunctionName(namespace, name)
	if previous, exists := tools.byFlatName[flatName]; exists && (previous.namespace != namespace || previous.name != name) {
		return nil, fmt.Errorf("%w: provider function name collision", ErrInvalidRequest)
	}
	tools.byFlatName[flatName] = flattenedTool{namespace: namespace, name: name}
	nested["type"], _ = json.Marshal("function")
	nested["name"], _ = json.Marshal(flatName)
	delete(nested, "namespace")
	converted, err := json.Marshal(nested)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	return converted, nil
}

func canonicalProviderFunctionName(namespace, name string) string {
	candidate := namespace + "__" + name
	if len(candidate) <= maxProviderFunctionNameBytes && providerFunctionNamePattern.MatchString(candidate) {
		return candidate
	}
	digest := sha256.Sum256([]byte(namespace + "\x00" + name))
	prefix := "mcp_" + hex.EncodeToString(digest[:20])
	suffix := sanitizeProviderFunctionSuffix(name)
	remaining := maxProviderFunctionNameBytes - len(prefix) - 1
	if len(suffix) > remaining {
		suffix = suffix[:remaining]
	}
	if suffix == "" {
		return prefix
	}
	return prefix + "_" + suffix
}

func sanitizeProviderFunctionSuffix(value string) string {
	var result strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			result.WriteRune(r)
		} else {
			result.WriteByte('_')
		}
	}
	return result.String()
}

func flattenFunctionCallInput(raw json.RawMessage, tools flattenedTools) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return raw, nil
	}
	if trimmed[0] == '[' {
		var values []json.RawMessage
		if json.Unmarshal(trimmed, &values) != nil {
			return raw, nil
		}
		for index, value := range values {
			converted, err := flattenFunctionCallInput(value, tools)
			if err != nil {
				return nil, err
			}
			values[index] = converted
		}
		return json.Marshal(values)
	}
	if trimmed[0] != '{' {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(trimmed, &object) != nil || object == nil {
		return raw, nil
	}
	if typeValue, _ := jsonStringField(object, "type"); typeValue == "function_call" {
		if namespace, ok := jsonStringField(object, "namespace"); ok == nil && namespace != "" {
			name, nameErr := jsonStringField(object, "name")
			if nameErr != nil || name == "" {
				return nil, ErrInvalidRequest
			}
			flatName := canonicalProviderFunctionName(namespace, name)
			if expected, exists := tools.byFlatName[flatName]; !exists || expected.namespace != namespace || expected.name != name {
				return nil, ErrInvalidRequest
			}
			object["name"], _ = json.Marshal(flatName)
			delete(object, "namespace")
		}
	}
	for key, value := range object {
		converted, err := flattenFunctionCallInput(value, tools)
		if err != nil {
			return nil, err
		}
		object[key] = converted
	}
	return json.Marshal(object)
}

func restoreOpenRouterResponses(body []byte, contentType string, tools flattenedTools) ([]byte, error) {
	if tools.empty() {
		return body, nil
	}
	if contentType == "application/json" {
		return restoreFunctionCalls(body, tools)
	}
	if contentType != "text/event-stream" {
		return nil, ErrUpstreamInvalid
	}
	return restoreFunctionCallsSSE(body, tools)
}

func restoreFunctionCalls(body []byte, tools flattenedTools) ([]byte, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, ErrUpstreamInvalid
	}
	value = restoreFunctionCallValue(value, tools)
	converted, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUpstreamInvalid
	}
	return converted, nil
}

func restoreFunctionCallValue(value any, tools flattenedTools) any {
	switch current := value.(type) {
	case []any:
		for index := range current {
			current[index] = restoreFunctionCallValue(current[index], tools)
		}
	case map[string]any:
		if current["type"] == "function_call" {
			if flatName, ok := current["name"].(string); ok {
				if tool, exists := tools.byFlatName[flatName]; exists {
					current["namespace"] = tool.namespace
					current["name"] = tool.name
				}
			}
		}
		for key, child := range current {
			current[key] = restoreFunctionCallValue(child, tools)
		}
	}
	return value
}

func restoreFunctionCallsSSE(body []byte, tools flattenedTools) ([]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), len(body)+1)
	var output bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			prefix := "data:"
			value := strings.TrimPrefix(line, prefix)
			leadingSpace := ""
			if strings.HasPrefix(value, " ") {
				leadingSpace = " "
				value = value[1:]
			}
			trimmed := strings.TrimSpace(value)
			if trimmed != "" && trimmed != "[DONE]" && json.Valid([]byte(trimmed)) {
				converted, err := restoreFunctionCalls([]byte(trimmed), tools)
				if err != nil {
					return nil, err
				}
				value = leadingSpace + string(converted)
			} else {
				value = leadingSpace + value
			}
			output.WriteString(prefix)
			output.WriteString(value)
		} else {
			output.WriteString(line)
		}
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, ErrUpstreamInvalid
	}
	return output.Bytes(), nil
}

func jsonStringField(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", errors.New("missing string field")
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", errors.New("invalid string field")
	}
	return value, nil
}
