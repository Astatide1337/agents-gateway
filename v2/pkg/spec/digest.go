package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
)

// RevisionDigest returns the stable content address of a resource. Encoding
// uses JSON because encoding/json sorts string map keys, while canonicalize
// recursively removes nil values and makes semantically unordered collections
// stable without changing workflow step order.
func RevisionDigest(resource Resource) (string, error) {
	if resource == nil {
		return "", fmt.Errorf("cannot digest a nil resource")
	}
	normalized, err := normalizedValue(resource)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal normalized resource: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// NormalizedJSON exposes the exact bytes hashed by RevisionDigest, which is
// useful for debugging reproducibility and for signing revision manifests.
func NormalizedJSON(resource Resource) ([]byte, error) {
	if resource == nil {
		return nil, fmt.Errorf("cannot normalize a nil resource")
	}
	normalized, err := normalizedValue(resource)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func normalizedValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err := strictjson.Validate(raw); err != nil {
		return nil, fmt.Errorf("resource JSON violates strict contract: %w", err)
	}
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	normalized := normalizeJSON(generic, "")
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	if err := strictjson.Validate(encoded); err != nil {
		return nil, fmt.Errorf("normalized resource JSON violates strict contract: %w", err)
	}
	var checked any
	decoder = json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&checked); err != nil {
		return nil, err
	}
	return checked, nil
}

func normalizeJSON(value any, path string) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "arguments" {
				// ToolGrant.arguments is an opaque capability constraint. It may
				// contain null members and order-sensitive arrays; only its
				// strict JSON meaning, not resource-level normalization rules,
				// applies inside this field.
				out[key] = child
				continue
			}
			if child == nil {
				continue
			}
			out[key] = normalizeJSON(child, path+"."+key)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = normalizeJSON(child, fmt.Sprintf("%s[%d]", path, i))
		}
		if unorderedCollection(path) {
			sort.SliceStable(out, func(i, j int) bool {
				a, _ := json.Marshal(out[i])
				b, _ := json.Marshal(out[j])
				return string(a) < string(b)
			})
		}
		return out
	default:
		return typed
	}
}

func unorderedCollection(path string) bool {
	for _, suffix := range []string{".tags", ".skills", ".allowedHosts", ".routes"} {
		if len(path) >= len(suffix) && path[len(path)-len(suffix):] == suffix {
			return true
		}
	}
	return false
}
