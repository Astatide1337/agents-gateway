// Package compat translates the legacy Python agent manifests into the v2
// resource model. The translator is deliberately conservative: legacy fields
// that cannot be represented as an enforced v2 capability are either rejected
// or emitted as an explicit warning, never widened into a permission.
package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Astatide1337/agents-gateway/v2/pkg/spec"
	"gopkg.in/yaml.v3"
)

const legacyVersion = "0.1.0"

// LegacyManifest mirrors agents_gateway/manifest.py. It is intentionally
// strict at the YAML boundary so a new legacy field cannot silently acquire a
// v2 meaning.
type LegacyManifest struct {
	ID          string                 `yaml:"id"`
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description"`
	Version     string                 `yaml:"version"`
	Runtime     LegacyRuntime          `yaml:"runtime"`
	Skills      []string               `yaml:"skills"`
	Tools       []string               `yaml:"tools"`
	Permissions map[string]interface{} `yaml:"permissions"`
	RiskLevel   string                 `yaml:"risk_level"`
	Tags        []string               `yaml:"tags"`
	Author      string                 `yaml:"author"`
}

// LegacyRuntime mirrors RuntimeConfig in agents_gateway/manifest.py.
type LegacyRuntime struct {
	Type        string `yaml:"type"`
	Command     string `yaml:"command"`
	DockerImage string `yaml:"docker_image"`
}

