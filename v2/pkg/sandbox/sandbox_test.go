package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Astatide1337/agents-gateway/v2/pkg/runner"
)

func testSandboxSpec(backend runner.BackendKind) runner.SandboxSpec {
	grade := runner.IsolationStandard
	if backend == runner.BackendContainerdRunsc {
		grade = runner.IsolationEnhanced
	}
	return runner.SandboxSpec{
		Backend: backend, IsolationGrade: grade, Image: "ghcr.io/example/agent:stable",
		ImageDigest: "sha256:" + strings.Repeat("a", 64), RunAsUser: "65532",
		ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
		Network:   runner.NetworkNone,
		Resources: runner.ResourceLimits{CPUs: 1.5, MemoryBytes: 512 << 20, DiskBytes: 2 << 30, PIDs: 128, Timeout: time.Minute},
		Mounts: []runner.Mount{
			{Kind: "workspace", Destination: "/workspace", ReadOnly: false},
			{Kind: "skills", Destination: "/skills", ReadOnly: true},
			{Kind: "artifact", Destination: "/artifacts", ReadOnly: true},
		},
	}
}

func TestBuildHardenedContainerdSpec(t *testing.T) {
	workspace := "/var/lib/agw-runner/workspaces/run-fixed"
	plan, err := BuildHardenedSpec(testSandboxSpec(runner.BackendContainerdRunsc), workspace, ContainerdRunscRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Runtime != ContainerdRunscRuntime || plan.Image != "ghcr.io/example/agent:stable@sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("unexpected runtime/image: %#v", plan)
	}
	if plan.RunAsUser != "65532" || !plan.ReadOnlyRootFS || !plan.NoNewPrivileges || !plan.DropAllCapabilities || plan.DirectInternet || plan.Privileged {
		t.Fatalf("hardening flags were not exact: %#v", plan)
	}
	if plan.Network != runner.NetworkNone || len(plan.Mounts) != 3 || plan.Mounts[0].Source != workspace || plan.Mounts[1].Source != workspace+"/skills" {
		t.Fatalf("unexpected mounts/network: %#v", plan)
	}
	if plan.Resources.CPUs != 1.5 || plan.Resources.MemoryBytes != 512<<20 || plan.Resources.PIDs != 128 {
		t.Fatalf("resource limits changed: %#v", plan.Resources)
	}
}

func TestBrokerSessionIsResolvedAndMountedAtFixedDestination(t *testing.T) {
	root, sessionID, runAsUser := validBrokerSession(t)
	spec := testSandboxSpec(runner.BackendPodman)
	spec.Network = runner.NetworkBrokered
	spec.BrokerSessionID = sessionID
	spec.RunAsUser = runAsUser
	plan, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", root)
	if err != nil {
		t.Fatalf("build brokered spec: %v", err)
	}
	if plan.Network != runner.NetworkBrokered || plan.BrokerSessionID != sessionID {
		t.Fatalf("broker semantic mode was not preserved: %#v", plan)
	}
	var brokerMount HardenedMount
	for _, mount := range plan.Mounts {
		if mount.Kind == "broker" {
			brokerMount = mount
		}
	}
	wantSource := filepath.Join(root, sessionID)
	if brokerMount.Source != wantSource || brokerMount.Destination != BrokerMountDestination || !brokerMount.ReadOnly {
		t.Fatalf("unexpected broker mount: %#v, want source %q", brokerMount, wantSource)
	}
	args, err := podmanArgs("agw-broker", plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--network=none") || !strings.Contains(joined, "type=bind,src="+wantSource+",dst=/run/agw,ro") {
		t.Fatalf("brokered plan did not produce fixed networkless mount: %q", joined)
	}
}

