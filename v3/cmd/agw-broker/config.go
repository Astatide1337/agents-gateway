package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/broker"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/objectstore"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	defaultListenAddress  = "127.0.0.1:8081"
	defaultCredentialsDir = "/run/agw/credentials"
	defaultEffectsPrefix  = "effects"

	maxIdentityBytes         = 256
	maxConfigValueBytes      = 1 << 20
	maxSecretFileMapBytes    = 64 << 10
	maxPricingBytes          = 128 << 10
	maxPathBytes             = 1024
	maxProjectedFiles        = 256
	maxPricingEntries        = 64
	maxPricingMicrosPerToken = int64(1_000_000_000_000)
	maxRunCostUSDBytes       = 16
	maxRunCostMicros         = int64(999_999_999_999)
)

type lookupEnv func(string) (string, bool)

// configError intentionally exposes only a stable code. Values from the
// environment, paths, URLs, provider responses, and filesystem errors never
// reach process diagnostics or HTTP responses.
type configError struct{ code string }

func (e configError) Error() string { return "configuration rejected: " + e.code }

type brokerConfig struct {
	ListenAddress string
	RunUID        string
	SpecDigest    string
	BaseSHA       string
	ToolSet       v1alpha1.ToolSetSpec
	ModelRoute    v1alpha1.ModelRouteSpec

	CredentialsDir   string
	WorkspaceDir     string
	ContextPackDir   string
	ResultScratchDir string
	SecretFiles      []secretFileSpec
	Pricing          broker.PricingTable

	ObjectStore     objectStoreConfig
	Effects         string
	RequireApproval bool

	MaxToolCalls   int64
	MaxCostUSD     string
	MaxModelTokens int64
}

type objectStoreConfig struct {
	Bucket         string
	Region         string
	Prefix         string
	Endpoint       string
	ForcePathStyle bool
	MaxObjectBytes int64

	AccessKeyFile    string
	SecretKeyFile    string
	SessionTokenFile string
}

