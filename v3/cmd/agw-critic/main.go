// agw-critic is the bounded execution-free critic runtime contract.
//
// It reads only the two immutable mounted artifacts, sends one request to an
// explicitly loopback-only Anthropic-compatible model listener, and writes
// exactly one AGW_CRITIC_INPUT_V1 frame. It never reads provider credentials
// and cannot emit a verdict, score, or Gate result.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/criticworkload"
	"github.com/Astatide1337/agents-gateway/v3/internal/findingcorroboration"
	"github.com/Astatide1337/agents-gateway/v3/internal/verifyfetch"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	defaultPatchPath   = "/workspace/input/patch.diff"
	defaultContextPath = "/workspace/input/context-pack.json"
	defaultGatewayURL  = "http://127.0.0.1:8082/v1/messages"

	maxRequestBytes  = int64(1 << 20)
	maxResponseBytes = int64(1 << 20)
	maxModelBytes    = 256
	maxFamilyBytes   = 128
	defaultTimeout   = 10 * time.Minute
	maxTimeout       = 20 * time.Minute
	defaultMaxTokens = 8192
	maxMaxTokens     = 32768
)

const criticSystemPrompt = `You are an execution-free code critic. Inspect the supplied immutable patch and ContextPack. Return exactly one canonical JSON object matching agents.astatide.com/finding-corroboration/v1alpha1. It must contain only schemaVersion, findings, and evidence. Do not return markdown, prose, verdicts, scores, Gate results, blocking decisions, or fields outside that schema. Every finding must be corroborated by evidence; model-only assertions remain advisory under the verifier's ADR-020 rules.`

var (
	errInvalidConfig    = errors.New("critic configuration is invalid")
	errInputUnavailable = errors.New("critic immutable input is unavailable")
	errGatewayFailed    = errors.New("critic loopback model gateway failed")
	errOutputInvalid    = errors.New("critic output is not canonical corroboration input")
)

