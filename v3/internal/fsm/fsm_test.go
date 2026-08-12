package fsm

import (
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestTerminalPhasesCannotMove(t *testing.T) {
	terminals := []v1alpha1.Phase{
		v1alpha1.PhaseSucceeded, v1alpha1.PhaseRejected, v1alpha1.PhaseFailed,
		v1alpha1.PhaseCancelled, v1alpha1.PhaseUnknownEffect,
	}
	for _, phase := range terminals {
		if !IsTerminal(phase) {
			t.Errorf("%s is not terminal", phase)
		}
		if err := ValidateTransition(phase, v1alpha1.PhaseWorking); err == nil {
			t.Errorf("terminal phase %s unexpectedly transitioned", phase)
		}
		if err := ValidateTransition(phase, phase); err == nil {
			t.Errorf("terminal phase %s accepted a same-phase transition", phase)
		}
	}
}

func TestPublishingCanBecomeUnknownEffect(t *testing.T) {
	got, err := Transition(v1alpha1.PhasePublishing, v1alpha1.PhaseUnknownEffect)
	if err != nil {
		t.Fatal(err)
	}
	if got != v1alpha1.PhaseUnknownEffect {
		t.Fatalf("phase = %s", got)
	}
}
