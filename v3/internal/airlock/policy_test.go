package airlock

import (
	"errors"
	"strings"
	"testing"
)

func TestDirectPolicyIsDestinationSpecificAndIPv6Symmetric(t *testing.T) {
	got, err := Render(DirectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got != DirectLockdownScript {
		t.Fatalf("direct script changed unexpectedly:\n%s", got)
	}
	for _, forbidden := range []string{
		"-A OUTPUT -o lo -j ACCEPT",
		"--uid-owner 1000 -j ACCEPT",
		"--uid-owner 1000 -p udp",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8080",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8082",
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8083",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8080",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8082",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8083",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("direct policy contains forbidden rule %q", forbidden)
		}
	}
	for _, required := range []string{
		"iptables-restore --wait --noflush",
		"ip6tables-restore --wait --noflush",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081 -j ACCEPT",
		"-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d ::1 --dport 8081 -j ACCEPT",
		"-A OUTPUT -m owner --uid-owner 1337 -j ACCEPT",
		"-P OUTPUT DROP",
		"-F OUTPUT",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("direct policy is missing required rule %q", required)
		}
	}
}

func TestGuardGatewayPolicyIsPureFutureCompositionFixture(t *testing.T) {
	got, err := Render(GuardGatewayPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081 -j ACCEPT",
		"--uid-owner 1000 -p tcp -d ::1 --dport 8081 -j ACCEPT",
		"--uid-owner 1337 -p tcp -d 127.0.0.1 --dport 8082 -j ACCEPT",
		"--uid-owner 1337 -p tcp -d ::1 --dport 8082 -j ACCEPT",
		"-A OUTPUT -m owner --uid-owner 1338 -j ACCEPT",
	} {
		if !strings.Contains(got, required) {
			t.Fatalf("future policy is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"--uid-owner 1000 -j ACCEPT",
		"--uid-owner 1337 -j ACCEPT",
		"-A OUTPUT -o lo -j ACCEPT",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("future policy contains overly broad rule %q", forbidden)
		}
	}
}

func TestRenderIsDeterministicAndRejectsUnrepresentableRules(t *testing.T) {
	policy := GuardGatewayPolicy()
	first, err := Render(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.Rules[0], policy.Rules[4] = policy.Rules[4], policy.Rules[0]
	second, err := Render(policy)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("rule order changed the rendered script")
	}
	for _, rule := range []Rule{
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.2", Port: BrokerPort},
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1;touch /tmp/pwn", Port: BrokerPort},
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: 0},
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: 2, Destination: "127.0.0.1", Port: BrokerPort},
		{UID: UIDAgent, Family: FamilyAny, AnyEgress: true},
	} {
		if _, err := Render(Policy{Rules: []Rule{rule}}); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("Render(%#v) error=%v, want ErrInvalidPolicy", rule, err)
		}
	}
	if _, err := Render(Policy{Rules: []Rule{{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: BrokerPort}, {UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: BrokerPort}}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("duplicate rule error=%v, want ErrInvalidPolicy", err)
	}
}
