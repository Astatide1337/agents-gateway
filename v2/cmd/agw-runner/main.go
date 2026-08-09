// agw-runner is the host-side sandbox daemon. It validates host/runtime
// prerequisites, leases work, supervises the strict runtime JSONL stream, and
// cleans up only after re-validating the lease.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	"github.com/Astatide1337/agents-gateway/v2/pkg/runtimeproto"
	"github.com/Astatide1337/agents-gateway/v2/pkg/sandbox"
	"github.com/Astatide1337/agents-gateway/v2/pkg/secretmaterialization"
	agentworkflow "github.com/Astatide1337/agents-gateway/v2/pkg/workflow"
	"github.com/Astatide1337/agents-gateway/v2/proto"
	"gopkg.in/yaml.v3"
)

var (
	ErrLeaseRequired             = errors.New("a valid lease is required")
	ErrRuntimeStreamIncomplete   = errors.New("runtime ended without a terminal event")
	ErrRuntimeStreamUnavailable  = errors.New("sandbox does not expose a runtime event stream")
	ErrSandboxControlUnavailable = errors.New("sandbox does not accept runner control input")
)

type Config struct {
	Backend       string           `yaml:"backend"`
	WorkspaceRoot string           `yaml:"workspaceRoot"`
	Secrets       SecretConfig     `yaml:"secrets"`
	Containerd    ContainerdConfig `yaml:"containerd"`
	Podman        PodmanConfig     `yaml:"podman"`
	ControlPlane  MTLSConfig       `yaml:"controlPlane"`
	Server        ServerTLSConfig  `yaml:"server"`
}

type SecretConfig struct {
	SourceRoot          string `yaml:"sourceRoot"`
	MaterializationRoot string `yaml:"materializationRoot"`
}

type ContainerdConfig struct {
	Address     string `yaml:"address"`
	Namespace   string `yaml:"namespace"`
	Snapshotter string `yaml:"snapshotter"`
	BrokerRoot  string `yaml:"brokerRoot"`
}

type PodmanConfig struct {
	Binary     string `yaml:"binary"`
	BrokerRoot string `yaml:"brokerRoot"`
}

type ServerTLSConfig struct {
	Transport    string `yaml:"transport"`
	ListenAddr   string `yaml:"listenAddr"`
	SocketPath   string `yaml:"socketPath"`
	CertFile     string `yaml:"certFile"`
	KeyFile      string `yaml:"keyFile"`
	ClientCAFile string `yaml:"clientCAFile"`
}

func (c ServerTLSConfig) TransportMode() string {
	mode := strings.ToLower(strings.TrimSpace(c.Transport))
	if mode == "" {
		return "mtls"
	}
	return mode
}

type MTLSConfig struct {
	Endpoint   string `yaml:"endpoint"`
	CAFile     string `yaml:"caFile"`
	CertFile   string `yaml:"certFile"`
	KeyFile    string `yaml:"keyFile"`
	ServerName string `yaml:"serverName"`
}

