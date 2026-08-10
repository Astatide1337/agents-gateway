// Package sandbox contains the host-side execution backends. It is the only
// v2 package allowed to talk to a local container runtime.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

const (
	ContainerdRunscRuntime = "io.containerd.runsc.v1"
	DefaultWorkspaceRoot   = "/var/lib/agw-runner/workspaces"
	DefaultBrokerRoot      = "/run/agw-broker"
	BrokerMountDestination = "/run/agw"
	BrokerClientConfigName = "client.json"
	BrokerSocketName       = "broker.sock"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// OutputHandle is implemented by backends that expose the agent's stdout for
// the runtime JSONL supervisor. The runner package intentionally keeps its
// lifecycle interface small, so this is an optional capability.
type OutputHandle interface {
	runner.SandboxHandle
	Stdout() io.Reader
}

type ControlHandle interface {
	Send(context.Context, []byte) error
}

// Reconciler removes resources left by a previous runner process before the
// daemon accepts new tasks.
type Reconciler interface {
	Reconcile(context.Context) error
}

type BackendConfig struct {
	WorkspaceRoot string
	BrokerRoot    string
	IDFactory     func() string
}

func (c BackendConfig) defaults() BackendConfig {
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = DefaultWorkspaceRoot
	}
	if c.BrokerRoot == "" {
		c.BrokerRoot = DefaultBrokerRoot
	}
	if c.IDFactory == nil {
		c.IDFactory = newID
	}
	return c
}

type HardenedSpec struct {
	Runtime             string
	Image               string
	RunAsUser           string
	ReadOnlyRootFS      bool
	NoNewPrivileges     bool
	DropAllCapabilities bool
	Network             runner.NetworkMode
	DirectInternet      bool
	Privileged          bool
	Mounts              []HardenedMount
	BrokerSessionID     string
	EnvironmentFile     string
	Environment         []string
	Resources           runner.ResourceLimits
}

type HardenedMount struct {
	Kind        string
	Source      string
	Destination string
	ReadOnly    bool
}

func BuildHardenedSpec(spec runner.SandboxSpec, workspace string, runtimeName string) (HardenedSpec, error) {
	return buildHardenedSpec(spec, workspace, runtimeName, "")
}

// buildHardenedSpec is the runtime-only builder. A non-empty brokerRoot is
// supplied by the host backend, never by the manifest or control plane.
func buildHardenedSpec(spec runner.SandboxSpec, workspace string, runtimeName, brokerRoot string) (HardenedSpec, error) {
	if err := runner.ValidateSandboxSpec(spec); err != nil {
		return HardenedSpec{}, err
	}
	if spec.Network == runner.NetworkBrokered && strings.TrimSpace(brokerRoot) == "" {
		return HardenedSpec{}, errors.New("brokered sandbox requires a configured broker root")
	}
	if spec.Network == runner.NetworkBrokered && spec.BrokerSessionID == "" {
		return HardenedSpec{}, errors.New("brokered sandbox requires a broker session id")
	}
	if spec.Network == runner.NetworkNone && spec.BrokerSessionID != "" {
		return HardenedSpec{}, errors.New("network-none sandbox must not carry a broker session id")
	}
	if strings.TrimSpace(workspace) == "" || !filepath.IsAbs(workspace) {
		return HardenedSpec{}, errors.New("workspace must be an absolute path")
	}
	if runtimeName == "" {
		runtimeName = ContainerdRunscRuntime
	}
	if spec.Backend == runner.BackendContainerdRunsc && runtimeName != ContainerdRunscRuntime {
		return HardenedSpec{}, fmt.Errorf("production backend requires runtime %q", ContainerdRunscRuntime)
	}
	if spec.Backend == runner.BackendPodman && runtimeName != "podman" {
		return HardenedSpec{}, errors.New("podman backend requires podman runtime")
	}
	uid, gid, err := numericUser(spec.RunAsUser)
	if err != nil {
		return HardenedSpec{}, err
	}

	image, err := pinnedImageReference(spec.Image, spec.ImageDigest)
	if err != nil {
		return HardenedSpec{}, err
	}
	mounts, err := buildMounts(spec.Mounts, workspace)
	if err != nil {
		return HardenedSpec{}, err
	}
	if spec.Network == runner.NetworkBrokered {
		sessionPath, err := resolveBrokerSession(brokerRoot, spec.BrokerSessionID, spec.RunAsUser)
		if err != nil {
			return HardenedSpec{}, err
		}
		mounts = append(mounts, HardenedMount{
			Kind: "broker", Source: sessionPath, Destination: BrokerMountDestination, ReadOnly: true,
		})
		for index := range mounts {
			if mounts[index].Kind != "skills" {
				continue
			}
			skillsPath := filepath.Join(sessionPath, "skills")
			if err := validateReadOnlySkills(skillsPath, int(uid), int(gid)); err != nil {
				return HardenedSpec{}, fmt.Errorf("broker skills: %w", err)
			}
			mounts[index].Source = skillsPath
		}
	}
	var environment []string
	if spec.EnvironmentFile != "" {
		if err := validateEnvironmentFile(spec.EnvironmentFile); err != nil {
			return HardenedSpec{}, err
		}
		if runtimeName != "podman" {
			environment, err = readEnvironmentFile(spec.EnvironmentFile)
			if err != nil {
				return HardenedSpec{}, err
			}
		}
	}
	return HardenedSpec{
		Runtime:             runtimeName,
		Image:               image,
		RunAsUser:           spec.RunAsUser,
		ReadOnlyRootFS:      spec.ReadOnlyRootFS,
		NoNewPrivileges:     spec.NoNewPrivileges,
		DropAllCapabilities: spec.DropAllCapabilities,
		Network:             spec.Network,
		DirectInternet:      spec.DirectInternet,
		Privileged:          spec.Privileged,
		Mounts:              mounts,
		BrokerSessionID:     spec.BrokerSessionID,
		EnvironmentFile:     spec.EnvironmentFile,
		Environment:         environment,
		Resources:           spec.Resources,
	}, nil
}

