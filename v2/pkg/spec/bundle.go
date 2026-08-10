package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Astatide1337/agents-gateway/v2/pkg/strictjson"
	"gopkg.in/yaml.v3"
)

// AgentBundle is the personal authoring format. It is intentionally not a
// Resource: the compiler expands it into the existing immutable resources.
type AgentBundle struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Metadata ObjectMeta      `json:"metadata" yaml:"metadata"`
	Spec     AgentBundleSpec `json:"spec" yaml:"spec"`
}

type AgentBundleSpec struct {
	Prompt       BundlePrompt          `json:"prompt" yaml:"prompt"`
	Runtime      BundleRuntimeSpec     `json:"runtime" yaml:"runtime"`
	Model        *BundleModelSpec      `json:"model,omitempty" yaml:"model,omitempty"`
	Sandbox      *BundleSandboxSpec    `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
	Skills       []SkillRef            `json:"skills,omitempty" yaml:"skills,omitempty"`
	MCP          BundleMCPConfig       `json:"mcp,omitempty" yaml:"mcp,omitempty"`
	Tools        BundleMCPConfig       `json:"tools,omitempty" yaml:"tools,omitempty"`
	Verification VerificationSpec      `json:"verification,omitempty" yaml:"verification,omitempty"`
	Artifacts    ArtifactPreferences   `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Environment  []EnvironmentVariable `json:"environment,omitempty" yaml:"environment,omitempty"`
	Limits       RunLimits             `json:"limits,omitempty" yaml:"limits,omitempty"`

	// Direct refs are the convenient form. Refs is accepted for files that
	// want to group reusable-resource references together.
	ToolSetRef        string     `json:"toolSetRef,omitempty" yaml:"toolSetRef,omitempty"`
	SkillSetRef       string     `json:"skillSetRef,omitempty" yaml:"skillSetRef,omitempty"`
	ModelRouteRef     string     `json:"modelRouteRef,omitempty" yaml:"modelRouteRef,omitempty"`
	SandboxProfileRef string     `json:"sandboxProfileRef,omitempty" yaml:"sandboxProfileRef,omitempty"`
	Refs              BundleRefs `json:"refs,omitempty" yaml:"refs,omitempty"`
}

type BundleRefs struct {
	ToolSet        string `json:"toolSet,omitempty" yaml:"toolSet,omitempty"`
	SkillSet       string `json:"skillSet,omitempty" yaml:"skillSet,omitempty"`
	ModelRoute     string `json:"modelRoute,omitempty" yaml:"modelRoute,omitempty"`
	SandboxProfile string `json:"sandboxProfile,omitempty" yaml:"sandboxProfile,omitempty"`
}

// BundlePrompt accepts either `prompt: | ...` or `prompt: {inline: ...}` /
// `{file: ...}` while rejecting null and unknown fields.
type BundlePrompt struct {
	Inline string `json:"inline,omitempty" yaml:"inline,omitempty"`
	File   string `json:"file,omitempty" yaml:"file,omitempty"`
}

type BundleRuntimeSpec struct {
	Harness string `json:"harness" yaml:"harness"`
	Image   string `json:"image" yaml:"image"`
}

