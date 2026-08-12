// Package fsm defines the fail-closed AgentRun lifecycle.
//
// The event table is the source of truth for phase transitions. A terminal
// phase has no outgoing lifecycle events. Direct same-phase updates are
// accepted as idempotent observations, while cancellation is an operational
// terminal outcome distinct from failure and an ambiguous external effect is
// terminal UnknownEffect so reconciliation cannot retry a mutation blindly.
package fsm

import (
	"errors"
	"fmt"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

// Phase is the API phase type used by AgentRun.
type Phase = v1alpha1.Phase

const (
	PhasePending       = v1alpha1.PhasePending
	PhaseCloning       = v1alpha1.PhaseCloning
	PhaseWorking       = v1alpha1.PhaseWorking
	PhaseCapturing     = v1alpha1.PhaseCapturing
	PhaseVerifying     = v1alpha1.PhaseVerifying
	PhaseGated         = v1alpha1.PhaseGated
	PhasePublishing    = v1alpha1.PhasePublishing
	PhaseSucceeded     = v1alpha1.PhaseSucceeded
	PhaseRejected      = v1alpha1.PhaseRejected
	PhaseFailed        = v1alpha1.PhaseFailed
	PhaseCancelled     = v1alpha1.PhaseCancelled
	PhaseUnknownEffect = v1alpha1.PhaseUnknownEffect

	// Short names are convenient for table-driven controller code while the
	// Phase-prefixed names remain unambiguous at call sites.
	Pending       = PhasePending
	Cloning       = PhaseCloning
	Working       = PhaseWorking
	Capturing     = PhaseCapturing
	Verifying     = PhaseVerifying
	Gated         = PhaseGated
	Publishing    = PhasePublishing
	Succeeded     = PhaseSucceeded
	Rejected      = PhaseRejected
	Failed        = PhaseFailed
	Cancelled     = PhaseCancelled
	UnknownEffect = PhaseUnknownEffect
)

// Event identifies an observed lifecycle result. Events are deliberately
// narrower than arbitrary phase assignment so callers cannot bypass the
// transition table.
type Event string

const (
	EventStartCloning       Event = "start-cloning"
	EventCloneSucceeded     Event = "clone-succeeded"
	EventWorkSucceeded      Event = "work-succeeded"
	EventCaptureSucceeded   Event = "capture-succeeded"
	EventVerifySucceeded    Event = "verify-succeeded"
	EventGateAccepted       Event = "gate-accepted"
	EventGateRejected       Event = "gate-rejected"
	EventShadowRejected     Event = "shadow-rejected-publish"
	EventSucceededNoPublish Event = "succeeded-without-publish"
	EventPublishSucceeded   Event = "publish-succeeded"
	EventShadowPublished    Event = "shadow-publish-succeeded"
	EventFailed             Event = "failed"
	EventCancelled          Event = "cancelled"
	EventUnknownEffect      Event = "unknown-effect"

	// Event aliases make the table readable in integrations that describe
	// operations rather than outcomes.
	EventClone   = EventStartCloning
	EventWork    = EventCloneSucceeded
	EventCapture = EventWorkSucceeded
	EventVerify  = EventCaptureSucceeded
	EventGate    = EventVerifySucceeded
	EventPublish = EventPublishSucceeded
	EventCancel  = EventCancelled
	EventUnknown = EventUnknownEffect
)

var (
	ErrInvalidPhase      = errors.New("invalid AgentRun phase")
	ErrInvalidTransition = errors.New("invalid AgentRun phase transition")
	ErrTerminalPhase     = errors.New("terminal AgentRun phase has no outgoing transitions")
	ErrUnknownEvent      = errors.New("unknown AgentRun lifecycle event")
)

// EventTransition is one row in the event-driven AgentRun transition table.
// Direct phase updates continue to use the package's Transition function.
type EventTransition struct {
	From  Phase
	Event Event
	To    Phase
}

// transitionTable is intentionally explicit. Failure and cancellation rows
// are generated below for each active phase so that the normal lifecycle rows
// stay easy to audit.
var transitionTable = []EventTransition{
	{From: PhasePending, Event: EventStartCloning, To: PhaseCloning},
	{From: PhaseCloning, Event: EventCloneSucceeded, To: PhaseWorking},
	{From: PhaseWorking, Event: EventWorkSucceeded, To: PhaseCapturing},
	{From: PhaseCapturing, Event: EventCaptureSucceeded, To: PhaseVerifying},
	{From: PhaseVerifying, Event: EventVerifySucceeded, To: PhaseGated},
	{From: PhaseVerifying, Event: EventGateRejected, To: PhaseRejected},
	{From: PhaseGated, Event: EventGateAccepted, To: PhasePublishing},
	{From: PhaseGated, Event: EventGateRejected, To: PhaseRejected},
	{From: PhaseGated, Event: EventShadowRejected, To: PhasePublishing},
	{From: PhaseGated, Event: EventSucceededNoPublish, To: PhaseSucceeded},
	{From: PhasePublishing, Event: EventPublishSucceeded, To: PhaseSucceeded},
	{From: PhasePublishing, Event: EventShadowPublished, To: PhaseRejected},
	{From: PhasePublishing, Event: EventUnknownEffect, To: PhaseUnknownEffect},
}

func init() {
	for _, phase := range activePhases() {
		transitionTable = append(transitionTable,
			EventTransition{From: phase, Event: EventFailed, To: PhaseFailed},
			EventTransition{From: phase, Event: EventCancelled, To: PhaseCancelled},
		)
		// Tool effects can become ambiguous while the harness is working, not
		// only while the controller publishes a PR. Any such uncertainty is a
		// terminal stop: the control loop must never replay the mutation.
		if phase != PhasePublishing {
			transitionTable = append(transitionTable, EventTransition{From: phase, Event: EventUnknownEffect, To: PhaseUnknownEffect})
		}
	}
}

// TransitionTable returns a copy of the table used by Apply. Returning a copy
// prevents a caller from changing lifecycle policy process-wide.
func TransitionTable() []EventTransition {
	return append([]EventTransition(nil), transitionTable...)
}

// IsKnownPhase reports whether phase is one of the API-declared phases.
func IsKnownPhase(phase Phase) bool {
	switch phase {
	case PhasePending, PhaseCloning, PhaseWorking, PhaseCapturing,
		PhaseVerifying, PhaseGated, PhasePublishing, PhaseSucceeded,
		PhaseRejected, PhaseFailed, PhaseCancelled, PhaseUnknownEffect:
		return true
	default:
		return false
	}
}

// ValidatePhase rejects an empty or unknown phase.
func ValidatePhase(phase Phase) error {
	if !IsKnownPhase(phase) {
		return fmt.Errorf("%w: %q", ErrInvalidPhase, phase)
	}
	return nil
}

// IsActive reports whether phase can still make progress or be cancelled.
func IsActive(phase Phase) bool {
	return IsKnownPhase(phase) && !IsTerminal(phase)
}

// Apply advances phase using one row of the transition table.
func Apply(phase Phase, event Event) (Phase, error) {
	if err := ValidatePhase(phase); err != nil {
		return phase, err
	}
	if IsTerminal(phase) {
		return phase, fmt.Errorf("%w: %s", ErrTerminalPhase, phase)
	}
	for _, transition := range transitionTable {
		if transition.From == phase && transition.Event == event {
			return transition.To, nil
		}
	}
	if !knownEvent(event) {
		return phase, fmt.Errorf("%w: %q", ErrUnknownEvent, event)
	}
	return phase, fmt.Errorf("%w: %s + %s", ErrInvalidTransition, phase, event)
}

// Next is an alias for Apply.
func Next(phase Phase, event Event) (Phase, error) {
	return Apply(phase, event)
}

// TransitionTo validates a direct phase update against the same transition
// table. It is useful when a controller has already classified an observation
// into a target phase but still must fail closed on illegal jumps.
func TransitionTo(from, to Phase) error {
	if err := ValidatePhase(from); err != nil {
		return err
	}
	if err := ValidatePhase(to); err != nil {
		return err
	}
	if err := ValidateTransition(from, to); err != nil {
		if IsTerminal(from) {
			return fmt.Errorf("%w: %s: %v", ErrTerminalPhase, from, err)
		}
		return fmt.Errorf("%w: %s -> %s: %v", ErrInvalidTransition, from, to, err)
	}
	return nil
}

// ValidTransition is an alias for CanTransition.
func ValidTransition(from, to Phase) bool {
	return CanTransition(from, to)
}

// Machine is a small in-memory façade around the table. Durable controllers
// should persist the phase and replay an observed event; Machine is useful for
// unit tests and local decision code.
type Machine struct {
	phase Phase
}

// New returns a machine initialized at a known non-terminal or terminal phase.
// A terminal machine is valid for observation but cannot be advanced.
func New(initial Phase) (*Machine, error) {
	if err := ValidatePhase(initial); err != nil {
		return nil, err
	}
	return &Machine{phase: initial}, nil
}

// Phase returns the current phase.
func (m *Machine) Phase() Phase {
	if m == nil {
		return ""
	}
	return m.phase
}

// Apply observes event and updates the machine only when the transition is
// valid. On error the current phase is unchanged.
func (m *Machine) Apply(event Event) error {
	if m == nil {
		return ErrInvalidPhase
	}
	next, err := Apply(m.phase, event)
	if err != nil {
		return err
	}
	m.phase = next
	return nil
}

// Cancel requests the distinct terminal cancellation outcome.
func (m *Machine) Cancel() error {
	return m.Apply(EventCancelled)
}

// MarkUnknownEffect records an ambiguous external effect and stops further
// automatic work.
func (m *Machine) MarkUnknownEffect() error {
	return m.Apply(EventUnknownEffect)
}

func activePhases() []Phase {
	return []Phase{
		PhasePending,
		PhaseCloning,
		PhaseWorking,
		PhaseCapturing,
		PhaseVerifying,
		PhaseGated,
		PhasePublishing,
	}
}

func knownEvent(event Event) bool {
	switch event {
	case EventStartCloning, EventCloneSucceeded, EventWorkSucceeded,
		EventCaptureSucceeded, EventVerifySucceeded, EventGateAccepted,
		EventGateRejected, EventShadowRejected, EventSucceededNoPublish,
		EventPublishSucceeded, EventShadowPublished, EventFailed,
		EventCancelled, EventUnknownEffect:
		return true
	default:
		return false
	}
}
