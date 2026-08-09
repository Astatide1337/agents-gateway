package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

const (
	podmanOwnershipLabelKey   = "io.agents-gateway.owner"
	podmanOwnershipLabelValue = "agw-runner"
	podmanCommandOutputLimit  = 16 << 10
)

var (
	ErrPodmanWorkspace  = errors.New("podman sandbox workspace preparation failed")
	ErrPodmanPlan       = errors.New("podman hardened plan failed")
	ErrPodmanMounts     = errors.New("podman mount preparation failed")
	ErrPodmanArguments  = errors.New("podman argument construction failed")
	ErrPodmanStart      = errors.New("podman sandbox start failed")
	ErrRuntimeBroker    = errors.New("runtime broker bridge startup failed")
	ErrRuntimeConfig    = errors.New("runtime adapter configuration failed")
	ErrRuntimeContract  = errors.New("runtime rejected the start contract")
	ErrPodmanRuntime    = errors.New("podman runtime setup failed")
	ErrPodmanMount      = errors.New("podman runtime mount setup failed")
	ErrPodmanPermission = errors.New("podman runtime permission check failed")
)

type PodmanConfig struct {
	Binary         string
	WorkspaceRoot  string
	BrokerRoot     string
	IDFactory      func() string
	Factory        PodmanCommandFactory
	WorkspaceQuota WorkspaceQuotaProvider
}

type PodmanCommandFactory interface {
	Start(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (PodmanProcess, error)
}

// PodmanCommandRunner is the narrow direct-argv capability used for lifecycle
// reconciliation and removal. It deliberately has no shell semantics.
type PodmanCommandRunner interface {
	Run(context.Context, string, []string, io.Writer, io.Writer) error
}

type PodmanProcess interface {
	Wait() (int, error)
	Signal(os.Signal) error
}

type RootlessPodmanBackend struct {
	binary         string
	workspaceRoot  string
	brokerRoot     string
	idFactory      func() string
	factory        PodmanCommandFactory
	workspaceQuota WorkspaceQuotaProvider
}

func NewRootlessPodmanBackend(cfg PodmanConfig) (*RootlessPodmanBackend, error) {
	base := BackendConfig{WorkspaceRoot: cfg.WorkspaceRoot, BrokerRoot: cfg.BrokerRoot, IDFactory: cfg.IDFactory}.defaults()
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = "podman"
	}
	factory := cfg.Factory
	if factory == nil {
		factory = execPodmanFactory{}
	}
	workspaceQuota := cfg.WorkspaceQuota
	if workspaceQuota == nil {
		workspaceQuota = NewPodmanTmpfsQuotaProvider()
	}
	if err := workspaceQuota.Validate(); err != nil {
		return nil, fmt.Errorf("workspace quota provider %q: %w", workspaceQuota.Name(), err)
	}
	if err := validateBrokerRoot(base.BrokerRoot); err != nil {
		return nil, err
	}
	return &RootlessPodmanBackend{
		binary: binary, workspaceRoot: base.WorkspaceRoot, brokerRoot: base.BrokerRoot,
		idFactory: base.IDFactory, factory: factory, workspaceQuota: workspaceQuota,
	}, nil
}

func (b *RootlessPodmanBackend) Name() runner.BackendKind { return runner.BackendPodman }
func (b *RootlessPodmanBackend) Capabilities() runner.BackendCapabilities {
	return runner.RootlessPodmanCapabilities()
}

func (b *RootlessPodmanBackend) Reconcile(ctx context.Context) error {
	runner, ok := b.factory.(PodmanCommandRunner)
	if !ok {
		return errors.New("podman command factory does not support lifecycle commands")
	}
	var stdout, stderr boundedBuffer
	if err := runner.Run(ctx, b.binary, []string{
		"ps", "-a", "--filter", podmanOwnershipFilter(), "--format", "{{.ID}}",
	}, &stdout, &stderr); err != nil {
		return commandError("list Agents Gateway containers", err, stderr.String())
	}
	for _, id := range parseContainerIDs(stdout.String()) {
		if err := b.forceRemoveContainer(ctx, id); err != nil {
			return err
		}
	}
	return b.workspaceQuota.Reconcile(ctx, b.workspaceRoot)
}

