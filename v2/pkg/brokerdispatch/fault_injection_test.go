package brokerdispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFaultInjectionControlPlaneRestartReconcilesOnlyAfterBrokerExit models a
// control-plane restart: the replacement wrapper first observes a live broker
// lock and leaves it alone, then removes the same wrapper-owned directory once
// the old listener has stopped without running its normal cleanup path.
func TestFaultInjectionControlPlaneRestartReconcilesOnlyAfterBrokerExit(t *testing.T) {
	inner := &fakeActivities{}
	factory := brokerFactory()
	first, manager := newTestDispatcher(t, inner, factory)
	input := brokerInput()
	result, err := first.ScheduleRunnerTask(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	prepared := inner.inputs()
	if len(prepared) != 1 || result.TaskID == "" {
		t.Fatalf("unexpected scheduled task: result=%#v inputs=%#v", result, prepared)
	}
	sessionID := prepared[0].Execution.BrokerSessionID
	sessionDir := filepath.Join(first.root, sessionID)
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("live broker session was not created: %v", err)
	}

	second, err := New(inner, Config{
		BrokerRoot: first.root, Manager: manager, Factory: factory, UserID: "user-1",
		RunnerUID: uint32(os.Geteuid()), RunnerGID: uint32(os.Getegid()), SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("replacement control plane removed a live broker session: %v", err)
	}

	record := first.recordForTask(result.TaskID)
	if record == nil || record.server == nil {
		t.Fatal("scheduled task has no broker session record")
	}
	// Close only the listener/lock. This is the crash boundary; the old
	// process does not call Activities.Close and therefore leaves its directory.
	if err := record.server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Reconcile(context.Background()); err != nil {
		t.Fatalf("replacement reconciliation failed: %v", err)
	}
	if _, err := os.Stat(sessionDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale broker session was not removed: %v", err)
	}
}