type config struct {
	PatchPath      string
	ContextPath    string
	GatewayURL     string
	Model          string
	ModelKind      string
	CriticFamily   string
	WorkerFamily   string
	PatchDigest    string
	ContextDigest  string
	MaxOutputBytes int64
	MaxFindings    int32
	MaxTokens      int
	Timeout        time.Duration
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature float64            `json:"temperature"`
	System      string             `json:"system"`
	Messages    []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Model        string             `json:"model"`
	Content      []anthropicContent `json:"content"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func main() {
	ctx, stop := signalContext(context.Background())
	defer stop()
	if err := execute(ctx, os.Getenv, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "agw-critic:", err)
		os.Exit(1)
	}
}

func signalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signalNotifyContext(parent)
}

// signalNotifyContext is kept in a small variable seam for command tests.
var signalNotifyContext = func(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func execute(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) error {
	if ctx == nil || stdout == nil || stderr == nil {
		return errInvalidConfig
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	switch getenv(criticworkload.CriticModeEnv) {
	case criticworkload.CriticModeMaterializeInput:
		return materializeInput(ctx, getenv)
	case criticworkload.CriticModeRenderGateway:
		return renderAgentGateway(getenv)
	case "", "execute":
		return executeCritic(ctx, getenv, stdout, stderr)
	default:
		return fmt.Errorf("%w: unsupported mode", errInvalidConfig)
	}
}

func executeCritic(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := configFromEnv(getenv)
	if err != nil {
		return err
	}
	patch, err := readImmutable(cfg.PatchPath, cfg.PatchDigest, criticworkload.MaxPromptBytes)
	if err != nil {
		return fmt.Errorf("%w: patch", err)
	}
	contextPack, err := readImmutable(cfg.ContextPath, cfg.ContextDigest, criticworkload.MaxPromptBytes)
	if err != nil {
		return fmt.Errorf("%w: context pack", err)
	}
	requestBody, err := buildRequest(cfg, patch, contextPack)
	if err != nil {
		return err
	}
	responseBody, err := callGateway(ctx, cfg, requestBody)
	if err != nil {
		return err
	}
	canonicalInput, err := parseResponse(responseBody, cfg.MaxOutputBytes, cfg.MaxFindings)
	if err != nil {
		return err
	}
	frame, err := criticworkload.EncodeOutputFrame(canonicalInput)
	if err != nil {
		return fmt.Errorf("%w: frame encoding", errOutputInvalid)
	}
	if _, err := stdout.Write(frame); err != nil {
		return errors.New("critic output write failed")
	}
	return nil
}

func materializeInput(ctx context.Context, getenv func(string) string) error {
	if getenv == nil {
		return errInvalidConfig
	}
	input, err := criticworkload.ParseCanonicalInput([]byte(getenv(criticworkload.InputEnv)))
	if err != nil {
		return fmt.Errorf("%w: canonical input", errInvalidConfig)
	}
	patchPath, err := validateMaterializedPath(getenv(criticworkload.CriticPatchPathEnv), "patch.diff")
	if err != nil {
		return fmt.Errorf("%w: patch path", errInvalidConfig)
	}
	contextPath, err := validateMaterializedPath(getenv(criticworkload.CriticContextPathEnv), "context-pack.json")
	if err != nil {
		return fmt.Errorf("%w: context path", errInvalidConfig)
	}
	if getenv(criticworkload.PatchDigestEnv) != input.Patch.Digest || getenv(criticworkload.ContextDigestEnv) != input.Context.Digest {
		return fmt.Errorf("%w: artifact digest binding", errInvalidConfig)
	}
	if input.Patch.SizeBytes+input.Context.SizeBytes > criticworkload.MaxPromptBytes-criticworkload.PromptEnvelopeBytes {
		return fmt.Errorf("%w: verified input exceeds prompt bound", errInvalidConfig)
	}
	endpoint := getenv(criticworkload.CriticObjectStoreEndpointEnv)
	region := getenv(criticworkload.CriticObjectStoreRegionEnv)
	bucket := getenv(criticworkload.CriticObjectStoreBucketEnv)
	forcePathStyle, err := parseBool(getenv(criticworkload.CriticObjectStoreForcePathStyleEnv))
	if err != nil {
		return fmt.Errorf("%w: object-store path style", errInvalidConfig)
	}
	storeConfig := verifyfetch.StoreConfig{Endpoint: endpoint, Region: region, Bucket: bucket, ForcePathStyle: forcePathStyle, MaxObjectBytes: criticworkload.MaxPatchBytes}
	if err := verifyfetch.ValidateStoreConfig(storeConfig); err != nil {
		return fmt.Errorf("%w: object-store configuration", errInvalidConfig)
	}
	accessPath := getenv(criticworkload.CriticObjectStoreAccessKeyFileEnv)
	secretPath := getenv(criticworkload.CriticObjectStoreSecretKeyFileEnv)
	if accessPath == "" {
		accessPath = criticworkload.CriticObjectStoreAccessKeyFile
	}
	if secretPath == "" {
		secretPath = criticworkload.CriticObjectStoreSecretKeyFile
	}
	accessKey, err := readCredentialFile(accessPath, verifyfetch.MaxAccessKeyBytes)
	if err != nil {
		return fmt.Errorf("%w: object-store access credential", errInvalidConfig)
	}
	secretKey, err := readCredentialFile(secretPath, verifyfetch.MaxSecretKeyBytes)
	if err != nil {
		return fmt.Errorf("%w: object-store secret credential", errInvalidConfig)
	}
	credentials := verifyfetch.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey, SessionToken: getenv(criticworkload.CriticObjectStoreSessionTokenEnv)}
	if err := verifyfetch.ValidateCredentials(credentials); err != nil {
		return fmt.Errorf("%w: object-store credentials", errInvalidConfig)
	}
	getter, err := verifyfetch.NewS3Getter(ctx, storeConfig, credentials)
	if err != nil {
		return fmt.Errorf("%w: object-store client", errInvalidConfig)
	}
	patch, err := fetchArtifact(ctx, getter, input.Patch, criticworkload.MaxPromptBytes, bucket)
	if err != nil {
		return fmt.Errorf("%w: patch fetch", errInputUnavailable)
	}
	contextPack, err := fetchArtifact(ctx, getter, input.Context, criticworkload.MaxPromptBytes, bucket)
	if err != nil {
		return fmt.Errorf("%w: context fetch", errInputUnavailable)
	}
	if err := writeImmutableFile(patchPath, patch, 0o444); err != nil {
		return fmt.Errorf("%w: patch materialization", errInputUnavailable)
	}
	if err := writeImmutableFile(contextPath, contextPack, 0o444); err != nil {
		return fmt.Errorf("%w: context materialization", errInputUnavailable)
	}
	return nil
}

func renderAgentGateway(getenv func(string) string) error {
	path, err := validateConfigPath(getenv(criticworkload.CriticGatewayConfigPathEnv))
	if err != nil {
		return fmt.Errorf("%w: gateway config path", errInvalidConfig)
	}
	model := getenv(criticworkload.CriticRouteModelEnv)
	if !validText(model, maxModelBytes) {
		return fmt.Errorf("%w: gateway model", errInvalidConfig)
	}
	binding := criticworkload.AgentGatewayClientBinding{Endpoint: getenv(criticworkload.CriticGatewayEndpointEnv), RouteRef: getenv(criticworkload.CriticGatewayRouteRefEnv)}
	if err := criticworkload.ValidateAgentGatewayClientBinding(binding); err != nil {
		return fmt.Errorf("%w: gateway binding", errInvalidConfig)
	}
	port, err := strconv.Atoi(getenv(criticworkload.CriticGatewayPortEnv))
	if err != nil || port != int(criticworkload.AgentGatewayModelPort) {
		return fmt.Errorf("%w: gateway port", errInvalidConfig)
	}
	config, err := criticworkload.RenderAgentGatewayConfig(model, binding)
	if err != nil {
		return fmt.Errorf("%w: gateway config", errInvalidConfig)
	}
	if digest(config) != getenv(criticworkload.CriticGatewayConfigDigestEnv) {
		return fmt.Errorf("%w: gateway config digest", errInvalidConfig)
	}
	if err := writeImmutableFile(path, config, 0o444); err != nil {
		return fmt.Errorf("%w: gateway config write", errInvalidConfig)
	}
	return nil
}

func fetchArtifact(ctx context.Context, getter verifyfetch.ObjectGetter, ref v1alpha1.ArtifactRef, max int64, bucket string) ([]byte, error) {
	if ref.SizeBytes <= 0 || ref.SizeBytes > max || ref.Digest == "" {
		return nil, errInputUnavailable
	}
	location, err := verifyfetch.ParseS3URI(ref.URI)
	if err != nil || location.Bucket != bucket {
		return nil, errInputUnavailable
	}
	body, err := getter.Get(ctx, location.Key, ref.SizeBytes)
	if err != nil || int64(len(body)) != ref.SizeBytes || digest(body) != ref.Digest {
		return nil, errInputUnavailable
	}
	return body, nil
}

func validateMaterializedPath(value, base string) (string, error) {
	validated, err := validatePath(value)
	if err != nil || filepath.Dir(validated) != filepath.Clean("/workspace/input") || filepath.Base(validated) != base {
		return "", errInvalidConfig
	}
	return validated, nil
}

func validateConfigPath(value string) (string, error) {
	validated, err := validatePath(value)
	if err != nil || validated != criticworkload.AgentGatewayConfigPath {
		return "", errInvalidConfig
	}
	return validated, nil
}

func parseBool(value string) (bool, error) {
	if value == "true" {
		return true, nil
	}
	if value == "false" {
		return false, nil
	}
	return false, errInvalidConfig
}

func readCredentialFile(path string, max int64) (string, error) {
	validated, err := validatePath(path)
	if err != nil {
		return "", errInputUnavailable
	}
	info, err := os.Lstat(validated)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errInputUnavailable
	}
	file, err := os.OpenFile(validated, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", errInputUnavailable
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) == 0 || int64(len(body)) > max || strings.TrimSpace(string(body)) != string(body) {
		return "", errInputUnavailable
	}
	return string(body), nil
}

func writeImmutableFile(path string, body []byte, mode os.FileMode) error {
	if path == "" || len(body) == 0 {
		return errInputUnavailable
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return errInputUnavailable
	}
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return errInputUnavailable
	}
	if err := file.Sync(); err != nil {
		return errInputUnavailable
	}
	if err := file.Chmod(mode); err != nil {
		return errInputUnavailable
	}
	return nil
}

func configFromEnv(getenv func(string) string) (config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	read := func(name, fallback string) string {
		if value := getenv(name); value != "" {
			return value
		}
		return fallback
	}
	cfg := config{
		PatchPath:      read("AGW_CRITIC_PATCH_PATH", defaultPatchPath),
		ContextPath:    read("AGW_CRITIC_CONTEXT_PATH", defaultContextPath),
		GatewayURL:     read("AGW_CRITIC_GATEWAY_URL", defaultGatewayURL),
		Model:          getenv("AGW_CRITIC_MODEL"),
		ModelKind:      getenv("AGW_CRITIC_MODEL_KIND"),
		CriticFamily:   getenv("AGW_CRITIC_MODEL_FAMILY"),
		WorkerFamily:   getenv("AGW_WORKER_MODEL_FAMILY"),
		PatchDigest:    getenv("AGW_CRITIC_PATCH_DIGEST"),
		ContextDigest:  getenv("AGW_CRITIC_CONTEXT_DIGEST"),
		MaxOutputBytes: criticworkload.MaxOutputBytes,
		MaxFindings:    v1alpha1.MaxCriticFindings,
		MaxTokens:      defaultMaxTokens,
		Timeout:        defaultTimeout,
	}
	var err error
	if cfg.PatchPath, err = validatePath(cfg.PatchPath); err != nil {
		return config{}, fmt.Errorf("%w: patch path", errInvalidConfig)
	}
	if cfg.ContextPath, err = validatePath(cfg.ContextPath); err != nil {
		return config{}, fmt.Errorf("%w: context path", errInvalidConfig)
	}
	if cfg.GatewayURL, err = validateGatewayURL(cfg.GatewayURL); err != nil {
		return config{}, fmt.Errorf("%w: gateway URL", errInvalidConfig)
	}
	if !validText(cfg.Model, maxModelBytes) || cfg.ModelKind != "anthropic-messages" || !validText(cfg.CriticFamily, maxFamilyBytes) || !validText(cfg.WorkerFamily, maxFamilyBytes) || cfg.CriticFamily == cfg.WorkerFamily {
		return config{}, fmt.Errorf("%w: model identity or family separation", errInvalidConfig)
	}
	if !canonical.ValidDigest(cfg.PatchDigest) || !canonical.ValidDigest(cfg.ContextDigest) {
		return config{}, fmt.Errorf("%w: input digest", errInvalidConfig)
	}
	if value := getenv("AGW_CRITIC_MAX_OUTPUT_BYTES"); value != "" {
		cfg.MaxOutputBytes, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return config{}, fmt.Errorf("%w: max output bytes", errInvalidConfig)
		}
	}
	if cfg.MaxOutputBytes < 1 || cfg.MaxOutputBytes > criticworkload.MaxOutputBytes {
		return config{}, fmt.Errorf("%w: max output bytes bound", errInvalidConfig)
	}
	if value := getenv("AGW_CRITIC_MAX_FINDINGS"); value != "" {
		parsed, parseErr := strconv.ParseInt(value, 10, 32)
		if parseErr != nil {
			return config{}, fmt.Errorf("%w: max findings", errInvalidConfig)
		}
		cfg.MaxFindings = int32(parsed)
	}
	if cfg.MaxFindings < 1 || cfg.MaxFindings > v1alpha1.MaxCriticFindings {
		return config{}, fmt.Errorf("%w: max findings bound", errInvalidConfig)
	}
	if value := getenv("AGW_CRITIC_MAX_TOKENS"); value != "" {
		cfg.MaxTokens, err = strconv.Atoi(value)
		if err != nil {
			return config{}, fmt.Errorf("%w: max tokens", errInvalidConfig)
		}
	}
	if cfg.MaxTokens < 1 || cfg.MaxTokens > maxMaxTokens {
		return config{}, fmt.Errorf("%w: max tokens bound", errInvalidConfig)
	}
	if value := getenv("AGW_CRITIC_TIMEOUT"); value != "" {
		cfg.Timeout, err = time.ParseDuration(value)
		if err != nil {
			return config{}, fmt.Errorf("%w: timeout", errInvalidConfig)
		}
	}
	if cfg.Timeout < time.Second || cfg.Timeout > maxTimeout {
		return config{}, fmt.Errorf("%w: timeout bound", errInvalidConfig)
	}
	return cfg, nil
}

func validatePath(value string) (string, error) {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return "", errInvalidConfig
	}
	return value, nil
}

func validateGatewayURL(value string) (string, error) {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value {
		return "", errInvalidConfig
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.Path != "/v1/messages" {
		return "", errInvalidConfig
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", errInvalidConfig
	}
	port := u.Port()
	if port == "" {
		return "", errInvalidConfig
	}
	if parsed, parseErr := strconv.Atoi(port); parseErr != nil || parsed < 1 || parsed > 65535 {
		return "", errInvalidConfig
	}
	return value, nil
}

func validText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func readImmutable(path, expectedDigest string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errInputUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errInputUnavailable
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) > max || len(body) == 0 || !utf8.Valid(body) || digest(body) != expectedDigest {
		return nil, errInputUnavailable
	}
	return body, nil
}

func buildRequest(cfg config, patch, contextPack []byte) ([]byte, error) {
	user := "PATCH (immutable sha256=" + strings.TrimPrefix(cfg.PatchDigest, "sha256:") + "):\n" + string(patch) + "\n\nCONTEXTPACK (immutable sha256=" + strings.TrimPrefix(cfg.ContextDigest, "sha256:") + "):\n" + string(contextPack)
	if int64(len(user)) > criticworkload.MaxPromptBytes {
		return nil, fmt.Errorf("%w: prompt exceeds bound", errInvalidConfig)
	}
	body, err := json.Marshal(anthropicRequest{
		Model: cfg.Model, MaxTokens: cfg.MaxTokens, Temperature: 0, System: criticSystemPrompt,
		Messages: []anthropicMessage{{Role: "user", Content: user}},
	})
	if err != nil || int64(len(body)) > maxRequestBytes || strictjson.ValidateObject(body) != nil {
		return nil, fmt.Errorf("%w: request contract", errInvalidConfig)
	}
	return body, nil
}

func callGateway(ctx context.Context, cfg config, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GatewayURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: request construction", errGatewayFailed)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "agents-gateway-critic")
	// Deliberately no Authorization, API key, cookie, or inherited provider
	// header is copied. The sidecar owns provider credentials.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: loopback request", errGatewayFailed)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil || int64(len(body)) > maxResponseBytes || response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: bounded response rejected", errGatewayFailed)
	}
	return body, nil
}

func parseResponse(body []byte, maxBytes int64, maxFindings int32) ([]byte, error) {
	if len(body) == 0 || int64(len(body)) > maxResponseBytes || strictjson.ValidateObject(body) != nil {
		return nil, errOutputInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response anthropicResponse
	if err := decoder.Decode(&response); err != nil {
		return nil, errOutputInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF || len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text == "" {
		return nil, errOutputInvalid
	}
	candidate := []byte(response.Content[0].Text)
	if int64(len(candidate)) > maxBytes {
		return nil, errOutputInvalid
	}
	parsed, err := findingcorroboration.ParseCanonicalInput(candidate)
	if err != nil || int32(len(parsed.Findings)) > maxFindings {
		return nil, errOutputInvalid
	}
	canonicalBody, err := findingcorroboration.CanonicalInputBytes(parsed)
	if err != nil || !bytes.Equal(canonicalBody, candidate) {
		return nil, errOutputInvalid
	}
	return canonicalBody, nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
