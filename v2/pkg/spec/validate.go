package spec

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

type ValidationOptions struct {
	Production bool
}

type ValidationError struct {
	Resource string
	Field    string
	Message  string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Resource, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Resource, e.Field, e.Message)
}

type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	parts := make([]string, len(e))
	for i := range e {
		parts[i] = e[i].Error()
	}
	return strings.Join(parts, "\n")
}

func ValidateAll(resources []Resource, options ValidationOptions) error {
	var validation ValidationErrors
	seen := make(map[string]struct{})
	for _, resource := range resources {
		if resource == nil || resource.Meta() == nil {
			validation = append(validation, ValidationError{Resource: "<nil>", Message: "resource is nil"})
			continue
		}
		key := resource.Meta().Kind + "/" + resource.Meta().Metadata.Namespace + "/" + resource.Meta().Metadata.Name
		if _, exists := seen[key]; exists {
			validation = append(validation, ValidationError{Resource: key, Message: "duplicate resource identity"})
		}
		seen[key] = struct{}{}
		validation = append(validation, validateResource(resource, options)...)
	}
	if len(validation) > 0 {
		return validation
	}
	return nil
}

func validateResource(resource Resource, options ValidationOptions) ValidationErrors {
	meta := resource.Meta()
	identity := ResourceKind(resource) + "/" + meta.Metadata.Name
	var out ValidationErrors
	if meta.APIVersion != APIVersion {
		out = append(out, ValidationError{Resource: identity, Field: "apiVersion", Message: "must be " + APIVersion})
	}
	if !dnsLabelPattern.MatchString(meta.Metadata.Name) {
		out = append(out, ValidationError{Resource: identity, Field: "metadata.name", Message: "must be a lowercase DNS label"})
	}
	if meta.Metadata.Namespace != "" && !dnsLabelPattern.MatchString(meta.Metadata.Namespace) {
		out = append(out, ValidationError{Resource: identity, Field: "metadata.namespace", Message: "must be a lowercase DNS label"})
	}

	switch typed := resource.(type) {
	case *Organization:
		if typed.Spec.DisplayName == "" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.displayName", Message: "is required"})
		}
	case *Project:
		out = appendRequired(out, identity, "spec.organizationRef", typed.Spec.OrganizationRef)
	case *Agent:
		out = appendRequired(out, identity, "spec.runtime.harness", typed.Spec.Runtime.Harness)
		out = appendRequired(out, identity, "spec.runtime.image", typed.Spec.Runtime.Image)
		if typed.Spec.Instructions.Inline == "" && typed.Spec.Instructions.File == "" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.instructions", Message: "one of inline or file is required"})
		}
		if typed.Spec.Instructions.Inline != "" && typed.Spec.Instructions.File != "" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.instructions", Message: "inline and file are mutually exclusive"})
		}
		if options.Production {
			out = appendImageDigest(out, identity, "spec.runtime.image", typed.Spec.Runtime.Image)
			out = appendRequired(out, identity, "spec.sandboxProfileRef", typed.Spec.SandboxProfileRef)
		}
		seenEnvironment := make(map[string]struct{}, len(typed.Spec.Environment))
		for i, variable := range typed.Spec.Environment {
			field := fmt.Sprintf("spec.environment[%d]", i)
			if !environmentNamePattern.MatchString(variable.Name) {
				out = append(out, ValidationError{Resource: identity, Field: field + ".name", Message: "must be an uppercase environment variable name"})
			}
			if _, exists := seenEnvironment[variable.Name]; exists {
				out = append(out, ValidationError{Resource: identity, Field: field + ".name", Message: "duplicate environment variable"})
			}
			seenEnvironment[variable.Name] = struct{}{}
			if (variable.Value == "") == (variable.Ref == "") {
				out = append(out, ValidationError{Resource: identity, Field: field, Message: "exactly one of value or valueFrom is required"})
			}
			if options.Production && variable.Value != "" {
				out = append(out, ValidationError{Resource: identity, Field: field + ".value", Message: "literal environment values are forbidden in production; use valueFrom"})
			}
		}
	case *SkillSet:
		if len(typed.Spec.Skills) == 0 {
			out = append(out, ValidationError{Resource: identity, Field: "spec.skills", Message: "must contain at least one skill"})
		}
		seenRefs := map[string]struct{}{}
		for i, skill := range typed.Spec.Skills {
			field := fmt.Sprintf("spec.skills[%d]", i)
			out = appendRequired(out, identity, field+".ref", skill.Ref)
			out = appendRequired(out, identity, field+".digest", skill.Digest)
			if skill.Ref != "" {
				if _, exists := seenRefs[skill.Ref]; exists {
					out = append(out, ValidationError{Resource: identity, Field: field + ".ref", Message: "duplicate skill reference"})
				}
				seenRefs[skill.Ref] = struct{}{}
			}
			if options.Production {
				out = appendDigest(out, identity, field+".digest", skill.Digest)
			}
		}
	case *ToolSet:
		if len(typed.Spec.Servers) == 0 {
			out = append(out, ValidationError{Resource: identity, Field: "spec.servers", Message: "must contain at least one server"})
		}
		seenServers := map[string]struct{}{}
		for i, server := range typed.Spec.Servers {
			field := fmt.Sprintf("spec.servers[%d]", i)
			out = appendRequired(out, identity, field+".name", server.Name)
			out = appendRequired(out, identity, field+".ref", server.Ref)
			if _, exists := seenServers[server.Name]; exists {
				out = append(out, ValidationError{Resource: identity, Field: field + ".name", Message: "duplicate server name"})
			}
			seenServers[server.Name] = struct{}{}
			if server.Image != "" && options.Production {
				out = appendImageDigest(out, identity, field+".image", server.Image)
			}
			if server.Ref != "" {
				if parsed, err := url.Parse(server.Ref); err != nil || parsed.Scheme == "" {
					out = append(out, ValidationError{Resource: identity, Field: field + ".ref", Message: "must be a URI with a scheme"})
				}
			}
			for j, grant := range server.Tools {
				grantField := fmt.Sprintf("%s.tools[%d]", field, j)
				out = appendRequired(out, identity, grantField+".name", grant.Name)
				if grant.Effect != "" && grant.Effect != "read" && grant.Effect != "write" && grant.Effect != "destructive" && grant.Effect != "unknown" {
					out = append(out, ValidationError{Resource: identity, Field: grantField + ".effect", Message: "must be read, write, destructive, or unknown"})
				}
				if grant.Approval != "" && grant.Approval != "allow" && grant.Approval != "approve" && grant.Approval != "propose/commit" && grant.Approval != "deny" {
					out = append(out, ValidationError{Resource: identity, Field: grantField + ".approval", Message: "must be allow, approve, propose/commit, or deny"})
				}
			}
		}
	case *SandboxProfile:
		out = appendRequired(out, identity, "spec.backend", typed.Spec.Backend)
		out = appendRequired(out, identity, "spec.image", typed.Spec.Image)
		out = appendRequired(out, identity, "spec.network.mode", typed.Spec.Network.Mode)
		if typed.Spec.Backend != "podman" && typed.Spec.Backend != "docker" && typed.Spec.Backend != "gvisor" && typed.Spec.Backend != "containerd-runsc" && typed.Spec.Backend != "firecracker" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.backend", Message: "must be podman, docker, gvisor, containerd-runsc, or firecracker"})
		}
		if typed.Spec.Filesystem.Root != "" && typed.Spec.Filesystem.Root != "read-only" && typed.Spec.Filesystem.Root != "writable" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.filesystem.root", Message: "must be read-only or writable"})
		}
		if typed.Spec.Network.Mode != "" && typed.Spec.Network.Mode != "brokered" && typed.Spec.Network.Mode != "none" && typed.Spec.Network.Mode != "direct" {
			out = append(out, ValidationError{Resource: identity, Field: "spec.network.mode", Message: "must be brokered, none, or direct"})
		}
		if options.Production {
			out = appendImageDigest(out, identity, "spec.image", typed.Spec.Image)
			if typed.Spec.Network.DirectInternet || typed.Spec.Network.Mode == "direct" {
				out = append(out, ValidationError{Resource: identity, Field: "spec.network.directInternet", Message: "must be false in production"})
			}
		}
	case *ModelRoute:
		if len(typed.Spec.Providers) == 0 {
			out = append(out, ValidationError{Resource: identity, Field: "spec.providers", Message: "must contain at least one provider"})
		}
		for i, provider := range typed.Spec.Providers {
			field := fmt.Sprintf("spec.providers[%d]", i)
			out = appendRequired(out, identity, field+".name", provider.Name)
			out = appendRequired(out, identity, field+".kind", provider.Kind)
			out = appendRequired(out, identity, field+".model", provider.Model)
		}
	case *Workflow:
		out = append(out, validateWorkflow(identity, typed.Spec.Steps)...)
	case *AgentRun:
		out = appendRequired(out, identity, "spec.agentRef", typed.Spec.AgentRef)
	case *WorkflowRun:
		out = appendRequired(out, identity, "spec.workflowRef", typed.Spec.WorkflowRef)
	case *Approval:
		out = appendRequired(out, identity, "spec.runRef", typed.Spec.RunRef)
		out = appendRequired(out, identity, "spec.reason", typed.Spec.Reason)
	case *Artifact:
		out = appendRequired(out, identity, "spec.runRef", typed.Spec.RunRef)
		out = appendRequired(out, identity, "spec.uri", typed.Spec.URI)
		out = appendRequired(out, identity, "spec.digest", typed.Spec.Digest)
		out = appendDigest(out, identity, "spec.digest", typed.Spec.Digest)
	case *Runner:
		out = appendRequired(out, identity, "spec.endpoint", typed.Spec.Endpoint)
	case *Credential:
		out = appendRequired(out, identity, "spec.kind", typed.Spec.Kind)
		out = appendRequired(out, identity, "spec.secretRef", typed.Spec.SecretRef)
	case *Entitlement:
		out = appendRequired(out, identity, "spec.owner", typed.Spec.Owner)
		out = appendRequired(out, identity, "spec.provider", typed.Spec.Provider)
		out = appendRequired(out, identity, "spec.kind", typed.Spec.Kind)
	}
	return out
}

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var environmentNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

