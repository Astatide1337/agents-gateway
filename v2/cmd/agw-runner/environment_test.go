package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/secretmaterialization"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
)

type runnerSecretResolver struct{ value []byte }

func (r runnerSecretResolver) Resolve(context.Context, string) ([]byte, error) {
	return append([]byte(nil), r.value...), nil
}

type environmentCaptureBackend struct {
	handle  *fakeHandle
	started atomic.Int32
	path    atomic.Value
	content atomic.Value
}

func (b *environmentCaptureBackend) Name() runner.BackendKind { return runner.BackendPodman }
func (b *environmentCaptureBackend) Capabilities() runner.BackendCapabilities {
	return runner.RootlessPodmanCapabilities()
}
func (b *environmentCaptureBackend) Start(_ context.Context, spec runner.SandboxSpec) (runner.SandboxHandle, error) {
	b.started.Add(1)
	if spec.EnvironmentFile == "" {
		return nil, errors.New("environment file was not attached")
	}
	data, err := os.ReadFile(spec.EnvironmentFile)
	if err != nil {
		return nil, err
	}
	b.path.Store(spec.EnvironmentFile)
	b.content.Store(string(data))
	return b.handle, nil
}

func TestDaemonMaterializesEnvironmentOnlyInsideRunnerAndCleansAfterRun(t *testing.T) {
	const secret = "runner-host-secret"
	lease := validRunLease()
	handle := &fakeHandle{stdout: strings.Join([]string{
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.started","run_id":"run-1","seq":1,"terminal":false,"data":{"agent_id":"a","sandbox_id":"s"}}`,
		`{"protocol":"agw.runtime.v1","kind":"event","type":"run.completed","run_id":"run-1","seq":2,"terminal":true,"data":{"result":"ok","output":{"id":"artifact-1","uri":"s3://artifacts/artifact-1","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size_bytes":2,"media_type":"application/json"}}}`,
		"",
	}, "\n")}
	backend := &environmentCaptureBackend{handle: handle}
	materialRoot := t.TempDir()
	if err := os.Chmod(materialRoot, 0700); err != nil {
		t.Fatal(err)
	}
	materializer, err := secretmaterialization.New(materialRoot, runnerSecretResolver{value: []byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	contract := validRunContract()
	contract.Execution.Environment = []agentworkflow.EnvironmentReference{{Name: "API_KEY", Ref: "secret://opaque-provider"}}
	request := RunRequest{RunID: "run-1", Lease: lease, Spec: validRuntimeSpec(), Contract: contract}
	result, err := (Daemon{Backend: backend, Leases: StaticLeaseSource{Lease: lease}, Materializer: materializer}).Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 2 || backend.started.Load() != 1 || handle.stops.Load() != 1 {
		t.Fatalf("unexpected run result=%#v starts=%d stops=%d", result, backend.started.Load(), handle.stops.Load())
	}
	if got := backend.content.Load().(string); got != "API_KEY="+secret+"\n" {
		t.Fatalf("runtime did not receive expected host material: %q", got)
	}
	path := backend.path.Load().(string)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("materialized environment remains after run: %v", err)
	}
	if strings.Contains(path, secret) {
		t.Fatal("secret appeared in materialized path")
	}
	if strings.Contains(handle.commands.String(), secret) || !strings.Contains(handle.commands.String(), "secret://opaque-provider") {
		t.Fatalf("run contract leaked or omitted the opaque reference: %q", handle.commands.String())
	}
}

func TestDaemonFailsClosedWhenEnvironmentMaterializerIsMissing(t *testing.T) {
	lease := validRunLease()
	handle := &fakeHandle{}
	backend := &environmentCaptureBackend{handle: handle}
	contract := validRunContract()
	contract.Execution.Environment = []agentworkflow.EnvironmentReference{{Name: "API_KEY", Ref: "secret://opaque-provider"}}
	_, err := (Daemon{Backend: backend, Leases: StaticLeaseSource{Lease: lease}}).Run(context.Background(), RunRequest{RunID: "run-1", Lease: lease, Spec: validRuntimeSpec(), Contract: contract})
	if err == nil || !strings.Contains(err.Error(), "materializer") {
		t.Fatalf("expected fail-closed materializer error, got %v", err)
	}
	if backend.started.Load() != 0 {
		t.Fatal("sandbox started without secret materializer")
	}
}
