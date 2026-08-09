// Package runner contains the host-side runner boundary. It defines policy
// and capability models without opening container runtime sockets or creating
// containers.
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type BackendKind string

const (
	BackendContainerdRunsc BackendKind = "containerd-runsc"
	BackendPodman          BackendKind = "podman"
)

type IsolationGrade string

const (
	IsolationStandard IsolationGrade = "standard/shared-kernel"
	IsolationEnhanced IsolationGrade = "enhanced/gvisor"
	IsolationStrong   IsolationGrade = "strong/microvm"
)

type NetworkMode string

const (
	NetworkNone     NetworkMode = "none"
	NetworkBrokered NetworkMode = "brokered"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Needed bool   `json:"needed"`
	Detail string `json:"detail,omitempty"`
}

type ReadinessReport struct {
	Backend        BackendKind    `json:"backend"`
	Ready          bool           `json:"ready"`
	IsolationGrade IsolationGrade `json:"isolation_grade"`
	CheckedAt      time.Time      `json:"checked_at"`
	Checks         []Check        `json:"checks"`
}

type BackendCapabilities struct {
	Backend               BackendKind    `json:"backend"`
	IsolationGrade        IsolationGrade `json:"isolation_grade"`
	ProductionSuitable    bool           `json:"production_suitable"`
	SupportsReadOnlyRoot  bool           `json:"supports_read_only_root"`
	SupportsCgroups       bool           `json:"supports_cgroups"`
	SupportsNetworkNS     bool           `json:"supports_network_namespaces"`
	SupportsNoNewPrivs    bool           `json:"supports_no_new_privileges"`
	RequiresDedicatedHost bool           `json:"requires_dedicated_host"`
	DirectInternet        bool           `json:"direct_internet"`
}

func ContainerdRunscCapabilities() BackendCapabilities {
	return BackendCapabilities{
		Backend: BackendContainerdRunsc, IsolationGrade: IsolationEnhanced,
		ProductionSuitable: true, SupportsReadOnlyRoot: true, SupportsCgroups: true,
		SupportsNetworkNS: true, SupportsNoNewPrivs: true, RequiresDedicatedHost: true,
		DirectInternet: false,
	}
}

// RootlessPodmanCapabilities describes the supported standalone runner. It is
// production-suitable for an owner-operated installation, but deliberately
// reports shared-kernel isolation and is not a hostile multi-tenant boundary.
func RootlessPodmanCapabilities() BackendCapabilities {
	return BackendCapabilities{
		Backend: BackendPodman, IsolationGrade: IsolationStandard,
		ProductionSuitable: true, SupportsReadOnlyRoot: true, SupportsCgroups: true,
		SupportsNetworkNS: true, SupportsNoNewPrivs: true, RequiresDedicatedHost: false,
		DirectInternet: false,
	}
}

type ResourceLimits struct {
	CPUs        float64       `json:"cpus"`
	MemoryBytes int64         `json:"memory_bytes"`
	DiskBytes   int64         `json:"disk_bytes"`
	PIDs        int64         `json:"pids"`
	Timeout     time.Duration `json:"timeout"`
}

