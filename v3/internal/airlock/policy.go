// Package airlock renders the fixed, owner-based OUTPUT policy used by the
// work Sandbox lockdown init container.
//
// The production policy is intentionally narrow for the current direct
// agent->broker topology. The future guard->agentgateway policy is exposed as
// a pure renderer fixture only; workload.Build does not select it.
package airlock

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	UIDAgent  uint32 = 1000
	UIDBroker uint32 = 1337
	// The direct broker becomes the strict guard in the three-container
	// topology, so both roles intentionally retain UID 1337. agentgateway is
	// the only future container allowed to initiate downstream traffic.
	UIDGuard   uint32 = UIDBroker
	UIDGateway uint32 = 1338

	BrokerPort  uint16 = 8081
	GatewayPort uint16 = 8082
)

type AddressFamily uint8

const (
	FamilyAny AddressFamily = iota
	FamilyIPv4
	FamilyIPv6
)

type Protocol uint8

const (
	ProtocolTCP Protocol = iota + 1
)

// Rule is an owner rule in the OUTPUT chain. An any-egress rule is deliberately
// explicit: it is used for the current broker's provider egress and for the
// future guard downstream egress. It must never be inferred from a loopback
// rule.
type Rule struct {
	UID         uint32
	Family      AddressFamily
	Protocol    Protocol
	Destination string
	Port        uint16
	AnyEgress   bool
}

type Policy struct {
	Rules []Rule
}

var ErrInvalidPolicy = errors.New("invalid airlock policy")

// DirectPolicy is the production Phase 1 policy. UID 1000 can initiate only
// TCP to the direct broker's loopback listener. UID 1337 retains its existing
// broad provider egress; this is intentionally not a destination restriction.
func DirectPolicy() Policy {
	return Policy{Rules: []Rule{
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: BrokerPort},
		{UID: UIDAgent, Family: FamilyIPv6, Protocol: ProtocolTCP, Destination: "::1", Port: BrokerPort},
		{UID: UIDBroker, Family: FamilyAny, AnyEgress: true},
	}}
}

// GuardGatewayPolicy is a test-only future topology. It is not referenced by
// workload.Build and therefore cannot broaden a production work Sandbox:
// UID 1000 reaches only the guard on 8081, guard UID 1337 reaches only the
// local agentgateway on 8082, and gateway UID 1338 owns downstream egress.
func GuardGatewayPolicy() Policy {
	return Policy{Rules: []Rule{
		{UID: UIDAgent, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: BrokerPort},
		{UID: UIDAgent, Family: FamilyIPv6, Protocol: ProtocolTCP, Destination: "::1", Port: BrokerPort},
		{UID: UIDGuard, Family: FamilyIPv4, Protocol: ProtocolTCP, Destination: "127.0.0.1", Port: GatewayPort},
		{UID: UIDGuard, Family: FamilyIPv6, Protocol: ProtocolTCP, Destination: "::1", Port: GatewayPort},
		{UID: UIDGateway, Family: FamilyAny, AnyEgress: true},
	}}
}

// DirectLockdownScript is kept as a fixed constant so workload manifests can
// carry a stable script contract. --noflush preserves unrelated filter chains;
// only OUTPUT is flushed and then rebuilt with a DROP policy. The two family
// restores are intentionally ordered rather than pretending to be one
// cross-family transaction; the init container's -e contract prevents any
// workload container from starting if either restore fails.
const DirectLockdownScript = `iptables-restore --wait --noflush <<'AGW_IPV4'
*filter
-P OUTPUT DROP
-F OUTPUT
-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d 127.0.0.1 --dport 8081 -j ACCEPT
-A OUTPUT -m owner --uid-owner 1337 -j ACCEPT
COMMIT
AGW_IPV4
ip6tables-restore --wait --noflush <<'AGW_IPV6'
*filter
-P OUTPUT DROP
-F OUTPUT
-A OUTPUT -o lo -m owner --uid-owner 1000 -p tcp -d ::1 --dport 8081 -j ACCEPT
-A OUTPUT -m owner --uid-owner 1337 -j ACCEPT
COMMIT
AGW_IPV6`

