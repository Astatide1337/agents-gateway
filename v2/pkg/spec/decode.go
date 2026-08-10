package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// DecodeAll decodes a stream of one or more YAML documents. Unknown fields are
// rejected, empty documents are ignored, and every document must be a known
// v1alpha1 resource kind.
func DecodeAll(r io.Reader) ([]Resource, error) {
	decoder := yaml.NewDecoder(r)
	var resources []Resource
	for document := 1; ; document++ {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode YAML document %d: %w", document, err)
		}
		if isEmptyDocument(&node) {
			continue
		}
		if containsYAMLAlias(&node) {
			return nil, fmt.Errorf("decode YAML document %d: YAML aliases are not permitted", document)
		}

		var meta struct {
			TypeMeta `yaml:",inline"`
			Metadata yaml.Node `yaml:"metadata"`
			Spec     yaml.Node `yaml:"spec"`
			Status   yaml.Node `yaml:"status"`
		}
		if err := decodeStrictNode(&node, &meta); err != nil {
			return nil, fmt.Errorf("decode YAML document %d metadata: %w", document, err)
		}
		if meta.APIVersion != APIVersion {
			return nil, fmt.Errorf("YAML document %d: unsupported apiVersion %q", document, meta.APIVersion)
		}

		resource, err := newResource(meta.Kind)
		if err != nil {
			return nil, fmt.Errorf("YAML document %d: %w", document, err)
		}
		if err := decodeStrictNode(&node, resource); err != nil {
			return nil, fmt.Errorf("decode YAML document %d (%s): %w", document, meta.Kind, err)
		}
		resources = append(resources, resource)
	}
	return resources, nil
}

func Decode(data []byte) ([]Resource, error) {
	return DecodeAll(bytes.NewReader(data))
}

func decodeStrictNode(node *yaml.Node, target any) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}

func isEmptyDocument(node *yaml.Node) bool {
	if node == nil || len(node.Content) == 0 {
		return true
	}
	root := node.Content[0]
	return root.Kind == 0 || (root.Kind == yaml.ScalarNode && root.Tag == "!!null")
}

func containsYAMLAlias(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode {
		return true
	}
	for _, child := range node.Content {
		if containsYAMLAlias(child) {
			return true
		}
	}
	return false
}

func newResource(kind string) (Resource, error) {
	switch kind {
	case KindOrganization:
		return &Organization{}, nil
	case KindProject:
		return &Project{}, nil
	case KindAgent:
		return &Agent{}, nil
	case KindSkillSet:
		return &SkillSet{}, nil
	case KindToolSet:
		return &ToolSet{}, nil
	case KindSandboxProfile:
		return &SandboxProfile{}, nil
	case KindModelRoute:
		return &ModelRoute{}, nil
	case KindWorkflow:
		return &Workflow{}, nil
	case KindAgentRun:
		return &AgentRun{}, nil
	case KindWorkflowRun:
		return &WorkflowRun{}, nil
	case KindApproval:
		return &Approval{}, nil
	case KindArtifact:
		return &Artifact{}, nil
	case KindRunner:
		return &Runner{}, nil
	case KindCredential:
		return &Credential{}, nil
	case KindEntitlement:
		return &Entitlement{}, nil
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
}

// ResourceKind returns the canonical kind for a decoded resource.
func ResourceKind(resource Resource) string {
	if resource == nil || resource.Meta() == nil {
		return ""
	}
	return resource.Meta().Kind
}

// AsJSON returns the canonical JSON representation used by RevisionDigest.
func AsJSON(resource Resource) ([]byte, error) {
	return NormalizedJSON(resource)
}