func TestMountPreparationDoesNotMutateBrokerCapabilities(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	broker := filepath.Join(t.TempDir(), "session")
	if err := os.Mkdir(broker, 0700); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(broker, "skills")
	if err := os.Mkdir(skills, 0555); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Geteuid(), os.Getegid()
	mounts := []HardenedMount{
		{Kind: "workspace", Source: workspace, Destination: "/workspace"},
		{Kind: "artifact", Source: filepath.Join(workspace, "artifact"), Destination: "/artifacts", ReadOnly: true},
		{Kind: "skills", Source: skills, Destination: "/skills", ReadOnly: true},
		{Kind: "broker", Source: broker, Destination: BrokerMountDestination, ReadOnly: true},
	}
	if err := createMountDirectories(workspace, fmt.Sprintf("%d:%d", uid, gid), mounts); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{broker: 0700, skills: 0555} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("broker capability %q changed mode: got %o want %o", path, info.Mode().Perm(), mode)
		}
	}
	artifactInfo, err := os.Stat(filepath.Join(workspace, "artifact"))
	if err != nil {
		t.Fatalf("workspace artifact mount was not prepared: %v", err)
	}
	if artifactInfo.Mode().Perm() != 0700 {
		t.Fatalf("workspace artifact mount mode = %o, want 700", artifactInfo.Mode().Perm())
	}
}

func TestRuntimeStartupErrorIsLowCardinality(t *testing.T) {
	tests := []struct {
		stderr string
		want   error
	}{
		{"agw-codex-adapter: start run broker bridge: open broker client config: permission denied", ErrRuntimeBroker},
		{"agw-codex-adapter: invalid adapter configuration: model invalid", ErrRuntimeConfig},
		{"agw-codex-adapter: decode run.start: private-prompt-must-not-escape", ErrRuntimeContract},
		{"arbitrary untrusted stderr", nil},
	}
	for _, test := range tests {
		got := runtimeStartupError(test.stderr)
		if !errors.Is(got, test.want) || (test.want == nil && got != nil) {
			t.Fatalf("runtimeStartupError(%q) = %v, want %v", test.stderr, got, test.want)
		}
		if got != nil && strings.Contains(got.Error(), "private-prompt") {
			t.Fatal("startup classifier copied untrusted stderr")
		}
	}
}

func TestRuntimeProcessErrorClassifiesPodmanSetupWithoutCopyingStderr(t *testing.T) {
	tests := []struct {
		code   int
		stderr string
		want   error
	}{
		{125, "Error: statfs /private/path: permission denied secret-value", ErrPodmanPermission},
		{125, "Error: OCI mount setup failed", ErrPodmanMount},
		{125, "Error: container setup failed", ErrPodmanRuntime},
		{1, "ordinary adapter failure", ErrRuntimePreamble},
		{2, "adapter configuration failure", ErrRuntimeEnvironment},
		{137, "killed", ErrRuntimeKilled},
		{126, "exec container process: Permission denied", ErrPodmanPermission},
		{127, "exec container process: no such file or directory", ErrRuntimeMissing},
	}
	for _, test := range tests {
		got := runtimeProcessError(test.code, test.stderr)
		if !errors.Is(got, test.want) || (test.want == nil && got != nil) {
			t.Fatalf("runtimeProcessError(%d, %q) = %v, want %v", test.code, test.stderr, got, test.want)
		}
		if got != nil && strings.Contains(got.Error(), "secret-value") {
			t.Fatal("runtime classifier copied untrusted stderr")
		}
	}
}