func loadConfig(lookup lookupEnv) (brokerConfig, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	// The command has no deployable trusted phase supervisor. Reject the
	// retired socket setting instead of silently accepting a configuration that
	// looks like it enables Explore -> Edit without an independent supervisor.
	if _, configured := lookup("AGW_TRUSTED_PHASE_SOCKET"); configured {
		return brokerConfig{}, configError{code: "trusted_phase_supervisor_unsupported"}
	}
	if hasBroadCredentialEnvironment(lookup) {
		return brokerConfig{}, configError{code: "object_store_default_credential_chain_disabled"}
	}

	listen, err := optionalEnv(lookup, "AGW_LISTEN_ADDRESS", maxPathBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if listen == "" {
		listen = defaultListenAddress
	}
	if !validLoopbackAddress(listen) {
		return brokerConfig{}, configError{code: "listen_address_must_be_127_0_0_1"}
	}

	runUID, err := requiredEnv(lookup, "AGW_RUN_UID", maxIdentityBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	specDigest, err := requiredEnv(lookup, "AGW_SPEC_DIGEST", maxIdentityBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	baseSHA, err := requiredEnv(lookup, "AGW_BASE_SHA", maxIdentityBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if !validServerIdentity(runUID, specDigest, baseSHA) {
		return brokerConfig{}, configError{code: "run_identity_invalid"}
	}

	toolSetRaw, err := requiredEnv(lookup, "AGW_TOOLSET_JSON", strictjson.MaxDocumentBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	modelRouteRaw, err := requiredEnv(lookup, "AGW_MODEL_ROUTE_JSON", strictjson.MaxDocumentBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	var toolSet v1alpha1.ToolSetSpec
	if err := decodeStrictObject([]byte(toolSetRaw), &toolSet); err != nil {
		return brokerConfig{}, configError{code: "toolset_json_invalid"}
	}
	var modelRoute v1alpha1.ModelRouteSpec
	if err := decodeStrictObject([]byte(modelRouteRaw), &modelRoute); err != nil {
		return brokerConfig{}, configError{code: "model_route_json_invalid"}
	}

	secretMapRaw, err := requiredEnv(lookup, "AGW_BROKER_SECRET_FILES", maxSecretFileMapBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	secretFiles, err := parseSecretFileMap([]byte(secretMapRaw))
	if err != nil {
		return brokerConfig{}, configError{code: "broker_secret_file_map_invalid"}
	}
	credentialsDir, err := optionalEnv(lookup, "AGW_CREDENTIALS_DIR", maxPathBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if credentialsDir == "" {
		credentialsDir = defaultCredentialsDir
	}
	if !validAbsoluteDirectory(credentialsDir) {
		return brokerConfig{}, configError{code: "credentials_directory_invalid"}
	}
	workspaceDir, err := optionalEnv(lookup, "AGW_WORKSPACE", maxPathBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if workspaceDir == "" {
		workspaceDir = "/workspace"
	}
	if !validAbsoluteDirectory(workspaceDir) {
		return brokerConfig{}, configError{code: "workspace_directory_invalid"}
	}
	contextPackDir, err := optionalEnv(lookup, "AGW_CONTEXT_PACK_DIR", maxPathBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if contextPackDir == "" {
		contextPackDir = "/opt/agw/context-pack"
	}
	if !validAbsoluteDirectory(contextPackDir) || contextPackDir == workspaceDir {
		return brokerConfig{}, configError{code: "context_pack_directory_invalid"}
	}
	resultScratchDir, err := optionalEnv(lookup, "AGW_BROKER_SCRATCH", maxPathBytes)
	if err != nil {
		return brokerConfig{}, err
	}
	if resultScratchDir != "" && !validAbsoluteDirectory(resultScratchDir) {
		return brokerConfig{}, configError{code: "result_scratch_directory_invalid"}
	}
	if err := validateCredentialContract(toolSet, modelRoute, secretFiles, credentialsDir); err != nil {
		return brokerConfig{}, err
	}
	maxToolCalls, err := requiredBoundedInt(lookup, "AGW_MAX_TOOL_CALLS", 1, broker.MaxConfiguredToolCalls)
	if err != nil {
		return brokerConfig{}, err
	}
	maxCostUSD, err := requiredBoundedCostUSD(lookup, "AGW_MAX_COST_USD")
	if err != nil {
		return brokerConfig{}, err
	}
	maxModelTokens, err := requiredBoundedInt(lookup, "AGW_MAX_MODEL_TOKENS", 0, broker.MaxConfiguredModelTokens)
	if err != nil {
		return brokerConfig{}, err
	}

	pricing, err := loadPricing(lookup, modelRoute)
	if err != nil {
		return brokerConfig{}, err
	}

	storeConfig, err := loadObjectStoreConfig(lookup)
	if err != nil {
		return brokerConfig{}, err
	}
	effectPrefix, err := optionalEnv(lookup, "AGW_EFFECTS_PREFIX", 512)
	if err != nil {
		return brokerConfig{}, err
	}
	if effectPrefix == "" {
		effectPrefix = defaultEffectsPrefix
	}
	if !validObjectPrefix(effectPrefix) {
		return brokerConfig{}, configError{code: "effect_prefix_invalid"}
	}
	requireApproval, err := optionalBool(lookup, "AGW_REQUIRE_APPROVAL_FOR_MUTATIONS", true)
	if err != nil {
		return brokerConfig{}, err
	}

	return brokerConfig{
		ListenAddress: listen, RunUID: runUID, SpecDigest: specDigest, BaseSHA: baseSHA,
		ToolSet: toolSet, ModelRoute: modelRoute, CredentialsDir: credentialsDir,
		WorkspaceDir: workspaceDir, ContextPackDir: contextPackDir, ResultScratchDir: resultScratchDir,
		SecretFiles: secretFiles, Pricing: pricing, ObjectStore: storeConfig,
		Effects: effectPrefix, RequireApproval: requireApproval,
		MaxToolCalls: maxToolCalls, MaxCostUSD: maxCostUSD, MaxModelTokens: maxModelTokens,
	}, nil
}

func requiredEnv(lookup lookupEnv, name string, max int) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", configError{code: "required_configuration_missing"}
	}
	if len(value) > max || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", configError{code: "configuration_value_out_of_bounds"}
	}
	return value, nil
}

func optionalEnv(lookup lookupEnv, name string, max int) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", nil
	}
	if len(value) > max || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", configError{code: "configuration_value_out_of_bounds"}
	}
	return value, nil
}

func optionalBool(lookup lookupEnv, name string, fallback bool) (bool, error) {
	value, err := optionalEnv(lookup, name, 5)
	if err != nil {
		return false, err
	}
	if value == "" {
		return fallback, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, configError{code: "boolean_configuration_invalid"}
	}
}

func parseBoundedInt(lookup lookupEnv, name string, fallback, min, max int64) (int64, error) {
	value, err := optionalEnv(lookup, name, 32)
	if err != nil {
		return 0, err
	}
	if value == "" {
		return fallback, nil
	}
	parsed, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || parsed < min || parsed > max {
		return 0, configError{code: "numeric_configuration_invalid"}
	}
	return parsed, nil
}

func requiredBoundedInt(lookup lookupEnv, name string, min, max int64) (int64, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return 0, configError{code: "required_configuration_missing"}
	}
	if len(value) > 32 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return 0, configError{code: "configuration_value_out_of_bounds"}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < min || parsed > max {
		return 0, configError{code: "numeric_configuration_invalid"}
	}
	return parsed, nil
}

func requiredBoundedCostUSD(lookup lookupEnv, name string) (string, error) {
	value, err := requiredEnv(lookup, name, maxRunCostUSDBytes)
	if err != nil {
		return "", err
	}
	if !validBoundedCostUSD(value) {
		return "", configError{code: "numeric_configuration_invalid"}
	}
	return value, nil
}

func validBoundedCostUSD(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 6 || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return false
	}
	if len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > 6) {
		return false
	}
	for _, part := range parts {
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > 999999 {
		return false
	}
	if len(parts) == 1 {
		return whole*1_000_000 <= maxRunCostMicros
	}
	fraction := parts[1]
	for len(fraction) < 6 {
		fraction += "0"
	}
	frac, err := strconv.ParseInt(fraction, 10, 64)
	return err == nil && whole*1_000_000 <= maxRunCostMicros-frac
}

func decodeStrictObject(body []byte, target any) error {
	if len(body) == 0 || strictjson.ValidateObject(body) != nil {
		return configError{code: "strict_json_invalid"}
	}
	return decodeStrict(body, target)
}

func decodeStrictValue(body []byte, target any) error {
	if len(body) == 0 || strictjson.Validate(body) != nil {
		return configError{code: "strict_json_invalid"}
	}
	return decodeStrict(body, target)
}

func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return configError{code: "strict_json_decode_failed"}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return configError{code: "strict_json_trailing_data"}
	}
	return nil
}

