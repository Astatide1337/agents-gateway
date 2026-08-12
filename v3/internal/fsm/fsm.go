// Package fsm owns the AgentRun lifecycle transition contract.
package fsm

import (
	"fmt"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func IsTerminal(phase v1alpha1.Phase) bool {
	switch phase {
	case v1alpha1.PhaseSucceeded, v1alpha1.PhaseRejected, v1alpha1.PhaseFailed,
		v1alpha1.PhaseCancelled, v1alpha1.PhaseUnknownEffect:
		return true
	default:
		return false
	}
}

func CanTransition(from, to v1alpha1.Phase) bool {
	if !IsKnownPhase(from) || !IsKnownPhase(to) {
		return false
	}
	if from == to {
		return !IsTerminal(from)
	}
	for _, transition := range transitionTable {
		if transition.From == from && transition.To == to {
			return true
		}
	}
	return false
}

func ValidateTransition(from, to v1alpha1.Phase) error {
	if !IsKnownPhase(from) || !IsKnownPhase(to) {
		return fmt.Errorf("unknown phase transition %q -> %q", from, to)
	}
	if CanTransition(from, to) {
		return nil
	}
	if IsTerminal(from) {
		return fmt.Errorf("terminal phase %q cannot transition to %q", from, to)
	}
	return fmt.Errorf("phase %q cannot transition to %q", from, to)
}

func Transition(from, to v1alpha1.Phase) (v1alpha1.Phase, error) {
	if err := ValidateTransition(from, to); err != nil {
		return from, err
	}
	return to, nil
}