func (c MTLSConfig) Empty() bool {
	return strings.TrimSpace(c.Endpoint) == "" && c.CAFile == "" && c.CertFile == "" && c.KeyFile == "" && c.ServerName == ""
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read runner config: %w", err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode runner config: %w", err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.Backend {
	case string(runner.BackendContainerdRunsc):
		if strings.TrimSpace(c.Containerd.Address) == "" {
			return errors.New("containerd.address is required")
		}
	case string(runner.BackendPodman):
		if os.Geteuid() == 0 {
			return errors.New("rootless podman backend cannot run as root")
		}
	default:
		return fmt.Errorf("backend must be %q or %q", runner.BackendContainerdRunsc, runner.BackendPodman)
	}
	if !c.ControlPlane.Empty() {
		if err := c.ControlPlane.Validate(); err != nil {
			return fmt.Errorf("controlPlane: %w", err)
		}
	}
	if err := c.Server.Validate(); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func (c MTLSConfig) Validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return errors.New("endpoint is required")
	}
	if strings.HasPrefix(strings.ToLower(c.Endpoint), "http://") {
		return errors.New("control-plane endpoint must not use plaintext HTTP")
	}
	if c.CAFile == "" || c.CertFile == "" || c.KeyFile == "" {
		return errors.New("caFile, certFile, and keyFile are required")
	}
	for name, path := range map[string]string{"caFile": c.CAFile, "certFile": c.CertFile, "keyFile": c.KeyFile} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s must be a file", name)
		}
		if name == "keyFile" && info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("%s must not be group/world accessible", name)
		}
	}
	caBytes, err := os.ReadFile(c.CAFile)
	if err != nil {
		return fmt.Errorf("read caFile: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return errors.New("caFile contains no valid certificates")
	}
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return fmt.Errorf("load client certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return errors.New("client certificate is empty")
	}
	return nil
}

func (c Config) Readiness(ctx context.Context) (runner.ReadinessReport, error) {
	if !c.ControlPlane.Empty() {
		if err := c.ControlPlane.Validate(); err != nil {
			return runner.ReadinessReport{}, fmt.Errorf("control-plane mTLS: %w", err)
		}
	}
	if err := c.Server.Validate(); err != nil {
		return runner.ReadinessReport{}, fmt.Errorf("server mTLS: %w", err)
	}
	backend := runner.BackendKind(c.Backend)
	report, err := runner.DetectHostPrerequisites(ctx, backend, runner.HostDetectionOptions{
		PodmanBinary: c.Podman.Binary,
	})
	if err != nil {
		return runner.ReadinessReport{}, err
	}
	if backend == runner.BackendPodman && os.Geteuid() == 0 {
		report.Ready = false
		report.Checks = append(report.Checks, runner.Check{Name: "rootless-process", Needed: true, Passed: false, Detail: "effective UID is 0"})
	}
	return report, nil
}

func (c Config) BuildBackend(ctx context.Context) (runner.RuntimeBackend, runner.ReadinessReport, error) {
	if err := c.Validate(); err != nil {
		return nil, runner.ReadinessReport{}, err
	}
	report, err := c.Readiness(ctx)
	if err != nil {
		return nil, runner.ReadinessReport{}, err
	}
	if !report.Ready {
		return nil, report, errors.New("runner host is not ready")
	}
	workspaceRoot := c.WorkspaceRoot
	switch c.Backend {
	case string(runner.BackendContainerdRunsc):
		backend, err := sandbox.NewContainerdRunscBackend(ctx, sandbox.ContainerdRunscConfig{
			Address: c.Containerd.Address, Namespace: c.Containerd.Namespace, Snapshotter: c.Containerd.Snapshotter,
			WorkspaceRoot: workspaceRoot, BrokerRoot: c.Containerd.BrokerRoot,
		})
		if err == nil {
			err = backend.Reconcile(ctx)
		}
		return backend, report, err
	case string(runner.BackendPodman):
		backend, err := sandbox.NewRootlessPodmanBackend(sandbox.PodmanConfig{Binary: c.Podman.Binary, WorkspaceRoot: workspaceRoot, BrokerRoot: c.Podman.BrokerRoot})
		if err == nil {
			err = backend.Reconcile(ctx)
		}
		return backend, report, err
	default:
		return nil, report, fmt.Errorf("unsupported backend %q", c.Backend)
	}
}

func (c Config) BuildMaterializer() (*secretmaterialization.Materializer, error) {
	sourceRoot := strings.TrimSpace(c.Secrets.SourceRoot)
	materializationRoot := strings.TrimSpace(c.Secrets.MaterializationRoot)
	if sourceRoot == "" && materializationRoot == "" {
		return nil, nil
	}
	if sourceRoot == "" || materializationRoot == "" {
		return nil, errors.New("secrets.sourceRoot and secrets.materializationRoot must be configured together")
	}
	resolver, err := secretmaterialization.NewFileResolver(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("configure secret source: %w", err)
	}
	materializer, err := secretmaterialization.New(materializationRoot, resolver)
	if err != nil {
		return nil, fmt.Errorf("configure secret materializer: %w", err)
	}
	return materializer, nil
}

