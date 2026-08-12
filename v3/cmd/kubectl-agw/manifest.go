package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const maxManifestBytes = 4 << 20

const (
	agentRunAPIVersion = "agents.astatide.com/v1alpha1"
	agentRunKind       = "AgentRun"
)

func readAndValidateManifest(path, namespace string) ([]byte, error) {
	if path == "" || path == "-" {
		return nil, fmt.Errorf("manifest file must be a regular path, not %q", path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return nil, fmt.Errorf("manifest file path contains NUL")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat manifest file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest path %q is not a regular file", path)
	}
	if info.Size() > maxManifestBytes {
		return nil, fmt.Errorf("manifest file is larger than %d bytes", maxManifestBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open manifest file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest file: %w", err)
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("manifest file is larger than %d bytes", maxManifestBytes)
	}
	if err := validateAgentRunDocument(data, namespace); err != nil {
		return nil, err
	}
	return data, nil
}

func validateAgentRunDocument(data []byte, namespace string) error {
	document, err := decodeSingleDocument(data)
	if err != nil {
		return fmt.Errorf("invalid AgentRun manifest: %w", err)
	}

	var apiVersion, kind string
	if err := decodeRequiredString(document, "apiVersion", &apiVersion); err != nil {
		return fmt.Errorf("invalid AgentRun manifest: %w", err)
	}
	if err := decodeRequiredString(document, "kind", &kind); err != nil {
		return fmt.Errorf("invalid AgentRun manifest: %w", err)
	}
	if apiVersion != agentRunAPIVersion || kind != agentRunKind {
		return fmt.Errorf("manifest must be %s %s, got %s %s", agentRunAPIVersion, agentRunKind, apiVersion, kind)
	}

	metadataRaw, ok := document["metadata"]
	if !ok || isJSONNull(metadataRaw) {
		return fmt.Errorf("invalid AgentRun manifest: metadata is required")
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(metadataRaw, &metadata); err != nil || metadata == nil {
		return fmt.Errorf("invalid AgentRun manifest: metadata must be an object")
	}
	var name string
	if err := decodeRequiredString(metadata, "name", &name); err != nil {
		return fmt.Errorf("invalid AgentRun manifest: metadata.%w", err)
	}
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return fmt.Errorf("invalid AgentRun name %q: %s", name, strings.Join(problems, "; "))
	}
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return fmt.Errorf("invalid namespace %q: %s", namespace, strings.Join(problems, "; "))
	}
	if raw, exists := metadata["namespace"]; exists {
		var manifestNamespace string
		if err := json.Unmarshal(raw, &manifestNamespace); err != nil || manifestNamespace == "" {
			return fmt.Errorf("metadata.namespace must be a non-empty string when present")
		}
		if manifestNamespace != namespace {
			return fmt.Errorf("manifest namespace %q does not match requested namespace %q", manifestNamespace, namespace)
		}
	}

	for _, field := range []string{"generateName", "uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "managedFields"} {
		if _, exists := metadata[field]; exists {
			return fmt.Errorf("metadata.%s is not accepted by kubectl agw run; submit a new AgentRun", field)
		}
	}
	if _, exists := document["status"]; exists {
		return fmt.Errorf("status is not accepted by kubectl agw run; status is server-owned")
	}
	specRaw, ok := document["spec"]
	if !ok || isJSONNull(specRaw) || !isJSONObject(specRaw) {
		return fmt.Errorf("spec must be a non-empty object")
	}
	return nil
}

func decodeSingleDocument(data []byte) (map[string]json.RawMessage, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var document map[string]json.RawMessage
	found := false
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			if found {
				return nil, fmt.Errorf("exactly one non-empty YAML/JSON document is required")
			}
			continue
		}
		if bytes.Equal(trimmed, []byte("null")) {
			if found {
				return nil, fmt.Errorf("exactly one non-empty YAML/JSON document is required")
			}
			continue
		}
		if found {
			return nil, fmt.Errorf("exactly one non-empty YAML/JSON document is required")
		}
		found = true
		if trimmed[0] != '{' {
			return nil, fmt.Errorf("document must be a JSON/YAML object")
		}
		if err := json.Unmarshal(trimmed, &document); err != nil || document == nil {
			return nil, fmt.Errorf("document must be a JSON/YAML object")
		}
	}
	if !found {
		return nil, fmt.Errorf("exactly one non-empty YAML/JSON document is required")
	}
	return document, nil
}

func decodeRequiredString(values map[string]json.RawMessage, key string, destination *string) error {
	raw, ok := values[key]
	if !ok || isJSONNull(raw) {
		return fmt.Errorf("%s is required", key)
	}
	if err := json.Unmarshal(raw, destination); err != nil || *destination == "" {
		return fmt.Errorf("%s must be a non-empty string", key)
	}
	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
