#!/usr/bin/env bash
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=phase0-common.sh
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-airlock
PHASE0_BASE_IMAGE="${PHASE0_BASE_IMAGE:-docker.io/library/busybox:1.36.1}"
PHASE0_NETWORK_IMAGE="${PHASE0_NETWORK_IMAGE:-nicolaka/netshoot:v0.13}"
PHASE0_LOCKDOWN_IMAGE="${PHASE0_LOCKDOWN_IMAGE:-nicolaka/netshoot:v0.13}"
PHASE0_NODE_SELECTOR_KEY="${PHASE0_NODE_SELECTOR_KEY:-agw.astatide.com/agents}"
PHASE0_NODE_SELECTOR_VALUE="${PHASE0_NODE_SELECTOR_VALUE:-true}"
PHASE0_TOLERATION_KEY="${PHASE0_TOLERATION_KEY:-agw.astatide.com/agents}"
PHASE0_TOLERATION_VALUE="${PHASE0_TOLERATION_VALUE:-true}"
PHASE0_AIRLOCK_POD="${PHASE0_AIRLOCK_POD:-agw-phase0-airlock}"
PHASE0_AIRLOCK_SERVICE="${PHASE0_AIRLOCK_SERVICE:-agw-phase0-airlock}"
PHASE0_DNS_CIDR="${PHASE0_DNS_CIDR:-10.43.0.10/32}"
PHASE0_POD_CIDR_V4="${PHASE0_POD_CIDR_V4:-10.42.0.0/16}"
PHASE0_SERVICE_CIDR_V4="${PHASE0_SERVICE_CIDR_V4:-10.43.0.0/16}"
PHASE0_POD_CIDR_V6="${PHASE0_POD_CIDR_V6:-fd42::/48}"
PHASE0_SERVICE_CIDR_V6="${PHASE0_SERVICE_CIDR_V6:-fd43::/112}"
PHASE0_PUBLIC_IPV4="${PHASE0_PUBLIC_IPV4:-1.1.1.1}"
PHASE0_PUBLIC_IPV6="${PHASE0_PUBLIC_IPV6:-2606:4700:4700::1111}"
PHASE0_METADATA_IPV4="${PHASE0_METADATA_IPV4:-169.254.169.254}"
PHASE0_API_HOST="${PHASE0_API_HOST:-kubernetes.default.svc}"
PHASE0_API_IPV4="${PHASE0_API_IPV4:-10.43.0.1}"
PHASE0_DNS_SERVER="${PHASE0_DNS_SERVER:-10.43.0.10}"
PHASE0_ALLOWED_URL="${PHASE0_ALLOWED_URL:-}"
PHASE0_UDP_TARGET="${PHASE0_UDP_TARGET:-1.1.1.1}"

# Fixed fixture ports. They are deliberately not environment inputs: the
# probe must not turn a user-controlled value into a firewall or shell test.
PHASE0_BROKER_PORT=8081
PHASE0_GATEWAY_PORT=8082
PHASE0_ADMIN_PORT=8083
PHASE0_ALTERNATE_PORT=8080

usage() {
  cat <<'EOF'
Usage: phase0-airlock.sh [common options]

Applies a disposable two-container airlock probe only with --apply --yes. The
lockdown init container runs as UID 0 with namespaced NET_ADMIN; agent is UID
1000 and broker is UID 1337. Agent assertions must fail closed for IPv4,
IPv6, Kubernetes API, metadata, DNS, UDP, loopback, pod-IP, and Service-IP
bypass attempts. The fixture starts listeners on the direct broker, future
gateway, admin, and alternate loopback ports so refusal is not merely an
unbound-port result. Set PHASE0_ALLOWED_URL to prove that the broker can reach
one explicitly approved public HTTPS endpoint.

Environment:
  PHASE0_NETWORK_IMAGE / PHASE0_LOCKDOWN_IMAGE
  PHASE0_ALLOWED_URL       Optional broker-only allowed HTTPS target
  PHASE0_AIRLOCK_SERVICE   Disposable Service name (default: agw-phase0-airlock)
  PHASE0_PUBLIC_IPV4/IPv6  Public connectivity probes
  PHASE0_METADATA_IPV4     Metadata endpoint probe
  PHASE0_API_HOST          Kubernetes API DNS name
  PHASE0_DNS_SERVER        Cluster DNS server
  PHASE0_UDP_TARGET        UDP 443 probe target
  PHASE0_DNS_CIDR, *_CIDR  NetworkPolicy CIDR inputs
EOF
  phase0_usage_common
}

