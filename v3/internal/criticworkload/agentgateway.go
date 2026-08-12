package criticworkload

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"github.com/Astatide1337/agents-gateway/v3/internal/resolved"
	"github.com/Astatide1337/agents-gateway/v3/pkg/strictjson"
)

const (
	// AgentGatewayVersion is the upstream version whose standalone LLM
	// configuration is compiled by this package. The image is still required
	// to be digest pinned by BuildOptions.
	AgentGatewayVersion = "v1.4.1"
	// GatewayJWTCapabilityEvidenceSchema identifies the canonical identity that
	// a JWT capability digest covers. The digest is a binding check; it does not
	// replace the external probe required to set Verified=true.
	GatewayJWTCapabilityEvidenceSchema = "agents.astatide.com/agentgateway-jwt-capability/v1alpha1"

	AgentGatewayImagePrefix = "cr.agentgateway.dev/agentgateway@sha256:"

	// AgentGatewayConfigPath is mounted read-only into the agentgateway
	// container. The critic never receives this mount.
	AgentGatewayConfigPath = "/run/agw/agentgateway/agentgateway.json"
	// GatewayClientTokenPath is a projected, short-lived ServiceAccount token.
	// The local sidecar reads it directly through agentgateway's supported file
	// credential form; no token is copied into an environment variable.
	GatewayClientTokenPath = "/var/run/secrets/agw-gateway/token"

	// The command has three deliberately separate modes. Only the normal
	// (empty) mode runs the critic; the two init modes have different mounts in
	// the Job and therefore cannot see provider credentials.
	CriticModeEnv              = "AGW_CRITIC_MODE"
	CriticModeMaterializeInput = "materialize-input"
	CriticModeRenderGateway    = "render-agentgateway"

	CriticPatchPathEnv           = "AGW_CRITIC_PATCH_PATH"
	CriticContextPathEnv         = "AGW_CRITIC_CONTEXT_PATH"
	CriticGatewayConfigPathEnv   = "AGW_CRITIC_GATEWAY_CONFIG_PATH"
	CriticGatewayConfigDigestEnv = "AGW_CRITIC_GATEWAY_CONFIG_DIGEST"
	CriticGatewayPortEnv         = "AGW_CRITIC_GATEWAY_PORT"
	CriticGatewayEndpointEnv     = "AGW_CRITIC_GATEWAY_ENDPOINT"
	CriticGatewayRouteRefEnv     = "AGW_CRITIC_GATEWAY_ROUTE_REF"

	CriticObjectStoreEndpointEnv       = "AGW_CRITIC_OBJECT_STORE_ENDPOINT"
	CriticObjectStoreRegionEnv         = "AGW_CRITIC_OBJECT_STORE_REGION"
	CriticObjectStoreBucketEnv         = "AGW_CRITIC_OBJECT_STORE_BUCKET"
	CriticObjectStoreForcePathStyleEnv = "AGW_CRITIC_OBJECT_STORE_FORCE_PATH_STYLE"
	CriticObjectStoreAccessKeyFileEnv  = "AGW_CRITIC_OBJECT_STORE_ACCESS_KEY_FILE"
	CriticObjectStoreSecretKeyFileEnv  = "AGW_CRITIC_OBJECT_STORE_SECRET_KEY_FILE"
	CriticObjectStoreSessionTokenEnv   = "AGW_CRITIC_OBJECT_STORE_SESSION_TOKEN"

	CriticObjectStoreAccessKeyFile = "/run/agw/object-store/access-key-id"
	CriticObjectStoreSecretKeyFile = "/run/agw/object-store/secret-access-key"

	AgentGatewayModelPort uint16 = 8082
)

var (
	// ErrAgentGatewayCapabilityRequired means the caller did not provide an
	// explicit, probe-backed capability result for the exact sidecar image.
	// This is an admission guard, not a permanent integration scaffold.
	ErrAgentGatewayCapabilityRequired = fmt.Errorf("agentgateway %s model capability is not proven", AgentGatewayVersion)
)

// AgentGatewayCapability is the result of the local/phase-0 capability probe.
// It is intentionally explicit: a version string alone does not enable the
// production workload. EvidenceDigest should identify the reviewed upstream
// config/source evidence used by the probe.
type AgentGatewayCapability struct {
	Image             string `json:"image"`
	Version           string `json:"version"`
	AnthropicMessages bool   `json:"anthropicMessages"`
	RetryAttemptsOne  bool   `json:"retryAttemptsOne"`
	EvidenceDigest    string `json:"evidenceDigest"`
}

// GatewayJWTCapability is explicit evidence that the central agw-system
// Gateway/HTTPRoute has strict JWT validation for the supplied issuer and
// audience. A ServiceAccount token alone does not prove that the central
// gateway will validate it, so production admission requires this probe.
type GatewayJWTCapability struct {
	Issuer         string `json:"issuer"`
	Audience       string `json:"audience"`
	PolicyRef      string `json:"policyRef"`
	Verified       bool   `json:"verified"`
	EvidenceDigest string `json:"evidenceDigest"`
}

