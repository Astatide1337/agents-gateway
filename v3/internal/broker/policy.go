package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

func compileToolSet(spec v1alpha1.ToolSetSpec) (map[string]compiledServer, map[v1alpha1.ToolProfileName]map[string]compiledTool, error) {
	if spec.MaxToolsPerPhase < 1 || spec.MaxToolsPerPhase > v1alpha1.MaxToolsPerPhase || len(spec.Servers) == 0 || len(spec.Servers) > 32 || len(spec.Profiles) != 3 {
		return nil, nil, ErrInvalidConfig
	}
	servers := make(map[string]compiledServer, len(spec.Servers))
	definitions := make(map[string]compiledTool)
	for _, server := range spec.Servers {
		if !safeName(server.Name, MaxProviderNameBytes) || len(server.Tools) == 0 || len(server.Tools) > 256 {
			return nil, nil, ErrInvalidConfig
		}
		if _, exists := servers[server.Name]; exists {
			return nil, nil, ErrInvalidConfig
		}
		endpoint, err := validatePublicEndpoint(server.Ref, "mcp")
		if err != nil {
			return nil, nil, ErrInvalidConfig
		}
		if server.CredentialsRef != "" && !safeCredentialRef(server.CredentialsRef) {
			return nil, nil, ErrInvalidConfig
		}
		servers[server.Name] = compiledServer{name: server.Name, endpoint: endpoint, credentialRef: server.CredentialsRef}
		for _, definition := range server.Tools {
			if !safeName(definition.Name, MaxToolNameBytes) || strings.Contains(definition.Name, "/") {
				return nil, nil, ErrInvalidConfig
			}
			if _, exists := definitions[definition.Name]; exists {
				// The local MCP protocol addresses tools by name. Ambiguity is
				// safer to reject than to invent a namespace convention.
				return nil, nil, ErrInvalidConfig
			}
			policyEffect, err := policyEffectFor(definition.Effect)
			if err != nil {
				return nil, nil, err
			}
			var exact json.RawMessage
			if definition.ExactArguments != nil {
				exact, err = encodeExactArguments(definition.ExactArguments)
				if err != nil {
					return nil, nil, ErrInvalidConfig
				}
			}
			definitions[definition.Name] = compiledTool{
				name: definition.Name, server: server.Name, effect: definition.Effect,
				policyEffect: policyEffect, exactArgs: exact,
			}
		}
	}

	profiles := make(map[v1alpha1.ToolProfileName]map[string]compiledTool, len(spec.Profiles))
	seenProfiles := make(map[v1alpha1.ToolProfileName]struct{}, len(spec.Profiles))
	for _, profile := range spec.Profiles {
		switch profile.Name {
		case v1alpha1.ToolProfileExplore, v1alpha1.ToolProfileEdit, v1alpha1.ToolProfileVerify:
		default:
			return nil, nil, ErrInvalidConfig
		}
		if _, exists := seenProfiles[profile.Name]; exists || len(profile.Tools) == 0 || len(profile.Tools) > int(spec.MaxToolsPerPhase) {
			return nil, nil, ErrInvalidConfig
		}
		seenProfiles[profile.Name] = struct{}{}
		compiled := make(map[string]compiledTool, len(profile.Tools))
		seenRefs := make(map[string]struct{}, len(profile.Tools))
		for _, ref := range profile.Tools {
			if !safeName(ref.Server, MaxProviderNameBytes) || !safeName(ref.Tool, MaxToolNameBytes) {
				return nil, nil, ErrInvalidConfig
			}
			refKey := ref.Server + "\x00" + ref.Tool
			if _, exists := seenRefs[refKey]; exists {
				return nil, nil, ErrInvalidConfig
			}
			seenRefs[refKey] = struct{}{}
			tool, exists := definitions[ref.Tool]
			if !exists || tool.server != ref.Server {
				return nil, nil, ErrInvalidConfig
			}
			compiled[ref.Tool] = cloneCompiledTool(tool)
		}
		profiles[profile.Name] = compiled
	}
	for _, required := range []v1alpha1.ToolProfileName{
		v1alpha1.ToolProfileExplore,
		v1alpha1.ToolProfileEdit,
		v1alpha1.ToolProfileVerify,
	} {
		if _, exists := seenProfiles[required]; !exists {
			return nil, nil, ErrInvalidConfig
		}
	}
	return servers, profiles, nil
}

func cloneCompiledTool(tool compiledTool) compiledTool {
	tool.exactArgs = append(json.RawMessage(nil), tool.exactArgs...)
	return tool
}