phase0_parse_common_args "$@" || exit 2
for arg in "${PHASE0_REMAINING_ARGS[@]}"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    *) phase0_die "unknown argument: $arg"; exit 2 ;;
  esac
done
for pair in \
  "network-image=$PHASE0_NETWORK_IMAGE" \
  "lockdown-image=$PHASE0_LOCKDOWN_IMAGE" \
  "node-selector-key=$PHASE0_NODE_SELECTOR_KEY" \
  "node-selector-value=$PHASE0_NODE_SELECTOR_VALUE" \
  "toleration-key=$PHASE0_TOLERATION_KEY" \
  "toleration-value=$PHASE0_TOLERATION_VALUE" \
  "airlock-pod=$PHASE0_AIRLOCK_POD" \
  "airlock-service=$PHASE0_AIRLOCK_SERVICE" \
  "dns-cidr=$PHASE0_DNS_CIDR" \
  "pod-cidr-v4=$PHASE0_POD_CIDR_V4" \
  "service-cidr-v4=$PHASE0_SERVICE_CIDR_V4" \
  "pod-cidr-v6=$PHASE0_POD_CIDR_V6" \
  "service-cidr-v6=$PHASE0_SERVICE_CIDR_V6" \
  "public-ipv4=$PHASE0_PUBLIC_IPV4" \
  "public-ipv6=$PHASE0_PUBLIC_IPV6" \
  "metadata-ipv4=$PHASE0_METADATA_IPV4" \
  "api-host=$PHASE0_API_HOST" \
  "api-ipv4=$PHASE0_API_IPV4" \
  "dns-server=$PHASE0_DNS_SERVER" \
  "udp-target=$PHASE0_UDP_TARGET" \
  "allowed-url=$PHASE0_ALLOWED_URL"; do
  phase0_validate_single_line "${pair%%=*}" "${pair#*=}" || exit 2
done
phase0_validate_image_ref network-image "$PHASE0_NETWORK_IMAGE" || exit 2
phase0_validate_image_ref lockdown-image "$PHASE0_LOCKDOWN_IMAGE" || exit 2
phase0_validate_dns_name airlock-pod "$PHASE0_AIRLOCK_POD" || exit 2
phase0_validate_dns_name airlock-service "$PHASE0_AIRLOCK_SERVICE" || exit 2
phase0_validate_dns_name api-host "$PHASE0_API_HOST" || exit 2
allowed_url_pattern="^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._~%+,:@!$'()*;=-]*)?$"
if [[ -n "$PHASE0_ALLOWED_URL" && ! "$PHASE0_ALLOWED_URL" =~ $allowed_url_pattern ]]; then
	phase0_die "PHASE0_ALLOWED_URL must be a single HTTPS origin/path without userinfo, query, fragment, or shell metacharacters"
  exit 2
fi
phase0_init_evidence || exit 2

manifest="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-airlock.XXXXXX.yaml")"
trap 'rm -f -- "$manifest"' EXIT
phase0_render_template \
  "$SCRIPT_DIR/../test/e2e/phase0/30-uid-airlock.yaml" "$manifest" \
  "PHASE0_NAMESPACE=$PHASE0_NAMESPACE" \
  "PHASE0_RUN_ID=$PHASE0_RUN_ID" \
  "PHASE0_AIRLOCK_POD=$PHASE0_AIRLOCK_POD" \
  "PHASE0_NETWORK_IMAGE=$PHASE0_NETWORK_IMAGE" \
  "PHASE0_LOCKDOWN_IMAGE=$PHASE0_LOCKDOWN_IMAGE" \
  "PHASE0_NODE_SELECTOR_KEY=$PHASE0_NODE_SELECTOR_KEY" \
  "PHASE0_NODE_SELECTOR_VALUE=$PHASE0_NODE_SELECTOR_VALUE" \
  "PHASE0_TOLERATION_KEY=$PHASE0_TOLERATION_KEY" \
  "PHASE0_TOLERATION_VALUE=$PHASE0_TOLERATION_VALUE" \
  "PHASE0_AIRLOCK_SERVICE=$PHASE0_AIRLOCK_SERVICE" \
  "PHASE0_DNS_CIDR=$PHASE0_DNS_CIDR" \
  "PHASE0_POD_CIDR_V4=$PHASE0_POD_CIDR_V4" \
  "PHASE0_SERVICE_CIDR_V4=$PHASE0_SERVICE_CIDR_V4" \
  "PHASE0_POD_CIDR_V6=$PHASE0_POD_CIDR_V6" \
  "PHASE0_SERVICE_CIDR_V6=$PHASE0_SERVICE_CIDR_V6"

phase0_manifest_action "$manifest" uid-airlock

