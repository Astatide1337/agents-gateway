package admission

import (
	"strings"
	"testing"

	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestValidateMCPEndpoint(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		code Code
	}{
		{name: "http", ref: "http://mcp.example.com/mcp", code: CodeMCPServerEndpointNotHTTPS},
		{name: "userinfo", ref: "https://agent:password@mcp.example.com/mcp", code: CodeMCPServerEndpointUserInfo},
		{name: "query", ref: "https://mcp.example.com/mcp?token=redacted", code: CodeMCPServerEndpointQuery},
		{name: "empty query", ref: "https://mcp.example.com/mcp?", code: CodeMCPServerEndpointQuery},
		{name: "fragment", ref: "https://mcp.example.com/mcp#tools", code: CodeMCPServerEndpointFragment},
		{name: "empty fragment", ref: "https://mcp.example.com/mcp#", code: CodeMCPServerEndpointFragment},
		{name: "localhost", ref: "https://localhost/mcp", code: CodeMCPServerEndpointLocal},
		{name: "nested localhost", ref: "https://mcp.localhost/mcp", code: CodeMCPServerEndpointLocal},
		{name: "local domain", ref: "https://mcp.local/mcp", code: CodeMCPServerEndpointLocal},
		{name: "loopback IPv4", ref: "https://127.0.0.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "private IPv4 10/8", ref: "https://10.0.0.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "private IPv4 172.16/12", ref: "https://172.16.0.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "private IPv4 192.168/16", ref: "https://192.168.1.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "link-local IPv4", ref: "https://169.254.1.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "unspecified IPv4", ref: "https://0.0.0.0/mcp", code: CodeMCPServerEndpointAddress},
		{name: "multicast IPv4", ref: "https://224.0.0.1/mcp", code: CodeMCPServerEndpointAddress},
		{name: "loopback IPv6", ref: "https://[::1]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "IPv4-mapped loopback", ref: "https://[::ffff:127.0.0.1]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "private IPv6", ref: "https://[fd00::1]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "link-local IPv6", ref: "https://[fe80::1]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "zoned link-local IPv6", ref: "https://[fe80::1%25eth0]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "unspecified IPv6", ref: "https://[::]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "multicast IPv6", ref: "https://[ff02::1]/mcp", code: CodeMCPServerEndpointAddress},
		{name: "relative URL", ref: "mcp.example.com/mcp", code: CodeMCPServerEndpointInvalid},
		{name: "missing host", ref: "https:///mcp", code: CodeMCPServerEndpointHost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, message := validateMCPEndpoint(test.ref)
			if code != test.code {
				t.Fatalf("code=%q message=%q, want code %q", code, message, test.code)
			}
			if strings.TrimSpace(message) == "" {
				t.Fatal("endpoint violation has no diagnostic message")
			}
		})
	}
}

func TestValidateMCPEndpointDoesNotResolveDNS(t *testing.T) {
	if code, message := validateMCPEndpoint("https://definitely-does-not-resolve.invalid/mcp"); code != "" {
		t.Fatalf("unresolved hostname was rejected: code=%q message=%q", code, message)
	}
}

func TestValidateToolSetSecurityExactArguments(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		value      string
		wantCode   Code
		wantNoCode Code
	}{
		{name: "token", key: "token", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "case variant", key: "TOKEN", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "separator variant", key: "token_value", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "password", key: "password", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "secret", key: "client-secret", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "credential", key: "credential_ref", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "camel api key", key: "apiKey", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "separator api key", key: "api-key", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "authorization header", key: "authorizationHeader", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "cookie", key: "cookie_header", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "private key", key: "private_key", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "access key", key: "access_key_id", value: `"value"`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "nested object", key: "request", value: `{"headers":{"client-secret":"value"}}`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "nested array", key: "request", value: `[{"accessKey":"value"}]`, wantCode: CodeCredentialLikeArgumentKey},
		{name: "safe argument", key: "contactId", value: `"123"`, wantNoCode: CodeCredentialLikeArgumentKey},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations := ValidateToolSetSecurity(&v1alpha1.ToolSet{Spec: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{
				Name: "github",
				Ref:  "https://mcp.example.com/mcp",
				Tools: []v1alpha1.ToolDefinition{{
					Name:           "get_issue",
					ExactArguments: map[string]apix.JSON{test.key: {Raw: []byte(test.value)}},
				}},
			}}}})
			if test.wantCode != "" && !hasCode(violations, test.wantCode) {
				t.Fatalf("violations=%#v, want code %q", violations, test.wantCode)
			}
			if test.wantNoCode != "" && hasCode(violations, test.wantNoCode) {
				t.Fatalf("violations=%#v, did not want code %q", violations, test.wantNoCode)
			}
		})
	}
}

func TestValidateToolSetSecurityRejectsMalformedExactArgumentsJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "malformed", raw: []byte(`{"unterminated"`)},
		{name: "concatenated", raw: []byte(`"one" "two"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations := ValidateToolSetSecurity(&v1alpha1.ToolSet{Spec: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{{
				Ref: "https://mcp.example.com/mcp",
				Tools: []v1alpha1.ToolDefinition{{
					ExactArguments: map[string]apix.JSON{"contactId": {Raw: test.raw}},
				}},
			}}}})
			if !hasCode(violations, CodeExactArgumentsInvalid) {
				t.Fatalf("violations=%#v, want %q", violations, CodeExactArgumentsInvalid)
			}
		})
	}
}

func TestValidateToolSetSecurityEndpointIntegration(t *testing.T) {
	violations := ValidateToolSetSecurity(&v1alpha1.ToolSet{Spec: v1alpha1.ToolSetSpec{Servers: []v1alpha1.ToolServer{
		{Name: "safe", Ref: "https://mcp.example.com/mcp"},
		{Name: "unsafe", Ref: "https://192.168.1.1/mcp"},
	}}})
	if !hasCode(violations, CodeMCPServerEndpointAddress) {
		t.Fatalf("violations=%#v, want literal-address rejection", violations)
	}
}
