package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
)

func validConfigEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	writeConfigFile(t, filepath.Join(dir, "access"), "access-key")
	writeConfigFile(t, filepath.Join(dir, "secret"), "secret-key")
	return map[string]string{
		"AGW_RUN_UID":                        "run-uid",
		"AGW_SPEC_DIGEST":                    "sha256:" + repeatByte('a', 64),
		"AGW_BASE_SHA":                       repeatByte('b', 40),
		"AGW_TOOLSET_JSON":                   `{"servers":[{"name":"github","ref":"https://mcp.example.test/mcp","tools":[{"name":"get_issue","effect":"read"}]}]}`,
		"AGW_MODEL_ROUTE_JSON":               `{"providers":[{"name":"openrouter","kind":"openrouter-responses","model":"free/model","priority":1}],"budget":{"maxCostUsd":"0"}}`,
		"AGW_BROKER_SECRET_FILES":            `[]`,
		"AGW_MAX_TOOL_CALLS":                 "60",
		"AGW_MAX_COST_USD":                   "2.00",
		"AGW_MAX_MODEL_TOKENS":               "0",
		"AGW_OBJECT_STORE_BUCKET":            "agw-artifacts",
		"AGW_OBJECT_STORE_REGION":            "us-east-1",
		"AGW_OBJECT_STORE_PREFIX":            "agents-gateway/v3",
		"AGW_OBJECT_STORE_FORCE_PATH_STYLE":  "false",
		"AGW_OBJECT_STORE_ACCESS_KEY_FILE":   filepath.Join(dir, "access"),
		"AGW_OBJECT_STORE_SECRET_KEY_FILE":   filepath.Join(dir, "secret"),
		"AGW_OBJECT_STORE_MAX_BYTES":         "65536",
		"AGW_REQUIRE_APPROVAL_FOR_MUTATIONS": "true",
	}
}

func TestLoadConfigUsesGeneratedObjectStorePathStyleContract(t *testing.T) {
	env := validConfigEnv(t)
	env["AGW_OBJECT_STORE_FORCE_PATH_STYLE"] = "true"

	config, err := loadConfig(envLookup(env))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if !config.ObjectStore.ForcePathStyle {
		t.Fatal("ForcePathStyle = false, want true")
	}
}

func TestLoadConfigParsesImmutableRunLimits(t *testing.T) {
	env := validConfigEnv(t)
	env["AGW_WORKSPACE"] = "/workspace"
	env["AGW_BROKER_SCRATCH"] = "/run/agw/broker"
	env["AGW_MAX_TOOL_CALLS"] = "17"
	env["AGW_MAX_COST_USD"] = "0.125000"
	env["AGW_MAX_MODEL_TOKENS"] = "4096"
	config, err := loadConfig(envLookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxToolCalls != 17 || config.MaxCostUSD != "0.125000" || config.MaxModelTokens != 4096 {
		t.Fatalf("run limits=%+v", config)
	}
	if config.WorkspaceDir != "/workspace" || config.ResultScratchDir != "/run/agw/broker" {
		t.Fatalf("result paths=%+v", config)
	}
}

func TestLoadConfigRejectsUnprovenPhaseSupervisorConfiguration(t *testing.T) {
	for _, value := range []string{"", "/run/agw/phase.sock"} {
		t.Run(value, func(t *testing.T) {
			env := validConfigEnv(t)
			env["AGW_TRUSTED_PHASE_SOCKET"] = value
			_, err := loadConfig(envLookup(env))
			if err == nil || err.Error() != "configuration rejected: trusted_phase_supervisor_unsupported" {
				t.Fatalf("loadConfig error=%v, want explicit unsupported phase supervisor error", err)
			}
		})
	}
}

func TestLoadConfigRejectsUnsafeResultPaths(t *testing.T) {
	for _, test := range []struct {
		name  string
		field string
		value string
	}{
		{name: "workspace relative", field: "AGW_WORKSPACE", value: "workspace"},
		{name: "workspace root", field: "AGW_WORKSPACE", value: "/"},
		{name: "workspace traversal", field: "AGW_WORKSPACE", value: "/workspace/../tmp"},
		{name: "scratch relative", field: "AGW_BROKER_SCRATCH", value: "scratch"},
		{name: "scratch root", field: "AGW_BROKER_SCRATCH", value: "/"},
		{name: "scratch trailing slash", field: "AGW_BROKER_SCRATCH", value: "/run/agw/broker/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := validConfigEnv(t)
			env[test.field] = test.value
			if _, err := loadConfig(envLookup(env)); err == nil {
				t.Fatalf("loadConfig accepted unsafe %s path", test.field)
			}
		})
	}
}