// gatewayJWTCapabilityEvidenceIdentity is deliberately limited to the values
// that determine which JWT-protected gateway contract the critic will use. It
// excludes Verified and EvidenceDigest so the digest is neither self
// referential nor a second assertion of the probe result.
type gatewayJWTCapabilityEvidenceIdentity struct {
	SchemaVersion          string            `json:"schemaVersion"`
	Endpoint               string            `json:"endpoint"`
	RouteRef               string            `json:"routeRef"`
	ServiceAccountName     string            `json:"serviceAccountName"`
	TokenExpirationSeconds int64             `json:"tokenExpirationSeconds"`
	Issuer                 string            `json:"issuer"`
	Audience               string            `json:"audience"`
	PolicyRef              string            `json:"policyRef"`
	PodSelector            map[string]string `json:"podSelector"`
}

// GatewayJWTCapabilityEvidenceDigest returns the canonical digest that must
// accompany a verified JWT capability. Keeping this derivation next to the
// workload contract prevents a valid digest from being replayed after the
// issuer, audience, route, or gateway target changes.
func GatewayJWTCapabilityEvidenceDigest(binding GatewayClientOptions) (string, error) {
	identity := gatewayJWTCapabilityEvidenceIdentity{
		SchemaVersion:          GatewayJWTCapabilityEvidenceSchema,
		Endpoint:               binding.Endpoint,
		RouteRef:               binding.RouteRef,
		ServiceAccountName:     binding.ServiceAccountName,
		TokenExpirationSeconds: binding.TokenExpirationSeconds,
		Issuer:                 binding.JWTCapability.Issuer,
		Audience:               binding.JWTCapability.Audience,
		PolicyRef:              binding.JWTCapability.PolicyRef,
		PodSelector:            copyStringMap(binding.PodSelector),
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal JWT capability identity: %w", err)
	}
	normalized, err := strictjson.Normalize(raw)
	if err != nil {
		return "", fmt.Errorf("normalize JWT capability identity: %w", err)
	}
	if len(normalized) == 0 || len(normalized) > MaxContractBytes {
		return "", fmt.Errorf("normalize JWT capability identity: size is outside the contract bound")
	}
	return digestBytes(normalized), nil
}

// AgentGatewayClientBinding identifies the central agw-system data plane. It
// deliberately contains no Secret reference or credential value: the Job
// projects a short-lived ServiceAccount token only into the local gateway.
type AgentGatewayClientBinding struct {
	Endpoint string `json:"endpoint"`
	RouteRef string `json:"routeRef"`
}

// ValidateAgentGatewayClientBinding validates the endpoint shape used by the
// v1.4.1 standalone forwarder. Localhost is accepted here so the renderer can
// be tested against a recording gateway; production admission applies the
// stronger agw-system Service check in job.go.
func ValidateAgentGatewayClientBinding(binding AgentGatewayClientBinding) error {
	if _, err := gatewayHostOverride(binding.Endpoint, false); err != nil {
		return err
	}
	if !validRouteRef(binding.RouteRef) {
		return fmt.Errorf("invalid agentgateway route reference")
	}
	return nil
}

func (capability AgentGatewayCapability) Validate(image string) error {
	if !resolved.ValidPinnedImage(image) || !strings.HasPrefix(image, AgentGatewayImagePrefix) {
		return fmt.Errorf("%w: image is not a pinned official agentgateway image", ErrAgentGatewayCapabilityRequired)
	}
	if capability.Image != image || capability.Version != AgentGatewayVersion || !capability.AnthropicMessages || !capability.RetryAttemptsOne || !canonical.ValidDigest(capability.EvidenceDigest) {
		return fmt.Errorf("%w: capability evidence does not match %s", ErrAgentGatewayCapabilityRequired, image)
	}
	return nil
}

type standaloneAgentGatewayConfig struct {
	Binds []agentGatewayBind `json:"binds"`
}

type agentGatewayBind struct {
	Port      uint16                 `json:"port"`
	Listeners []agentGatewayListener `json:"listeners"`
}

type agentGatewayListener struct {
	Protocol string              `json:"protocol"`
	Routes   []agentGatewayRoute `json:"routes"`
}

type agentGatewayRoute struct {
	Matches  []agentGatewayMatch     `json:"matches"`
	Policies agentGatewayRoutePolicy `json:"policies"`
	Backends []agentGatewayBackend   `json:"backends"`
}

type agentGatewayMatch struct {
	Path agentGatewayPathMatch `json:"path"`
}

type agentGatewayPathMatch struct {
	PathPrefix string `json:"pathPrefix"`
}

type agentGatewayRoutePolicy struct {
	Retry agentGatewayRetryPolicy `json:"retry"`
	AI    agentGatewayAIPolicy    `json:"ai"`
}

type agentGatewayRetryPolicy struct {
	// attempts is the total number of attempts, not the number of retries.
	// v1.4.1's standalone route policy therefore makes this request exactly
	// once and does not silently add a failover attempt. The explicit empty
	// codes list is required by the v1.4.1 schema and prevents response-code
	// retries as well.
	Attempts int   `json:"attempts"`
	Codes    []int `json:"codes"`
}