func (b *RootlessPodmanBackend) Start(ctx context.Context, spec runner.SandboxSpec) (runner.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Backend != runner.BackendPodman {
		return nil, fmt.Errorf("podman backend received %q spec", spec.Backend)
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("rootless Podman backend requires a non-root process")
	}
	id := b.idFactory()
	uid, gid, err := numericUser(spec.RunAsUser)
	if err != nil {
		return nil, err
	}
	allocation, err := b.workspaceQuota.Provision(ctx, WorkspaceQuotaRequest{
		Root: b.workspaceRoot, ID: id, Bytes: spec.Resources.DiskBytes,
		UID: int64(uid), GID: int64(gid),
	})
	if err != nil {
		return nil, errors.Join(ErrPodmanWorkspace, fmt.Errorf("provision workspace quota: %w", err))
	}
	workspace := allocation.Path()
	cleanupWorkspace := true
	defer func() {
		if cleanupWorkspace {
			_ = allocation.Release()
		}
	}()
	plan, err := buildHardenedSpec(spec, workspace, "podman", b.brokerRoot)
	if err != nil {
		return nil, errors.Join(ErrPodmanPlan, err)
	}
	if err := createMountDirectories(workspace, plan.RunAsUser, plan.Mounts); err != nil {
		return nil, errors.Join(ErrPodmanMounts, err)
	}
	args, err := podmanArgsWithQuota(id, plan, allocation.Mount())
	if err != nil {
		return nil, errors.Join(ErrPodmanArguments, err)
	}
	stdoutReader, stdoutWriter := io.Pipe()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, errors.New("create podman stdin pipe")
	}
	stderr := &boundedBuffer{}
	process, err := b.factory.Start(ctx, b.binary, args, stdinReader, stdoutWriter, stderr)
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, errors.Join(ErrPodmanStart, commandError("start podman sandbox", err, stderr.String()))
	}
	cleanupWorkspace = false
	return &podmanHandle{
		id: id, process: process, workspace: workspace, stdout: stdoutReader,
		stdoutWriter: stdoutWriter, stdinWriter: stdinWriter, startedAt: time.Now().UTC(),
		binary: b.binary, factory: b.factory, stderr: stderr, quotaLease: allocation,
	}, nil
}

func podmanArgs(id string, plan HardenedSpec) ([]string, error) {
	uid, gid, err := numericUser(plan.RunAsUser)
	if err != nil {
		return nil, err
	}
	return podmanArgsWithQuota(id, plan, WorkspaceQuotaMount{
		Kind: WorkspaceQuotaTmpfs, Bytes: plan.Resources.DiskBytes,
		UID: int64(uid), GID: int64(gid),
	})
}

