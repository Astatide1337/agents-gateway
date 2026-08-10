package main

import "testing"

func TestConfigureUnixRunnerRequiresNoTLSMaterial(t *testing.T) {
	t.Setenv("AGW_RUNNER_TRANSPORT", "unix")
	t.Setenv("AGW_RUNNER_SOCKET", "/run/agw-runner/runner.sock")
	if _, err := configureRunnerClient(); err != nil {
		t.Fatalf("configure local runner: %v", err)
	}
}

func TestConfigureRunnerRejectsUnknownTransport(t *testing.T) {
	t.Setenv("AGW_RUNNER_TRANSPORT", "tcp")
	if _, err := configureRunnerClient(); err == nil {
		t.Fatal("expected unknown runner transport to fail closed")
	}
}