type RunRequest struct {
	RunID        string             `json:"run_id"`
	Lease        runner.Lease       `json:"lease"`
	CurrentLease runner.Lease       `json:"current_lease"`
	Spec         runner.SandboxSpec `json:"spec"`
	Contract     RunContract        `json:"contract"`
}

// RunContract is the bounded, immutable input delivered to the adapter. It
// includes declarative instructions but excludes runtime prompt/input bytes,
// secrets, credentials, and runtime sockets; payloads remain externalized
// behind immutable references.
type RunContract struct {
	OrganizationID   string                          `json:"organization_id"`
	ProjectID        string                          `json:"project_id"`
	RunID            string                          `json:"run_id"`
	WorkflowName     string                          `json:"workflow_name"`
	StepID           string                          `json:"step_id"`
	AgentRef         string                          `json:"agent_ref"`
	InputRef         string                          `json:"input_ref,omitempty"`
	DependencyOutput []agentworkflow.ArtifactRef     `json:"dependency_output,omitempty"`
	Execution        agentworkflow.ExecutionContract `json:"execution"`
}

const (
	maxContractFieldBytes = 4096
	maxContractRefs       = 256
	maxContractBytes      = 64 << 10
)

func (c RunContract) Validate(runID string) error {
	if c.RunID != runID {
		return errors.New("run contract run_id does not match request")
	}
	for name, value := range map[string]string{
		"organization_id": c.OrganizationID, "project_id": c.ProjectID,
		"run_id": c.RunID, "workflow_name": c.WorkflowName, "step_id": c.StepID,
		"agent_ref": c.AgentRef,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("run contract %s is required", name)
		}
		if len(value) > maxContractFieldBytes {
			return fmt.Errorf("run contract %s exceeds %d bytes", name, maxContractFieldBytes)
		}
	}
	if len(c.InputRef) > maxContractFieldBytes {
		return fmt.Errorf("run contract input_ref exceeds %d bytes", maxContractFieldBytes)
	}
	if len(c.DependencyOutput) > maxContractRefs {
		return fmt.Errorf("run contract has too many dependency outputs")
	}
	for i, ref := range c.DependencyOutput {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("run contract dependency_output[%d]: %w", i, err)
		}
	}
	if err := c.Execution.Validate(); err != nil {
		return fmt.Errorf("run contract execution: %w", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode run contract: %w", err)
	}
	if len(data) > maxContractBytes {
		return fmt.Errorf("run contract exceeds %d bytes", maxContractBytes)
	}
	return nil
}

type LeaseSource interface {
	Current(context.Context, string) (runner.Lease, error)
}

type StaticLeaseSource struct{ Lease runner.Lease }

func (s StaticLeaseSource) Current(ctx context.Context, runID string) (runner.Lease, error) {
	if err := ctx.Err(); err != nil {
		return runner.Lease{}, err
	}
	return s.Lease, nil
}

type EventSink func(context.Context, proto.Envelope) error

type RunResult struct {
	ExitStatus   runner.ExitStatus `json:"exit_status"`
	Events       int               `json:"events"`
	TerminalType string            `json:"terminal_type,omitempty"`
}

type TerminalOutcomeError struct {
	Type string
}

func (e *TerminalOutcomeError) Error() string {
	return fmt.Sprintf("runtime terminal event %q did not complete successfully", e.Type)
}

type Daemon struct {
	Backend      runner.RuntimeBackend
	Leases       LeaseSource
	Materializer EnvironmentMaterializer
	Sink         EventSink
	OnStart      func(sandbox.OutputHandle)
	Now          func() time.Time
	CleanupWait  time.Duration
}