func podmanArgsWithQuota(id string, plan HardenedSpec, quotaMount WorkspaceQuotaMount) ([]string, error) {
	if plan.Runtime != "podman" {
		return nil, errors.New("podman plan has an invalid runtime")
	}
	uid, gid, err := numericUser(plan.RunAsUser)
	if err != nil {
		return nil, err
	}
	if quotaMount.UID != int64(uid) || quotaMount.GID != int64(gid) {
		return nil, errors.New("workspace quota ownership does not match sandbox user")
	}
	if quotaMount.Bytes != plan.Resources.DiskBytes {
		return nil, errors.New("workspace quota size does not match sandbox disk limit")
	}
	if err := validateWorkspaceQuotaMount(quotaMount); err != nil {
		return nil, err
	}
	args := []string{
		"run", "--rm", "--interactive", "--name", id,
		"--label", podmanOwnershipLabelKey + "=" + podmanOwnershipLabelValue,
		"--user", plan.RunAsUser,
		"--userns=keep-id",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--network=none",
		"--pids-limit", strconv.FormatInt(plan.Resources.PIDs, 10),
		"--memory", strconv.FormatInt(plan.Resources.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(plan.Resources.CPUs, 'f', 3, 64),
		"--workdir", "/workspace",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m",
	}
	switch quotaMount.Kind {
	case WorkspaceQuotaTmpfs:
		// The mount is private to this container. mode=01777 lets the configured
		// non-root --user write to it; sticky-bit semantics prevent one user from
		// deleting another user's files, while nosuid/nodev remove escalation and
		// device-node paths. Podman 4.9 rejects uid=/gid= tmpfs options, so the
		// container user is enforced by --user and --userns=keep-id instead.
		workspaceTmpfs := fmt.Sprintf("/workspace:rw,nosuid,nodev,size=%d,mode=01777", quotaMount.Bytes)
		args = append(args, "--tmpfs", workspaceTmpfs)
	case WorkspaceQuotaProject:
		args = append(args, "--mount", "type=bind,src="+quotaMount.Source+",dst=/workspace,rw")
	default:
		return nil, fmt.Errorf("unsupported workspace quota kind %q", quotaMount.Kind)
	}
	for _, mount := range plan.Mounts {
		if err := validateHardenedMount(mount); err != nil {
			return nil, err
		}
		if mount.Kind == "workspace" {
			continue
		}
		if strings.ContainsAny(mount.Source+mount.Destination, ",\x00") {
			return nil, errors.New("mount paths contain forbidden podman option characters")
		}
		mode := "rw"
		if mount.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--mount", "type=bind,src="+mount.Source+",dst="+mount.Destination+","+mode)
	}
	if plan.EnvironmentFile != "" {
		if err := validateEnvironmentFile(plan.EnvironmentFile); err != nil {
			return nil, err
		}
		args = append(args, "--env-file", plan.EnvironmentFile)
	}
	args = append(args, plan.Image)
	return args, nil
}

type podmanHandle struct {
	id           string
	process      PodmanProcess
	workspace    string
	stdout       *io.PipeReader
	stdoutWriter *io.PipeWriter
	stdinWriter  io.WriteCloser
	startedAt    time.Time
	binary       string
	factory      PodmanCommandFactory
	stderr       *boundedBuffer
	quotaLease   WorkspaceQuotaLease
	cleanupOnce  sync.Once
	cleanupErr   error
}

func (h *podmanHandle) ID() string        { return h.id }
func (h *podmanHandle) Stdout() io.Reader { return h.stdout }
func (h *podmanHandle) Send(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { _, err := h.stdinWriter.Write(payload); done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (h *podmanHandle) Wait(ctx context.Context) (runner.ExitStatus, error) {
	type result struct {
		code int
		err  error
	}
	results := make(chan result, 1)
	go func() {
		code, err := h.process.Wait()
		results <- result{code: code, err: err}
	}()
	select {
	case result := <-results:
		_ = h.stdoutWriter.Close()
		finished := time.Now().UTC()
		if result.err != nil {
			stderr := h.stderr.String()
			return runner.ExitStatus{Code: result.code, StartedAt: h.startedAt, FinishedAt: finished}, errors.Join(runtimeProcessError(result.code, stderr), commandError("wait for podman sandbox", result.err, stderr))
		}
		if result.code != 0 && h.stderr.String() != "" {
			return runner.ExitStatus{Code: result.code, StartedAt: h.startedAt, FinishedAt: finished}, commandError(
				fmt.Sprintf("podman sandbox %s exited with code %d", h.id, result.code),
				errors.New("sandbox process failed"), h.stderr.String(),
			)
		}
		return runner.ExitStatus{Code: result.code, StartedAt: h.startedAt, FinishedAt: finished}, nil
	case <-ctx.Done():
		return runner.ExitStatus{}, ctx.Err()
	}
}

func runtimeStartupError(stderr string) error {
	// The adapter owns these fixed prefixes. Map them to low-cardinality
	// sentinels while keeping the untrusted stderr bytes out of durable state.
	switch {
	case strings.Contains(stderr, "agw-codex-adapter: start run broker bridge:"):
		return ErrRuntimeBroker
	case strings.Contains(stderr, "agw-codex-adapter: invalid adapter configuration:"):
		return ErrRuntimeConfig
	case strings.Contains(stderr, "agw-codex-adapter: read run.start:"),
		strings.Contains(stderr, "agw-codex-adapter: first frame must be request run.start"),
		strings.Contains(stderr, "agw-codex-adapter: run.start must have sequence 1"),
		strings.Contains(stderr, "agw-codex-adapter: decode run.start:"),
		strings.Contains(stderr, "agw-codex-adapter: run.start run_id does not match"),
		strings.Contains(stderr, "agw-codex-adapter: agent id:"):
		return ErrRuntimeContract
	default:
		return nil
	}
}

func runtimeProcessError(exitCode int, stderr string) error {
	if startup := runtimeStartupError(stderr); startup != nil {
		return startup
	}
	if exitCode != 125 {
		return nil
	}
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "permission denied") || strings.Contains(lower, "operation not permitted") {
		return ErrPodmanPermission
	}
	if strings.Contains(lower, "mount") || strings.Contains(lower, "statfs") {
		return ErrPodmanMount
	}
	return ErrPodmanRuntime
}

func (h *podmanHandle) Stop(ctx context.Context) error {
	h.cleanupOnce.Do(func() {
		var stopErr error
		if err := h.process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			stopErr = fmt.Errorf("kill podman sandbox %s: %w", h.id, err)
		}
		cleanupCtx, cancel := boundedCleanupContext(ctx)
		defer cancel()
		if err := h.forceRemove(cleanupCtx); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
		if err := h.cleanupFiles(); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
		h.cleanupErr = stopErr
	})
	return h.cleanupErr
}

func (h *podmanHandle) forceRemove(ctx context.Context) error {
	runner, ok := h.factory.(PodmanCommandRunner)
	if !ok {
		return errors.New("podman command factory does not support container removal")
	}
	var stderr boundedBuffer
	if err := runner.Run(ctx, h.binary, []string{
		"rm", "--force", "--time", "0", "--ignore", h.id,
	}, io.Discard, &stderr); err != nil {
		return commandError("force-remove Agents Gateway container "+h.id, err, stderr.String())
	}
	return nil
}

func (h *podmanHandle) cleanupFiles() error {
	_ = h.stdinWriter.Close()
	_ = h.stdout.Close()
	_ = h.stdoutWriter.Close()
	if h.quotaLease != nil {
		if err := h.quotaLease.Release(); err != nil {
			return fmt.Errorf("release podman workspace quota: %w", err)
		}
	} else if err := os.RemoveAll(h.workspace); err != nil {
		return fmt.Errorf("remove podman workspace: %w", err)
	}
	return nil
}

type execPodmanFactory struct{}

func (execPodmanFactory) Start(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (PodmanProcess, error) {
	if strings.TrimSpace(name) == "" || len(args) == 0 {
		return nil, errors.New("podman command and arguments are required")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return &execPodmanProcess{cmd: cmd}, cmd.Start()
}

func (execPodmanFactory) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	if strings.TrimSpace(name) == "" || len(args) == 0 {
		return errors.New("podman command and arguments are required")
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type execPodmanProcess struct{ cmd *exec.Cmd }

func (p *execPodmanProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState == nil {
		return -1, err
	}
	return p.cmd.ProcessState.ExitCode(), err
}
func (p *execPodmanProcess) Signal(signal os.Signal) error { return p.cmd.Process.Signal(signal) }

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := podmanCommandOutputLimit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() == 0 {
		return ""
	}
	value := strings.TrimSpace(b.buf.String())
	if b.truncated {
		value += " [truncated]"
	}
	return value
}

func podmanOwnershipFilter() string {
	return "label=" + podmanOwnershipLabelKey + "=" + podmanOwnershipLabelValue
}

func (b *RootlessPodmanBackend) forceRemoveContainer(ctx context.Context, id string) error {
	if !safeContainerID(id) {
		return fmt.Errorf("refusing unsafe Podman container ID %q", id)
	}
	runner, ok := b.factory.(PodmanCommandRunner)
	if !ok {
		return errors.New("podman command factory does not support container removal")
	}
	var stderr boundedBuffer
	if err := runner.Run(ctx, b.binary, []string{
		"rm", "--force", "--time", "0", "--ignore", id,
	}, io.Discard, &stderr); err != nil {
		return commandError("force-remove Agents Gateway container "+id, err, stderr.String())
	}
	return nil
}

func parseContainerIDs(output string) []string {
	var ids []string
	for _, line := range strings.Split(output, "\n") {
		id := strings.TrimSpace(line)
		if id == "" || !safeContainerID(id) {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func safeContainerID(id string) bool {
	if id == "" || strings.HasPrefix(id, "-") {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func commandError(action string, err error, stderr string) error {
	if stderr == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	// Container stderr is untrusted and may contain prompts, credentials, or
	// model output. Preserve enough evidence to correlate repeated failures
	// without copying those bytes into control-plane errors or logs.
	digest := sha256.Sum256([]byte(stderr))
	return fmt.Errorf("%s: %w (stderr captured: %d bytes, sha256:%x)", action, err, len(stderr), digest)
}

func boundedCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil || ctx.Err() != nil {
		return context.WithTimeout(context.Background(), 10*time.Second)
	}
	return context.WithTimeout(ctx, 10*time.Second)
}
