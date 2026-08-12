package fsm

import (
	"errors"
	"testing"

	v1alpha1 "github.com/Astatide1337/agents-gateway/v3/api/v1alpha1"
)

func TestAgentRunTransitionTable(t *testing.T) {
	tests := []struct {
		name  string
		from  Phase
		event Event
		want  Phase
	}{
		{"pending to cloning", PhasePending, EventStartCloning, PhaseCloning},
		{"cloning to working", PhaseCloning, EventCloneSucceeded, PhaseWorking},
		{"working to capturing", PhaseWorking, EventWorkSucceeded, PhaseCapturing},
		{"capturing to verifying", PhaseCapturing, EventCaptureSucceeded, PhaseVerifying},
		{"verifying to gated", PhaseVerifying, EventVerifySucceeded, PhaseGated},
		{"gated to publishing", PhaseGated, EventGateAccepted, PhasePublishing},
		{"gated to rejected", PhaseGated, EventGateRejected, PhaseRejected},
		{"shadow rejection proceeds to publish", PhaseGated, EventShadowRejected, PhasePublishing},
		{"publishing to succeeded", PhasePublishing, EventPublishSucceeded, PhaseSucceeded},
		{"shadow publication retains rejected outcome", PhasePublishing, EventShadowPublished, PhaseRejected},
		{"publishing to unknown effect", PhasePublishing, EventUnknownEffect, PhaseUnknownEffect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Apply(test.from, test.event)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("Apply() = %s, want %s", got, test.want)
			}
			if !CanTransition(test.from, test.want) {
				t.Fatalf("CanTransition(%s, %s) = false", test.from, test.want)
			}
		})
	}
}

func TestCancellationIsAllowedFromEveryActivePhaseAndIsTerminal(t *testing.T) {
	for _, phase := range []Phase{
		PhasePending,
		PhaseCloning,
		PhaseWorking,
		PhaseCapturing,
		PhaseVerifying,
		PhaseGated,
		PhasePublishing,
	} {
		next, err := Apply(phase, EventCancelled)
		if err != nil {
			t.Fatalf("Apply(%s, cancelled) error = %v", phase, err)
		}
		if next != PhaseCancelled || !IsTerminal(next) {
			t.Fatalf("cancel from %s = %s, want terminal Cancelled", phase, next)
		}
	}
	for _, phase := range terminalPhases() {
		if _, err := Apply(phase, EventCancelled); !errors.Is(err, ErrTerminalPhase) {
			t.Errorf("terminal phase %s cancellation error = %v, want ErrTerminalPhase", phase, err)
		}
		if CanTransition(phase, PhaseCancelled) {
			t.Errorf("terminal phase %s can transition to Cancelled", phase)
		}
	}
}

func TestUnknownEffectIsTerminalAndCannotBeRetried(t *testing.T) {
	next, err := Apply(PhasePublishing, EventUnknownEffect)
	if err != nil {
		t.Fatal(err)
	}
	if next != PhaseUnknownEffect || !IsTerminal(next) {
		t.Fatalf("unknown effect phase = %s, want terminal UnknownEffect", next)
	}
	for _, event := range []Event{EventPublishSucceeded, EventFailed, EventCancelled, EventUnknownEffect} {
		if _, err := Apply(next, event); !errors.Is(err, ErrTerminalPhase) {
			t.Errorf("retry event %s from UnknownEffect error = %v, want ErrTerminalPhase", event, err)
		}
	}
	workUnknown, err := Apply(PhaseWorking, EventUnknownEffect)
	if err != nil || workUnknown != PhaseUnknownEffect {
		t.Fatalf("ambiguous broker effect did not stop Working: phase=%s err=%v", workUnknown, err)
	}
}

func TestInvalidTransitionsFailClosedAndDoNotMutateMachine(t *testing.T) {
	invalid := []struct {
		from Phase
		to   Phase
	}{
		{PhasePending, PhaseWorking},
		{PhaseCloning, PhaseGated},
		{PhaseVerifying, PhasePublishing},
	}
	for _, test := range invalid {
		if CanTransition(test.from, test.to) {
			t.Errorf("CanTransition(%s, %s) = true", test.from, test.to)
		}
		if err := TransitionTo(test.from, test.to); err == nil {
			t.Errorf("TransitionTo(%s, %s) unexpectedly succeeded", test.from, test.to)
		}
	}

	machine, err := New(PhasePending)
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Apply(EventWorkSucceeded); err == nil {
		t.Fatal("invalid event unexpectedly succeeded")
	}
	if machine.Phase() != PhasePending {
		t.Fatalf("invalid event mutated machine to %s", machine.Phase())
	}
	if _, err := Apply(Phase("invented"), EventStartCloning); !errors.Is(err, ErrInvalidPhase) {
		t.Fatalf("unknown phase error = %v, want ErrInvalidPhase", err)
	}
	if CanTransition(Phase("invented"), Phase("invented")) {
		t.Fatal("unknown phase self-transition was allowed")
	}
	if _, err := Apply(PhasePending, Event("invented")); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("unknown event error = %v, want ErrUnknownEvent", err)
	}
}

func TestTerminalPhasesAreExactlyTheDeclaredOutcomes(t *testing.T) {
	got := make(map[Phase]bool)
	for _, phase := range allPhases() {
		if IsTerminal(phase) {
			got[phase] = true
		}
	}
	want := map[Phase]bool{
		v1alpha1.PhaseSucceeded:     true,
		v1alpha1.PhaseRejected:      true,
		v1alpha1.PhaseFailed:        true,
		v1alpha1.PhaseCancelled:     true,
		v1alpha1.PhaseUnknownEffect: true,
	}
	if len(got) != len(want) {
		t.Fatalf("terminal phases = %#v, want %#v", got, want)
	}
	for phase := range want {
		if !got[phase] {
			t.Errorf("missing terminal phase %s", phase)
		}
	}
}

func terminalPhases() []Phase {
	return []Phase{PhaseSucceeded, PhaseRejected, PhaseFailed, PhaseCancelled, PhaseUnknownEffect}
}

func allPhases() []Phase {
	return append(activePhases(), terminalPhases()...)
}