const maxEnvironmentFileBytes = 16 << 20

func validateEnvironmentFile(path string) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "." || !filepath.IsAbs(clean) || clean != path {
		return errors.New("materialized environment path must be a clean absolute path")
	}
	info, err := lstatNoSymlink(clean)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !sameOwner(info, os.Geteuid(), os.Getegid()) || info.Size() < 1 || info.Size() > maxEnvironmentFileBytes {
		return errors.New("materialized environment file is not private and bounded")
	}
	return nil
}

func readEnvironmentFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > maxEnvironmentFileBytes {
		return nil, errors.New("read materialized environment")
	}
	var result []string
	seen := make(map[string]struct{})
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		name, value, ok := bytes.Cut(line, []byte{'='})
		if !ok || !environmentNamePattern.Match(name) || bytes.IndexByte(value, 0) >= 0 || bytes.IndexAny(value, "\r\n") >= 0 {
			return nil, errors.New("materialized environment is invalid")
		}
		key := string(name)
		if _, exists := seen[key]; exists {
			return nil, errors.New("materialized environment contains a duplicate")
		}
		seen[key] = struct{}{}
		result = append(result, string(line))
	}
	if len(result) == 0 || len(result) > 256 {
		return nil, errors.New("materialized environment is empty or too large")
	}
	return result, nil
}

func validateReadOnlySkills(root string, uid, gid int) error {
	root = filepath.Clean(root)
	entries := 0
	var total int64
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > 4096 {
			return errors.New("skills tree exceeds entry limit")
		}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !sameOwner(info, uid, gid) {
			return errors.New("skills tree contains an unsafe entry")
		}
		if entry.IsDir() {
			if info.Mode().Perm() != 0555 {
				return errors.New("skills directories must have mode 0555")
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0444 {
			return errors.New("skill files must be read-only regular files")
		}
		total += info.Size()
		if info.Size() < 0 || total > 64<<20 {
			return errors.New("skills tree exceeds size limit")
		}
		return nil
	})
}

func validateBrokerRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return errors.New("broker root is required")
	}
	clean := filepath.Clean(root)
	if clean == "." || clean == string(filepath.Separator) || !filepath.IsAbs(clean) {
		return errors.New("broker root must be a non-root absolute path")
	}
	return nil
}