if [[ "$PHASE0_MODE" != apply || "$PHASE0_CLEANUP" == 1 ]]; then
  if (( PHASE0_CLEANUP )); then
    phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" uid-airlock
  fi
  phase0_finish
  exit $?
fi

if ! phase0_cluster_ready; then
  phase0_finish
  exit $?
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" wait --for=condition=Ready "pod/$PHASE0_AIRLOCK_POD" --timeout=60s >"$PHASE0_RUN_DIR/ready.txt" 2>&1; then
  phase0_result PASS airlock-pod "agent and broker containers are ready after lockdown"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/ready.txt"
  phase0_result FAIL airlock-pod "pod did not become ready"
fi

lockdown_log="$PHASE0_RUN_DIR/lockdown.log"
if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "pod/$PHASE0_AIRLOCK_POD" -c lockdown >"$lockdown_log" 2>&1; then
  phase0_sanitize_file "$lockdown_log"
  grep -q '^lockdown_uid=0$' "$lockdown_log" && phase0_result PASS lockdown-uid "lockdown ran as UID 0" || phase0_result FAIL lockdown-uid "lockdown did not run as UID 0"
  grep -q '^ipv4_rules=installed$' "$lockdown_log" && phase0_result PASS ipv4-rules "IPv4 owner/drop rules installed" || phase0_result FAIL ipv4-rules "IPv4 owner/drop rules were not confirmed"
  if grep -q '^ipv6_rules=installed$' "$lockdown_log" && \
     grep -q '^agent_ipv4_broker=127.0.0.1:8081/tcp$' "$lockdown_log" && \
     grep -q '^agent_ipv6_broker=::1:8081/tcp$' "$lockdown_log" && \
     grep -q '^agent_loopback_default=drop$' "$lockdown_log" && \
     grep -q '^broker_egress=uid-1337-any$' "$lockdown_log"; then
	phase0_result PASS ipv6-rules "IPv6 owner/drop rules installed"
  else
	phase0_result FAIL ipv6-rules "fixed IPv4/IPv6 owner rules were not confirmed"
  fi
else
  phase0_sanitize_file "$lockdown_log"
  phase0_result FAIL lockdown-log "lockdown init logs were unavailable"
fi

exec_capture() {
  local name="$1" container="$2"
  shift 2
  local output="$PHASE0_RUN_DIR/${name}.txt"
  local rc=0
  if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$PHASE0_AIRLOCK_POD" -c "$container" -- "$@" >"$output" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  phase0_sanitize_file "$output"
  return "$rc"
}

expect_blocked() {
	local check="$1" container="$2"
	shift 2
	if ! exec_capture "$check" "$container" sh -ceu 'if "$@"; then printf "probe=connected\n"; else printf "probe=blocked\n"; fi' probe "$@"; then
		phase0_result FAIL "$check" "the probe command or kubectl exec failed"
	elif grep -q '^probe=blocked$' "$PHASE0_RUN_DIR/${check}.txt"; then
		phase0_result PASS "$check" "connection attempt was blocked"
	else
		phase0_result FAIL "$check" "agent unexpectedly reached the target"
	fi
}

expect_allowed() {
	local check="$1" container="$2"
	shift 2
	if ! exec_capture "$check" "$container" sh -ceu 'if "$@"; then printf "probe=connected\n"; else printf "probe=blocked\n"; fi' probe "$@"; then
		phase0_result FAIL "$check" "the broker probe command or kubectl exec failed"
	elif grep -q '^probe=connected$' "$PHASE0_RUN_DIR/${check}.txt"; then
		phase0_result PASS "$check" "broker reached the explicitly configured target"
	else
		phase0_result FAIL "$check" "broker could not reach the explicitly configured target"
	fi
}

probe_private_ip_set() {
  local label="$1" resource="$2" jsonpath="$3"
  local output="$PHASE0_RUN_DIR/${label}-addresses.txt"
  local -a addresses=()
  local address family_count_v4=0 family_count_v6=0 index=0
  if ! phase0_kubectl -n "$PHASE0_NAMESPACE" get "$resource" -o "jsonpath=$jsonpath" >"$output" 2>&1; then
    phase0_sanitize_file "$output"
    phase0_result FAIL "$label-discovery" "could not discover private target addresses"
    return 0
  fi
  phase0_sanitize_file "$output"
  mapfile -t addresses <"$output"
  for address in "${addresses[@]}"; do
    [[ -z "$address" ]] && continue
    if [[ ! "$address" =~ ^[0-9A-Fa-f:.]+$ ]]; then
      phase0_result FAIL "$label-address" "discovered address contains unsupported characters"
      continue
    fi
    if [[ "$address" == *:* ]]; then
      family_count_v6=$((family_count_v6 + 1))
      expect_blocked "agent-${label}-ipv6-${index}" agent nc -6 -z -v -w 3 "$address" "$PHASE0_BROKER_PORT"
    else
      family_count_v4=$((family_count_v4 + 1))
      expect_blocked "agent-${label}-ipv4-${index}" agent nc -4 -z -v -w 3 "$address" "$PHASE0_BROKER_PORT"
    fi
    index=$((index + 1))
  done
  if (( family_count_v4 == 0 )); then
    phase0_result FAIL "$label-ipv4" "no IPv4 private target address was discovered"
  fi
  if (( family_count_v6 == 0 )); then
    phase0_result SKIP "$label-ipv6" "the selected cluster did not expose an IPv6 private target address"
  fi
}

