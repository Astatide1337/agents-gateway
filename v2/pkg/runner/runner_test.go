package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func validSandbox(backend BackendKind) SandboxSpec {
	grade := IsolationStandard
	if backend == BackendContainerdRunsc {
		grade = IsolationEnhanced
	}
	return SandboxSpec{
		Backend: backend, IsolationGrade: grade, Image: "ghcr.io/example/agent",
		ImageDigest: "sha256:" + strings.Repeat("a", 64), RunAsUser: "65532", ReadOnlyRootFS: true,
		NoNewPrivileges: true, DropAllCapabilities: true, Network: NetworkBrokered,
		Resources: ResourceLimits{CPUs: 1, MemoryBytes: 256 << 20, DiskBytes: 1 << 30, PIDs: 128, Timeout: time.Minute},
		Mounts:    []Mount{{Kind: "workspace", Destination: "/workspace", ReadOnly: false}, {Kind: "skills", Destination: "/skills", ReadOnly: true}},
	}
}

func TestCapabilitiesDeclareIsolationGrades(t *testing.T) {
	production := ContainerdRunscCapabilities()
	if !production.ProductionSuitable || production.IsolationGrade != IsolationEnhanced || production.DirectInternet {
		t.Fatalf("unexpected production capability profile: %#v", production)
	}
	standalone := RootlessPodmanCapabilities()
	if !standalone.ProductionSuitable || standalone.IsolationGrade != IsolationStandard || standalone.RequiresDedicatedHost {
		t.Fatalf("unexpected rootless podman capability profile: %#v", standalone)
	}
}

func TestValidateSandboxSpecHardening(t *testing.T) {
	for _, backend := range []BackendKind{BackendContainerdRunsc, BackendPodman} {
		if err := ValidateSandboxSpec(validSandbox(backend)); err != nil {
			t.Fatalf("valid %s spec rejected: %v", backend, err)
		}
	}
	cases := []struct {
		name   string
		mutate func(*SandboxSpec)
	}{
		{"floating image", func(s *SandboxSpec) { s.ImageDigest = "latest" }},
		{"writable root", func(s *SandboxSpec) { s.ReadOnlyRootFS = false }},
		{"direct internet", func(s *SandboxSpec) { s.DirectInternet = true }},
		{"runtime socket", func(s *SandboxSpec) { s.RuntimeSocket = "/run/containerd/containerd.sock" }},
		{"host mount", func(s *SandboxSpec) { s.Mounts = append(s.Mounts, Mount{Kind: "host", Destination: "/host"}) }},
		{"arbitrary destination", func(s *SandboxSpec) { s.Mounts[1].Destination = "/etc" }},
		{"duplicate kind", func(s *SandboxSpec) {
			s.Mounts = append(s.Mounts, Mount{Kind: "skills", Destination: "/other", ReadOnly: true})
		}},
		{"read-only workspace", func(s *SandboxSpec) { s.Mounts[0].ReadOnly = true }},
		{"writable skills", func(s *SandboxSpec) { s.Mounts[1].ReadOnly = false }},
		{"missing workspace", func(s *SandboxSpec) { s.Mounts = s.Mounts[1:] }},
		{"missing limits", func(s *SandboxSpec) { s.Resources.MemoryBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSandbox(BackendContainerdRunsc)
			tc.mutate(&spec)
			if err := ValidateSandboxSpec(spec); err == nil {
				t.Fatal("expected hardening validation error")
			}
		})
	}
}

func TestValidateExecutableSandboxSpecRejectsUnimplementedBrokeredNetwork(t *testing.T) {
	for _, backend := range []BackendKind{BackendContainerdRunsc, BackendPodman} {
		spec := validSandbox(backend)
		if err := ValidateExecutableSandboxSpec(spec); err == nil || !strings.Contains(err.Error(), "not operational") {
			t.Fatalf("brokered %s execution was not rejected: %v", backend, err)
		}
		spec.Network = NetworkNone
		if err := ValidateExecutableSandboxSpec(spec); err != nil {
			t.Fatalf("networkless %s execution rejected: %v", backend, err)
		}
	}
}