func TestLoadConfigRejectsUnboundedImmutableRunLimits(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "missing tool calls", field: "AGW_MAX_TOOL_CALLS", value: ""},
		{name: "tool calls zero", field: "AGW_MAX_TOOL_CALLS", value: "0"},
		{name: "tool calls above maximum", field: "AGW_MAX_TOOL_CALLS", value: "10001"},
		{name: "cost malformed", field: "AGW_MAX_COST_USD", value: "01.00"},
		{name: "cost above maximum", field: "AGW_MAX_COST_USD", value: "1000000"},
		{name: "tokens negative", field: "AGW_MAX_MODEL_TOKENS", value: "-1"},
		{name: "tokens above maximum", field: "AGW_MAX_MODEL_TOKENS", value: "1000000001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := validConfigEnv(t)
			if test.value == "" {
				delete(env, test.field)
			} else {
				env[test.field] = test.value
			}
			if _, err := loadConfig(envLookup(env)); err == nil {
				t.Fatal("loadConfig accepted invalid immutable run limit")
			}
		})
	}
}

func TestLoadConfigRejectsObjectStoreLimitAboveGeneralCeiling(t *testing.T) {
	env := validConfigEnv(t)
	value := strconv.FormatInt(objectstore.GeneralMaxObjectBytes+1, 10)
	env["AGW_OBJECT_STORE_MAX_BYTES"] = value

	_, err := loadConfig(envLookup(env))
	if err == nil || err.Error() != "configuration rejected: numeric_configuration_invalid" {
		t.Fatalf("loadConfig error=%v, want bounded numeric configuration error", err)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatalf("loadConfig error exposed configured size: %v", err)
	}
}

func TestLoadConfigAcceptsGeneralObjectStoreLimitAtConstructionBoundary(t *testing.T) {
	env := validConfigEnv(t)
	env["AGW_OBJECT_STORE_MAX_BYTES"] = strconv.FormatInt(objectstore.GeneralMaxObjectBytes, 10)

	config, err := loadConfig(envLookup(env))
	if err != nil {
		t.Fatalf("loadConfig() rejected the shared boundary: %v", err)
	}
	if config.ObjectStore.MaxObjectBytes != objectstore.GeneralMaxObjectBytes {
		t.Fatalf("parsed object limit=%d, want %d", config.ObjectStore.MaxObjectBytes, objectstore.GeneralMaxObjectBytes)
	}
	if _, err := newObjectStore(context.Background(), config.ObjectStore); err != nil {
		t.Fatalf("object-store construction rejected a parser-accepted boundary: %v", err)
	}
}

func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envLookup(values map[string]string) lookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func repeatByte(value byte, count int) string {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return string(result)
}

func TestLoadConfigRequiresPricingForNonzeroBudget(t *testing.T) {
	env := validConfigEnv(t)
	env["AGW_MODEL_ROUTE_JSON"] = `{"providers":[{"name":"openrouter","kind":"openrouter-responses","model":"free/model","priority":1}],"budget":{"maxCostUsd":"2.00"}}`
	if _, err := loadConfig(envLookup(env)); err == nil || err.Error() != "configuration rejected: pricing_required_for_nonzero_budget" {
		t.Fatalf("loadConfig() error = %v", err)
	}
}

func TestLoadConfigBindsExactServerIdentity(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "digest", field: "AGW_SPEC_DIGEST", value: "sha256:not-a-digest"},
		{name: "base uppercase", field: "AGW_BASE_SHA", value: repeatByte('B', 40)},
		{name: "uid whitespace", field: "AGW_RUN_UID", value: "run uid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := validConfigEnv(t)
			env[test.field] = test.value
			if _, err := loadConfig(envLookup(env)); err == nil || err.Error() != "configuration rejected: run_identity_invalid" {
				t.Fatalf("loadConfig() error = %v", err)
			}
		})
	}
}

