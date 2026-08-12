// Package broker is the per-run, loopback-only policy boundary for Agents
// Gateway v3.
//
// A Broker is constructed from one immutable resolved ToolSet and ModelRoute.
// It owns no Kubernetes client and has no web framework dependency.  The
// controller/runtime integration supplies credentials, the effect ledger, and
// immutable artifact storage through small interfaces.  This keeps the
// security-sensitive decisions testable without accidentally making the pod
// runtime an authority for policy or publication.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
	"github.com/Astatide1337/agents-gateway/v3/internal/contextartifact"
	"github.com/Astatide1337/agents-gateway/v3/internal/effects"
	"github.com/Astatide1337/agents-gateway/v3/internal/policycontract"
	"github.com/Astatide1337/agents-gateway/v3/pkg/toolpolicy"
)

const (
	DefaultMaxToolCalls          int64 = 1000
	DefaultMaxModelRequests      int64 = 1000
	DefaultMaxModelTokens        int64 = 1_000_000
	DefaultMaxMCPRequestBytes          = 1 << 20
	DefaultMaxMCPResponseBytes         = 8 << 20
	DefaultMaxToolResultBytes    int64 = 32 << 10
	DefaultMaxModelRequestBytes        = 1 << 20
	DefaultMaxModelResponseBytes       = 32 << 20
	DefaultMaxArtifactBytes            = 8 << 20
	MaxConfiguredToolCalls       int64 = 10000
	MaxConfiguredModelRequests   int64 = 1_000_000
	MaxConfiguredModelTokens     int64 = 1_000_000_000
	MaxCredentialBytes                 = 16 << 10
	MaxProviderNameBytes               = 128
	MaxModelNameBytes                  = 256
	MaxEndpointBytes                   = 2048
	MaxToolNameBytes                   = 253
	MaxCallArgumentsBytes              = 1 << 20
)

var (
	ErrInvalidConfig           = errors.New("broker: invalid configuration")
	ErrInvalidRequest          = errors.New("broker: invalid request")
	ErrDenied                  = errors.New("broker: tool call denied")
	ErrApprovalRequired        = errors.New("broker: tool call requires approval")
	ErrBudgetExceeded          = errors.New("broker: configured budget exceeded")
	ErrCredentialUnavailable   = errors.New("broker: credential unavailable")
	ErrEffectLedgerUnavailable = errors.New("broker: effect ledger unavailable")
	ErrEffectAlreadyClaimed    = errors.New("broker: effect was already claimed")
	ErrUnknownEffect           = errors.New("broker: effect outcome is unknown")
	ErrUpstreamUnavailable     = errors.New("broker: upstream request failed")
	ErrUpstreamInvalid         = errors.New("broker: upstream returned an invalid response")
	ErrUnsupportedProvider     = errors.New("broker: provider is unsupported")
	ErrArtifactUnavailable     = errors.New("broker: artifact upload unavailable")
	ErrArtifactConflict        = errors.New("broker: immutable artifact conflicts with existing content")
	ErrRuntimeUnavailable      = errors.New("broker: runtime completion unavailable")
	ErrPhaseTransitionDenied   = errors.New("broker: phase transition denied")
	ErrPhaseDenied             = errors.New("broker: tool is unavailable in the current phase")
	ErrMCPUnavailable          = errors.New("broker: MCP handler unavailable")
	ErrToolResultUnavailable   = errors.New("broker: tool result storage unavailable")
	ErrToolResultInvalid       = errors.New("broker: tool result is invalid")
)

// BrokerMode selects which host-owned surface a Broker provides. The empty
// value is normalized to BrokerModeAgent for compatibility with callers that
// predate explicit verifier mode.
type BrokerMode string

const (
	BrokerModeAgent    BrokerMode = "agent"
	BrokerModeVerifier BrokerMode = "verifier"
)

// PhaseTransitionAuthorizer is the trusted host-side capability required to
// cross the explore -> edit boundary. It is never consulted by, or exposed
// through, the MCP HTTP handler.
type PhaseTransitionAuthorizer interface {
	AuthorizeExploreToEdit(context.Context) error
}

// PhaseTransitionAuthorizerFunc adapts a host callback to
// PhaseTransitionAuthorizer.
type PhaseTransitionAuthorizerFunc func(context.Context) error

func (f PhaseTransitionAuthorizerFunc) AuthorizeExploreToEdit(ctx context.Context) error {
	if f == nil {
		return ErrPhaseTransitionDenied
	}
	return f(ctx)
}