func appendRequired(out ValidationErrors, resource, field, value string) ValidationErrors {
	if strings.TrimSpace(value) == "" {
		return append(out, ValidationError{Resource: resource, Field: field, Message: "is required"})
	}
	return out
}

func appendDigest(out ValidationErrors, resource, field, value string) ValidationErrors {
	if !digestPattern.MatchString(value) {
		return append(out, ValidationError{Resource: resource, Field: field, Message: "must be a lowercase sha256:<64 hex> digest"})
	}
	return out
}

func appendImageDigest(out ValidationErrors, resource, field, value string) ValidationErrors {
	value = strings.TrimSpace(value)
	if at := strings.LastIndex(value, "@sha256:"); at > 0 {
		value = value[at+1:]
	}
	if !digestPattern.MatchString(value) {
		return append(out, ValidationError{Resource: resource, Field: field, Message: "must be sha256:<64 hex> or an image@sha256:<64 hex> reference"})
	}
	return out
}

func validateWorkflow(resource string, steps []WorkflowStep) ValidationErrors {
	var out ValidationErrors
	if len(steps) == 0 {
		return append(out, ValidationError{Resource: resource, Field: "spec.steps", Message: "must contain at least one step"})
	}
	byID := make(map[string]WorkflowStep, len(steps))
	for i, step := range steps {
		field := fmt.Sprintf("spec.steps[%d]", i)
		if !dnsLabelPattern.MatchString(step.ID) {
			out = append(out, ValidationError{Resource: resource, Field: field + ".id", Message: "must be a lowercase DNS label"})
		}
		if _, exists := byID[step.ID]; exists {
			out = append(out, ValidationError{Resource: resource, Field: field + ".id", Message: "duplicate step id"})
		}
		if step.Agent == "" && step.Approval == nil {
			out = append(out, ValidationError{Resource: resource, Field: field, Message: "must define agent or approval"})
		}
		if step.Agent != "" && step.Approval != nil {
			out = append(out, ValidationError{Resource: resource, Field: field, Message: "agent and approval are mutually exclusive"})
		}
		if step.Retries < 0 {
			out = append(out, ValidationError{Resource: resource, Field: field + ".retries", Message: "must not be negative"})
		}
		byID[step.ID] = step
	}
	graph := make(map[string][]string, len(byID))
	inDegree := make(map[string]int, len(byID))
	for id := range byID {
		inDegree[id] = 0
	}
	for id, step := range byID {
		for _, dependency := range step.Needs {
			if _, exists := byID[dependency]; !exists {
				out = append(out, ValidationError{Resource: resource, Field: "spec.steps." + id + ".needs", Message: fmt.Sprintf("unknown dependency %q", dependency)})
				continue
			}
			graph[dependency] = append(graph[dependency], id)
			inDegree[id]++
		}
	}
	queue := make([]string, 0, len(byID))
	for id, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	processed := 0
	for len(queue) > 0 {
		sort.Strings(queue)
		id := queue[0]
		queue = queue[1:]
		processed++
		for _, child := range graph[id] {
			inDegree[child]--
			if inDegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if processed != len(byID) {
		out = append(out, ValidationError{Resource: resource, Field: "spec.steps", Message: "contains a dependency cycle"})
	}
	return out
}
