package static

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func root(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestDisposableYAMLDocumentsParse(t *testing.T) {
	for _, name := range []string{"kustomization.yaml", "namespace.yaml", "rbac.yaml", "networkpolicy.yaml", "configmap.yaml", "pod.yaml"} {
		body, err := os.ReadFile(filepath.Join(root(t), name))
		if err != nil {
			t.Fatal(err)
		}
		for documentNumber, document := range strings.Split(string(body), "\n---") {
			var value map[string]any
			if err := yaml.Unmarshal([]byte(document), &value); err != nil {
				t.Fatalf("%s document %d: %v", name, documentNumber+1, err)
			}
			if len(value) == 0 {
				t.Fatalf("%s document %d is empty", name, documentNumber+1)
			}
		}
	}
}

func TestAgentgatewayConfigIsRecordingOnlyAndManagementIsUnix(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(root(t), "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"port: 8082",
		"prefixMode: never",
		"host: http://127.0.0.1:9090/mcp",
		"file: /run/agentgateway/canary/token",
		"name: x-phase0-canary",
		"adminAddr: unix:///run/agentgateway/runtime/admin.sock",
		"statsAddr: unix:///run/agentgateway/runtime/stats.sock",
		"readinessAddr: unix:///run/agentgateway/runtime/readiness.sock",
		"    config:",
		"level: warn",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("configmap missing %q", required)
		}
	}
	if strings.Contains(text, "adminAddr: 127.") || strings.Contains(text, "statsAddr: 0.0.0.0") || strings.Contains(text, "readinessAddr: 0.0.0.0") {
		t.Fatal("management endpoint was configured as an agent-reachable TCP listener")
	}
}

func TestPodSecurityAndAirlockSentinel(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(root(t), "pod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"hostUsers: false",
		"automountServiceAccountToken: false",
		"type: RuntimeDefault",
		"runAsUser: 1000",
		"runAsUser: 1337",
		"runAsUser: 1338",
		"runAsUser: 1339",
		"add: [CHOWN, DAC_OVERRIDE]",
		"chown 1338:1338 /run/agentgateway/runtime",
		"chmod 0700 /run/agentgateway/runtime",
		"value: recording-only",
		"readOnlyRootFilesystem: true",
		"drop: [ALL]",
		"@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("pod missing %q", required)
		}
	}
	if strings.Contains(text, "agentgateway:v1.4.1") || strings.Contains(text, "secretRef:") || strings.Contains(text, "kind: ClusterRole") {
		t.Fatal("pod contains an unapproved credential or mutable gateway reference")
	}
	airlock, err := os.ReadFile(filepath.Join(root(t), "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"iptables -w -F OUTPUT",
		"ip6tables -w -F OUTPUT",
		"iptables -w -A INPUT -i lo -m conntrack --ctstate NEW",
		"ip6tables -w -A INPUT -i lo -m conntrack --ctstate NEW",
		"--dports 8081,8082,9090",
		"--uid-owner 1000",
		"--uid-owner 1337",
		"--uid-owner 1338",
		"--dport 8081",
		"--dport 8082",
		"--dport 9090",
		"-d ::1",
	} {
		if !strings.Contains(string(airlock), required) {
			t.Fatalf("airlock missing %q", required)
		}
	}
	for _, line := range strings.Split(string(airlock), "\n") {
		if strings.Contains(line, "-A OUTPUT -o lo") && !strings.Contains(line, "-m owner") {
			t.Fatalf("loopback OUTPUT rule is not owner-scoped: %q", line)
		}
	}
	if strings.Contains(string(airlock), "public-test") || strings.Contains(string(airlock), "0.0.0.0/0") || strings.Contains(string(airlock), "-d ::/0") {
		t.Fatal("recording-only probe contains a public-test wildcard allow")
	}
}

func TestCanaryMountIsNotVisibleToAgentOrGuard(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(root(t), "pod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var pod struct {
		Spec struct {
			InitContainers []containerSpec `json:"initContainers"`
			Containers     []containerSpec `json:"containers"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(body, &pod); err != nil {
		t.Fatal(err)
	}
	find := func(containers []containerSpec, name string) containerSpec {
		for _, container := range containers {
			if container.Name == name {
				return container
			}
		}
		t.Fatalf("container %q missing", name)
		return containerSpec{}
	}
	hasMount := func(container containerSpec, name string) bool {
		for _, mount := range container.VolumeMounts {
			if mount.Name == name {
				return true
			}
		}
		return false
	}
	hasEnv := func(container containerSpec, name string) bool {
		for _, env := range container.Env {
			if env.Name == name {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"agent", "agw-guard", "evidence"} {
		container := find(pod.Spec.Containers, name)
		if hasMount(container, "canary") || hasEnv(container, "AGW_RECORDING_CANARY") {
			t.Fatalf("credential canary is visible to %s", name)
		}
	}
	if gateway := find(pod.Spec.Containers, "agentgateway"); !hasMount(gateway, "canary") || hasEnv(gateway, "AGW_RECORDING_CANARY") {
		t.Fatal("agentgateway does not have exactly the intended canary file boundary")
	}
	if fixture := find(pod.Spec.InitContainers, "credential-fixture"); !hasMount(fixture, "canary") || !hasMount(fixture, "agentgateway-runtime") {
		t.Fatal("credential fixture does not prepare both owned runtime volumes")
	}
}

type containerSpec struct {
	Name string `json:"name"`
	Env  []struct {
		Name string `json:"name"`
	} `json:"env"`
	VolumeMounts []struct {
		Name string `json:"name"`
	} `json:"volumeMounts"`
}