// EnvironmentMaterializer is the trusted runner-side boundary for resolving
// opaque host secret references. Implementations return only a private path;
// resolved values never cross into this process's run contract or event sink.
type EnvironmentMaterializer interface {
	Materialize(context.Context, string, []secretmaterialization.Reference) (*secretmaterialization.Materialization, error)
}

func (d Daemon) Run(ctx context.Context, request RunRequest) (result RunResult, runErr error) {
	if strings.TrimSpace(request.RunID) == "" || request.Lease.RunID != request.RunID {
		return RunResult{}, ErrLeaseRequired
	}
	if d.Backend == nil || d.Leases == nil {
		return RunResult{}, errors.New("runner backend and lease source are required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.CleanupWait <= 0 {
		d.CleanupWait = 15 * time.Second
	}
	if err := d.validateLease(ctx, request); err != nil {
		return RunResult{}, err
	}
	if err := request.Contract.Validate(request.RunID); err != nil {
		return RunResult{}, fmt.Errorf("run contract: %w", err)
	}
	if len(request.Contract.Execution.Environment) > 0 {
		if d.Materializer == nil {
			return RunResult{}, errors.New("secret materializer is required for environment references")
		}
		references := make([]secretmaterialization.Reference, len(request.Contract.Execution.Environment))
		for index, reference := range request.Contract.Execution.Environment {
			references[index] = secretmaterialization.Reference{Name: reference.Name, Ref: reference.Ref}
		}
		materialized, err := d.Materializer.Materialize(ctx, request.RunID, references)
		if err != nil {
			return RunResult{}, fmt.Errorf("materialize run environment: %w", err)
		}
		if materialized == nil || materialized.EnvironmentFile() == "" {
			return RunResult{}, errors.New("secret materializer returned no environment file")
		}
		request.Spec.EnvironmentFile = materialized.EnvironmentFile()
		defer func() {
			if cleanupErr := materialized.Cleanup(); cleanupErr != nil {
				runErr = errors.Join(runErr, cleanupErr)
			}
		}()
	}
	handle, err := d.Backend.Start(ctx, request.Spec)
	if err != nil {
		return RunResult{}, fmt.Errorf("start sandbox: %w", err)
	}
	output, ok := handle.(sandbox.OutputHandle)
	if !ok {
		_ = d.cleanup(ctx, request, handle)
		return RunResult{}, ErrRuntimeStreamUnavailable
	}
	control, ok := handle.(sandbox.ControlHandle)
	if !ok {
		cleanupErr := d.cleanup(ctx, request, handle)
		return RunResult{}, errors.Join(ErrSandboxControlUnavailable, cleanupErr)
	}
	if d.OnStart != nil {
		d.OnStart(output)
	}
	if err := sendRunStart(ctx, request, control); err != nil {
		cleanupErr := d.cleanup(ctx, request, handle)
		return RunResult{}, errors.Join(fmt.Errorf("send run contract: %w", err), cleanupErr)
	}

	supervisorDone := make(chan supervisionResult, 1)
	go func() { supervisorDone <- superviseRuntime(ctx, request.RunID, output.Stdout(), d.Sink) }()
	waitDone := make(chan waitResult, 1)
	go func() {
		status, waitErr := handle.Wait(ctx)
		waitDone <- waitResult{status: status, err: waitErr}
	}()

	var events supervisionResult
	select {
	case events = <-supervisorDone:
		if events.err != nil {
			cleanupErr := d.cleanup(ctx, request, handle)
			if cleanupErr != nil {
				return RunResult{Events: events.count}, errors.Join(events.err, cleanupErr)
			}
			return RunResult{Events: events.count, TerminalType: events.terminalType}, events.err
		}
	case <-ctx.Done():
		cleanupErr := d.cleanup(ctx, request, handle)
		if cleanupErr != nil {
			return RunResult{}, errors.Join(ctx.Err(), cleanupErr)
		}
		return RunResult{}, ctx.Err()
	case result := <-waitDone:
		if result.err != nil {
			cleanupErr := d.cleanup(ctx, request, handle)
			if cleanupErr != nil {
				return RunResult{}, errors.Join(result.err, cleanupErr)
			}
			return RunResult{}, result.err
		}
		select {
		case events = <-supervisorDone:
		case <-time.After(d.CleanupWait):
			cleanupErr := d.cleanup(ctx, request, handle)
			return RunResult{ExitStatus: result.status}, errors.Join(ErrRuntimeStreamIncomplete, cleanupErr)
		}
		cleanupErr := d.cleanup(ctx, request, handle)
		if events.err != nil || !events.terminal {
			if !events.terminal {
				events.err = errors.Join(events.err, ErrRuntimeStreamIncomplete)
			}
			return RunResult{ExitStatus: result.status, Events: events.count, TerminalType: events.terminalType}, errors.Join(events.err, cleanupErr)
		}
		outcomeErr := validateTerminalOutcome(events, result.status)
		return RunResult{ExitStatus: result.status, Events: events.count, TerminalType: events.terminalType}, errors.Join(outcomeErr, cleanupErr)
	}

	// A valid stream that is still running must eventually be collected. A
	// supervisor error has already triggered cleanup above.
	select {
	case result := <-waitDone:
		cleanupErr := d.cleanup(ctx, request, handle)
		if !events.terminal {
			return RunResult{ExitStatus: result.status, Events: events.count, TerminalType: events.terminalType}, errors.Join(ErrRuntimeStreamIncomplete, cleanupErr)
		}
		outcomeErr := validateTerminalOutcome(events, result.status)
		return RunResult{ExitStatus: result.status, Events: events.count, TerminalType: events.terminalType}, errors.Join(outcomeErr, cleanupErr)
	case <-ctx.Done():
		cleanupErr := d.cleanup(ctx, request, handle)
		return RunResult{Events: events.count}, errors.Join(ctx.Err(), cleanupErr)
	}
}

type waitResult struct {
	status runner.ExitStatus
	err    error
}

type supervisionResult struct {
	count        int
	terminal     bool
	terminalType string
	terminalData json.RawMessage
	err          error
}

func superviseRuntime(ctx context.Context, runID string, reader io.Reader, sink EventSink) supervisionResult {
	if reader == nil {
		return supervisionResult{err: ErrRuntimeStreamUnavailable}
	}
	decoder := runtimeproto.NewDecoder(reader)
	validator := runtimeproto.SequenceValidator{}
	result := supervisionResult{}
	for {
		frame, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			return result
		}
		if err != nil {
			result.err = fmt.Errorf("runtime event stream: %w", err)
			return result
		}
		if frame.RunID != runID {
			result.err = fmt.Errorf("runtime event stream: run_id %q does not match %q", frame.RunID, runID)
			return result
		}
		if err := validator.Accept(frame); err != nil {
			result.err = fmt.Errorf("runtime event stream validation: %w", err)
			return result
		}
		if sink != nil {
			if err := sink(ctx, frame); err != nil {
				result.err = fmt.Errorf("runtime event sink: %w", err)
				return result
			}
		}
		result.count++
		result.terminal = frame.Terminal
		if frame.Terminal {
			result.terminalType = frame.Type
			result.terminalData = append(json.RawMessage(nil), frame.Data...)
		}
	}
}

