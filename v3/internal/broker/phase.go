package broker

import (
	"context"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

// CurrentPhase returns the broker's current host-owned tool profile.
func (b *Broker) CurrentPhase() v1alpha1.ToolProfileName {
	if b == nil {
		return ""
	}
	b.phaseMu.RLock()
	defer b.phaseMu.RUnlock()
	return b.phase
}

// TrustedPhaseTransitioner is the deliberately narrow host-supervisor seam
// for the explore -> edit transition. Broker is the intended implementation;
// its TransitionToEdit method still requires the independently configured
// PhaseTransitionAuthorizer. A transport must not treat possession of this
// interface as authorization by itself.
type TrustedPhaseTransitioner interface {
	TransitionToEdit(context.Context) error
}

// PhaseSupervisor is the broker-side half of the trusted phase-supervisor
// contract. It owns an in-memory capability that can only be attached to a
// request by RuntimeHandler's private supervisor handler. The capability is
// deliberately not serializable, exposed as a token, or accepted from MCP or
// the agent-facing HTTP listener.
//
// A future deployment adapter may construct one PhaseSupervisor, pass
// Authorizer() to Broker, and pass the same value to RuntimeHandlerConfig. The
// adapter must authenticate its caller outside this package; this capability
// then binds that authenticated request to this exact Broker instance. The
// current command deliberately does not construct or expose this path.
type PhaseSupervisor struct {
	capability *phaseSupervisorCapability
}

// Keep this capability non-zero-sized. Go permits pointers to distinct
// zero-sized allocations to compare equal, which would make pointer identity
// an unsafe authority boundary.
type phaseSupervisorCapability struct{ marker byte }

type phaseSupervisorContextKey struct{}

type phaseSupervisorAuthorizer struct {
	capability *phaseSupervisorCapability
}

// NewPhaseSupervisor creates the paired host-only capability used to authorize
// Explore -> Edit. Without the RuntimeHandler private supervisor path, the
// returned Authorizer rejects every context, including context.Background().
func NewPhaseSupervisor() *PhaseSupervisor {
	return &PhaseSupervisor{capability: &phaseSupervisorCapability{}}
}

// Authorizer returns the broker-side authorization half of the capability.
// The returned value is safe to retain in Broker.Config; it contains no token
// and cannot be satisfied by an agent-authored HTTP request.
func (s *PhaseSupervisor) Authorizer() PhaseTransitionAuthorizer {
	if s == nil || s.capability == nil {
		return nil
	}
	return phaseSupervisorAuthorizer{capability: s.capability}
}

func (s *PhaseSupervisor) withAuthority(ctx context.Context) context.Context {
	if s == nil || s.capability == nil || ctx == nil {
		return nil
	}
	return context.WithValue(ctx, phaseSupervisorContextKey{}, s.capability)
}

func (s *PhaseSupervisor) hasAuthority(ctx context.Context) bool {
	if s == nil || s.capability == nil || ctx == nil {
		return false
	}
	capability, ok := ctx.Value(phaseSupervisorContextKey{}).(*phaseSupervisorCapability)
	return ok && capability == s.capability
}

func (a phaseSupervisorAuthorizer) AuthorizeExploreToEdit(ctx context.Context) error {
	if ctx == nil || a.capability == nil {
		return ErrPhaseTransitionDenied
	}
	capability, ok := ctx.Value(phaseSupervisorContextKey{}).(*phaseSupervisorCapability)
	if !ok || capability != a.capability {
		return ErrPhaseTransitionDenied
	}
	return nil
}

// TransitionToEdit performs the only agent-mode phase transition. Every call,
// including an idempotent replay after the phase is already edit, must pass the
// host authorizer. Otherwise a caller could wait for an authorized transition
// and then invoke the method without authority while still receiving success.
func (b *Broker) TransitionToEdit(ctx context.Context) error {
	if b == nil || ctx == nil {
		return ErrPhaseTransitionDenied
	}
	b.transitionMu.Lock()
	defer b.transitionMu.Unlock()

	b.phaseMu.RLock()
	phase := b.phase
	mode := b.mode
	authorizer := b.phaseAuthorizer
	b.phaseMu.RUnlock()
	if mode != BrokerModeAgent || (phase != v1alpha1.ToolProfileExplore && phase != v1alpha1.ToolProfileEdit) || authorizer == nil {
		return ErrPhaseTransitionDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := authorizer.AuthorizeExploreToEdit(ctx); err != nil {
		return ErrPhaseTransitionDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	b.phaseMu.Lock()
	defer b.phaseMu.Unlock()
	if b.phase == v1alpha1.ToolProfileEdit {
		return nil
	}
	if b.mode != BrokerModeAgent || b.phase != v1alpha1.ToolProfileExplore {
		return ErrPhaseTransitionDenied
	}
	b.phase = v1alpha1.ToolProfileEdit
	return nil
}

type toolLookupResult uint8

const (
	toolLookupUnknown toolLookupResult = iota
	toolLookupAllowed
	toolLookupPhaseDenied
)

// lookupToolForCall takes a phase snapshot before any budget or policy work.
// The compiled maps are immutable, so the returned tool remains a valid
// request snapshot if a concurrent host transition changes the current phase.
func (b *Broker) lookupToolForCall(server, name string) (compiledTool, toolLookupResult) {
	if b == nil {
		return compiledTool{}, toolLookupUnknown
	}
	b.phaseMu.RLock()
	defer b.phaseMu.RUnlock()
	tool, known := b.tools[name]
	if !known || tool.server != server {
		return compiledTool{}, toolLookupUnknown
	}
	current, allowed := b.profiles[b.phase][name]
	if !allowed {
		return tool, toolLookupPhaseDenied
	}
	return current, toolLookupAllowed
}

func (b *Broker) lookupToolByName(name string) (compiledTool, toolLookupResult) {
	if b == nil {
		return compiledTool{}, toolLookupUnknown
	}
	b.phaseMu.RLock()
	defer b.phaseMu.RUnlock()
	tool, known := b.tools[name]
	if !known {
		return compiledTool{}, toolLookupUnknown
	}
	current, allowed := b.profiles[b.phase][name]
	if !allowed {
		return tool, toolLookupPhaseDenied
	}
	return current, toolLookupAllowed
}

func (b *Broker) currentTools() map[string]compiledTool {
	if b == nil {
		return nil
	}
	b.phaseMu.RLock()
	defer b.phaseMu.RUnlock()
	current := b.profiles[b.phase]
	tools := make(map[string]compiledTool, len(current))
	for name, tool := range current {
		tools[name] = cloneCompiledTool(tool)
	}
	return tools
}
