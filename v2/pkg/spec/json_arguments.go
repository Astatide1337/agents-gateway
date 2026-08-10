package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
	"gopkg.in/yaml.v3"
)

const maxJSONArgumentsYAMLDepth = 100

// JSONArguments preserves the original JSON number lexemes used by an exact
// ToolGrant argument constraint. The wire value is always a JSON object; the
// pointer on ToolGrant distinguishes an absent constraint from an exact {}.
type JSONArguments struct {
	raw json.RawMessage
}

func NewJSONArguments(raw json.RawMessage) (*JSONArguments, error) {
	var arguments JSONArguments
	if err := arguments.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &arguments, nil
}

func (a JSONArguments) Raw() json.RawMessage {
	return append(json.RawMessage(nil), a.raw...)
}

func (a JSONArguments) MarshalJSON() ([]byte, error) {
	if strictjson.ValidateObject(a.raw) != nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	return append([]byte(nil), a.raw...), nil
}

func (a *JSONArguments) UnmarshalJSON(raw []byte) error {
	if a == nil {
		return errors.New("arguments destination is nil")
	}
	if strictjson.ValidateObject(raw) != nil {
		return errors.New("arguments must be a JSON object")
	}
	a.raw = append(a.raw[:0], raw...)
	return nil
}

func (a *JSONArguments) UnmarshalYAML(node *yaml.Node) error {
	if a == nil {
		return errors.New("arguments destination is nil")
	}
	raw, err := yamlNodeToJSON(node, 0)
	if err != nil {
		return fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	if len(raw) > strictjson.MaxDocumentBytes {
		return errors.New("arguments must be a bounded JSON object")
	}
	return a.UnmarshalJSON(raw)
}

func (a JSONArguments) MarshalYAML() (any, error) {
	if strictjson.ValidateObject(a.raw) != nil {
		return nil, errors.New("arguments must be a JSON object")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(a.raw, &document); err != nil || len(document.Content) != 1 {
		return nil, errors.New("arguments could not be encoded as YAML")
	}
	return document.Content[0], nil
}

func yamlNodeToJSON(node *yaml.Node, depth int) ([]byte, error) {
	if node == nil || depth > maxJSONArgumentsYAMLDepth {
		return nil, errors.New("invalid or excessively nested YAML value")
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) != 1 {
			return nil, errors.New("invalid YAML document")
		}
		return yamlNodeToJSON(node.Content[0], depth+1)
	case yaml.AliasNode:
		return nil, errors.New("YAML aliases are not permitted")
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return nil, errors.New("invalid YAML mapping")
		}
		var buffer bytes.Buffer
		buffer.WriteByte('{')
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || !utf8.ValidString(key.Value) {
				return nil, errors.New("YAML object keys must be strings")
			}
			if _, exists := seen[key.Value]; exists {
				return nil, errors.New("duplicate YAML object key")
			}
			seen[key.Value] = struct{}{}
			encodedKey, _ := json.Marshal(key.Value)
			encodedValue, err := yamlNodeToJSON(node.Content[index+1], depth+1)
			if err != nil {
				return nil, err
			}
			if index > 0 {
				buffer.WriteByte(',')
			}
			buffer.Write(encodedKey)
			buffer.WriteByte(':')
			buffer.Write(encodedValue)
		}
		buffer.WriteByte('}')
		return buffer.Bytes(), nil
	case yaml.SequenceNode:
		var buffer bytes.Buffer
		buffer.WriteByte('[')
		for index, child := range node.Content {
			encoded, err := yamlNodeToJSON(child, depth+1)
			if err != nil {
				return nil, err
			}
			if index > 0 {
				buffer.WriteByte(',')
			}
			buffer.Write(encoded)
		}
		buffer.WriteByte(']')
		return buffer.Bytes(), nil
	case yaml.ScalarNode:
		return yamlScalarToJSON(node)
	default:
		return nil, errors.New("unsupported YAML value")
	}
}

func yamlScalarToJSON(node *yaml.Node) ([]byte, error) {
	switch node.Tag {
	case "!!null":
		return []byte("null"), nil
	case "!!str":
		if !utf8.ValidString(node.Value) {
			return nil, errors.New("YAML string is not valid UTF-8")
		}
		return json.Marshal(node.Value)
	case "!!bool":
		switch node.Value {
		case "true":
			return []byte("true"), nil
		case "false":
			return []byte("false"), nil
		default:
			return nil, errors.New("YAML boolean is not a JSON boolean")
		}
	case "!!int", "!!float":
		if validJSONNumberLexeme(node.Value) {
			return []byte(node.Value), nil
		}
	default:
		return nil, errors.New("YAML scalar is not a JSON scalar")
	}
	return nil, errors.New("invalid YAML scalar")
}

func validJSONNumberLexeme(raw string) bool {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || (trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9')) {
		return false
	}
	return strictjson.Validate(trimmed) == nil
}