func TestPodmanBackendResolvesBrokerSessionBeforeStart(t *testing.T) {
	root, sessionID, runAsUser := validBrokerSession(t)
	factory := &fakePodmanFactory{process: &fakePodmanProcess{}}
	backend, err := NewRootlessPodmanBackend(PodmanConfig{
		WorkspaceRoot: t.TempDir(), BrokerRoot: root, Factory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := testSandboxSpec(runner.BackendPodman)
	spec.Network = runner.NetworkBrokered
	spec.BrokerSessionID = sessionID
	spec.RunAsUser = runAsUser
	handle, err := backend.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("start brokered podman sandbox: %v", err)
	}
	if err := handle.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	starts := append([][]string(nil), factory.starts...)
	factory.mu.Unlock()
	if len(starts) != 1 {
		t.Fatalf("recorded starts = %d, want 1", len(starts))
	}
	joined := strings.Join(starts[0], "\x00")
	if !strings.Contains(joined, "--network=none") || !strings.Contains(joined, "dst=/run/agw,ro") {
		t.Fatalf("backend did not pass fixed broker mount/network policy: %q", joined)
	}
}

func TestBrokerSessionResolutionRejectsAdversarialFilesystemInputs(t *testing.T) {
	root, sessionID, runAsUser := validBrokerSession(t)
	spec := testSandboxSpec(runner.BackendPodman)
	spec.Network = runner.NetworkBrokered
	spec.BrokerSessionID = sessionID
	spec.RunAsUser = runAsUser

	tests := []struct {
		name string
		edit func(string, *runner.SandboxSpec)
	}{
		{name: "traversal session id", edit: func(_ string, spec *runner.SandboxSpec) { spec.BrokerSessionID = "../escape" }},
		{name: "absolute session id", edit: func(_ string, spec *runner.SandboxSpec) { spec.BrokerSessionID = "/tmp/session" }},
		{name: "missing broker root", edit: func(_ string, spec *runner.SandboxSpec) { spec.BrokerSessionID = "ags_missing" }},
		{name: "wrong owner", edit: func(_ string, spec *runner.SandboxSpec) {
			uid, _, _ := numericUser(spec.RunAsUser)
			if uid == 65535 {
				spec.RunAsUser = "65534"
			} else {
				spec.RunAsUser = strconv.FormatUint(uint64(uid+1), 10)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := spec
			candidate.Mounts = append([]runner.Mount(nil), spec.Mounts...)
			candidateRoot := root
			test.edit(candidateRoot, &candidate)
			if _, err := buildHardenedSpec(candidate, "/var/lib/agw-runner/workspaces/run-fixed", "podman", candidateRoot); err == nil {
				t.Fatal("unsafe broker session was accepted")
			}
		})
	}

	t.Run("symlinked session directory", func(t *testing.T) {
		linkRoot := t.TempDir()
		if err := os.Chmod(linkRoot, 0700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(linkRoot, sessionID)
		if err := os.Symlink(filepath.Join(root, sessionID), link); err != nil {
			t.Fatal(err)
		}
		if _, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", linkRoot); err == nil {
			t.Fatal("symlinked broker session was accepted")
		}
	})

	t.Run("wrong root permissions", func(t *testing.T) {
		badRoot := filepath.Join(t.TempDir(), "brokers")
		if err := os.Mkdir(badRoot, 0750); err != nil {
			t.Fatal(err)
		}
		if _, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", badRoot); err == nil {
			t.Fatal("group-readable broker root was accepted")
		}
	})

	for _, name := range []string{BrokerClientConfigName, BrokerSocketName} {
		t.Run("missing "+name, func(t *testing.T) {
			copyRoot := copyBrokerRoot(t, root, sessionID, runAsUser)
			if err := os.Remove(filepath.Join(copyRoot, sessionID, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", copyRoot); err == nil {
				t.Fatalf("missing %s was accepted", name)
			}
		})
	}

	t.Run("regular file in place of socket", func(t *testing.T) {
		copyRoot := copyBrokerRoot(t, root, sessionID, runAsUser)
		socket := filepath.Join(copyRoot, sessionID, BrokerSocketName)
		if err := os.Remove(socket); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(socket, []byte("not a socket"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", copyRoot); err == nil {
			t.Fatal("regular file was accepted as broker socket")
		}
	})

	t.Run("symlinked client config", func(t *testing.T) {
		copyRoot := copyBrokerRoot(t, root, sessionID, runAsUser)
		config := filepath.Join(copyRoot, sessionID, BrokerClientConfigName)
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, sessionID, BrokerClientConfigName), config); err != nil {
			t.Fatal(err)
		}
		if _, err := buildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run-fixed", "podman", copyRoot); err == nil {
			t.Fatal("symlinked broker client config was accepted")
		}
	})
}