agent_uid="$(exec_capture agent-uid agent id -u && tr -d '[:space:]' <"$PHASE0_RUN_DIR/agent-uid.txt" || true)"
broker_uid="$(exec_capture broker-uid broker id -u && tr -d '[:space:]' <"$PHASE0_RUN_DIR/broker-uid.txt" || true)"
[[ "$agent_uid" == 1000 ]] && phase0_result PASS agent-uid "agent UID is 1000" || phase0_result FAIL agent-uid "agent UID is not 1000"
[[ "$broker_uid" == 1337 ]] && phase0_result PASS broker-uid "broker UID is 1337" || phase0_result FAIL broker-uid "broker UID is not 1337"

agent_net="$(exec_capture agent-netns agent readlink /proc/1/ns/net && tr -d '[:space:]' <"$PHASE0_RUN_DIR/agent-netns.txt" || true)"
broker_net="$(exec_capture broker-netns broker readlink /proc/1/ns/net && tr -d '[:space:]' <"$PHASE0_RUN_DIR/broker-netns.txt" || true)"
[[ -n "$agent_net" && "$agent_net" == "$broker_net" ]] && phase0_result PASS shared-netns "agent and broker share the expected pod network namespace" || phase0_result FAIL shared-netns "agent and broker network namespace identifiers differ"

for command in curl dig nc; do
  if exec_capture "agent-tool-${command}" agent sh -ceu "command -v ${command} >/dev/null"; then
    phase0_result PASS "agent-tool-${command}" "required probe tool is installed"
  else
    phase0_result FAIL "agent-tool-${command}" "required probe tool is missing"
  fi
done
if exec_capture broker-tool-socat broker sh -ceu 'command -v socat >/dev/null'; then
  phase0_result PASS broker-tool-socat "broker fixture has the fixed listener tool"
else
  phase0_result FAIL broker-tool-socat "broker fixture is missing socat"
fi

# The only direct-mode agent allow is TCP to the broker listener. All other
# loopback ports, including the future gateway/admin/alternate ports, must be
# refused even though the disposable broker container has listeners there.
expect_allowed agent-broker-ipv4 agent nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_BROKER_PORT"
if [[ -n "$PHASE0_PUBLIC_IPV6" ]]; then
  expect_allowed agent-broker-ipv6 agent nc -6 -z -v -w 3 ::1 "$PHASE0_BROKER_PORT"
else
  phase0_result SKIP agent-broker-ipv6 "no IPv6 loopback target configured"
fi
expect_blocked agent-broker-udp-ipv4 agent nc -4 -u -z -v -w 2 127.0.0.1 "$PHASE0_BROKER_PORT"
if [[ -n "$PHASE0_PUBLIC_IPV6" ]]; then
  expect_blocked agent-broker-udp-ipv6 agent nc -6 -u -z -v -w 2 ::1 "$PHASE0_BROKER_PORT"
fi
expect_blocked agent-gateway-ipv4 agent nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_GATEWAY_PORT"
expect_blocked agent-admin-ipv4 agent nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_ADMIN_PORT"
expect_blocked agent-alternate-ipv4 agent nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_ALTERNATE_PORT"
if [[ -n "$PHASE0_PUBLIC_IPV6" ]]; then
  expect_blocked agent-gateway-ipv6 agent nc -6 -z -v -w 3 ::1 "$PHASE0_GATEWAY_PORT"
  expect_blocked agent-admin-ipv6 agent nc -6 -z -v -w 3 ::1 "$PHASE0_ADMIN_PORT"
  expect_blocked agent-alternate-ipv6 agent nc -6 -z -v -w 3 ::1 "$PHASE0_ALTERNATE_PORT"
fi

