// Package toolsecurity contains the shared, dependency-free ToolSet boundary
// checks used both before admission and while resolving an immutable run.
//
// These checks are intentionally syntactic. DNS resolution and connection-time
// address validation belong to the broker, where they can be repeated after
// every lookup and redirect decision.
package toolsecurity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"unicode"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

// Violation is a bounded, value-only ToolSet security diagnostic. Code is a
// stable string so callers in other packages do not need to depend on the
// admission package.
type Violation struct {
	Code    string
	Field   string
	Message string
}

func ValidateToolSetSecurity(toolSet *v1alpha1.ToolSet) []Violation {
	if toolSet == nil {
		return []Violation{{Code: "InvalidObject", Field: "toolSet", Message: "ToolSet is required"}}
	}
	violations := make([]Violation, 0)
	for serverIndex, server := range toolSet.Spec.Servers {
		endpointField := fmt.Sprintf("spec.servers[%d].ref", serverIndex)
		if code, message := ValidateMCPEndpoint(server.Ref); code != "" {
			violations = append(violations, Violation{Code: code, Field: endpointField, Message: message})
		}
		for toolIndex, tool := range server.Tools {
			field := fmt.Sprintf("spec.servers[%d].tools[%d].exactArguments", serverIndex, toolIndex)
			violations = append(violations, ValidateExactArguments(field, tool.ExactArguments)...)
		}
	}
	return violations
}

func ValidateMCPEndpoint(raw string) (string, string) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" {
		return "MCPServerEndpointInvalid", "MCP server endpoint must be a valid absolute HTTPS URL"
	}
	if parsed.Scheme == "" {
		return "MCPServerEndpointInvalid", "MCP server endpoint must be an absolute HTTPS URL"
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "MCPServerEndpointNotHTTPS", "MCP server endpoint must use HTTPS"
	}
	if parsed.User != nil {
		return "MCPServerEndpointUserInfo", "MCP server endpoint must not contain userinfo"
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return "MCPServerEndpointQuery", "MCP server endpoint must not contain a query"
	}
	if parsed.Fragment != "" || strings.Contains(raw, "#") {
		return "MCPServerEndpointFragment", "MCP server endpoint must not contain a fragment"
	}
	if parsed.Host == "" {
		return "MCPServerEndpointHost", "MCP server endpoint must contain a host"
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if hostname == "" {
		return "MCPServerEndpointHost", "MCP server endpoint must contain a host"
	}
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || hostname == "local" || strings.HasSuffix(hostname, ".local") {
		return "MCPServerEndpointLocal", "MCP server endpoint must not target localhost or a .local host"
	}
	ipHost := hostname
	if zoneIndex := strings.LastIndexByte(ipHost, '%'); zoneIndex >= 0 {
		ipHost = ipHost[:zoneIndex]
	}
	if ip := net.ParseIP(ipHost); ip != nil && forbiddenLiteralIP(ip) {
		return "MCPServerEndpointAddress", "MCP server endpoint must not target a loopback, private, link-local, unspecified, or multicast IP"
	}
	return "", ""
}

func ValidateExactArguments(field string, arguments map[string]apix.JSON) []Violation {
	if arguments == nil {
		return nil
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return []Violation{{Code: "ExactArgumentsInvalid", Field: field, Message: "exactArguments must be a JSON object"}}
	}
	decoded, err := decodeSingleJSON(encoded)
	if err != nil {
		return []Violation{{Code: "ExactArgumentsInvalid", Field: field, Message: "exactArguments must be a JSON object"}}
	}
	if _, ok := decoded.(map[string]any); !ok {
		return []Violation{{Code: "ExactArgumentsInvalid", Field: field, Message: "exactArguments must be a JSON object"}}
	}
	violations := make([]Violation, 0)
	for _, key := range sortedJSONKeys(arguments) {
		keyField := exactArgumentField(field, key)
		if credentialLikeArgumentKey(key) {
			violations = append(violations, Violation{Code: "CredentialLikeArgumentKey", Field: keyField, Message: "exactArguments contains a credential-like argument key"})
		}
		value, err := decodeSingleJSON(arguments[key].Raw)
		if err != nil {
			violations = append(violations, Violation{Code: "ExactArgumentsInvalid", Field: keyField, Message: "exactArguments values must contain exactly one valid JSON value"})
			continue
		}
		appendNestedCredentialKeyViolations(&violations, keyField, value)
	}
	return violations
}

func forbiddenLiteralIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil {
		ip = ipv4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func sortedJSONKeys(arguments map[string]apix.JSON) []string {
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decodeSingleJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func appendNestedCredentialKeyViolations(violations *[]Violation, field string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			keyField := exactArgumentField(field, key)
			if credentialLikeArgumentKey(key) {
				*violations = append(*violations, Violation{Code: "CredentialLikeArgumentKey", Field: keyField, Message: "exactArguments contains a credential-like argument key"})
			}
			appendNestedCredentialKeyViolations(violations, keyField, typed[key])
		}
	case []any:
		for index, item := range typed {
			appendNestedCredentialKeyViolations(violations, fmt.Sprintf("%s[%d]", field, index), item)
		}
	}
}

func exactArgumentField(parent, key string) string {
	return fmt.Sprintf(`%s["%s"]`, parent, key)
}

func credentialLikeArgumentKey(key string) bool {
	normalized := normalizeCredentialKey(key)
	if normalized == "" {
		return false
	}
	for _, marker := range credentialKeyMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func normalizeCredentialKey(key string) string {
	var builder strings.Builder
	for _, runeValue := range strings.ToLower(key) {
		if unicode.IsLetter(runeValue) || unicode.IsDigit(runeValue) {
			builder.WriteRune(runeValue)
		}
	}
	return builder.String()
}

var credentialKeyMarkers = []string{"token", "password", "secret", "credential", "apikey", "authorization", "cookie", "privatekey", "accesskey"}