func TestLeaseFencing(t *testing.T) {
	now := time.Unix(100, 0)
	current := Lease{RunID: "run", LeaseID: "lease", Owner: "runner-a", FencingToken: 7, ExpiresAt: now.Add(time.Minute)}
	if err := ValidateLease(current, current, now); err != nil {
		t.Fatal(err)
	}
	stale := current
	stale.FencingToken = 6
	if !errors.Is(ValidateLease(current, stale, now), ErrLeaseTokenStale) {
		t.Fatal("expected stale fencing token rejection")
	}
	expired := current
	expired.ExpiresAt = now
	if !errors.Is(ValidateLease(current, expired, now), ErrLeaseExpired) {
		t.Fatal("expected expired lease rejection")
	}
	wrongRun := current
	wrongRun.RunID = "other"
	if !errors.Is(ValidateLease(current, wrongRun, now), ErrLeaseRunMismatch) {
		t.Fatal("expected run mismatch rejection")
	}
}

type fakeProbe struct {
	paths map[string]bool
	files map[string][]byte
	stats []string
}

func (p *fakeProbe) LookPath(name string) (string, error) {
	if p.paths[name] {
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found")
}
func (p *fakeProbe) Stat(path string) error {
	p.stats = append(p.stats, path)
	if p.paths[path] {
		return nil
	}
	return errors.New("not found")
}
func (p *fakeProbe) ReadFile(path string) ([]byte, error) {
	value, ok := p.files[path]
	if !ok {
		return nil, errors.New("not found")
	}
	return value, nil
}

func TestDetectHostPrerequisitesIsReadOnly(t *testing.T) {
	probe := &fakeProbe{
		paths: map[string]bool{"containerd": true, "runsc": true, "/run/containerd/containerd.sock": true, "/sys/fs/cgroup/cgroup.controllers": true},
		files: map[string][]byte{"/proc/sys/kernel/unprivileged_userns_clone": []byte("1\n")},
	}
	report, err := DetectHostPrerequisites(context.Background(), BackendContainerdRunsc, HostDetectionOptions{Probe: probe})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Ready || report.IsolationGrade != IsolationEnhanced {
		t.Fatalf("unexpected readiness report: %#v", report)
	}
	for _, path := range probe.stats {
		if strings.HasSuffix(path, ".sock") && path != "/run/containerd/containerd.sock" {
			t.Fatalf("unexpected socket access: %s", path)
		}
	}

	probe.paths["/run/containerd/containerd.sock"] = false
	report, err = DetectHostPrerequisites(context.Background(), BackendContainerdRunsc, HostDetectionOptions{Probe: probe})
	if err != nil || report.Ready {
		t.Fatalf("missing runtime socket should make host unready: %#v, %v", report, err)
	}
}

func TestDetectHostPrerequisitesHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DetectHostPrerequisites(ctx, BackendPodman, HostDetectionOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestDetectPodmanRequiresSubordinateIDRanges(t *testing.T) {
	probe := &fakeProbe{
		paths: map[string]bool{"podman": true, "/sys/fs/cgroup/cgroup.controllers": true},
		files: map[string][]byte{
			"/proc/sys/kernel/unprivileged_userns_clone": []byte("1\n"),
			"/etc/subuid": []byte("agw-runner:100000:65536\n"),
			"/etc/subgid": []byte("agw-runner:100000:65536\n"),
		},
	}
	options := HostDetectionOptions{Probe: probe, Username: "agw-runner"}
	report, err := DetectHostPrerequisites(context.Background(), BackendPodman, options)
	if err != nil || !report.Ready {
		t.Fatalf("valid rootless Podman host rejected: %#v err=%v", report, err)
	}
	probe.files["/etc/subgid"] = []byte("other:100000:65536\n")
	report, err = DetectHostPrerequisites(context.Background(), BackendPodman, options)
	if err != nil || report.Ready {
		t.Fatalf("missing subordinate GID range accepted: %#v err=%v", report, err)
	}
}