func loadPricing(lookup lookupEnv, route v1alpha1.ModelRouteSpec) (broker.PricingTable, error) {
	inline, err := optionalEnv(lookup, "AGW_PRICING_JSON", maxPricingBytes)
	if err != nil {
		return nil, err
	}
	file, err := optionalEnv(lookup, "AGW_PRICING_FILE", maxPathBytes)
	if err != nil {
		return nil, err
	}
	if inline != "" && file != "" {
		return nil, configError{code: "pricing_sources_ambiguous"}
	}
	var body []byte
	if inline != "" {
		body = []byte(inline)
	} else if file != "" {
		body, err = readBoundedFile(file, maxPricingBytes, false)
		if err != nil {
			return nil, configError{code: "pricing_file_unreadable"}
		}
		defer wipeBytes(body)
	}
	nonzeroBudget := nonzeroDecimal(route.Budget.MaxCostUSD)
	if len(body) == 0 {
		if nonzeroBudget {
			return nil, configError{code: "pricing_required_for_nonzero_budget"}
		}
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if err := decodeStrictObject(body, &entries); err != nil || len(entries) > maxPricingEntries {
		return nil, configError{code: "pricing_json_invalid"}
	}
	table := make(broker.PricingTable, len(entries))
	for provider, raw := range entries {
		if !validProviderName(provider) {
			return nil, configError{code: "pricing_provider_invalid"}
		}
		var entry struct {
			InputMicrosPerToken  int64 `json:"inputMicrosPerToken"`
			OutputMicrosPerToken int64 `json:"outputMicrosPerToken"`
		}
		if err := decodeStrictObject(raw, &entry); err != nil || entry.InputMicrosPerToken < 0 || entry.OutputMicrosPerToken < 0 || entry.InputMicrosPerToken > maxPricingMicrosPerToken || entry.OutputMicrosPerToken > maxPricingMicrosPerToken {
			return nil, configError{code: "pricing_entry_invalid"}
		}
		table[provider] = broker.Pricing{InputMicrosPerToken: entry.InputMicrosPerToken, OutputMicrosPerToken: entry.OutputMicrosPerToken}
	}
	if nonzeroBudget {
		for _, provider := range route.Providers {
			if _, ok := table[provider.Name]; !ok {
				return nil, configError{code: "pricing_provider_missing"}
			}
		}
	} else {
		for provider := range table {
			found := false
			for _, configured := range route.Providers {
				if configured.Name == provider {
					found = true
					break
				}
			}
			if !found {
				return nil, configError{code: "pricing_provider_unknown"}
			}
		}
	}
	return table, nil
}

func loadObjectStoreConfig(lookup lookupEnv) (objectStoreConfig, error) {
	bucket, err := requiredEnv(lookup, "AGW_OBJECT_STORE_BUCKET", 63)
	if err != nil {
		return objectStoreConfig{}, err
	}
	region, err := requiredEnv(lookup, "AGW_OBJECT_STORE_REGION", 64)
	if err != nil {
		return objectStoreConfig{}, err
	}
	prefix, err := requiredEnv(lookup, "AGW_OBJECT_STORE_PREFIX", 1024)
	if err != nil {
		return objectStoreConfig{}, err
	}
	if !validObjectPrefix(prefix) {
		return objectStoreConfig{}, configError{code: "object_store_prefix_invalid"}
	}
	endpoint, err := optionalEnv(lookup, "AGW_OBJECT_STORE_ENDPOINT", 512)
	if err != nil {
		return objectStoreConfig{}, err
	}
	pathStyle, err := optionalBool(lookup, "AGW_OBJECT_STORE_PATH_STYLE", false)
	if err != nil {
		return objectStoreConfig{}, err
	}
	maxBytes, err := parseBoundedInt(lookup, "AGW_OBJECT_STORE_MAX_BYTES", objectstore.GeneralMaxObjectBytes, 1, objectstore.GeneralMaxObjectBytes)
	if err != nil {
		return objectStoreConfig{}, err
	}
	accessFile, err := requiredObjectStorePath(lookup, "AGW_OBJECT_STORE_ACCESS_KEY_FILE")
	if err != nil {
		return objectStoreConfig{}, err
	}
	secretFile, err := requiredObjectStorePath(lookup, "AGW_OBJECT_STORE_SECRET_KEY_FILE")
	if err != nil {
		return objectStoreConfig{}, err
	}
	sessionFile, err := optionalPathEnv(lookup, "AGW_OBJECT_STORE_SESSION_TOKEN_FILE")
	if err != nil {
		return objectStoreConfig{}, err
	}
	if sessionFile == accessFile || sessionFile == secretFile || accessFile == secretFile {
		return objectStoreConfig{}, configError{code: "object_store_credential_files_must_be_distinct"}
	}
	return objectStoreConfig{
		Bucket: bucket, Region: region, Prefix: prefix, Endpoint: endpoint,
		ForcePathStyle: pathStyle, MaxObjectBytes: maxBytes,
		AccessKeyFile: accessFile, SecretKeyFile: secretFile, SessionTokenFile: sessionFile,
	}, nil
}

func requiredObjectStorePath(lookup lookupEnv, name string) (string, error) {
	value, ok := lookup(name)
	if !ok || value == "" {
		return "", configError{code: "object_store_scoped_credentials_required"}
	}
	if len(value) > maxPathBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return "", configError{code: "credential_file_path_invalid"}
	}
	if !validFilePath(value) {
		return "", configError{code: "credential_file_path_invalid"}
	}
	return value, nil
}

