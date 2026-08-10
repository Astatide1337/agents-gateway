package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
	containerd "github.com/containerd/containerd/v2/client"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
)

type ContainerdRunscConfig struct {
	Address        string
	Namespace      string
	Snapshotter    string
	WorkspaceRoot  string
	BrokerRoot     string
	Client         *containerd.Client
	IDFactory      func() string
	AllowLiveStart bool
}

type ContainerdRunscBackend struct {
	client        *containerd.Client
	address       string
	namespace     string
	snapshotter   string
	workspaceRoot string
	brokerRoot    string
	idFactory     func() string
}

func NewContainerdRunscBackend(ctx context.Context, cfg ContainerdRunscConfig) (*ContainerdRunscBackend, error) {
	if strings.TrimSpace(cfg.Address) == "" {
		return nil, errors.New("containerd address is required")
	}
	if cfg.Client == nil {
		client, err := containerd.New(cfg.Address)
		if err != nil {
			return nil, fmt.Errorf("connect to containerd: %w", err)
		}
		cfg.Client = client
	}
	backend := newContainerdRunscBackend(cfg)
	if err := backend.ready(ctx); err != nil {
		_ = cfg.Client.Close()
		return nil, err
	}
	return backend, nil
}

func NewContainerdRunscBackendWithClient(cfg ContainerdRunscConfig) (*ContainerdRunscBackend, error) {
	if cfg.Client == nil {
		return nil, errors.New("containerd client is required")
	}
	return newContainerdRunscBackend(cfg), nil
}

func newContainerdRunscBackend(cfg ContainerdRunscConfig) *ContainerdRunscBackend {
	base := BackendConfig{WorkspaceRoot: cfg.WorkspaceRoot, BrokerRoot: cfg.BrokerRoot, IDFactory: cfg.IDFactory}.defaults()
	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		namespace = "agents-gateway"
	}
	snapshotter := strings.TrimSpace(cfg.Snapshotter)
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	return &ContainerdRunscBackend{
		client: cfg.Client, address: cfg.Address, namespace: namespace,
		snapshotter: snapshotter, workspaceRoot: base.WorkspaceRoot, brokerRoot: base.BrokerRoot, idFactory: base.IDFactory,
	}
}

func (b *ContainerdRunscBackend) Name() runner.BackendKind { return runner.BackendContainerdRunsc }
func (b *ContainerdRunscBackend) Capabilities() runner.BackendCapabilities {
	return runner.ContainerdRunscCapabilities()
}

func (b *ContainerdRunscBackend) Reconcile(ctx context.Context) error {
	namespacesList, err := b.client.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("list containerd namespaces: %w", err)
	}
	for _, namespace := range namespacesList {
		labels, labelErr := b.client.NamespaceService().Labels(ctx, namespace)
		if labelErr != nil {
			return fmt.Errorf("read namespace %q labels: %w", namespace, labelErr)
		}
		if labels["io.agents-gateway.managed"] != "true" {
			continue
		}
		namespaceCtx := namespaces.WithNamespace(ctx, namespace)
		containers, listErr := b.client.Containers(namespaceCtx)
		if listErr != nil {
			return fmt.Errorf("list namespace %q containers: %w", namespace, listErr)
		}
		for _, container := range containers {
			task, taskErr := container.Task(namespaceCtx, nil)
			if taskErr == nil {
				if _, deleteErr := task.Delete(namespaceCtx, containerd.WithProcessKill); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
					return fmt.Errorf("delete orphan task %q: %w", container.ID(), deleteErr)
				}
			} else if !errdefs.IsNotFound(taskErr) {
				return fmt.Errorf("load orphan task %q: %w", container.ID(), taskErr)
			}
			if deleteErr := container.Delete(namespaceCtx, snapshotCleanup); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
				return fmt.Errorf("delete orphan container %q: %w", container.ID(), deleteErr)
			}
		}
		if deleteErr := b.client.NamespaceService().Delete(ctx, namespace); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
			return fmt.Errorf("delete orphan namespace %q: %w", namespace, deleteErr)
		}
	}
	return cleanupOrphanWorkspaces(b.workspaceRoot)
}

func (b *ContainerdRunscBackend) ready(ctx context.Context) error {
	if b.client == nil {
		return errors.New("containerd client is nil")
	}
	serving, err := b.client.IsServing(ctx)
	if err != nil {
		return fmt.Errorf("containerd health check: %w", err)
	}
	if !serving {
		return errors.New("containerd is not serving")
	}
	if _, err := b.client.RuntimeInfo(ctx, ContainerdRunscRuntime, nil); err != nil {
		return fmt.Errorf("required runtime %q is unavailable: %w", ContainerdRunscRuntime, err)
	}
	return nil
}