func TestLoadConfigRequiresExactPricingProviders(t *testing.T) {
	env := validConfigEnv(t)
	env["AGW_MODEL_ROUTE_JSON"] = `{"providers":[{"name":"openrouter","kind":"openrouter-responses","model":"free/model","priority":1}],"budget":{"maxCostUsd":"2.00"}}`
	env["AGW_PRICING_JSON"] = `{"other":{"inputMicrosPerToken":0,"outputMicrosPerToken":0}}`
	if _, err := loadConfig(envLookup(env)); err == nil || err.Error() != "configuration rejected: pricing_provider_missing" {
		t.Fatalf("loadConfig() error = %v", err)
	}
	env["AGW_PRICING_JSON"] = `{"openrouter":{"inputMicrosPerToken":0,"outputMicrosPerToken":0}}`
	config, err := loadConfig(envLookup(env))
	if err != nil || config.Pricing["openrouter"].InputMicrosPerToken != 0 {
		t.Fatalf("loadConfig() = %#v, %v", config, err)
	}
}

func TestLoadConfigRejectsUnknownAndDuplicateJSON(t *testing.T) {
	tests := []struct {
		name  string
		field string
		body  string
	}{
		{name: "toolset unknown", field: "AGW_TOOLSET_JSON", body: `{"servers":[],"extra":true}`},
		{name: "model route unknown", field: "AGW_MODEL_ROUTE_JSON", body: `{"providers":[],"budget":{"maxCostUsd":"0"},"extra":true}`},
		{name: "duplicate toolset", field: "AGW_TOOLSET_JSON", body: `{"servers":[],"servers":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := validConfigEnv(t)
			env[test.field] = test.body
			if _, err := loadConfig(envLookup(env)); err == nil {
				t.Fatal("loadConfig accepted invalid JSON")
			}
		})
	}
}

func TestLoadConfigRejectsBroadCredentialEnvironment(t *testing.T) {
	env := validConfigEnv(t)
	env["AWS_ACCESS_KEY_ID"] = "must-not-be-read"
	if _, err := loadConfig(envLookup(env)); err == nil || err.Error() != "configuration rejected: object_store_default_credential_chain_disabled" {
		t.Fatalf("loadConfig() error = %v", err)
	}
}

func TestLoadConfigBindsLogicalCredentialToProjectedFile(t *testing.T) {
	env := validConfigEnv(t)
	logicalRef := "mcp-github"
	dir := t.TempDir()
	credentialPath := filepath.Join(dir, "item-0")
	writeConfigFile(t, credentialPath, "token-value")
	env["AGW_CREDENTIALS_DIR"] = dir
	env["AGW_TOOLSET_JSON"] = `{"servers":[{"name":"github","ref":"https://mcp.example.test/mcp","credentialsRef":"mcp-github","tools":[{"name":"get_issue","effect":"read"}]}]}`
	env["AGW_BROKER_SECRET_FILES"] = `[{"key":"` + credentialProjectedKey(logicalRef) + `","path":"item-0"}]`
	config, err := loadConfig(envLookup(env))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	resolver, err := newFileCredentialResolver(config.ToolSet, config.ModelRoute, config.SecretFiles, config.CredentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	value, err := resolver.ResolveToken(nil, logicalRef)
	if err == nil || value != nil {
		t.Fatal("nil context should not read a credential")
	}
	value, err = resolver.ResolveToken(context.Background(), logicalRef)
	if err != nil || string(value) != "token-value" {
		t.Fatalf("ResolveToken() = %q, %v", value, err)
	}
	wipeBytes(value)
	if got := string(readConfigBytes(t, credentialPath)); got != "token-value" {
		t.Fatalf("resolver altered source file: %q", got)
	}
}

func TestParseSecretFileMapRejectsPathReordering(t *testing.T) {
	_, err := parseSecretFileMap([]byte(`[{"key":"broker-` + repeatByte('a', 64) + `","path":"item-1"}]`))
	if err == nil {
		t.Fatal("parseSecretFileMap accepted a non-canonical projected path")
	}
}

func TestReadBoundedFileRejectsSymlinkAndNonPrintable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	writeConfigFile(t, target, "secret")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(link, 64, true); err == nil {
		t.Fatal("readBoundedFile accepted a symlink")
	}
	bad := filepath.Join(dir, "bad")
	writeConfigFile(t, bad, "secret\n")
	if _, err := readBoundedFile(bad, 64, true); err == nil {
		t.Fatal("readBoundedFile accepted non-printable credential bytes")
	}
}

func readConfigBytes(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