func optionalPathEnv(lookup lookupEnv, name string) (string, error) {
	value, err := optionalEnv(lookup, name, maxPathBytes)
	if err != nil {
		return "", err
	}
	if value != "" && !validFilePath(value) {
		return "", configError{code: "credential_file_path_invalid"}
	}
	return value, nil
}

func validFilePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\r\n") && !strings.Contains(value, "/../") && !strings.HasSuffix(value, "/..")
}

func validAbsoluteDirectory(value string) bool {
	return validFilePath(value) && value != "/" && !strings.HasSuffix(value, "/")
}

func validObjectPrefix(value string) bool {
	if value == "" || len(value) > 1024 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return false
		}
		for _, char := range segment {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
				return false
			}
		}
	}
	return true
}

func validLoopbackAddress(value string) bool {
	if !strings.HasPrefix(value, "127.0.0.1:") || strings.Count(value, ":") != 1 {
		return false
	}
	port, err := strconv.Atoi(strings.TrimPrefix(value, "127.0.0.1:"))
	return err == nil && port >= 1 && port <= 65535
}

func nonzeroDecimal(value string) bool {
	for _, char := range value {
		if char != '0' && char != '.' {
			return true
		}
	}
	return false
}

func validProviderName(value string) bool {
	if value == "" || len(value) > broker.MaxProviderNameBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
			return false
		}
	}
	return true
}