type BundleModelSpec struct {
	Provider      string  `json:"provider" yaml:"provider"`
	Kind          string  `json:"kind,omitempty" yaml:"kind,omitempty"`
	Model         string  `json:"model" yaml:"model"`
	CredentialRef string  `json:"credentialRef,omitempty" yaml:"credentialRef,omitempty"`
	Priority      int     `json:"priority,omitempty" yaml:"priority,omitempty"`
	Fallback      bool    `json:"fallback,omitempty" yaml:"fallback,omitempty"`
	MaxCostUSD    float64 `json:"maxCostUsd,omitempty" yaml:"maxCostUsd,omitempty"`
	Cooldown      string  `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
}

type BundleSandboxSpec struct {
	Backend    string         `json:"backend" yaml:"backend"`
	Image      string         `json:"image,omitempty" yaml:"image,omitempty"`
	Resources  ResourceLimits `json:"resources,omitempty" yaml:"resources,omitempty"`
	Filesystem FilesystemSpec `json:"filesystem,omitempty" yaml:"filesystem,omitempty"`
	Network    NetworkSpec    `json:"network,omitempty" yaml:"network,omitempty"`
}

type BundleMCPConfig struct {
	Servers []MCPServer `json:"servers" yaml:"servers"`
}

type CompiledAgentBundle struct {
	Bundle      *AgentBundle
	Resources   []Resource
	Agent       *Agent
	AgentDigest string
	// BundleDigest covers the complete generated resource set. AgentDigest is
	// the digest required by the existing AgentRun API.
	BundleDigest string
}

type BundleCompileOptions struct {
	BaseDir string
	Input   string
}

// DecodeAgentBundle decodes exactly one strict YAML or JSON AgentBundle.
func DecodeAgentBundle(data []byte) (*AgentBundle, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("agent bundle is empty")
	}
	if json.Valid(data) {
		if err := strictjson.Validate(data); err != nil {
			return nil, fmt.Errorf("decode AgentBundle JSON: %w", err)
		}
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("decode AgentBundle: %w", err)
	}
	if isEmptyDocument(&node) {
		return nil, errors.New("agent bundle is empty")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode AgentBundle: %w", err)
		}
		if !isEmptyDocument(&extra) {
			return nil, errors.New("agent bundle must contain exactly one document")
		}
	}
	if containsYAMLAlias(&node) {
		return nil, errors.New("agent bundle: YAML aliases are not permitted")
	}
	var bundle AgentBundle
	if err := decodeStrictNode(&node, &bundle); err != nil {
		return nil, fmt.Errorf("decode AgentBundle: %w", err)
	}
	if bundle.APIVersion != APIVersion {
		return nil, fmt.Errorf("agent bundle apiVersion must be %s", APIVersion)
	}
	if bundle.Kind != KindAgentBundle {
		return nil, fmt.Errorf("agent bundle kind must be %s", KindAgentBundle)
	}
	return &bundle, nil
}

func LoadAgentBundle(path string) (*AgentBundle, string, error) {
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, "", fmt.Errorf("resolve bundle path: %w", err)
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return nil, "", fmt.Errorf("read bundle %q: %w", path, err)
	}
	bundle, err := DecodeAgentBundle(data)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", path, err)
	}
	return bundle, filepath.Dir(clean), nil
}

// CompileAgentBundle expands a bundle into canonical resources. It never
// creates Credential resources and never copies secret values into output.
func CompileAgentBundle(bundle *AgentBundle, options BundleCompileOptions) (*CompiledAgentBundle, error) {
	if bundle == nil {
		return nil, errors.New("agent bundle is nil")
	}
	if err := ValidateAgentBundle(bundle); err != nil {
		return nil, err
	}
	baseDir := options.BaseDir
	if baseDir == "" {
		baseDir = "."
	}
	instructions, err := resolveBundlePrompt(bundle.Spec.Prompt, baseDir)
	if err != nil {
		return nil, err
	}
	if options.Input != "" {
		instructions += "\n\n--- User input ---\n" + options.Input
	}

	name := bundle.Metadata.Name
	ns := bundle.Metadata.Namespace
	toolRef, skillRef, modelRef, sandboxRef := bundleRefs(bundle.Spec)
	resources := make([]Resource, 0, 5)

	if len(bundle.Spec.Skills) > 0 {
		if skillRef != "" {
			return nil, errors.New("bundle cannot define both inline skills and skillSetRef")
		}
		skillRef = derivedBundleName(name, "skills")
		resources = append(resources, &SkillSet{ResourceMeta: bundleMeta(KindSkillSet, skillRef, ns), Spec: SkillSetSpec{Skills: append([]SkillRef(nil), bundle.Spec.Skills...)}})
	}
	mcp := bundle.Spec.MCP
	if len(bundle.Spec.Tools.Servers) > 0 {
		if len(mcp.Servers) > 0 {
			return nil, errors.New("bundle cannot define both spec.mcp and spec.tools")
		}
		mcp = bundle.Spec.Tools
	}
	if len(mcp.Servers) > 0 {
		if toolRef != "" {
			return nil, errors.New("bundle cannot define both inline MCP servers and toolSetRef")
		}
		toolRef = derivedBundleName(name, "tools")
		resources = append(resources, &ToolSet{ResourceMeta: bundleMeta(KindToolSet, toolRef, ns), Spec: ToolSetSpec{Servers: append([]MCPServer(nil), mcp.Servers...)}})
	}
	if bundle.Spec.Model != nil {
		if modelRef != "" {
			return nil, errors.New("bundle cannot define both inline model and modelRouteRef")
		}
		modelRef = derivedBundleName(name, "model")
		model := bundle.Spec.Model
		kind := model.Kind
		if kind == "" {
			kind = model.Provider
		}
		resources = append(resources, &ModelRoute{ResourceMeta: bundleMeta(KindModelRoute, modelRef, ns), Spec: ModelRouteSpec{
			Providers: []ModelProvider{{Name: model.Provider, Kind: kind, Model: model.Model, Credential: model.CredentialRef, Priority: model.Priority, Fallback: model.Fallback}},
			Budget:    BudgetSpec{MaxCostUSD: model.MaxCostUSD, Cooldown: model.Cooldown},
		}})
	}
	if bundle.Spec.Sandbox != nil {
		if sandboxRef != "" {
			return nil, errors.New("bundle cannot define both inline sandbox and sandboxProfileRef")
		}
		sandboxRef = derivedBundleName(name, "sandbox")
		sandbox := bundle.Spec.Sandbox
		profileImage := sandbox.Image
		if profileImage == "" {
			profileImage = bundle.Spec.Runtime.Image
		}
		filesystem := sandbox.Filesystem
		if filesystem.Root == "" {
			filesystem.Root = "read-only"
		}
		if filesystem.Workspace == "" {
			filesystem.Workspace = "ephemeral"
		}
		network := sandbox.Network
		if network.Mode == "" {
			network.Mode = "brokered"
		}
		resources = append(resources, &SandboxProfile{ResourceMeta: bundleMeta(KindSandboxProfile, sandboxRef, ns), Spec: SandboxProfileSpec{
			Backend: sandbox.Backend, Image: profileImage, Resources: sandbox.Resources, Filesystem: filesystem, Network: network,
		}})
	}
	if modelRef == "" && len(mcp.Servers) > 0 {
		return nil, errors.New("tool-enabled bundle requires model or modelRouteRef")
	}
	if modelRef == "" {
		return nil, errors.New("bundle requires model or modelRouteRef")
	}
	if sandboxRef == "" {
		return nil, errors.New("bundle requires sandbox or sandboxProfileRef")
	}
	var artifactPreferences *ArtifactPreferences
	if bundle.Spec.Artifacts.Enabled || bundle.Spec.Artifacts.Required || bundle.Spec.Artifacts.PrimaryKind != "" || bundle.Spec.Artifacts.PrimaryMediaType != "" || len(bundle.Spec.Artifacts.PreferredKinds) > 0 {
		preferences := bundle.Spec.Artifacts
		preferences.PreferredKinds = append([]string(nil), preferences.PreferredKinds...)
		artifactPreferences = &preferences
	}
	agent := &Agent{ResourceMeta: bundleMeta(KindAgent, name, ns), Spec: AgentSpec{
		Runtime:      RuntimeSpec{Harness: bundle.Spec.Runtime.Harness, Image: bundle.Spec.Runtime.Image},
		Instructions: InstructionsSpec{Inline: instructions}, SkillSetRef: skillRef, ToolSetRef: toolRef,
		ModelRouteRef: modelRef, SandboxProfileRef: sandboxRef, Limits: bundle.Spec.Limits,
		Verification: bundle.Spec.Verification, Artifacts: artifactPreferences,
		Environment: append([]EnvironmentVariable(nil), bundle.Spec.Environment...),
	}}
	resources = append(resources, agent)
	if err := ValidateAll(resources, ValidationOptions{Production: true}); err != nil {
		return nil, fmt.Errorf("compiled AgentBundle is invalid: %w", err)
	}
	// Canonical resource order is part of the CLI's deterministic plan output.
	sort.Slice(resources, func(i, j int) bool {
		return ResourceKind(resources[i])+"/"+resources[i].Meta().Metadata.Name < ResourceKind(resources[j])+"/"+resources[j].Meta().Metadata.Name
	})
	digest, err := RevisionDigest(agent)
	if err != nil {
		return nil, fmt.Errorf("digest compiled agent: %w", err)
	}
	completeDigest, err := digestResourceSet(resources)
	if err != nil {
		return nil, fmt.Errorf("digest compiled bundle: %w", err)
	}
	return &CompiledAgentBundle{Bundle: bundle, Resources: resources, Agent: agent, AgentDigest: digest, BundleDigest: completeDigest}, nil
}

func digestResourceSet(resources []Resource) (string, error) {
	var buffer bytes.Buffer
	for _, resource := range resources {
		body, err := AsJSON(resource)
		if err != nil {
			return "", err
		}
		buffer.WriteString(ResourceKind(resource))
		buffer.WriteByte('/')
		buffer.WriteString(resource.Meta().Metadata.Namespace)
		buffer.WriteByte('/')
		buffer.WriteString(resource.Meta().Metadata.Name)
		buffer.WriteByte('\n')
		buffer.Write(body)
		buffer.WriteByte('\n')
	}
	sum := sha256.Sum256(buffer.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ValidateAgentBundle checks the authoring contract before expansion. Bundle
// images and skills are always pinned because the bundle is intended to be a
// reproducible execution unit; development mode can still be used with the
// lower-level multi-resource CLI.
func ValidateAgentBundle(bundle *AgentBundle) error {
	if bundle.APIVersion != APIVersion || bundle.Kind != KindAgentBundle {
		return errors.New("bundle must declare the Agents Gateway v1alpha1 AgentBundle kind")
	}
	if !dnsLabelPattern.MatchString(bundle.Metadata.Name) {
		return errors.New("bundle metadata.name must be a lowercase DNS label")
	}
	if bundle.Spec.Prompt.Inline == "" && bundle.Spec.Prompt.File == "" {
		return errors.New("bundle spec.prompt requires inline text or file")
	}
	if bundle.Spec.Prompt.Inline != "" && bundle.Spec.Prompt.File != "" {
		return errors.New("bundle spec.prompt.inline and file are mutually exclusive")
	}
	if strings.TrimSpace(bundle.Spec.Runtime.Harness) == "" || strings.TrimSpace(bundle.Spec.Runtime.Image) == "" {
		return errors.New("bundle spec.runtime requires harness and image")
	}
	if !imageDigestPattern(bundle.Spec.Runtime.Image) {
		return errors.New("bundle spec.runtime.image must be pinned with @sha256:<64 hex>")
	}
	if bundle.Spec.Sandbox != nil {
		if bundle.Spec.Sandbox.Backend == "" {
			return errors.New("bundle spec.sandbox.backend is required")
		}
		profileImage := bundle.Spec.Sandbox.Image
		if profileImage == "" {
			profileImage = bundle.Spec.Runtime.Image
		}
		if !imageDigestPattern(profileImage) {
			return errors.New("bundle sandbox image must be pinned with @sha256:<64 hex>")
		}
	}
	for i, skill := range bundle.Spec.Skills {
		if strings.TrimSpace(skill.Ref) == "" || !digestPattern.MatchString(skill.Digest) {
			return fmt.Errorf("bundle spec.skills[%d] requires a pinned ref and sha256 digest", i)
		}
	}
	return nil
}

func resolveBundlePrompt(prompt BundlePrompt, baseDir string) (string, error) {
	if prompt.Inline != "" {
		return prompt.Inline, nil
	}
	root, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve prompt base directory: %w", err)
	}
	if filepath.IsAbs(prompt.File) {
		return "", errors.New("prompt.file must be relative to the bundle directory")
	}
	candidate := filepath.Join(root, filepath.Clean(prompt.File))
	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve prompt.file: %w", err)
	}
	rel, err := filepath.Rel(root, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("prompt.file escapes the bundle directory")
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("prompt.file must name a regular file")
	}
	data, err := os.ReadFile(realPath)
	if err != nil {
		return "", fmt.Errorf("read prompt.file: %w", err)
	}
	if len(data) > strictjson.MaxDocumentBytes {
		return "", errors.New("prompt.file exceeds the 1 MiB prompt limit")
	}
	return string(data), nil
}

func bundleRefs(spec AgentBundleSpec) (string, string, string, string) {
	return firstNonEmpty(spec.ToolSetRef, spec.Refs.ToolSet), firstNonEmpty(spec.SkillSetRef, spec.Refs.SkillSet), firstNonEmpty(spec.ModelRouteRef, spec.Refs.ModelRoute), firstNonEmpty(spec.SandboxProfileRef, spec.Refs.SandboxProfile)
}

func bundleMeta(kind, name, namespace string) ResourceMeta {
	return ResourceMeta{TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: kind}, Metadata: ObjectMeta{Name: name, Namespace: namespace}}
}

func derivedBundleName(base, suffix string) string {
	candidate := base + "-" + suffix
	if len(candidate) <= 63 {
		return candidate
	}
	hash := sha256.Sum256([]byte(candidate))
	short := hex.EncodeToString(hash[:])[:8]
	return strings.TrimRight(base[:63-len(suffix)-10], "-") + "-" + short + "-" + suffix
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func imageDigestPattern(value string) bool {
	value = strings.TrimSpace(value)
	at := strings.LastIndex(value, "@sha256:")
	return at > 0 && digestPattern.MatchString(value[at+1:])
}

func (p *BundlePrompt) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("prompt cannot be null")
	}
	if len(raw) > 0 && bytes.TrimSpace(raw)[0] == '"' {
		return json.Unmarshal(raw, &p.Inline)
	}
	type plain BundlePrompt
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Inline == "" && value.File == "" {
		return errors.New("prompt requires inline or file")
	}
	*p = BundlePrompt(value)
	return nil
}

func (p *BundlePrompt) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		return errors.New("prompt cannot be null")
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		p.Inline = node.Value
		return nil
	}
	type plain BundlePrompt
	var value plain
	if err := decodeStrictNode(node, &value); err != nil {
		return err
	}
	if value.Inline == "" && value.File == "" {
		return errors.New("prompt requires inline or file")
	}
	*p = BundlePrompt(value)
	return nil
}

func (c *BundleMCPConfig) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return errors.New("MCP configuration cannot be null")
	}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return json.Unmarshal(trimmed, &c.Servers)
	}
	type plain BundleMCPConfig
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*c = BundleMCPConfig(value)
	return nil
}

func (c *BundleMCPConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		return errors.New("MCP configuration cannot be null")
	}
	if node.Kind == yaml.SequenceNode {
		var servers []MCPServer
		if err := decodeStrictNode(node, &servers); err != nil {
			return err
		}
		c.Servers = servers
		return nil
	}
	type plain BundleMCPConfig
	var value plain
	if err := decodeStrictNode(node, &value); err != nil {
		return err
	}
	*c = BundleMCPConfig(value)
	return nil
}

func (s *AgentBundleSpec) UnmarshalJSON(raw []byte) error {
	if strictjson.ValidateObject(raw) != nil {
		return errors.New("bundle spec must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, field := range []string{"prompt", "runtime", "model", "sandbox", "skills", "mcp", "tools"} {
		if value, ok := fields[field]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("bundle spec.%s cannot be null", field)
		}
	}
	type plain AgentBundleSpec
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*s = AgentBundleSpec(value)
	return nil
}

func (s *AgentBundleSpec) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return errors.New("bundle spec must be a YAML object")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if value.Tag == "!!null" {
			switch key.Value {
			case "prompt", "runtime", "model", "sandbox", "skills", "mcp", "tools":
				return fmt.Errorf("bundle spec.%s cannot be null", key.Value)
			}
		}
	}
	type plain AgentBundleSpec
	var value plain
	if err := decodeStrictNode(node, &value); err != nil {
		return err
	}
	*s = AgentBundleSpec(value)
	return nil
}