func validateTerminalOutcome(events supervisionResult, status runner.ExitStatus) error {
	if !events.terminal {
		return ErrRuntimeStreamIncomplete
	}
	switch events.terminalType {
	case proto.EventRunCompleted:
		if _, err := terminalArtifact(events.terminalData); err != nil {
			return err
		}
		if status.Code != 0 || status.Signaled {
			return fmt.Errorf("run.completed requires zero exit status, got code=%d signaled=%t signal=%q", status.Code, status.Signaled, status.Signal)
		}
		return nil
	case proto.EventRunFailed, proto.EventUnknownEffect, proto.EventRunCancelled:
		return &TerminalOutcomeError{Type: events.terminalType}
	default:
		return fmt.Errorf("unsupported terminal event %q", events.terminalType)
	}
}

func sendRunStart(ctx context.Context, request RunRequest, control sandbox.ControlHandle) error {
	data, err := json.Marshal(request.Contract)
	if err != nil {
		return err
	}
	frame := proto.Envelope{
		Protocol: proto.ProtocolVersion,
		Kind:     proto.KindRequest,
		Type:     proto.RequestRunStart,
		RunID:    request.RunID,
		Seq:      1,
		Data:     data,
	}
	payload, err := runtimeproto.EncodeLine(frame)
	if err != nil {
		return err
	}
	return control.Send(ctx, payload)
}