type Mount struct {
	Kind        string `json:"kind"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

type SandboxSpec struct {
	Backend             BackendKind    `json:"backend"`
	IsolationGrade      IsolationGrade `json:"isolation_grade"`
	Image               string         `json:"image"`
	ImageDigest         string         `json:"image_digest"`
	RunAsUser           string         `json:"run_as_user"`
	ReadOnlyRootFS      bool           `json:"read_only_root_fs"`
	NoNewPrivileges     bool           `json:"no_new_privileges"`
	DropAllCapabilities bool           `json:"drop_all_capabilities"`
	Network             NetworkMode    `json:"network"`
	DirectInternet      bool           `json:"direct_internet"`
	Privileged          bool           `json:"privileged"`
	RuntimeSocket       string         `json:"runtime_socket,omitempty"`
	// BrokerSessionID is an opaque reference to a host-created, short-lived
	// broker session. It is intentionally not a path, socket name, or token.
	// The selected runtime resolves it under its configured BrokerRoot.
	BrokerSessionID string `json:"broker_session_id,omitempty"`
	// EnvironmentFile is populated only inside the trusted runner process after
	// host-side secret materialization. It is never accepted from or serialized
	// onto the control-plane wire contract.
	EnvironmentFile string         `json:"-"`
	Mounts          []Mount        `json:"mounts,omitempty"`
	Resources       ResourceLimits `json:"resources"`
}

func ValidateSandboxSpec(spec SandboxSpec) error {
	if spec.Backend != BackendContainerdRunsc && spec.Backend != BackendPodman {
		return fmt.Errorf("unsupported backend %q", spec.Backend)
	}
	if spec.IsolationGrade == "" {
		return errors.New("isolation grade is required")
	}
	if spec.Backend == BackendContainerdRunsc && spec.IsolationGrade != IsolationEnhanced {
		return errors.New("containerd-runsc requires enhanced/gvisor isolation")
	}
	if spec.Backend == BackendPodman && spec.IsolationGrade != IsolationStandard {
		return errors.New("rootless podman requires standard/shared-kernel isolation")
	}
	if strings.TrimSpace(spec.Image) == "" {
		return errors.New("image is required")
	}
	if !strings.HasPrefix(spec.ImageDigest, "sha256:") || len(spec.ImageDigest) != len("sha256:")+64 {
		return errors.New("image_digest must be a sha256 digest")
	}
	if strings.TrimSpace(spec.RunAsUser) == "" || spec.RunAsUser == "0" || spec.RunAsUser == "root" {
		return errors.New("run_as_user must identify a non-root user")
	}
	if !spec.ReadOnlyRootFS || !spec.NoNewPrivileges || !spec.DropAllCapabilities {
		return errors.New("sandbox must use read-only root, no-new-privileges, and drop all capabilities")
	}
	if spec.Privileged || spec.DirectInternet || spec.RuntimeSocket != "" {
		return errors.New("privileged mode, direct internet, and runtime sockets are forbidden")
	}
	if spec.Network != NetworkNone && spec.Network != NetworkBrokered {
		return fmt.Errorf("unsupported network mode %q", spec.Network)
	}
	if err := ValidateBrokerSessionID(spec.BrokerSessionID); err != nil {
		return err
	}
	if spec.Resources.CPUs <= 0 || spec.Resources.MemoryBytes <= 0 || spec.Resources.DiskBytes <= 0 || spec.Resources.PIDs <= 0 || spec.Resources.Timeout <= 0 {
		return errors.New("all resource limits must be positive")
	}
	if spec.Resources.Timeout > 24*time.Hour {
		return errors.New("sandbox timeout must not exceed 24 hours")
	}
	if spec.Resources.PIDs > 65536 {
		return errors.New("pid limit is unreasonably large")
	}
	allowedMounts := map[string]struct {
		destination string
		readOnly    bool
	}{
		"workspace": {destination: "/workspace", readOnly: false},
		"artifact":  {destination: "/artifacts", readOnly: true},
		"skills":    {destination: "/skills", readOnly: true},
	}
	seenKinds := make(map[string]struct{}, len(spec.Mounts))
	seenDestinations := make(map[string]struct{}, len(spec.Mounts))
	for i, mount := range spec.Mounts {
		policy, allowed := allowedMounts[mount.Kind]
		if !allowed {
			return fmt.Errorf("mount %d has forbidden kind %q", i, mount.Kind)
		}
		if _, exists := seenKinds[mount.Kind]; exists {
			return fmt.Errorf("mount %d duplicates kind %q", i, mount.Kind)
		}
		seenKinds[mount.Kind] = struct{}{}
		if _, exists := seenDestinations[mount.Destination]; exists {
			return fmt.Errorf("mount %d duplicates destination %q", i, mount.Destination)
		}
		seenDestinations[mount.Destination] = struct{}{}
		if mount.Destination != policy.destination || !filepath.IsAbs(mount.Destination) || filepath.Clean(mount.Destination) != mount.Destination {
			return fmt.Errorf("mount %d kind %q must use destination %q", i, mount.Kind, policy.destination)
		}
		if mount.ReadOnly != policy.readOnly {
			return fmt.Errorf("mount %d kind %q has invalid read-only policy", i, mount.Kind)
		}
	}
	if _, exists := seenKinds["workspace"]; !exists {
		return errors.New("a writable /workspace mount is required")
	}
	return nil
}

// ValidateBrokerSessionID validates the opaque broker reference without
// interpreting it as a filesystem path. Empty is valid for network=none.
func ValidateBrokerSessionID(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return errors.New("broker session id is too long")
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' {
			continue
		}
		return errors.New("broker session id must be opaque and path-free")
	}
	return nil
}

// ValidateExecutableSandboxSpec applies the runtime feature gate in addition
// to declarative schema validation. The sandbox backends resolve the broker
// session under their private BrokerRoot before calling the runtime-specific
// builder; this public validator intentionally remains conservative for
// callers that do not have a configured host broker root.
func ValidateExecutableSandboxSpec(spec SandboxSpec) error {
	if err := ValidateSandboxSpec(spec); err != nil {
		return err
	}
	if spec.Network == NetworkBrokered {
		return errors.New("brokered sandbox networking is not operational")
	}
	return nil
}

type ExitStatus struct {
	Code       int
	Signaled   bool
	Signal     string
	StartedAt  time.Time
	FinishedAt time.Time
}

type SandboxHandle interface {
	ID() string
	Wait(context.Context) (ExitStatus, error)
	Stop(context.Context) error
}

// RuntimeBackend is deliberately narrow. Implementations own container
// lifecycle; the control plane never receives a runtime socket.
type RuntimeBackend interface {
	Name() BackendKind
	Capabilities() BackendCapabilities
	Start(context.Context, SandboxSpec) (SandboxHandle, error)
}
