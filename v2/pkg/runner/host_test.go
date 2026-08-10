package runner

import (
	"context"
	"testing"
)

type configuredBinaryProbe struct {
	paths   map[string]bool
	files   map[string][]byte
	lookups []string
}

func (p *configuredBinaryProbe) LookPath(name string) (string, error) {
	p.lookups = append(p.lookups, name)
	if p.paths[name] {
		return name, nil
	}
	return "", errProbeNotFound
}

func (p *configuredBinaryProbe) Stat(path string) error {
	if p.paths[path] {
		return nil
	}
	return errProbeNotFound
}

func (p *configuredBinaryProbe) ReadFile(path string) ([]byte, error) {
	value, ok := p.files[path]
	if !ok {
		return nil, errProbeNotFound
	}
	return value, nil
}

var errProbeNotFound = &probeError{}

type probeError struct{}

func (*probeError) Error() string { return "not found" }

func TestDetectPodmanUsesConfiguredBinary(t *testing.T) {
	probe := &configuredBinaryProbe{
		paths: map[string]bool{
			"/opt/agw/bin/podman":               true,
			"/sys/fs/cgroup/cgroup.controllers": true,
		},
		files: map[string][]byte{
			"/proc/sys/kernel/unprivileged_userns_clone": []byte("1\n"),
			"/etc/subuid": []byte("agw-runner:100000:65536\n"),
			"/etc/subgid": []byte("agw-runner:100000:65536\n"),
		},
	}
	report, err := DetectHostPrerequisites(context.Background(), BackendPodman, HostDetectionOptions{
		Probe: probe, PodmanBinary: "/opt/agw/bin/podman", Username: "agw-runner",
	})
	if err != nil || !report.Ready {
		t.Fatalf("configured Podman binary was rejected: %#v err=%v", report, err)
	}
	if len(probe.lookups) != 3 || probe.lookups[2] != "/opt/agw/bin/podman" {
		t.Fatalf("readiness did not use configured Podman binary: %#v", probe.lookups)
	}
	for _, lookup := range probe.lookups {
		if lookup == "podman" {
			t.Fatalf("readiness still used the hardcoded Podman binary: %#v", probe.lookups)
		}
	}
}