func validBrokerSession(t *testing.T) (string, string, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root cannot exercise the rootless ownership baseline")
	}
	parent, err := os.MkdirTemp("/tmp", "agw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	root := filepath.Join(parent, "brokers")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	sessionID := "ags_test_session-1"
	session := filepath.Join(root, sessionID)
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, BrokerClientConfigName), []byte(`{"socket":"/run/agw/broker.sock"}`), 0400); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(session, BrokerSocketName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(session, BrokerSocketName), 0600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	skills := filepath.Join(session, "skills")
	if err := os.Mkdir(skills, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# test\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skills, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return root, sessionID, strconv.Itoa(os.Geteuid())
}

func copyBrokerRoot(t *testing.T, source, sessionID, runAsUser string) string {
	t.Helper()
	parent, err := os.MkdirTemp("/tmp", "agw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	root := filepath.Join(parent, "brokers")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	sourceSession := filepath.Join(source, sessionID)
	destinationSession := filepath.Join(root, sessionID)
	if err := os.Mkdir(destinationSession, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sourceSession, BrokerClientConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destinationSession, BrokerClientConfigName), data, 0400); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(destinationSession, BrokerSocketName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(destinationSession, BrokerSocketName), 0600); err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	skills := filepath.Join(destinationSession, "skills")
	if err := os.Mkdir(skills, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("# test\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skills, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_ = runAsUser
	return root
}

func TestBuildHardenedSpecRejectsUnpinnedOrUnsafeInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*runner.SandboxSpec)
	}{
		{"non-hex digest", func(s *runner.SandboxSpec) { s.ImageDigest = "sha256:" + strings.Repeat("z", 64) }},
		{"digest mismatch", func(s *runner.SandboxSpec) { s.Image = "repo/image@sha256:" + strings.Repeat("b", 64) }},
		{"missing workspace", func(s *runner.SandboxSpec) {
			s.Mounts = []runner.Mount{{Kind: "skills", Destination: "/skills", ReadOnly: true}}
		}},
		{"runtime socket", func(s *runner.SandboxSpec) { s.RuntimeSocket = "/run/containerd/containerd.sock" }},
		{"unimplemented brokered network", func(s *runner.SandboxSpec) { s.Network = runner.NetworkBrokered }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSandboxSpec(runner.BackendContainerdRunsc)
			tc.mutate(&spec)
			if _, err := BuildHardenedSpec(spec, "/var/lib/agw-runner/workspaces/run", ContainerdRunscRuntime); err == nil {
				t.Fatal("unsafe sandbox spec was accepted")
			}
		})
	}
}

func TestPodmanArgsAreTypedAndHardened(t *testing.T) {
	plan, err := BuildHardenedSpec(testSandboxSpec(runner.BackendPodman), "/var/lib/agw-runner/workspaces/run", "podman")
	if err != nil {
		t.Fatal(err)
	}
	args, err := podmanArgs("agw-fixed", plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	for _, required := range []string{"run", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--network=none", "--user\x0065532", "--userns=keep-id", "--label\x00" + podmanOwnershipLabelKey + "=" + podmanOwnershipLabelValue, "--tmpfs\x00/workspace:rw,nosuid,nodev,size=2147483648,mode=01777", "ghcr.io/example/agent:stable@sha256:" + strings.Repeat("a", 64)} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing hardened argv item %q in %q", required, joined)
		}
	}
	if strings.Contains(joined, "containerd.sock") || strings.Contains(joined, "docker.sock") || strings.Contains(joined, "--privileged") {
		t.Fatalf("runtime socket or privilege appeared in argv: %q", joined)
	}
	if strings.Contains(joined, "src=/var/lib/agw-runner/workspaces/run,dst=/workspace") {
		t.Fatalf("workspace must be a quota-bounded tmpfs, not an unbounded bind mount: %q", joined)
	}
	if strings.Contains(joined, "uid=") || strings.Contains(joined, "gid=") {
		t.Fatalf("Podman tmpfs uid=/gid= options are not portable: %q", joined)
	}
	if !strings.Contains(joined, "--user\x0065532") || !strings.Contains(joined, "--userns=keep-id") {
		t.Fatalf("workspace fix must retain non-root user enforcement: %q", joined)
	}
}