# Direct mode intentionally leaves UID 1337's provider egress broad. These
# probes document that fact; they are not a claim that provider destinations
# are restricted until the future guard policy is proven separately.
expect_allowed broker-gateway-ipv4 broker nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_GATEWAY_PORT"
expect_allowed broker-admin-ipv4 broker nc -4 -z -v -w 3 127.0.0.1 "$PHASE0_ADMIN_PORT"
if [[ -n "$PHASE0_PUBLIC_IPV6" ]]; then
  expect_allowed broker-gateway-ipv6 broker nc -6 -z -v -w 3 ::1 "$PHASE0_GATEWAY_PORT"
  expect_allowed broker-admin-ipv6 broker nc -6 -z -v -w 3 ::1 "$PHASE0_ADMIN_PORT"
fi

# Resolve actual pod and Service addresses after admission. The values are
# passed as exec arguments, never interpolated into a shell program.
probe_private_ip_set pod-ip "pod/$PHASE0_AIRLOCK_POD" "{range .status.podIPs[*]}{.ip}{\"\\n\"}{end}"
# kubectl's JSONPath implementation does not iterate the scalar-compatible
# Service clusterIPs field reliably on all supported client versions. The
# singular clusterIP is authoritative for a single-stack Service and works
# for both IPv4 and IPv6 clusters.
probe_private_ip_set service-ip "service/$PHASE0_AIRLOCK_SERVICE" "{.spec.clusterIP}{\"\\n\"}"

expect_blocked agent-public-ipv4 agent nc -z -v -w 3 "$PHASE0_PUBLIC_IPV4" 443
if [[ -n "$PHASE0_PUBLIC_IPV6" ]]; then
  expect_blocked agent-public-ipv6 agent nc -6 -z -v -w 3 "$PHASE0_PUBLIC_IPV6" 443
else
  phase0_result SKIP agent-public-ipv6 "no IPv6 target configured"
fi
expect_blocked agent-metadata agent curl -fsS --connect-timeout 3 --max-time 5 "http://$PHASE0_METADATA_IPV4/"
expect_blocked agent-api agent nc -z -v -w 3 "$PHASE0_API_IPV4" 443
expect_blocked agent-dns agent dig +time=2 +tries=1 +short "@$PHASE0_DNS_SERVER" kubernetes.default.svc
expect_blocked agent-udp agent nc -u -z -v -w 2 "$PHASE0_UDP_TARGET" 443

if [[ -n "$PHASE0_ALLOWED_URL" ]]; then
	expect_allowed broker-allowed-https broker env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy curl --noproxy '*' -fsS -o /dev/null --connect-timeout 3 --max-time 5 "$PHASE0_ALLOWED_URL"
else
  phase0_result SKIP broker-allowed-https "set PHASE0_ALLOWED_URL to prove broker egress"
fi

if exec_capture agent-capabilities agent sh -c 'awk "/^CapEff:/ { print \$2 }" /proc/self/status' && [[ "$(tr -d '[:space:]' <"$PHASE0_RUN_DIR/agent-capabilities.txt")" =~ ^0+$ ]]; then
  phase0_result PASS agent-capabilities "agent has no effective Linux capabilities"
else
  phase0_result FAIL agent-capabilities "agent has unexpected effective capabilities"
fi
if exec_capture agent-token agent sh -c 'test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token'; then
  phase0_result PASS agent-service-account "agent cannot see a service-account token"
else
  phase0_result FAIL agent-service-account "agent can see a service-account token"
fi
if exec_capture agent-firewall agent sh -ceu '
  for firewall in iptables ip6tables; do
    if command -v "$firewall" >/dev/null 2>&1; then
      if "$firewall" -L >/dev/null 2>&1 || "$firewall" -A OUTPUT -j ACCEPT >/dev/null 2>&1; then
        printf "firewall=accessible\\n"
        exit 1
      fi
    fi
  done
  printf "firewall=blocked\\n"
  exit 0
'; then
  if grep -q '^firewall=blocked$' "$PHASE0_RUN_DIR/agent-firewall.txt"; then
    phase0_result PASS agent-firewall "agent could neither inspect nor alter IPv4/IPv6 firewall rules"
  else
    phase0_result FAIL agent-firewall "firewall probe returned no blocked marker"
  fi
else
  if grep -q '^firewall=accessible$' "$PHASE0_RUN_DIR/agent-firewall.txt"; then
    phase0_result FAIL agent-firewall "agent could inspect or alter IPv4/IPv6 firewall rules"
  else
    phase0_result FAIL agent-firewall "firewall inspection probe could not execute"
  fi
fi

if (( ! PHASE0_KEEP )); then
  phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" uid-airlock
fi
phase0_finish
