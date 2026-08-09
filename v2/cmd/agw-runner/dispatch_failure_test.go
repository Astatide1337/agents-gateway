package main

import (
	"errors"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/sandbox"
)

func TestExecutionFailureCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{sandbox.ErrPodmanWorkspace, "podman_workspace_failed"},
		{sandbox.ErrPodmanPlan, "podman_plan_failed"},
		{sandbox.ErrPodmanMounts, "podman_mounts_failed"},
		{sandbox.ErrPodmanArguments, "podman_arguments_failed"},
		{sandbox.ErrPodmanStart, "podman_start_failed"},
		{sandbox.ErrRuntimeBroker, "runtime_broker_start_failed"},
		{sandbox.ErrRuntimeConfig, "runtime_adapter_config_failed"},
		{sandbox.ErrRuntimeContract, "runtime_contract_rejected"},
		{sandbox.ErrPodmanPermission, "podman_runtime_permission_failed"},
		{sandbox.ErrPodmanMount, "podman_runtime_mount_failed"},
		{sandbox.ErrPodmanRuntime, "podman_runtime_setup_failed"},
		{sandbox.ErrRuntimePreamble, "runtime_adapter_preamble_failed"},
		{sandbox.ErrRuntimeEnvironment, "runtime_adapter_environment_failed"},
		{sandbox.ErrRuntimeEntrypoint, "runtime_adapter_entrypoint_failed"},
		{sandbox.ErrRuntimeKilled, "runtime_adapter_killed"},
		{ErrRuntimeContractSend, "runtime_contract_send_failed"},
		{ErrRuntimeStreamIncomplete, "runtime_stream_incomplete"},
		{ErrRuntimeStream, "runtime_stream_failed"},
		{ErrRuntimeWait, "runtime_wait_failed"},
		{ErrRuntimeCleanup, "runtime_cleanup_failed"},
		{errors.New("unknown"), "execution_failed"},
	}
	for _, test := range tests {
		if got := executionFailureCode(errors.Join(errors.New("context"), test.err)); got != test.want {
			t.Fatalf("executionFailureCode(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}