func flattenCompiledProfiles(profiles map[v1alpha1.ToolProfileName]map[string]compiledTool) map[string]compiledTool {
	tools := make(map[string]compiledTool)
	for _, profile := range profiles {
		for name, tool := range profile {
			if _, exists := tools[name]; !exists {
				tools[name] = cloneCompiledTool(tool)
			}
		}
	}
	return tools
}

func compileProviders(spec v1alpha1.ModelRouteSpec, custom map[string]string) ([]compiledProvider, error) {
	if len(spec.Providers) == 0 || len(spec.Providers) > 16 || spec.Budget.MaxCostUSD == "" {
		return nil, ErrInvalidConfig
	}
	providers := make([]compiledProvider, 0, len(spec.Providers))
	seenNames := make(map[string]struct{}, len(spec.Providers))
	seenModels := make(map[string]struct{}, len(spec.Providers))
	for _, provider := range spec.Providers {
		if !safeName(provider.Name, MaxProviderNameBytes) || provider.Kind == "" || provider.Model == "" || provider.Priority < 1 || provider.Priority > 1000 {
			return nil, ErrInvalidConfig
		}
		if _, exists := seenNames[provider.Name]; exists {
			return nil, ErrInvalidConfig
		}
		seenNames[provider.Name] = struct{}{}
		if !safeModel(provider.Model) {
			return nil, ErrInvalidConfig
		}
		if _, exists := seenModels[provider.Model]; exists {
			// A single model can be routed through more than one provider,
			// but that makes provider selection ambiguous for a small core
			// with no health/priority failover policy. Reject it explicitly.
			return nil, ErrInvalidConfig
		}
		seenModels[provider.Model] = struct{}{}
		if provider.CredentialRef != "" && !safeCredentialRef(provider.CredentialRef) {
			return nil, ErrInvalidConfig
		}
		endpoint := ""
		if custom != nil {
			endpoint = strings.TrimSpace(custom[provider.Name])
		}
		if endpoint == "" {
			switch provider.Kind {
			case "openrouter-responses":
				endpoint = "https://openrouter.ai/api/v1/responses"
			case "openai-responses":
				endpoint = "https://api.openai.com/v1/responses"
			case "openrouter-anthropic-messages":
				endpoint = "https://openrouter.ai/api/v1/messages"
			case "anthropic-messages":
				endpoint = "https://api.anthropic.com/v1/messages"
			default:
				return nil, ErrUnsupportedProvider
			}
		}
		var validated string
		var err error
		switch provider.Kind {
		case "openrouter-responses", "openai-responses":
			validated, err = validateResponsesProviderEndpoint(endpoint, provider.Kind)
		case "openrouter-anthropic-messages", "anthropic-messages":
			validated, err = validateAnthropicProviderEndpoint(endpoint, provider.Kind)
		default:
			return nil, ErrUnsupportedProvider
		}
		if err != nil {
			return nil, ErrInvalidConfig
		}
		providers = append(providers, compiledProvider{
			name: provider.Name, kind: provider.Kind, model: provider.Model,
			credentialRef: provider.CredentialRef, endpoint: validated, priority: provider.Priority,
		})
	}
	sort.SliceStable(providers, func(i, j int) bool {
		if providers[i].priority != providers[j].priority {
			return providers[i].priority < providers[j].priority
		}
		return providers[i].name < providers[j].name
	})
	return providers, nil
}

func policyEffectFor(effect v1alpha1.EffectKind) (toolpolicy.Effect, error) {
	switch effect {
	case v1alpha1.EffectRead:
		return toolpolicy.EffectRead, nil
	case v1alpha1.EffectWrite, v1alpha1.EffectPublish:
		return toolpolicy.EffectWrite, nil
	default:
		return "", ErrInvalidConfig
	}
}

func encodeExactArguments(arguments map[string]apix.JSON) (json.RawMessage, error) {
	if len(arguments) > 64 {
		return nil, ErrInvalidConfig
	}
	encoded, err := json.Marshal(arguments)
	if err != nil || strictjson.ValidateObject(encoded) != nil {
		return nil, ErrInvalidConfig
	}
	normalized, err := strictjson.Normalize(encoded)
	if err != nil || strictjson.ValidateObject(normalized) != nil {
		return nil, ErrInvalidConfig
	}
	return normalized, nil
}