type agentGatewayAIPolicy struct {
	// Explicitly bind the client path to the Anthropic Messages input/output
	// format. Without this route-type declaration the generic AI route is free
	// to normalize the response to another client format.
	Routes map[string]string `json:"routes"`
}

type agentGatewayBackend struct {
	AI       agentGatewayAIBackend     `json:"ai"`
	Policies agentGatewayBackendPolicy `json:"policies"`
}

type agentGatewayAIBackend struct {
	Name         string                        `json:"name"`
	Provider     agentGatewayAnthropicProvider `json:"provider"`
	HostOverride string                        `json:"hostOverride"`
}

type agentGatewayAnthropicProvider struct {
	Anthropic agentGatewayAnthropicModel `json:"anthropic"`
}

type agentGatewayAnthropicModel struct {
	Model string `json:"model"`
}

type agentGatewayBackendPolicy struct {
	BackendAuth agentGatewayBackendAuth `json:"backendAuth"`
}

type agentGatewayBackendAuth struct {
	Key agentGatewayBackendKey `json:"key"`
}

type agentGatewayBackendKey struct {
	Value    agentGatewayFileValue             `json:"value"`
	Location agentGatewayAuthorizationLocation `json:"location"`
}

type agentGatewayFileValue struct {
	File string `json:"file"`
}

type agentGatewayAuthorizationLocation struct {
	Header agentGatewayAuthorizationHeader `json:"header"`
}

type agentGatewayAuthorizationHeader struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// RenderAgentGatewayConfig returns the exact standalone v1.4.1 configuration
// used by the run-local forwarding sidecar. The sidecar exposes native
// Anthropic Messages on loopback, forwards to the central agw-system gateway,
// injects only the scoped gateway-client token, and makes one total attempt.
// Provider credentials remain in the central gateway and are not represented
// in this config.
func RenderAgentGatewayConfig(model string, binding AgentGatewayClientBinding) ([]byte, error) {
	if !validModelText(model) || ValidateAgentGatewayClientBinding(binding) != nil {
		return nil, fmt.Errorf("invalid agentgateway binding")
	}
	hostOverride, err := gatewayHostOverride(binding.Endpoint, false)
	if err != nil {
		return nil, fmt.Errorf("invalid agentgateway endpoint")
	}
	config := standaloneAgentGatewayConfig{
		Binds: []agentGatewayBind{{
			Port: AgentGatewayModelPort,
			Listeners: []agentGatewayListener{{
				Protocol: "HTTP",
				Routes: []agentGatewayRoute{{
					Matches: []agentGatewayMatch{{Path: agentGatewayPathMatch{PathPrefix: "/v1/messages"}}},
					Policies: agentGatewayRoutePolicy{
						Retry: agentGatewayRetryPolicy{Attempts: 1, Codes: []int{}},
						AI:    agentGatewayAIPolicy{Routes: map[string]string{"/v1/messages": "messages"}},
					},
					Backends: []agentGatewayBackend{{
						AI: agentGatewayAIBackend{
							Name:         binding.RouteRef,
							Provider:     agentGatewayAnthropicProvider{Anthropic: agentGatewayAnthropicModel{Model: model}},
							HostOverride: hostOverride,
						},
						Policies: agentGatewayBackendPolicy{BackendAuth: agentGatewayBackendAuth{Key: agentGatewayBackendKey{
							Value:    agentGatewayFileValue{File: GatewayClientTokenPath},
							Location: agentGatewayAuthorizationLocation{Header: agentGatewayAuthorizationHeader{Name: "authorization", Prefix: "Bearer "}},
						}}},
					}},
				}},
			}},
		}},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	normalized, err := strictjson.Normalize(raw)
	if err != nil || len(normalized) > 16<<10 || strictjson.ValidateObject(normalized) != nil {
		return nil, fmt.Errorf("invalid rendered agentgateway config")
	}
	return normalized, nil
}

func gatewayHostOverride(endpoint string, requireClusterService bool) (string, error) {
	if endpoint == "" || strings.ContainsAny(endpoint, "\x00\r\n") {
		return "", fmt.Errorf("invalid agentgateway endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || !strings.EqualFold(u.Scheme, "http") || u.Host == "" {
		return "", fmt.Errorf("agentgateway endpoint must be an HTTP origin")
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, " /\\") {
		return "", fmt.Errorf("agentgateway endpoint host is invalid")
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "", fmt.Errorf("agentgateway endpoint cannot be unspecified")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("agentgateway endpoint port is invalid")
	}
	if requireClusterService {
		lower := strings.ToLower(host)
		if (!strings.HasSuffix(lower, ".svc") && !strings.HasSuffix(lower, ".svc.cluster.local")) || !strings.Contains(lower, ".agw-system.") {
			return "", fmt.Errorf("agentgateway endpoint must be an agw-system Service")
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), nil
}

func validModelText(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func validRouteRef(value string) bool {
	return value != "" && len(value) <= 253 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "/\\\x00\r\n\t")
}