// CredentialResolver is deliberately a fixed-key contract.  The broker can
// ask only for the value associated with a logical reference; it cannot select
// a Kubernetes Secret, arbitrary Secret key, environment variable, or path.
// The runsecret adapter should implement this by reading the projected broker
// key for ref and returning a copy that the caller may wipe.
type CredentialResolver interface {
	ResolveToken(context.Context, string) ([]byte, error)
}

// EffectLedger is the v3 durable claim/commit protocol.  *effects.Ledger
// satisfies this interface directly.  A broker never performs a mutating MCP
// call unless Claim returns Execute=true.
type EffectLedger interface {
	Claim(context.Context, effects.Claim) (effects.ClaimDecision, error)
	Commit(context.Context, effects.Outcome) error
}

// ArtifactStore is the immutable object-store seam used by UploadArtifact.
// Put returning created=false must mean the object already existed; an
// ambiguous write must be returned as an error by the implementation.
type ArtifactStore interface {
	Put(context.Context, string, []byte, string) (created bool, uri string, err error)
	Get(context.Context, string) ([]byte, error)
}

// ArtifactReference is the broker's storage-neutral evidence reference.  URI
// is never allowed to contain credentials, a query, or a fragment.
type ArtifactReference struct {
	URI       string `json:"uri"`
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"sizeBytes"`
	MediaType string `json:"mediaType"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ArtifactUpload is a bounded immutable upload request. Key is not accepted
// from the caller: Broker.UploadArtifact derives the content-addressed key
// from the run identity and digest.
type ArtifactUpload struct {
	Data      []byte
	MediaType string
	Kind      string
	Name      string
}

// CostEstimator supplies provider-specific micro-dollar accounting. Requiring
// an estimator when a non-zero cost budget is configured prevents the broker
// from pretending that a token counter is a spend limit. Implementations must
// return only a bounded non-negative integer; their errors are not exposed.
type CostEstimator interface {
	EstimateCost(context.Context, string, string, int64, int64) (int64, error)
}

// Pricing is a simple deterministic estimator useful for providers with a
// known per-token price. Values are micro-USD per token, avoiding float math.
type Pricing struct {
	InputMicrosPerToken  int64
	OutputMicrosPerToken int64
}

// PricingTable implements CostEstimator. Keys are provider names, not
// user-controlled URLs or credentials.
type PricingTable map[string]Pricing

func (p PricingTable) EstimateCost(_ context.Context, provider, _ string, inputTokens, outputTokens int64) (int64, error) {
	price, ok := p[provider]
	if !ok || inputTokens < 0 || outputTokens < 0 || price.InputMicrosPerToken < 0 || price.OutputMicrosPerToken < 0 {
		return 0, ErrInvalidConfig
	}
	if inputTokens > (1<<63-1)/maxInt64(price.InputMicrosPerToken, 1) || outputTokens > (1<<63-1)/maxInt64(price.OutputMicrosPerToken, 1) {
		return 0, ErrBudgetExceeded
	}
	input := inputTokens * price.InputMicrosPerToken
	output := outputTokens * price.OutputMicrosPerToken
	if input > (1<<63-1)-output {
		return 0, ErrBudgetExceeded
	}
	return input + output, nil
}

// Config is the host-provided immutable execution contract. ToolSet and
// ModelRoute must already be resolved from the AgentRun snapshot; the broker
// does not read Kubernetes objects or re-resolve mutable references.
type Config struct {
	RunUID     string
	SpecDigest string
	BaseSHA    string
	ToolSet    v1alpha1.ToolSetSpec
	ModelRoute v1alpha1.ModelRouteSpec
	Mode       BrokerMode

	// PhaseTransitionAuthorizer is a trusted host-side capability. Agent-mode
	// brokers fail closed when it is absent or rejects TransitionToEdit. The
	// verifier mode never uses it because verify is controller/verifier-owned.
	PhaseTransitionAuthorizer PhaseTransitionAuthorizer

	Credentials CredentialResolver
	Effects     EffectLedger
	Artifacts   ArtifactStore
	// ResultStore is the authorized immutable store for raw MCP tool results
	// that cannot safely be returned inline. When nil, Artifacts is used. A
	// filesystem-backed implementation must be constructed with
	// NewFilesystemResultStore; callers must never pass an arbitrary path as a
	// tool result reference.
	ResultStore   ArtifactStore
	Pricing       CostEstimator
	WorkspaceRoot string
	// ContextRoot is a dedicated read-only volume containing the init-created
	// ContextPack. It must never point at the agent-writable workspace.
	ContextRoot string
	// PolicyContract is the descriptor-only contract decoded from the sealed
	// ContextPack. Its digest must match the run's materialized policy file.
	PolicyContract policycontract.Compiled

	// ProviderEndpoints maps a resolved provider name to its exact Responses
	// endpoint. Empty entries use the built-in endpoint for the provider kind.
	ProviderEndpoints map[string]string

	MaxToolCalls int64
	// MaxCostUSD is the immutable AgentRun spend cap. An empty value is only
	// accepted by direct callers that predate the run-level cap; production
	// broker configuration always supplies it from AGW_MAX_COST_USD.
	MaxCostUSD            string
	MaxModelRequests      int64
	MaxModelTokens        int64
	MaxMCPRequestBytes    int
	MaxMCPResponseBytes   int
	MaxToolResultBytes    int64
	MaxModelRequestBytes  int64
	MaxModelResponseBytes int64
	MaxArtifactBytes      int64

	RequireApprovalForMutations bool
	Clock                       func() time.Time

	// These seams are intentionally package-private. Production always uses
	// the secure transport constructed by New. Same-package tests can inject a
	// deterministic RoundTripper without exposing a way for integrations to
	// bypass DNS/IP/redirect defenses accidentally.
	transport http.RoundTripper
	resolver  DNSResolver
}

type compiledServer struct {
	name          string
	endpoint      string
	credentialRef string
}

type compiledTool struct {
	name         string
	server       string
	effect       v1alpha1.EffectKind
	policyEffect toolpolicy.Effect
	exactArgs    json.RawMessage
}

type compiledProvider struct {
	name          string
	kind          string
	model         string
	credentialRef string
	endpoint      string
	priority      int32
}

type budgetState struct {
	mu              sync.Mutex
	toolCalls       int64
	modelRequests   int64
	modelTokens     int64
	modelCostMicros int64
	reservedTokens  int64
	reservedCost    int64
	maxToolCalls    int64
	maxRequests     int64
	maxTokens       int64
	maxCostMicros   int64
}

// Usage is a bounded projection safe for status or diagnostics. It contains
// no request bodies, model responses, endpoint strings, or credential data.
type Usage struct {
	ToolCalls       int64
	ModelRequests   int64
	ModelTokens     int64
	ModelCostMicros int64
}

// Broker is safe for concurrent calls from the local HTTP server.
type Broker struct {
	runUID          string
	specDigest      string
	baseSHA         string
	servers         map[string]compiledServer
	tools           map[string]compiledTool
	profiles        map[v1alpha1.ToolProfileName]map[string]compiledTool
	providers       []compiledProvider
	credentials     CredentialResolver
	effects         EffectLedger
	artifacts       ArtifactStore
	resultStore     ArtifactStore
	workspaceRoot   string
	contextRoot     string
	policyContract  policycontract.Compiled
	pricing         CostEstimator
	client          *http.Client
	limits          limits
	budget          budgetState
	clock           func() time.Time
	requireApproval bool
	mode            BrokerMode
	phaseMu         sync.RWMutex
	transitionMu    sync.Mutex
	phase           v1alpha1.ToolProfileName
	phaseAuthorizer PhaseTransitionAuthorizer
}

type limits struct {
	maxToolCalls     int64
	maxMCPRequest    int
	maxMCPResponse   int
	maxToolResult    int64
	maxModelRequest  int64
	maxModelResponse int64
	maxArtifact      int64
}

// New compiles and validates the resolved contract. It fails closed on an
// unsupported provider, missing cost accounting, unsafe endpoint syntax, or a
// credential-bearing configuration. It copies all mutable maps and JSON
// values before returning.
func New(config Config) (*Broker, error) {
	if !validRunIdentity(config.RunUID, config.SpecDigest, config.BaseSHA) {
		return nil, ErrInvalidConfig
	}
	if config.WorkspaceRoot != "" && !validWorkspaceRoot(config.WorkspaceRoot) {
		return nil, ErrInvalidConfig
	}
	if config.ContextRoot != "" && !validWorkspaceRoot(config.ContextRoot) {
		return nil, ErrInvalidConfig
	}
	if config.PolicyContract.Digest() != "" {
		manifest := config.PolicyContract.Manifest()
		if manifest.BaseSHA != config.BaseSHA || manifest.ResolvedSpecDigest != config.SpecDigest {
			return nil, ErrInvalidConfig
		}
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	toolLimits, err := normalizeLimits(config)
	if err != nil {
		return nil, err
	}
	servers, profiles, err := compileToolSet(config.ToolSet)
	if err != nil {
		return nil, err
	}
	tools := flattenCompiledProfiles(profiles)
	mode := config.Mode
	if mode == "" {
		mode = BrokerModeAgent
	}
	if mode != BrokerModeAgent && mode != BrokerModeVerifier {
		return nil, ErrInvalidConfig
	}
	providers, err := compileProviders(config.ModelRoute, config.ProviderEndpoints)
	if err != nil {
		return nil, err
	}
	maxCost, err := parseMicros(config.ModelRoute.Budget.MaxCostUSD)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	if config.MaxCostUSD != "" {
		runCost, runCostErr := parseMicros(config.MaxCostUSD)
		if runCostErr != nil {
			return nil, ErrInvalidConfig
		}
		// Both budgets are hard ceilings. In particular, a route budget of
		// zero is a real zero-spend contract and must never be replaced by
		// the run-level value merely because zero is falsy.
		if runCost < maxCost {
			maxCost = runCost
		}
	}
	if maxCost > 0 && config.Pricing == nil {
		return nil, ErrInvalidConfig
	}
	if config.Credentials == nil && (hasCredentialRefs(servers, providers)) {
		return nil, ErrInvalidConfig
	}
	if hasMutatingTools(tools) && config.Effects == nil {
		return nil, ErrInvalidConfig
	}
	client, err := newSecureHTTPClient(config.resolver, config.transport, toolLimits.maxMCPResponse, toolLimits.maxModelResponse)
	if err != nil {
		return nil, err
	}
	maxRequests := config.MaxModelRequests
	if maxRequests == 0 || (config.ModelRoute.Budget.MaxRequests > 0 && config.ModelRoute.Budget.MaxRequests < maxRequests) {
		maxRequests = config.ModelRoute.Budget.MaxRequests
	}
	if maxRequests == 0 {
		maxRequests = DefaultMaxModelRequests
	}
	maxTokens := config.MaxModelTokens
	if maxTokens == 0 || (config.ModelRoute.Budget.MaxTokens > 0 && config.ModelRoute.Budget.MaxTokens < maxTokens) {
		maxTokens = config.ModelRoute.Budget.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = DefaultMaxModelTokens
	}
	if maxRequests < 1 || maxRequests > MaxConfiguredModelRequests || maxTokens < 1 || maxTokens > MaxConfiguredModelTokens {
		return nil, ErrInvalidConfig
	}
	return &Broker{
		runUID: config.RunUID, specDigest: config.SpecDigest, baseSHA: config.BaseSHA,
		servers: servers, tools: tools, profiles: profiles, providers: providers,
		credentials: config.Credentials, effects: config.Effects, artifacts: config.Artifacts,
		resultStore:    chooseResultStore(config.ResultStore, config.Artifacts),
		workspaceRoot:  config.WorkspaceRoot,
		contextRoot:    config.ContextRoot,
		policyContract: config.PolicyContract,
		pricing:        config.Pricing, client: client, limits: toolLimits, clock: clock,
		budget:          budgetState{maxToolCalls: toolLimits.maxToolCalls, maxRequests: maxRequests, maxTokens: maxTokens, maxCostMicros: maxCost},
		requireApproval: config.RequireApprovalForMutations,
		mode:            mode,
		phase:           initialBrokerPhase(mode),
		phaseAuthorizer: config.PhaseTransitionAuthorizer,
	}, nil
}

// PublishContextArtifact independently verifies the dedicated context volume
// and stores only its deterministic credential-free evidence bundle. It is
// called by the trusted broker process before the loopback service starts.
func (b *Broker) PublishContextArtifact(ctx context.Context) (v1alpha1.ArtifactRef, error) {
	if b == nil || ctx == nil || b.contextRoot == "" || b.artifacts == nil {
		return v1alpha1.ArtifactRef{}, ErrArtifactUnavailable
	}
	ref, err := contextartifact.Publish(ctx, b.artifacts, b.contextRoot, b.runUID, b.specDigest, b.baseSHA)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	return ref, nil
}

func chooseResultStore(resultStore, artifacts ArtifactStore) ArtifactStore {
	if resultStore != nil {
		return resultStore
	}
	return artifacts
}

func initialBrokerPhase(mode BrokerMode) v1alpha1.ToolProfileName {
	if mode == BrokerModeVerifier {
		return v1alpha1.ToolProfileVerify
	}
	return v1alpha1.ToolProfileExplore
}