func normalizeLimits(config Config) (limits, error) {
	toolCalls := config.MaxToolCalls
	if toolCalls == 0 {
		toolCalls = DefaultMaxToolCalls
	}
	if toolCalls < 1 || toolCalls > MaxConfiguredToolCalls {
		return limits{}, ErrInvalidConfig
	}
	mcpRequest := config.MaxMCPRequestBytes
	if mcpRequest == 0 {
		mcpRequest = DefaultMaxMCPRequestBytes
	}
	mcpResponse := config.MaxMCPResponseBytes
	if mcpResponse == 0 {
		mcpResponse = DefaultMaxMCPResponseBytes
	}
	toolResult := config.MaxToolResultBytes
	if toolResult == 0 {
		toolResult = DefaultMaxToolResultBytes
	}
	modelRequest := config.MaxModelRequestBytes
	if modelRequest == 0 {
		modelRequest = DefaultMaxModelRequestBytes
	}
	modelResponse := config.MaxModelResponseBytes
	if modelResponse == 0 {
		modelResponse = DefaultMaxModelResponseBytes
	}
	artifact := config.MaxArtifactBytes
	if artifact == 0 {
		artifact = DefaultMaxArtifactBytes
	}
	if mcpRequest < 256 || mcpRequest > strictjson.MaxDocumentBytes || mcpResponse < 256 || mcpResponse > 64<<20 || toolResult < 256 || toolResult > int64(strictjson.MaxDocumentBytes) || toolResult > int64(mcpResponse) || modelRequest < 256 || modelRequest > strictjson.MaxDocumentBytes || modelResponse < 256 || modelResponse > 128<<20 || artifact < 1 || artifact > 128<<20 {
		return limits{}, ErrInvalidConfig
	}
	return limits{maxToolCalls: toolCalls, maxMCPRequest: mcpRequest, maxMCPResponse: mcpResponse, maxToolResult: toolResult, maxModelRequest: modelRequest, maxModelResponse: modelResponse, maxArtifact: artifact}, nil
}

func hasCredentialRefs(servers map[string]compiledServer, providers []compiledProvider) bool {
	for _, server := range servers {
		if server.credentialRef != "" {
			return true
		}
	}
	for _, provider := range providers {
		if provider.credentialRef != "" {
			return true
		}
	}
	return false
}

func hasMutatingTools(tools map[string]compiledTool) bool {
	for _, tool := range tools {
		if tool.effect != v1alpha1.EffectRead {
			return true
		}
	}
	return false
}

func validRunIdentity(runUID, specDigest, baseSHA string) bool {
	return safeName(runUID, 128) && canonical.ValidDigest(specDigest) && validBaseSHA(baseSHA)
}

func validBaseSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func safeName(value string, max int) bool {
	if value == "" || len(value) > max || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

func safeCredentialRef(value string) bool {
	if value == "" || len(value) > 253 || strings.HasSuffix(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func safeModel(value string) bool {
	if value == "" || len(value) > MaxModelNameBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func parseMicros(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, ErrInvalidConfig
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || len(parts[0]) > 12 {
		return 0, ErrInvalidConfig
	}
	if len(parts[0]) > 1 && parts[0][0] == '0' {
		return 0, ErrInvalidConfig
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return 0, ErrInvalidConfig
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if fraction == "" || len(fraction) > 6 {
			return 0, ErrInvalidConfig
		}
		for _, r := range fraction {
			if r < '0' || r > '9' {
				return 0, ErrInvalidConfig
			}
		}
	}
	for len(fraction) < 6 {
		fraction += "0"
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > math.MaxInt64/1_000_000 {
		return 0, ErrInvalidConfig
	}
	frac, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil || whole*1_000_000 > math.MaxInt64-frac {
		return 0, ErrInvalidConfig
	}
	return whole*1_000_000 + frac, nil
}

func requestDigest(server, tool string, arguments []byte) string {
	sum := sha256.Sum256(append(append([]byte(server), 0), append(append([]byte(tool), 0), arguments...)...))
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func digestBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return canonical.DigestPrefix + hex.EncodeToString(sum[:])
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func boundedCredential(value []byte) bool {
	if len(value) == 0 || len(value) > MaxCredentialBytes {
		return false
	}
	for _, b := range value {
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func safeArtifactURI(value string) bool {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n?#@") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.Path == "" || strings.Contains(parsed.Path, "\\") || strings.Contains(parsed.Path, "/../") || strings.HasSuffix(parsed.Path, "/..") {
		return false
	}
	switch parsed.Scheme {
	case "s3", "https", "artifact":
		return safeHostname(parsed.Hostname())
	default:
		return false
	}
}

func validateArtifactReference(reference ArtifactReference, dataLength int) error {
	if !safeArtifactURI(reference.URI) || !canonical.ValidDigest(reference.Digest) || reference.SizeBytes != int64(dataLength) || reference.SizeBytes < 0 || reference.MediaType == "" || len(reference.MediaType) > 128 || strings.ContainsAny(reference.MediaType, "\x00\r\n") {
		return ErrArtifactUnavailable
	}
	return nil
}