func (b *ContainerdRunscBackend) Start(ctx context.Context, spec runner.SandboxSpec) (runner.SandboxHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Backend != runner.BackendContainerdRunsc {
		return nil, fmt.Errorf("containerd backend received %q spec", spec.Backend)
	}
	id := b.idFactory()
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("sandbox id is required")
	}
	workspace, err := prepareWorkspace(b.workspaceRoot, id)
	if err != nil {
		return nil, err
	}
	cleanupWorkspace := true
	defer func() {
		if cleanupWorkspace {
			_ = os.RemoveAll(workspace)
		}
	}()
	plan, err := buildHardenedSpec(spec, workspace, ContainerdRunscRuntime, b.brokerRoot)
	if err != nil {
		return nil, err
	}
	if err := createMountDirectories(workspace, plan.RunAsUser, plan.Mounts); err != nil {
		return nil, err
	}

	namespace := namespaceForRun(b.namespace, id)
	baseCtx := namespaces.WithNamespace(ctx, namespace)
	if err := b.client.NamespaceService().Create(ctx, namespace, map[string]string{
		"io.agents-gateway.run":     id,
		"io.agents-gateway.managed": "true",
	}); err != nil {
		return nil, fmt.Errorf("create containerd namespace: %w", err)
	}
	namespaceCreated := true
	defer func() {
		if namespaceCreated {
			_ = b.client.NamespaceService().Delete(context.WithoutCancel(ctx), namespace)
		}
	}()

	image, err := b.client.GetImage(baseCtx, plan.Image)
	if err != nil {
		return nil, fmt.Errorf("load pinned image %q: %w", plan.Image, err)
	}
	if got := image.Target().Digest.String(); !strings.HasSuffix(plan.Image, "@"+got) {
		return nil, fmt.Errorf("containerd resolved image digest %q, requested %q", got, plan.Image)
	}
	specOpts, err := containerdSpecOpts(plan, image)
	if err != nil {
		return nil, err
	}
	container, err := b.client.NewContainer(baseCtx, id,
		containerd.WithImage(image),
		containerd.WithImageName(plan.Image),
		containerd.WithSnapshotter(b.snapshotter),
		containerd.WithNewSnapshot(id, image),
		containerd.WithRuntime(ContainerdRunscRuntime, nil),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		return nil, fmt.Errorf("create containerd container: %w", err)
	}
	stdoutReader, stdoutWriter := io.Pipe()
	stdinReader, stdinWriter := io.Pipe()
	task, err := container.NewTask(baseCtx, cio.NewCreator(cio.WithStreams(stdinReader, stdoutWriter, io.Discard)))
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = container.Delete(context.WithoutCancel(ctx), snapshotCleanup)
		_ = b.client.NamespaceService().Delete(context.WithoutCancel(ctx), namespace)
		return nil, fmt.Errorf("create containerd task: %w", err)
	}
	if err := task.Start(baseCtx); err != nil {
		_, _ = task.Delete(context.WithoutCancel(ctx))
		_ = container.Delete(context.WithoutCancel(ctx), snapshotCleanup)
		_ = b.client.NamespaceService().Delete(context.WithoutCancel(ctx), namespace)
		return nil, fmt.Errorf("start containerd task: %w", err)
	}
	cleanupWorkspace = false
	namespaceCreated = false
	return &containerdHandle{
		id: id, task: task, container: container, client: b.client,
		namespace: namespace, workspace: workspace, stdout: stdoutReader, stdoutWriter: stdoutWriter, stdinWriter: stdinWriter,
		startedAt: time.Now().UTC(),
	}, nil
}

