package workload

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Astatide1337/agents-gateway/v3/internal/canonical"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// AgentGatewaySidecarVersion is the only agentgateway revision reviewed by
	// the repository's composition spike. A version string is not sufficient to
	// enable the sidecar; it is paired with an image digest and probe evidence.
	AgentGatewaySidecarVersion = "v1.4.1"
	AgentGatewayImagePrefix    = "cr.agentgateway.dev/agentgateway@sha256:"

	// AgentGatewayConfigMapKey is the future per-run ConfigMap key. The
	// workload builder does not mount it yet because the work path has no
	// proven guard-to-agentgateway runtime adapter.
	AgentGatewayConfigMapKey = "agentgateway.yaml"
)

var (
	// ErrAgentGatewaySidecarUnsupported is returned after a complete, valid
	// enablement envelope has been supplied. The envelope is intentionally
	// modeled now so chart/operator wiring has one explicit contract, but Build
	// must not emit an unconnected third container. The current work broker is
	// still the direct provider/MCP data plane; GuardGatewayPolicy and the
	// Phase-0 chain are fixtures, not a production work-pod implementation.
	ErrAgentGatewaySidecarUnsupported = errors.New("agentgateway sidecar is not established for the work Sandbox")
)

// AgentGatewaySidecarOptions is the operator-resolved seam for a future
// per-run agentgateway container. Zero values are the only accepted disabled
// configuration. Enabled values are validated completely before Build rejects
// them as unsupported, so a partial configuration can never silently broaden
// the pod or be mistaken for production support.
//
// ConfigMapName must identify an immutable, per-run, credential-free
// ConfigMap. ConfigDigest binds the exact rendered configuration that the
// future adapter must mount under AgentGatewayConfigMapKey. CapabilityVerified
// and EvidenceDigest must come from a probe for the exact image/configuration;
// they are not inferred from the upstream version.
type AgentGatewaySidecarOptions struct {
	Enabled            bool
	Image              string
	ConfigMapName      string
	CapabilityVersion  string
	CapabilityVerified bool
	EvidenceDigest     string
	ConfigDigest       string
}

func validateAgentGatewaySidecar(options AgentGatewaySidecarOptions) error {
	if !options.Enabled {
		if options.Image != "" || options.ConfigMapName != "" || options.CapabilityVersion != "" || options.CapabilityVerified || options.EvidenceDigest != "" || options.ConfigDigest != "" {
			return fmt.Errorf("%w: disabled agentgateway configuration must be empty", ErrInvalidInput)
		}
		return nil
	}

	if !validPinnedImage(options.Image) || !strings.HasPrefix(options.Image, AgentGatewayImagePrefix) {
		return fmt.Errorf("%w: enabled agentgateway image must be a pinned official digest", ErrInvalidInput)
	}
	if options.ConfigMapName == "" || len(validation.IsDNS1123Subdomain(options.ConfigMapName)) != 0 {
		return fmt.Errorf("%w: enabled agentgateway ConfigMap name is invalid", ErrInvalidInput)
	}
	if options.CapabilityVersion != AgentGatewaySidecarVersion || !options.CapabilityVerified || !canonical.ValidDigest(options.EvidenceDigest) {
		return fmt.Errorf("%w: enabled agentgateway capability evidence is incomplete or mismatched", ErrInvalidInput)
	}
	if !canonical.ValidDigest(options.ConfigDigest) {
		return fmt.Errorf("%w: enabled agentgateway configuration digest is invalid", ErrInvalidInput)
	}

	// Do not turn the Phase-0 fixture into an implicit production backend. A
	// future change must first add the guard/runtime adapter and its tests, then
	// replace this rejection with construction of the proven topology.
	return fmt.Errorf("%w: current work broker has no proven guard-to-agentgateway adapter", ErrAgentGatewaySidecarUnsupported)
}

// ValidateAgentGatewaySidecar validates the operator-level sidecar envelope
// without building a Sandbox. The run-plan layer calls this before any
// credential materialization so unsupported or partial enablement cannot cause
// a run-side effect before it is rejected.
func ValidateAgentGatewaySidecar(options AgentGatewaySidecarOptions) error {
	return validateAgentGatewaySidecar(options)
}
