package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

// TestFaultInjectionRunnerProcessCrash is intentionally a process-level test.
// The child has persisted a task and entered the runner before it is killed;
// the parent then opens the same durable task directory as a new runner.
func TestFaultInjectionRunnerProcessCrash(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "child-ready")
	command := exec.Command(os.Args[0], "-test.run=TestFaultInjectionRunnerCrashHelper", "-test.v")
	command.Env = append(os.Environ(),
		"AGW_FAULT_RUNNER_CHILD=1",
		"AGW_FAULT_RUNNER_ROOT="+root,
		"AGW_FAULT_RUNNER_READY="+ready,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("runner crash helper did not persist and start its task")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("crash helper exited cleanly; the test did not exercise process loss")
	}

	restarted, err := NewPersistentDispatch(root, &faultCrashBackend{})
	if err != nil {
		t.Fatalf("reload runner state after process crash: %v", err)
	}
	input := faultCrashScheduleInput()
	status, err := restarted.Status(context.Background(), StatusTaskInput{
		OrganizationID: input.OrganizationID, ProjectID: input.ProjectID, RunID: input.RunID, TaskID: "task-" + stableTaskID(taskStorageKey(input)),
	}, "fault-owner")
	if err != nil {
		t.Fatalf("read reconciled task: %v", err)
	}
	if status.Status != "lost" || status.Error != "runner_restart" {
		t.Fatalf("crashed task was not reconciled to lost/runner_restart: %#v", status)
	}
}

// TestFaultInjectionRunnerCrashHelper is run only in the child process above.
// It intentionally blocks inside an active sandbox so the parent can terminate
// the runner without allowing normal cleanup to convert the event into a
// graceful failure.
func TestFaultInjectionRunnerCrashHelper(t *testing.T) {
	if os.Getenv("AGW_FAULT_RUNNER_CHILD") != "1" {
		return
	}
	root := os.Getenv("AGW_FAULT_RUNNER_ROOT")
	ready := os.Getenv("AGW_FAULT_RUNNER_READY")
	dispatch, err := NewPersistentDispatch(root, &faultCrashBackend{})
	if err != nil {
		t.Fatal(err)
	}
	input := faultCrashScheduleInput()
	result, err := dispatch.Schedule(context.Background(), input, "fault-owner")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "scheduled" || result.TaskID == "" {
		t.Fatalf("unexpected helper schedule result: %#v", result)
	}
	if err := os.WriteFile(ready, []byte(result.TaskID), 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

type faultCrashBackend struct{}

func (faultCrashBackend) Name() runner.BackendKind { return runner.BackendPodman }
func (faultCrashBackend) Capabilities() runner.BackendCapabilities {
	return runner.RootlessPodmanCapabilities()
}
func (faultCrashBackend) Start(context.Context, runner.SandboxSpec) (runner.SandboxHandle, error) {
	reader, writer := io.Pipe()
	return &faultCrashHandle{reader: reader, writer: writer}, nil
}

type faultCrashHandle struct {
	reader *io.PipeReader
	writer *io.PipeWriter
}

func (h *faultCrashHandle) ID() string        { return "fault-crash-sandbox" }
func (h *faultCrashHandle) Stdout() io.Reader { return h.reader }
func (h *faultCrashHandle) Send(ctx context.Context, _ []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
func (h *faultCrashHandle) Wait(ctx context.Context) (runner.ExitStatus, error) {
	<-ctx.Done()
	return runner.ExitStatus{}, ctx.Err()
}
func (h *faultCrashHandle) Stop(context.Context) error {
	_ = h.writer.Close()
	_ = h.reader.Close()
	return nil
}

func faultCrashScheduleInput() ScheduleTaskInput {
	return ScheduleTaskInput{
		OrganizationID: "fault-org", ProjectID: "fault-project", RunID: "fault-run",
		WorkflowName: "fault-workflow", StepID: "fault-step", AgentRef: "agent/fault",
		IdempotencyKey: "fault-idempotency", Execution: validRuntimeSpec(), Contract: validExecutionContract(),
	}
}