func containerdSpecOpts(plan HardenedSpec, image containerd.Image) ([]oci.SpecOpts, error) {
	mounts := make([]runtimespec.Mount, 0, len(plan.Mounts))
	for _, mount := range plan.Mounts {
		if err := validateHardenedMount(mount); err != nil {
			return nil, err
		}
		options := []string{"rbind"}
		if mount.ReadOnly {
			options = append(options, "ro")
		}
		mounts = append(mounts, runtimespec.Mount{
			Destination: mount.Destination, Type: "bind", Source: mount.Source, Options: options,
		})
	}
	opts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithRootFSReadonly(),
		oci.WithUser(plan.RunAsUser),
		oci.WithUIDGID(parseUIDGID(plan.RunAsUser)),
		oci.WithNoNewPrivileges,
		oci.WithCapabilities(nil),
		oci.WithPidsLimit(plan.Resources.PIDs),
		oci.WithMemoryLimit(uint64(plan.Resources.MemoryBytes)),
		oci.WithCPUs(fmt.Sprintf("%.3f", plan.Resources.CPUs)),
		oci.WithMounts(mounts),
		oci.WithProcessCwd("/workspace"),
		oci.WithLinuxNamespace(runtimespec.LinuxNamespace{Type: runtimespec.NetworkNamespace}),
		oci.WithAnnotations(map[string]string{
			"io.agents-gateway.runtime":         ContainerdRunscRuntime,
			"io.agents-gateway.network":         string(plan.Network),
			"io.agents-gateway.direct-internet": "false",
		}),
	}
	if len(plan.Environment) > 0 {
		opts = append(opts, oci.WithEnv(plan.Environment))
	}
	return opts, nil
}

// WithUIDGID is applied after WithUser so numeric credentials remain explicit
// in the OCI spec. User strings are validated by runner.ValidateSandboxSpec;
// this helper intentionally fails closed for named users.
func parseUIDGID(value string) (uint32, uint32) {
	uid, gid, err := numericUser(value)
	if err != nil {
		return 0, 0
	}
	return uid, gid
}

func namespaceForRun(base, id string) string {
	value := safeID(base) + "-" + safeID(id)
	if len(value) > 60 {
		value = value[:60]
	}
	return value
}

type containerdHandle struct {
	id           string
	task         containerd.Task
	container    containerd.Container
	client       *containerd.Client
	namespace    string
	workspace    string
	stdout       *io.PipeReader
	stdoutWriter *io.PipeWriter
	stdinWriter  *io.PipeWriter
	startedAt    time.Time
	cleanupOnce  sync.Once
	cleanupErr   error
}

func (h *containerdHandle) ID() string        { return h.id }
func (h *containerdHandle) Stdout() io.Reader { return h.stdout }
func (h *containerdHandle) Send(ctx context.Context, payload []byte) error {
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
func (h *containerdHandle) Wait(ctx context.Context) (runner.ExitStatus, error) {
	ch, err := h.task.Wait(ctx)
	if err != nil {
		return runner.ExitStatus{}, err
	}
	select {
	case status := <-ch:
		_ = h.stdoutWriter.Close()
		code, finished, statusErr := status.Result()
		if statusErr != nil {
			return runner.ExitStatus{Code: int(code), StartedAt: h.startedAt, FinishedAt: finished}, statusErr
		}
		return runner.ExitStatus{Code: int(code), StartedAt: h.startedAt, FinishedAt: finished}, nil
	case <-ctx.Done():
		return runner.ExitStatus{}, ctx.Err()
	}
}

func (h *containerdHandle) Stop(ctx context.Context) error {
	if status, err := h.task.Status(ctx); err == nil && status.Status == containerd.Running {
		if err := h.task.Kill(ctx, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill sandbox %s: %w", h.id, err)
		}
	}
	return h.cleanup(context.WithoutCancel(ctx))
}

func (h *containerdHandle) cleanup(ctx context.Context) error {
	h.cleanupOnce.Do(func() {
		defer func() {
			_ = h.stdinWriter.Close()
			_ = h.stdout.Close()
			_ = h.stdoutWriter.Close()
			_ = os.RemoveAll(h.workspace)
		}()
		if _, err := h.task.Delete(ctx); err != nil && h.cleanupErr == nil {
			h.cleanupErr = fmt.Errorf("delete sandbox task: %w", err)
		}
		if err := h.container.Delete(ctx, snapshotCleanup); err != nil && h.cleanupErr == nil {
			h.cleanupErr = fmt.Errorf("delete sandbox container: %w", err)
		}
		if err := h.client.NamespaceService().Delete(ctx, h.namespace); err != nil && h.cleanupErr == nil {
			h.cleanupErr = fmt.Errorf("delete containerd namespace: %w", err)
		}
	})
	return h.cleanupErr
}

func snapshotCleanup(ctx context.Context, client *containerd.Client, container corecontainers.Container) error {
	return containerd.WithSnapshotCleanup(ctx, client, container)
}