func TestRuntimeReceivesOnlyPrivateMaterializedEnvironmentFile(t *testing.T) {
	root := t.TempDir()
	envFile := filepath.Join(root, "env")
	if err := os.WriteFile(envFile, []byte("API_KEY=host-only-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := testSandboxSpec(runner.BackendPodman)
	spec.EnvironmentFile = envFile
	plan, err := BuildHardenedSpec(spec, filepath.Join(root, "workspace"), "podman")
	if err != nil {
		t.Fatal(err)
	}
	args, err := podmanArgs("agw-env", plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--env-file\x00"+envFile) {
		t.Fatalf("podman did not receive env-file path: %q", joined)
	}
	if strings.Contains(joined, "host-only-value") {
		t.Fatal("secret value appeared in runtime argv")
	}

	containerdSpec := testSandboxSpec(runner.BackendContainerdRunsc)
	containerdSpec.EnvironmentFile = envFile
	containerdPlan, err := BuildHardenedSpec(containerdSpec, filepath.Join(root, "workspace-containerd"), ContainerdRunscRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if len(containerdPlan.Environment) != 1 || containerdPlan.Environment[0] != "API_KEY=host-only-value" {
		t.Fatalf("containerd environment was not parsed at the host boundary: %#v", containerdPlan.Environment)
	}

	if err := os.Chmod(envFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildHardenedSpec(spec, filepath.Join(root, "workspace-unsafe"), "podman"); err == nil {
		t.Fatal("group-readable environment file was accepted")
	}
}

type fakePodmanProcess struct {
	mu      sync.Mutex
	signals []os.Signal
}

func (p *fakePodmanProcess) Wait() (int, error) { return 0, nil }
func (p *fakePodmanProcess) Signal(signal os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, signal)
	return nil
}

type fakePodmanFactory struct {
	mu      sync.Mutex
	starts  [][]string
	runs    [][]string
	process *fakePodmanProcess
}

func (f *fakePodmanFactory) Start(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) (PodmanProcess, error) {
	// The argv snapshot lets tests verify that the host backend, rather than a
	// manifest, selected every source and destination.
	return f.start(ctx, name, args, stdin, stdout, stderr)
}

func (f *fakePodmanFactory) start(_ context.Context, _ string, args []string, _ io.Reader, _ io.Writer, _ io.Writer) (PodmanProcess, error) {
	f.mu.Lock()
	f.starts = append(f.starts, append([]string(nil), args...))
	f.mu.Unlock()
	return f.process, nil
}

func (f *fakePodmanFactory) Run(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
	f.mu.Lock()
	f.runs = append(f.runs, append([]string(nil), args...))
	f.mu.Unlock()
	if len(args) > 0 && args[0] == "ps" {
		_, _ = io.WriteString(stdout, "deadbeef\n-unsafe\nnot valid\n")
	}
	return nil
}

func TestPodmanReconcileRemovesOnlyOwnedContainersAndOrphanWorkspaces(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, "run-agw-orphan")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "keep-me"), 0700); err != nil {
		t.Fatal(err)
	}
	factory := &fakePodmanFactory{process: &fakePodmanProcess{}}
	backend, err := NewRootlessPodmanBackend(PodmanConfig{
		Binary:        "/custom/podman",
		WorkspaceRoot: root,
		Factory:       factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	factory.mu.Lock()
	runs := append([][]string(nil), factory.runs...)
	factory.mu.Unlock()
	if len(runs) != 2 {
		t.Fatalf("expected one ps and one rm command, got %#v", runs)
	}
	if got := strings.Join(runs[0], "\x00"); got != "ps\x00-a\x00--filter\x00"+podmanOwnershipFilter()+"\x00--format\x00{{.ID}}" {
		t.Fatalf("unexpected ownership-filtered list argv: %q", got)
	}
	if got := strings.Join(runs[1], "\x00"); got != "rm\x00--force\x00--time\x000\x00--ignore\x00deadbeef" {
		t.Fatalf("unexpected owned-container removal argv: %q", got)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan workspace remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep-me")); err != nil {
		t.Fatalf("unowned workspace was removed: %v", err)
	}
}

func TestPodmanStopForceRemovesContainerAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "run-agw-fixed")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	factory := &fakePodmanFactory{process: &fakePodmanProcess{}}
	stdout, stdoutWriter := io.Pipe()
	stdinReader, stdinWriter := io.Pipe()
	_ = stdinReader.Close()
	handle := &podmanHandle{
		id: "agw-fixed", process: factory.process, workspace: workspace,
		stdout: stdout, stdoutWriter: stdoutWriter, stdinWriter: stdinWriter,
		binary: "/custom/podman", factory: factory, stderr: &boundedBuffer{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := handle.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := handle.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	runs := append([][]string(nil), factory.runs...)
	factory.mu.Unlock()
	if len(runs) != 1 {
		t.Fatalf("expected exactly one forced removal, got %#v", runs)
	}
	if got := strings.Join(runs[0], "\x00"); got != "rm\x00--force\x00--time\x000\x00--ignore\x00agw-fixed" {
		t.Fatalf("unexpected forced removal argv: %q", got)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after Stop: %v", err)
	}
	factory.process.mu.Lock()
	defer factory.process.mu.Unlock()
	if len(factory.process.signals) != 1 {
		t.Fatalf("expected one process kill, got %d", len(factory.process.signals))
	}
}

func TestPodmanDiagnosticsAreBounded(t *testing.T) {
	var output boundedBuffer
	_, _ = output.Write([]byte(strings.Repeat("x", podmanCommandOutputLimit+1024)))
	if len(output.String()) > podmanCommandOutputLimit+len(" [truncated]") {
		t.Fatalf("diagnostic exceeded bound: %d", len(output.String()))
	}
	if !strings.HasSuffix(output.String(), "[truncated]") {
		t.Fatalf("missing truncation marker: %q", output.String()[podmanCommandOutputLimit-16:])
	}
}

func TestPodmanCommandErrorDoesNotLeakSandboxStderr(t *testing.T) {
	err := commandError("start", errors.New("failed"), "API_TOKEN=super-secret")
	if strings.Contains(err.Error(), "super-secret") || !strings.Contains(err.Error(), "sha256:") {
		t.Fatalf("unsafe diagnostic: %v", err)
	}
}

func TestParseContainerIDsRejectsUnsafeOutput(t *testing.T) {
	got := parseContainerIDs("safe-id\n--all\nwith space\n")
	if len(got) != 1 || got[0] != "safe-id" {
		t.Fatalf("unexpected parsed IDs: %#v", got)
	}
}

func TestPodmanFactoryRequiresLifecycleRunner(t *testing.T) {
	backend, err := NewRootlessPodmanBackend(PodmanConfig{WorkspaceRoot: t.TempDir(), Factory: startOnlyFactory{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Reconcile(context.Background()); err == nil || !strings.Contains(err.Error(), "lifecycle commands") {
		t.Fatalf("expected lifecycle capability error, got %v", err)
	}
}

type startOnlyFactory struct{}

func (startOnlyFactory) Start(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (PodmanProcess, error) {
	return nil, fmt.Errorf("not implemented")
}