// resolveBrokerSession turns an opaque ID into one exact, private session
// directory. It deliberately does not accept a source path, destination, or
// socket path from the SandboxSpec.
func resolveBrokerSession(root, sessionID, runAsUser string) (string, error) {
	if err := runner.ValidateBrokerSessionID(sessionID); err != nil {
		return "", err
	}
	if sessionID == "" {
		return "", errors.New("broker session id is required")
	}
	if err := validateBrokerRoot(root); err != nil {
		return "", err
	}
	uid, gid, err := numericUser(runAsUser)
	if err != nil {
		return "", err
	}
	if err := validatePrivateDirectory(root, os.Geteuid(), os.Getegid()); err != nil {
		return "", fmt.Errorf("broker root: %w", err)
	}
	sessionPath := filepath.Join(filepath.Clean(root), sessionID)
	if !within(filepath.Clean(root), sessionPath) || filepath.Base(sessionPath) != sessionID {
		return "", errors.New("broker session path escapes broker root")
	}
	if err := validatePrivateDirectory(sessionPath, int(uid), int(gid)); err != nil {
		return "", fmt.Errorf("broker session: %w", err)
	}
	for _, name := range []string{BrokerClientConfigName, BrokerSocketName} {
		path := filepath.Join(sessionPath, name)
		if !within(sessionPath, path) {
			return "", errors.New("broker session entry escapes session directory")
		}
		info, err := lstatNoSymlink(path)
		if err != nil {
			return "", fmt.Errorf("broker session %s: %w", name, err)
		}
		if !sameOwner(info, int(uid), int(gid)) {
			return "", fmt.Errorf("broker session %s has wrong ownership", name)
		}
		if name == BrokerClientConfigName {
			if info.Mode().Perm() != 0400 {
				return "", errors.New("broker client config must have mode 0400")
			}
			if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<10 {
				return "", errors.New("broker client config must be a non-empty regular file")
			}
		} else {
			if info.Mode().Perm() != 0600 {
				return "", errors.New("broker session broker.sock must have mode 0600")
			}
			if info.Mode()&os.ModeSocket == 0 {
				return "", errors.New("broker session broker.sock must be a Unix socket")
			}
		}
	}
	return sessionPath, nil
}

func validatePrivateDirectory(path string, uid, gid int) error {
	info, err := lstatNoSymlink(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	if !sameOwner(info, uid, gid) {
		return errors.New("directory has wrong ownership")
	}
	if info.Mode().Perm() != 0700 {
		return errors.New("directory must have mode 0700")
	}
	return nil
}

func lstatNoSymlink(path string) (os.FileInfo, error) {
	clean := filepath.Clean(path)
	if clean == "." || !filepath.IsAbs(clean) {
		return nil, errors.New("path must be absolute")
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("path contains a symlink")
		}
	}
	return os.Lstat(clean)
}

func sameOwner(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == uid && int(stat.Gid) == gid
}

func numericUser(value string) (uint32, uint32, error) {
	parts := strings.Split(value, ":")
	if len(parts) > 2 || len(parts) == 0 {
		return 0, 0, errors.New("run_as_user must be numeric uid[:gid]")
	}
	parse := func(raw string) (uint32, error) {
		parsed, err := strconv.ParseUint(raw, 10, 31)
		if err != nil || parsed == 0 {
			return 0, errors.New("run_as_user must contain non-root numeric uid/gid")
		}
		return uint32(parsed), nil
	}
	uid, err := parse(parts[0])
	if err != nil {
		return 0, 0, err
	}
	gid := uid
	if len(parts) == 2 {
		gid, err = parse(parts[1])
		if err != nil {
			return 0, 0, err
		}
	}
	return uid, gid, nil
}

func buildMounts(mounts []runner.Mount, workspace string) ([]HardenedMount, error) {
	result := make([]HardenedMount, 0, len(mounts))
	foundWorkspace := false
	for _, mount := range mounts {
		if mount.Kind == "workspace" {
			foundWorkspace = true
		}
		source := filepath.Join(workspace, mount.Kind)
		if mount.Kind == "workspace" {
			source = workspace
		}
		if !within(workspace, source) {
			return nil, fmt.Errorf("mount source escapes dedicated workspace: %q", source)
		}
		result = append(result, HardenedMount{
			Kind: mount.Kind, Source: source, Destination: mount.Destination, ReadOnly: mount.ReadOnly,
		})
	}
	if !foundWorkspace {
		return nil, errors.New("a dedicated workspace mount is required")
	}
	return result, nil
}