func validServerIdentity(runUID, specDigest, baseSHA string) bool {
	if runUID == "" || len(runUID) > maxIdentityBytes || strings.TrimSpace(runUID) != runUID || !utf8.ValidString(runUID) {
		return false
	}
	for _, char := range runUID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._-", char)) {
			return false
		}
	}
	if !canonical.ValidDigest(specDigest) || (len(baseSHA) != 40 && len(baseSHA) != 64) {
		return false
	}
	for _, char := range baseSHA {
		if !((char >= 'a' && char <= 'f') || (char >= '0' && char <= '9')) {
			return false
		}
	}
	return true
}

func hasBroadCredentialEnvironment(lookup lookupEnv) bool {
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_PROFILE",
	} {
		if _, ok := lookup(name); ok {
			return true
		}
	}
	return false
}

// newRuntimeExitToken satisfies the legacy constructor's internal invariant.
// The resulting token is never placed in an environment, file, request, or
// command line. The public router does not mount the process-exit path.
func newRuntimeExitToken() ([]byte, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, configError{code: "runtime_boundary_initialization_failed"}
	}
	for index, value := range token {
		// Convert random bytes to printable bytes without storing a second
		// secret-bearing configuration value.
		token[index] = 'A' + (value % 26)
	}
	return token, nil
}

func credentialProjectedKey(logicalRef string) string {
	digest := sha256.Sum256([]byte("agw-broker-credential\x00" + logicalRef))
	return "broker-" + hex.EncodeToString(digest[:])
}

func sortedCredentialRefs(toolSet v1alpha1.ToolSetSpec, route v1alpha1.ModelRouteSpec) []string {
	refs := make(map[string]struct{})
	for _, server := range toolSet.Servers {
		if server.CredentialsRef != "" {
			refs[server.CredentialsRef] = struct{}{}
		}
	}
	for _, provider := range route.Providers {
		if provider.CredentialRef != "" {
			refs[provider.CredentialRef] = struct{}{}
		}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result
}

func contextStillLive(ctx context.Context) error {
	if ctx == nil {
		return configError{code: "context_invalid"}
	}
	return ctx.Err()
}
