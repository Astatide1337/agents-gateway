package phase0

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readPhase0Fixture(t *testing.T, relative string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../.."))
	contents, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(contents)
}

func TestUIDAirlockFixtureHasNoBroadLoopbackAllow(t *testing.T) {
	fixture := readPhase0Fixture(t, "test/e2e/phase0/30-uid-airlock.yaml")
	for _, forbidden := range []string{
		"-A OUTPUT -o lo -j ACCEPT",
		"-A OUTPUT -o lo -m owner --uid-owner 1337 -j ACCEPT",
		"--uid-owner 1000 -j ACCEPT",
		"iptables -F OUTPUT",
		"ip6tables -F OUTPUT",
	} {
		if strings.Contains(fixture, forbidden) {
			t.Fatalf("UID airlock fixture contains forbidden fragment %q", forbidden)
		}
	}
	for _, required := range []string{
		"command: [\"/bin/sh\", \"-ceu\"]",
		"iptables-restore --wait --noflush",
		"ip6tables-restore --wait --noflush",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081 -j ACCEPT",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d ::1 --dport 8081 -j ACCEPT",
		"-A OUTPUT -m owner --uid-owner 1337 -j ACCEPT",
		"agent_loopback_default=drop",
		"broker_egress=uid-1337-any",
	} {
		if !strings.Contains(fixture, required) {
			t.Fatalf("UID airlock fixture is missing %q", required)
		}
	}
}

func TestUIDAirlockProbeCoversLoopbackPrivateAddressAndFirewallBypasses(t *testing.T) {
	script := readPhase0Fixture(t, "scripts/phase0-airlock.sh")
	for _, required := range []string{
		"agent-broker-ipv4",
		"agent-broker-ipv6",
		"agent-broker-udp-ipv4",
		"agent-gateway-ipv4",
		"agent-gateway-ipv6",
		"agent-admin-ipv4",
		"agent-admin-ipv6",
		"agent-alternate-ipv4",
		"agent-alternate-ipv6",
		"probe_private_ip_set pod-ip",
		"probe_private_ip_set service-ip",
		"agent-firewall",
		"ip6tables",
		"PHASE0_AIRLOCK_SERVICE",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("Phase 0 airlock probe is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"iptables -A OUTPUT -o lo -j ACCEPT",
		"ip6tables -A OUTPUT -o lo -j ACCEPT",
		"iptables -A OUTPUT -m owner --uid-owner 1000 -j ACCEPT",
		"ip6tables -A OUTPUT -m owner --uid-owner 1000 -j ACCEPT",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("Phase 0 airlock probe contains forbidden fragment %q", forbidden)
		}
	}
}
