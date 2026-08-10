package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
	"gopkg.in/yaml.v3"
)

type toolGrantJSON struct {
	Name        string          `json:"name"`
	Resources   []string        `json:"resources,omitempty"`
	Effect      string          `json:"effect,omitempty"`
	Approval    string          `json:"approval,omitempty"`
	Arguments   json.RawMessage `json:"arguments"`
	InputSchema map[string]any  `json:"inputSchema,omitempty"`
}

func (g *ToolGrant) UnmarshalJSON(raw []byte) error {
	if g == nil {
		return errors.New("tool grant destination is nil")
	}
	if strictjson.ValidateObject(raw) != nil {
		return errors.New("tool grant must be a JSON object")
	}
	var wire toolGrantJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	decoded := ToolGrant{
		Name: wire.Name, Resources: wire.Resources, Effect: wire.Effect,
		Approval: wire.Approval, InputSchema: wire.InputSchema,
	}
	if wire.Arguments != nil {
		if bytes.Equal(bytes.TrimSpace(wire.Arguments), []byte("null")) {
			return errors.New("tool grant arguments must be a JSON object")
		}
		arguments, err := NewJSONArguments(wire.Arguments)
		if err != nil {
			return errors.New("tool grant arguments must be a JSON object")
		}
		decoded.Arguments = arguments
	}
	*g = decoded
	return nil
}

func (g *ToolGrant) UnmarshalYAML(node *yaml.Node) error {
	if g == nil {
		return errors.New("tool grant destination is nil")
	}
	if node == nil || node.Kind != yaml.MappingNode || len(node.Content)%2 != 0 {
		return errors.New("tool grant must be a YAML object")
	}
	allowed := map[string]struct{}{
		"name": {}, "resources": {}, "effect": {}, "approval": {},
		"arguments": {}, "inputSchema": {},
	}
	seen := make(map[string]struct{}, len(node.Content)/2)
	argumentsPresent := false
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return errors.New("tool grant keys must be strings")
		}
		if _, exists := allowed[key.Value]; !exists {
			return fmt.Errorf("field %s not found in type spec.ToolGrant", key.Value)
		}
		if _, exists := seen[key.Value]; exists {
			return errors.New("tool grant contains a duplicate field")
		}
		seen[key.Value] = struct{}{}
		if key.Value == "arguments" {
			argumentsPresent = true
			if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
				return errors.New("tool grant arguments must be a JSON object")
			}
		}
	}
	type plainToolGrant ToolGrant
	var decoded plainToolGrant
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	if argumentsPresent && decoded.Arguments == nil {
		return errors.New("tool grant arguments must be a JSON object")
	}
	*g = ToolGrant(decoded)
	return nil
}