func (d Daemon) validateLease(ctx context.Context, request RunRequest) error {
	current, err := d.Leases.Current(ctx, request.RunID)
	if err != nil {
		return fmt.Errorf("get current lease: %w", err)
	}
	now := d.Now().UTC()
	if err := runner.ValidateLease(current, request.Lease, now); err != nil {
		return fmt.Errorf("lease validation: %w", err)
	}
	return nil
}

func (d Daemon) cleanup(ctx context.Context, request RunRequest, handle runner.SandboxHandle) error {
	validationCtx, cancelValidation := context.WithTimeout(context.WithoutCancel(ctx), d.CleanupWait)
	defer cancelValidation()
	if err := d.validateLease(validationCtx, request); err != nil {
		return fmt.Errorf("refusing cleanup: %w", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.CleanupWait)
	defer cancel()
	return handle.Stop(cleanupCtx)
}

func jsonEventSink(w io.Writer) EventSink {
	return func(ctx context.Context, frame proto.Envelope) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := runtimeproto.EncodeLine(frame)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: agw-runner doctor|serve|run --config PATH")
		return 2
	}
	command := args[0]
	flags := flag.NewFlagSet("agw-runner "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "/etc/agw/runner.yaml", "runner configuration")
	requestPath := flags.String("request", "", "JSON run request; use - for stdin")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch command {
	case "doctor":
		report, readinessErr := cfg.Readiness(ctx)
		payload := map[string]any{"ready": readinessErr == nil && report.Ready, "report": report}
		if readinessErr != nil {
			payload["error"] = readinessErr.Error()
		}
		_ = json.NewEncoder(stdout).Encode(payload)
		if readinessErr != nil || !report.Ready {
			return 1
		}
		return 0
	case "serve":
		backend, report, err := cfg.BuildBackend(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		httpServer, err := NewHTTPServer(cfg, backend, report)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := httpServer.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "run":
		if *requestPath == "" {
			fmt.Fprintln(stderr, "--request is required")
			return 2
		}
		requestReader := io.Reader(os.Stdin)
		var file *os.File
		if *requestPath != "-" {
			file, err = os.Open(filepath.Clean(*requestPath))
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			defer file.Close()
			requestReader = file
		}
		var request RunRequest
		if err := json.NewDecoder(requestReader).Decode(&request); err != nil {
			fmt.Fprintf(stderr, "decode run request: %v\n", err)
			return 1
		}
		backend, _, err := cfg.BuildBackend(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		materializer, err := cfg.BuildMaterializer()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		daemon := Daemon{Backend: backend, Leases: StaticLeaseSource{Lease: request.CurrentLease}, Materializer: materializer, Sink: jsonEventSink(stdout)}
		result, err := daemon.Run(ctx, request)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		_ = json.NewEncoder(stderr).Encode(result)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", command)
		return 2
	}
}