func Render(policy Policy) (string, error) {
	rules, err := normalizedRules(policy)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	renderFamily(&b, "iptables", "AGW_IPV4", FamilyIPv4, rules)
	renderFamily(&b, "ip6tables", "AGW_IPV6", FamilyIPv6, rules)
	return b.String(), nil
}

func renderFamily(b *strings.Builder, command, marker string, family AddressFamily, rules []Rule) {
	fmt.Fprintf(b, "%s-restore --wait --noflush <<'%s'\n", command, marker)
	b.WriteString("*filter\n-P OUTPUT DROP\n-F OUTPUT\n")
	for _, rule := range rules {
		if rule.Family != FamilyAny && rule.Family != family {
			continue
		}
		if rule.AnyEgress {
			fmt.Fprintf(b, "-A OUTPUT -m owner --uid-owner %d -j ACCEPT\n", rule.UID)
			continue
		}
		fmt.Fprintf(b, "-A OUTPUT -o lo -m owner --uid-owner %d -p tcp -d %s --dport %d -j ACCEPT\n", rule.UID, rule.Destination, rule.Port)
	}
	b.WriteString("COMMIT\n")
	b.WriteString(marker)
	if marker == "AGW_IPV4" {
		b.WriteByte('\n')
	}
}

func normalizedRules(policy Policy) ([]Rule, error) {
	if len(policy.Rules) == 0 {
		return nil, fmt.Errorf("%w: at least one rule is required", ErrInvalidPolicy)
	}
	rules := append([]Rule(nil), policy.Rules...)
	for _, rule := range rules {
		if err := validateRule(rule); err != nil {
			return nil, err
		}
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Family != rules[j].Family {
			return familySortRank(rules[i].Family) < familySortRank(rules[j].Family)
		}
		if rules[i].UID != rules[j].UID {
			return rules[i].UID < rules[j].UID
		}
		if rules[i].AnyEgress != rules[j].AnyEgress {
			return !rules[i].AnyEgress
		}
		if rules[i].Destination != rules[j].Destination {
			return rules[i].Destination < rules[j].Destination
		}
		return rules[i].Port < rules[j].Port
	})
	for i := 1; i < len(rules); i++ {
		if rules[i] == rules[i-1] {
			return nil, fmt.Errorf("%w: duplicate rule for UID %d", ErrInvalidPolicy, rules[i].UID)
		}
	}
	return rules, nil
}

func familySortRank(family AddressFamily) int {
	if family == FamilyAny {
		return 3
	}
	return int(family)
}

func validateRule(rule Rule) error {
	if rule.UID == 0 {
		return fmt.Errorf("%w: UID must be non-zero", ErrInvalidPolicy)
	}
	if rule.AnyEgress {
		if rule.UID == UIDAgent {
			return fmt.Errorf("%w: agent UID cannot receive any egress", ErrInvalidPolicy)
		}
		if rule.Family != FamilyAny || rule.Protocol != 0 || rule.Destination != "" || rule.Port != 0 {
			return fmt.Errorf("%w: any-egress rule must not carry destination fields", ErrInvalidPolicy)
		}
		return nil
	}
	if rule.Family != FamilyIPv4 && rule.Family != FamilyIPv6 {
		return fmt.Errorf("%w: exact rule must select IPv4 or IPv6", ErrInvalidPolicy)
	}
	if rule.Protocol != ProtocolTCP || rule.Port == 0 {
		return fmt.Errorf("%w: exact rule must be TCP with a non-zero port", ErrInvalidPolicy)
	}
	if (rule.Family == FamilyIPv4 && rule.Destination != "127.0.0.1") ||
		(rule.Family == FamilyIPv6 && rule.Destination != "::1") {
		return fmt.Errorf("%w: exact rule destination must be the selected loopback address", ErrInvalidPolicy)
	}
	return nil
}