// Warning is a machine-readable compatibility warning. Warnings are part of
// the translation result so callers can display them, fail a migration gate,
// or persist them in an audit event without parsing human text.
type Warning struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// TranslationError is returned when a legacy value cannot be safely
// represented. In particular, dangerous permission-shaped fields are rejected
// instead of being copied into a broader v2 capability.
type TranslationError struct {
	Code    string `json:"code"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e TranslationError) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + " (" + e.Field + "): " + e.Message
}

// TranslationErrors preserves all independently detectable translation
// failures while still satisfying the standard error interface.
type TranslationErrors []TranslationError

func (e TranslationErrors) Error() string {
	parts := make([]string, len(e))
	for i, item := range e {
		parts[i] = item.Error()
	}
	return strings.Join(parts, "\n")
}

// Translation contains v2 resources and warnings. A result with warnings is
// still usable; a result with an error must not be applied.
type Translation struct {
	Resources []spec.Resource `json:"resources"`
	Warnings  []Warning       `json:"warnings"`
}

// DecodeLegacy decodes exactly one legacy YAML manifest with strict field
// checking. Unknown fields are compatibility errors, not ignored input.
func DecodeLegacy(data []byte) (LegacyManifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var manifest LegacyManifest
	if err := decoder.Decode(&manifest); err != nil {
		return LegacyManifest{}, TranslationErrors{{
			Code:    "unsupported_legacy_field",
			Field:   "manifest",
			Message: err.Error(),
		}}
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return LegacyManifest{}, TranslationErrors{{
				Code:    "multiple_manifests",
				Field:   "manifest",
				Message: "a legacy agent.yaml must contain exactly one document",
			}}
		}
		return LegacyManifest{}, TranslationErrors{{
			Code:    "invalid_legacy_yaml",
			Field:   "manifest",
			Message: err.Error(),
		}}
	}
	return manifest, nil
}

// TranslateYAML decodes and translates one legacy manifest.
func TranslateYAML(data []byte) (Translation, error) {
	manifest, err := DecodeLegacy(data)
	if err != nil {
		return Translation{}, err
	}
	return Translate(manifest)
}

// TranslateFile reads and translates a legacy agent.yaml.
func TranslateFile(path string) (Translation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Translation{}, fmt.Errorf("read legacy manifest %q: %w", path, err)
	}
	return TranslateYAML(data)
}

// Translate converts one legacy manifest into an Agent, a safe SandboxProfile,
// and optional SkillSet and ToolSet resources.
func Translate(input LegacyManifest) (Translation, error) {
	manifest := normalize(input)
	if err := validateLegacyManifest(manifest); err != nil {
		return Translation{}, err
	}

	warnings := make([]Warning, 0, 8)
	if len(manifest.Permissions) > 0 {
		if key, found := dangerousPermissionPath(manifest.Permissions, ""); found {
			return Translation{}, TranslationErrors{{
				Code:    "unsupported_dangerous_field",
				Field:   "permissions." + key,
				Message: "dangerous legacy permission cannot be represented safely in v2",
			}}
		}
		warnings = append(warnings, Warning{
			Code:    "opaque_permissions",
			Field:   "permissions",
			Message: "legacy permissions are opaque and were not converted into v2 authorization; configure ToolSet and SandboxProfile policy explicitly",
		})
	}

	harness, backend, err := mapRuntime(manifest.Runtime.Type)
	if err != nil {
		return Translation{}, err
	}

	image := strings.TrimSpace(manifest.Runtime.DockerImage)
	if image == "" {
		warnings = append(warnings, Warning{
			Code:    "missing_runtime_image",
			Field:   "runtime.docker_image",
			Message: "legacy manifest has no container image; generated v2 resources are not executable until an image is configured",
		})
	} else if !pinnedImage(image) {
		warnings = append(warnings, Warning{
			Code:    "unpinned_image",
			Field:   "runtime.docker_image",
			Message: "legacy image is not pinned by digest; production v2 validation will reject it",
		})
	}

	if manifest.Runtime.Type == "process" || manifest.Runtime.Type == "local-stub" {
		warnings = append(warnings, Warning{
			Code:    "unsafe_legacy_runtime",
			Field:   "runtime.type",
			Message: "legacy " + manifest.Runtime.Type + " execution is not carried into v2; the generated sandbox is isolated and requires an explicit image/harness migration",
		})
	}
	if manifest.Runtime.Command != "" {
		warnings = append(warnings, Warning{
			Code:    "legacy_command_not_enforced",
			Field:   "runtime.command",
			Message: "legacy command is preserved as metadata only and will not be executed by the v2 translator",
		})
	}
	warnings = append(warnings, Warning{
		Code:    "missing_instructions",
		Field:   "instructions",
		Message: "legacy manifest has no v2 instruction source; configure instructions before execution",
	})

	resources := make([]spec.Resource, 0, 4)
	agent := spec.Agent{
		ResourceMeta: agentMeta(spec.KindAgent, manifest.ID, manifest),
		Spec: spec.AgentSpec{
			Runtime:           spec.RuntimeSpec{Harness: harness, Image: image},
			SandboxProfileRef: manifest.ID + "-sandbox",
		},
	}
	if len(manifest.Skills) > 0 {
		skillSet, skillWarnings := translateSkills(manifest)
		warnings = append(warnings, skillWarnings...)
		agent.Spec.SkillSetRef = skillSet.Metadata.Name
		resources = append(resources, &skillSet)
	}
	if len(manifest.Tools) > 0 {
		toolSet, toolWarnings := translateTools(manifest)
		warnings = append(warnings, toolWarnings...)
		agent.Spec.ToolSetRef = toolSet.Metadata.Name
		resources = append(resources, &toolSet)
	}
	if manifest.Description == "" {
		warnings = append(warnings, Warning{
			Code:    "missing_description",
			Field:   "description",
			Message: "legacy manifest has no description",
		})
	}
	if len(manifest.Skills) == 0 && len(manifest.Tools) == 0 {
		warnings = append(warnings, Warning{
			Code:    "no_capability_bindings",
			Field:   "skills/tools",
			Message: "legacy manifest declares no enforceable v2 skills or tools",
		})
	}

	sandbox := spec.SandboxProfile{
		ResourceMeta: relatedMeta(spec.KindSandboxProfile, manifest.ID+"-sandbox", manifest.ID),
		Spec: spec.SandboxProfileSpec{
			Backend: backend,
			Image:   image,
			Resources: spec.ResourceLimits{
				CPU:    "1",
				Memory: "512Mi",
				Disk:   "1Gi",
				PIDs:   128,
			},
			Filesystem: spec.FilesystemSpec{Root: "read-only", Workspace: "isolated"},
			Network:    spec.NetworkSpec{Mode: "none", DirectInternet: false},
		},
	}

	resources = append([]spec.Resource{&agent}, resources...)
	resources = append(resources, &sandbox)
	return Translation{Resources: resources, Warnings: warnings}, nil
}

func normalize(input LegacyManifest) LegacyManifest {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Version = strings.TrimSpace(input.Version)
	if input.Version == "" {
		input.Version = legacyVersion
	}
	input.Runtime.Type = strings.ToLower(strings.TrimSpace(input.Runtime.Type))
	input.Runtime.Command = strings.TrimSpace(input.Runtime.Command)
	input.Runtime.DockerImage = strings.TrimSpace(input.Runtime.DockerImage)
	input.RiskLevel = strings.ToLower(strings.TrimSpace(input.RiskLevel))
	if input.RiskLevel == "" {
		input.RiskLevel = "low"
	}
	input.Author = strings.TrimSpace(input.Author)
	return input
}

func validateLegacyManifest(manifest LegacyManifest) error {
	var validation TranslationErrors
	if manifest.ID == "" {
		validation = append(validation, TranslationError{Code: "missing_required_field", Field: "id", Message: "id is required"})
	} else if !dnsLabelPattern.MatchString(manifest.ID) {
		validation = append(validation, TranslationError{Code: "invalid_identity", Field: "id", Message: "id must be a lowercase DNS label so v2 identity is preserved"})
	}
	if manifest.Name == "" {
		validation = append(validation, TranslationError{Code: "missing_required_field", Field: "name", Message: "name is required"})
	}
	if manifest.Runtime.Type == "" {
		validation = append(validation, TranslationError{Code: "missing_required_field", Field: "runtime.type", Message: "runtime.type is required"})
	}
	if manifest.RiskLevel != "low" && manifest.RiskLevel != "medium" && manifest.RiskLevel != "high" {
		validation = append(validation, TranslationError{Code: "unsupported_value", Field: "risk_level", Message: "risk_level must be low, medium, or high"})
	}
	for i, skill := range manifest.Skills {
		if strings.TrimSpace(skill) == "" {
			validation = append(validation, TranslationError{Code: "invalid_capability", Field: fmt.Sprintf("skills[%d]", i), Message: "skill name must not be empty"})
		}
	}
	for i, tool := range manifest.Tools {
		if strings.TrimSpace(tool) == "" {
			validation = append(validation, TranslationError{Code: "invalid_capability", Field: fmt.Sprintf("tools[%d]", i), Message: "tool name must not be empty"})
		}
	}
	for _, suffix := range []string{"-sandbox", "-skills", "-tools"} {
		if len(manifest.ID)+len(suffix) > 63 || !dnsLabelPattern.MatchString(manifest.ID+suffix) {
			validation = append(validation, TranslationError{Code: "invalid_derived_identity", Field: "id", Message: "derived v2 resource name " + manifest.ID + suffix + " is not a lowercase DNS label"})
		}
	}
	if len(validation) > 0 {
		return validation
	}
	if manifest.Runtime.Type == "docker" && manifest.Runtime.DockerImage == "" {
		return TranslationErrors{{Code: "missing_runtime_image", Field: "runtime.docker_image", Message: "docker runtime requires docker_image"}}
	}
	return nil
}

func mapRuntime(runtimeType string) (harness, backend string, err error) {
	switch runtimeType {
	case "docker":
		return "legacy-docker", "docker", nil
	case "process":
		// Never map a host process to a host process. The generated profile is
		// intentionally an isolated, non-direct network sandbox.
		return "legacy-process", "podman", nil
	case "local-stub":
		return "legacy-local-stub", "podman", nil
	default:
		return "", "", TranslationErrors{{Code: "unsupported_runtime", Field: "runtime.type", Message: "runtime type " + runtimeType + " is not supported by the compatibility translator"}}
	}
}

func translateSkills(manifest LegacyManifest) (spec.SkillSet, []Warning) {
	skills := make([]spec.SkillRef, 0, len(manifest.Skills))
	warnings := []Warning{{
		Code:    "missing_skill_enforcement",
		Field:   "skills",
		Message: "legacy skills are names only; generated SkillSet references are unpinned and must be explicitly resolved before execution",
	}}
	for _, name := range manifest.Skills {
		name = strings.TrimSpace(name)
		skills = append(skills, spec.SkillRef{
			Name:   name,
			Ref:    "legacy://skill/" + escapeRefPart(name),
			Digest: "",
		})
		warnings = append(warnings, Warning{
			Code:    "unpinned_skill",
			Field:   "skills[" + name + "]",
			Message: "skill source has no legacy content digest",
		})
	}
	return spec.SkillSet{
		ResourceMeta: relatedMeta(spec.KindSkillSet, manifest.ID+"-skills", manifest.ID),
		Spec:         spec.SkillSetSpec{Skills: skills},
	}, warnings
}

func translateTools(manifest LegacyManifest) (spec.ToolSet, []Warning) {
	grants := make([]spec.ToolGrant, 0, len(manifest.Tools))
	for _, name := range manifest.Tools {
		grants = append(grants, spec.ToolGrant{
			Name:     strings.TrimSpace(name),
			Effect:   "write",
			Approval: "deny",
		})
	}
	return spec.ToolSet{
			ResourceMeta: relatedMeta(spec.KindToolSet, manifest.ID+"-tools", manifest.ID),
			Spec: spec.ToolSetSpec{Servers: []spec.MCPServer{{
				Name:  "legacy-unbound",
				Ref:   "legacy://mcp/unbound",
				Tools: grants,
			}}},
		}, []Warning{{
			Code:    "missing_tool_enforcement",
			Field:   "tools",
			Message: "legacy tool names are not bound to an MCP server; generated grants are denied until explicitly configured, and TOOLS.md is not enforcement",
		}}
}

func agentMeta(kind, name string, manifest LegacyManifest) spec.ResourceMeta {
	annotations := map[string]string{
		"legacy.astatide.com/display-name": manifest.Name,
		"legacy.astatide.com/description":  manifest.Description,
		"legacy.astatide.com/version":      manifest.Version,
		"legacy.astatide.com/risk-level":   manifest.RiskLevel,
		"legacy.astatide.com/author":       manifest.Author,
	}
	if manifest.Runtime.Command != "" {
		annotations["legacy.astatide.com/runtime-command"] = manifest.Runtime.Command
	}
	if len(manifest.Tags) > 0 {
		encoded, _ := json.Marshal(manifest.Tags)
		annotations["legacy.astatide.com/tags"] = string(encoded)
	}
	return spec.ResourceMeta{
		TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: kind},
		Metadata: spec.ObjectMeta{Name: name, Annotations: annotations},
	}
}

func relatedMeta(kind, name, agentID string) spec.ResourceMeta {
	return spec.ResourceMeta{
		TypeMeta: spec.TypeMeta{APIVersion: spec.APIVersion, Kind: kind},
		Metadata: spec.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"legacy.astatide.com/agent": agentID},
		},
	}
}

func isDangerousPermissionKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"), " ", "_"))
	switch normalized {
	case "privileged", "allow_privilege_escalation", "host_network", "network", "internet", "host_mounts", "mounts", "volumes", "binds", "host_paths", "docker_socket", "container_socket", "cap_add", "capabilities", "devices", "sys_admin", "run_as_root", "root", "environment", "env", "secrets":
		return true
	default:
		return false
	}
}

func dangerousPermissionPath(value interface{}, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			current := key
			if path != "" {
				current = path + "." + key
			}
			if isDangerousPermissionKey(key) {
				return current, true
			}
			if nested, found := dangerousPermissionPath(typed[key], current); found {
				return nested, true
			}
		}
	case []interface{}:
		for index, item := range typed {
			current := fmt.Sprintf("%s[%d]", path, index)
			if nested, found := dangerousPermissionPath(item, current); found {
				return nested, true
			}
		}
	}
	return "", false
}

func escapeRefPart(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			builder.WriteRune(r)
		default:
			fmt.Fprintf(&builder, "%%%02X", r)
		}
	}
	return builder.String()
}

func pinnedImage(image string) bool {
	return digestImagePattern.MatchString(image)
}

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var digestImagePattern = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)