func validateHardenedMount(mount HardenedMount) error {
	if mount.Kind == "broker" {
		if mount.Destination != BrokerMountDestination || !mount.ReadOnly {
			return errors.New("broker mount must be read-only at /run/agw")
		}
		if !filepath.IsAbs(mount.Source) || filepath.Clean(mount.Source) != mount.Source {
			return errors.New("broker mount source must be a clean absolute path")
		}
		return nil
	}
	allowed := map[string]struct {
		destination string
		readOnly    bool
	}{
		"workspace": {destination: "/workspace", readOnly: false},
		"artifact":  {destination: "/artifacts", readOnly: true},
		"skills":    {destination: "/skills", readOnly: true},
	}
	policy, ok := allowed[mount.Kind]
	if !ok || mount.Destination != policy.destination || mount.ReadOnly != policy.readOnly {
		return fmt.Errorf("invalid hardened mount %q", mount.Kind)
	}
	return nil
}

func pinnedImageReference(image, digest string) (string, error) {
	image = strings.TrimSpace(image)
	digest = strings.TrimSpace(digest)
	if image == "" || digest == "" {
		return "", errors.New("image and image digest are required")
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", errors.New("image digest must be sha256:<64 hex characters>")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
		return "", errors.New("image digest must contain lowercase hexadecimal characters")
	}
	if at := strings.LastIndexByte(image, '@'); at >= 0 {
		if image[at+1:] != digest {
			return "", errors.New("image reference digest does not match image_digest")
		}
		return image, nil
	}
	return image + "@" + digest, nil
}

func prepareWorkspace(root, id string) (string, error) {
	root = filepath.Clean(root)
	if root == "." || !filepath.IsAbs(root) || root == string(filepath.Separator) {
		return "", errors.New("workspace root must be a non-root absolute path")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", fmt.Errorf("create workspace root: %w", err)
	}
	if info, err := os.Lstat(root); err != nil {
		return "", fmt.Errorf("inspect workspace root: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("workspace root must not be a symlink")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", fmt.Errorf("harden workspace root: %w", err)
	}
	workspace, err := os.MkdirTemp(root, "run-"+safeID(id)+"-")
	if err != nil {
		return "", fmt.Errorf("create dedicated workspace: %w", err)
	}
	if err := os.Chmod(workspace, 0700); err != nil {
		_ = os.RemoveAll(workspace)
		return "", fmt.Errorf("harden dedicated workspace: %w", err)
	}
	return workspace, nil
}

func createMountDirectories(workspace, runAsUser string, mounts []HardenedMount) error {
	uid, gid, err := numericUser(runAsUser)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 && os.Geteuid() != int(uid) {
		return fmt.Errorf("runner uid %d must match sandbox uid %d (or run as root to prepare ownership)", os.Geteuid(), uid)
	}
	if err := ensureOwnership(workspace, int(uid), int(gid)); err != nil {
		return fmt.Errorf("set workspace ownership: %w", err)
	}
	for _, mount := range mounts {
		if mount.Source == workspace {
			continue
		}
		// Broker sessions and broker-materialized skills are capabilities owned
		// by the control plane. They were already validated by
		// buildHardenedSpec and must remain immutable at the runner boundary.
		if mount.Kind == "broker" || (mount.Kind == "skills" && !within(workspace, mount.Source)) {
			continue
		}
		if !within(workspace, mount.Source) {
			return fmt.Errorf("%s mount source is outside the dedicated workspace", mount.Kind)
		}
		if err := os.MkdirAll(mount.Source, 0700); err != nil {
			return fmt.Errorf("create %s mount: %w", mount.Kind, err)
		}
		if err := os.Chmod(mount.Source, 0700); err != nil {
			return fmt.Errorf("harden %s mount: %w", mount.Kind, err)
		}
		if err := ensureOwnership(mount.Source, int(uid), int(gid)); err != nil {
			return fmt.Errorf("set %s mount ownership: %w", mount.Kind, err)
		}
	}
	return nil
}

func ensureOwnership(path string, uid, gid int) error {
	if os.Geteuid() == uid && os.Getegid() == gid {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func cleanupOrphanWorkspaces(root string) error {
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) || !filepath.IsAbs(root) {
		return errors.New("workspace root must be a non-root absolute path")
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "run-agw-") {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "sandbox"
	}
	return b.String()
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "agw-fallback-sandbox"
	}
	return "agw-" + hex.EncodeToString(raw[:])
}
