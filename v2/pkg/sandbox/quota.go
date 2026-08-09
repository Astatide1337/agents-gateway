package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WorkspaceQuotaKind identifies the filesystem mechanism that enforces the
// workspace limit. Providers are deliberately injected behind this small
// boundary so a host with project-quota support can use it without changing
// runner provisioning or Podman's argument construction.
type WorkspaceQuotaKind string

const (
	// WorkspaceQuotaTmpfs is the portable rootless-Podman implementation. The
	// kernel enforces the byte limit on the mounted filesystem; writes return
	// ENOSPC rather than consuming an unbounded host directory.
	WorkspaceQuotaTmpfs WorkspaceQuotaKind = "tmpfs"
	// WorkspaceQuotaProject is reserved for a host-prepared ext4/XFS project
	// quota provider. It must be supplied explicitly by an owner-operated host;
	// the default runner never silently claims this capability.
	WorkspaceQuotaProject WorkspaceQuotaKind = "project"
)

var (
	ErrWorkspaceQuotaExhausted   = errors.New("workspace quota exhausted")
	ErrWorkspaceQuotaUnavailable = errors.New("workspace quota enforcement unavailable")
)

type WorkspaceQuotaRequest struct {
	Root  string
	ID    string
	Bytes int64
	UID   int64
	GID   int64
}

// WorkspaceQuotaMount is the only quota-provider output consumed by the
// Podman backend. Source is required for a project/bind-backed quota and must
// be empty for tmpfs. No manifest or control-plane value can populate this
// structure.
type WorkspaceQuotaMount struct {
	Kind   WorkspaceQuotaKind
	Source string
	Bytes  int64
	UID    int64
	GID    int64
}

type WorkspaceQuotaLease interface {
	Path() string
	Mount() WorkspaceQuotaMount
	Release() error
}

// WorkspaceQuotaProvider provisions one exact, short-lived workspace and
// owns its cleanup. Production providers must implement real kernel/runtime
// enforcement; an implementation that only checks usage in the host process
// is not a valid production provider.
type WorkspaceQuotaProvider interface {
	Name() string
	Validate() error
	Provision(context.Context, WorkspaceQuotaRequest) (WorkspaceQuotaLease, error)
	Reconcile(context.Context, string) error
}

// PodmanTmpfsQuotaProvider is the default owner-operated rootless backend.
// Podman creates a private tmpfs at /workspace with the requested size. The
// host directory is only a lifecycle marker and is never used as the writable
// container mount, so a missing quota cannot degrade into an unbounded bind.
type PodmanTmpfsQuotaProvider struct{}

func NewPodmanTmpfsQuotaProvider() WorkspaceQuotaProvider {
	return PodmanTmpfsQuotaProvider{}
}

func (PodmanTmpfsQuotaProvider) Name() string { return string(WorkspaceQuotaTmpfs) }

func (PodmanTmpfsQuotaProvider) Validate() error {
	// tmpfs is a standard Linux filesystem and Podman exposes its size option
	// directly for rootless containers. The actual mount is still validated by
	// podmanArgs before a process is started.
	return nil
}

func (PodmanTmpfsQuotaProvider) Provision(ctx context.Context, request WorkspaceQuotaRequest) (WorkspaceQuotaLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceQuotaRequest(request); err != nil {
		return nil, err
	}
	path, err := prepareWorkspace(request.Root, request.ID)
	if err != nil {
		return nil, err
	}
	return &filesystemWorkspaceQuotaLease{
		path:  path,
		mount: WorkspaceQuotaMount{Kind: WorkspaceQuotaTmpfs, Bytes: request.Bytes, UID: request.UID, GID: request.GID},
	}, nil
}

func (PodmanTmpfsQuotaProvider) Reconcile(_ context.Context, root string) error {
	return cleanupOrphanWorkspaces(root)
}

type filesystemWorkspaceQuotaLease struct {
	path    string
	mount   WorkspaceQuotaMount
	release sync.Once
	err     error
}

func (l *filesystemWorkspaceQuotaLease) Path() string               { return l.path }
func (l *filesystemWorkspaceQuotaLease) Mount() WorkspaceQuotaMount { return l.mount }
func (l *filesystemWorkspaceQuotaLease) Release() error {
	l.release.Do(func() {
		if err := os.RemoveAll(l.path); err != nil {
			l.err = fmt.Errorf("remove workspace quota allocation: %w", err)
		}
	})
	return l.err
}

func validateWorkspaceQuotaRequest(request WorkspaceQuotaRequest) error {
	root := filepath.Clean(strings.TrimSpace(request.Root))
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return errors.New("workspace quota root must be a non-root absolute path")
	}
	if strings.TrimSpace(request.ID) == "" || safeID(request.ID) != request.ID {
		return errors.New("workspace quota id must contain only lowercase letters, digits, and hyphens")
	}
	if request.Bytes <= 0 {
		return errors.New("workspace quota bytes must be positive")
	}
	if request.UID < 0 || request.GID < 0 {
		return errors.New("workspace quota ownership must be non-negative")
	}
	return nil
}

func validateWorkspaceQuotaMount(mount WorkspaceQuotaMount) error {
	if mount.Bytes <= 0 {
		return errors.New("workspace quota mount size must be positive")
	}
	if mount.UID < 0 || mount.GID < 0 {
		return errors.New("workspace quota mount ownership must be non-negative")
	}
	switch mount.Kind {
	case WorkspaceQuotaTmpfs:
		if mount.Source != "" {
			return errors.New("tmpfs workspace quota must not have a source")
		}
	case WorkspaceQuotaProject:
		if err := validateProjectQuotaSource(mount.Source); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported workspace quota kind %q", mount.Kind)
	}
	return nil
}

func validateProjectQuotaSource(source string) error {
	clean := filepath.Clean(strings.TrimSpace(source))
	if clean == "." || clean == string(filepath.Separator) || !filepath.IsAbs(clean) {
		return errors.New("project quota source must be a non-root absolute path")
	}
	if strings.ContainsAny(clean, ",\x00\n\r") {
		return errors.New("project quota source contains forbidden mount characters")
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect project quota source: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("project quota source must be a real directory")
	}
	return nil
}
