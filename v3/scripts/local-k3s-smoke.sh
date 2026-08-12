#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Disposable, Docker-contained two-node k3s smoke harness.
#
# An apply run creates one uniquely named Docker network, one k3s server, one
# k3s agent, and one temporary kubeconfig. The EXIT trap removes only those
# resources after the smoke (or an interrupted run) finishes.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

DOCKER_BIN=${AGW_DOCKER_BIN:-docker}
KUBECTL_BIN=${AGW_KUBECTL_BIN:-kubectl}
K3S_IMAGE=${AGW_K3S_IMAGE:-rancher/k3s:v1.35.0-k3s1}
TIMEOUT_SECONDS=${AGW_K3S_TIMEOUT_SECONDS:-180}
RUN_PHASE0=0
INSTALL_UPSTREAM=0
RUN_AGENTGATEWAY_CHAIN=0
MODE=plan
YES=0

RUN_ID=""
NETWORK_NAME=""
SERVER_NAME=""
AGENT_NAME=""
CONTEXT_NAME=""
API_PORT=""
WORK_DIR=""
KUBECONFIG_FILE=""
AGENT_NODE=""

RESOURCES_RESERVED=0
CLEANUP_FAILED=0
PHASE0_INCONCLUSIVE=0

usage() {
  cat <<'EOF'
Usage: local-k3s-smoke.sh [--plan | --apply --yes] [options]

Create a bounded, disposable two-node k3s cluster inside Docker, verify the
API, node readiness, the Agents Gateway worker label, and its NoSchedule taint,
then remove the exact containers/network/temp kubeconfig before exiting.

Actions:
  --plan                 Print the bounded plan; performs no Docker/Kubernetes action (default).
  --apply --yes          Create and test the disposable cluster. --yes is mandatory.

Options:
  --image IMAGE          k3s image (default: rancher/k3s:v1.35.0-k3s1).
  --timeout SECONDS      Readiness timeout, 30..600 (default: 180).
  --phase0               Run the repository's Phase-0 wrapper inside this
                         disposable cluster before cleanup.
  --install-upstream     With --phase0, request the pinned Agent Sandbox
                         install for its Sandbox probe; default: disabled.
                         Requires AGENT_SANDBOX_MANIFEST_SHA256.
  --agentgateway-chain   Run the optional provider-free guard -> reviewed
                         agentgateway -> recording chain in this cluster.
                         Builds only local fixtures and removes them on exit.
  -h, --help             Show this help.

The API is published on a dynamically allocated 127.0.0.1 port only. No
Coolify port is selected or changed. The cluster is never retained by this
command; use --phase0 for probes that must run before the trap tears it down.
Exit 3 means the required smoke checks passed but Phase-0 had explicit optional
skips (for example, no object-store endpoint was supplied); exit 1/2 means a
required check or harness safety condition failed.
EOF
}

die() {
  printf 'local-k3s-smoke: ERROR: %s\n' "$*" >&2
  exit 2
}

warn() {
  printf 'local-k3s-smoke: WARN: %s\n' "$*" >&2
}

validate_single_line() {
  local label=$1 value=$2
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "$label must not contain a newline"
}

validate_image() {
  local value=$1
  validate_single_line image "$value"
  [[ -n "$value" && "$value" != -* ]] || die 'image is empty or starts with a dash'
  [[ "$value" != *[[:space:]]* ]] || die 'image must not contain whitespace'
}

validate_timeout() {
  [[ "$TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || die 'timeout must be an integer'
  (( TIMEOUT_SECONDS >= 30 && TIMEOUT_SECONDS <= 600 )) || die 'timeout must be between 30 and 600 seconds'
}

parse_args() {
  local action_count=0 arg
  while (($#)); do
    arg=$1
    case "$arg" in
      --plan)
        MODE=plan
        action_count=$((action_count + 1))
        shift
        ;;
      --apply)
        MODE=apply
        action_count=$((action_count + 1))
        shift
        ;;
      --yes)
        YES=1
        shift
        ;;
      --phase0)
        RUN_PHASE0=1
        shift
        ;;
      --install-upstream)
        INSTALL_UPSTREAM=1
        shift
        ;;
      --agentgateway-chain)
        RUN_AGENTGATEWAY_CHAIN=1
        shift
        ;;
      --image)
        (($# >= 2)) || die '--image requires a value'
        K3S_IMAGE=$2
        shift 2
        ;;
      --timeout)
        (($# >= 2)) || die '--timeout requires a value'
        TIMEOUT_SECONDS=$2
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      --)
        shift
        (($# == 0)) || die "unexpected arguments after --: $*"
        ;;
      *)
        die "unknown argument: $arg"
        ;;
    esac
  done
  (( action_count <= 1 )) || die 'choose exactly one of --plan or --apply'
  if [[ "$MODE" == apply ]]; then
    (( YES == 1 )) || die '--apply is destructive to disposable Docker resources; add --yes explicitly'
  elif (( YES == 1 )); then
    die '--yes is only valid with --apply'
  fi
  validate_image "$K3S_IMAGE"
  validate_timeout
  if (( RUN_PHASE0 == 1 )) && [[ "$MODE" != apply ]]; then
    die '--phase0 requires --apply --yes'
  fi
  if (( INSTALL_UPSTREAM == 1 && RUN_PHASE0 == 0 )); then
    die '--install-upstream requires --phase0'
  fi
  if (( RUN_AGENTGATEWAY_CHAIN == 1 && RUN_PHASE0 == 1 )); then
    die '--agentgateway-chain cannot be combined with --phase0; run the bounded probes separately'
  fi
  if (( RUN_AGENTGATEWAY_CHAIN == 1 && RUN_PHASE0 == 0 && INSTALL_UPSTREAM == 1 )); then
    die '--install-upstream requires --phase0'
  fi
}

make_identity() {
  local stamp random_part
  stamp=$(date -u +%Y%m%d%H%M%S)
  random_part=${RANDOM}${RANDOM}
  RUN_ID="agw-k3s-${stamp}-${BASHPID:-$$}-${random_part}"
  NETWORK_NAME="${RUN_ID}-net"
  SERVER_NAME="${RUN_ID}-server"
  AGENT_NAME="${RUN_ID}-agent"
  CONTEXT_NAME="${RUN_ID}"
}

validate_identity() {
  local value label
  for label in run-id network server agent context; do
    case "$label" in
      run-id) value=$RUN_ID ;;
      network) value=$NETWORK_NAME ;;
      server) value=$SERVER_NAME ;;
      agent) value=$AGENT_NAME ;;
      context) value=$CONTEXT_NAME ;;
    esac
    [[ "$value" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || die "generated $label is not DNS-safe: $value"
  done
}

run_docker() {
  "$DOCKER_BIN" "$@"
}

run_kubectl() {
  "$KUBECTL_BIN" --kubeconfig "$KUBECONFIG_FILE" --context "$CONTEXT_NAME" "$@"
}

run_kubectl_config() {
  "$KUBECTL_BIN" --kubeconfig "$KUBECONFIG_FILE" "$@"
}

docker_command_available() {
  command -v -- "$DOCKER_BIN" >/dev/null 2>&1
}

kubectl_command_available() {
  command -v -- "$KUBECTL_BIN" >/dev/null 2>&1
}

check_host_capacity() {
  local available_mib free_mib cpus
  available_mib=$(awk '/^MemAvailable:/ { printf "%d", $2 / 1024; exit }' /proc/meminfo)
  free_mib=$(df -Pm "$WORK_DIR" | awk 'NR == 2 { print $4; exit }')
  cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf '0')
  (( cpus >= 2 )) || die "at least two host CPUs are required (found $cpus)"
  (( available_mib >= 3000 )) || die "only ${available_mib}MiB RAM is available; refusing a 2.5GiB bounded cluster"
  (( free_mib >= 2048 )) || die "only ${free_mib}MiB disk is available; refusing the disposable cluster"
  printf 'host capacity: cpus=%s mem_available_mib=%s disk_free_mib=%s\n' "$cpus" "$available_mib" "$free_mib"
}

container_ids_by_exact_name() {
  local name=$1
  run_docker ps -aq --filter "name=^/${name}$"
}

network_exists_by_exact_name() {
  run_docker network ls -q --filter "name=^${NETWORK_NAME}$"
}

assert_names_free() {
  local ids
  ids=$(container_ids_by_exact_name "$SERVER_NAME")
  [[ -z "$ids" ]] || die "generated server container name already exists: $SERVER_NAME"
  ids=$(container_ids_by_exact_name "$AGENT_NAME")
  [[ -z "$ids" ]] || die "generated agent container name already exists: $AGENT_NAME"
  ids=$(network_exists_by_exact_name)
  [[ -z "$ids" ]] || die "generated network name already exists: $NETWORK_NAME"
}

label_value_for_container() {
  local container=$1 key=$2
  run_docker inspect -f "{{ index .Config.Labels \"$key\" }}" "$container" 2>/dev/null || true
}

label_value_for_network() {
  local network=$1 key=$2
  run_docker network inspect -f "{{ index .Labels \"$key\" }}" "$network" 2>/dev/null || true
}

remove_owned_container() {
  local name=$1 id owner role
  id=$(container_ids_by_exact_name "$name" | head -n 1)
  [[ -n "$id" ]] || return 0
  owner=$(label_value_for_container "$id" 'com.astatide.agw.run-id')
  role=$(label_value_for_container "$id" 'com.astatide.agw.component')
  if [[ "$owner" != "$RUN_ID" || "$role" != 'local-k3s-smoke' ]]; then
    warn "refusing to remove container $name: ownership labels do not match this run"
    CLEANUP_FAILED=1
    return 1
  fi
  if ! run_docker rm -f "$id" >/dev/null; then
    warn "could not remove owned container $name ($id)"
    CLEANUP_FAILED=1
    return 1
  fi
  printf 'removed container: %s\n' "$name"
}

remove_owned_network() {
  local id owner role
  id=$(network_exists_by_exact_name | head -n 1)
  [[ -n "$id" ]] || return 0
  owner=$(label_value_for_network "$NETWORK_NAME" 'com.astatide.agw.run-id')
  role=$(label_value_for_network "$NETWORK_NAME" 'com.astatide.agw.component')
  if [[ "$owner" != "$RUN_ID" || "$role" != 'local-k3s-smoke' ]]; then
    warn "refusing to remove network $NETWORK_NAME: ownership labels do not match this run"
    CLEANUP_FAILED=1
    return 1
  fi
  if ! run_docker network rm "$id" >/dev/null; then
    warn "could not remove owned network $NETWORK_NAME ($id)"
    CLEANUP_FAILED=1
    return 1
  fi
  printf 'removed network: %s\n' "$NETWORK_NAME"
}

remove_temp_dir() {
  [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]] || return 0
  local tmp_root=${TMPDIR:-/tmp}
  if [[ "$WORK_DIR" != "$tmp_root"/agw-k3s-smoke.* ]]; then
    warn "refusing to remove unexpected temp path: $WORK_DIR"
    CLEANUP_FAILED=1
    return 1
  fi
  if ! find -- "$WORK_DIR" -depth -delete; then
    warn "could not remove temp work directory: $WORK_DIR"
    CLEANUP_FAILED=1
    return 1
  fi
  printf 'removed temp directory: %s\n' "$WORK_DIR"
}

cleanup() {
  local original_status=$?
  trap - EXIT INT TERM
  if (( RESOURCES_RESERVED == 1 )); then
    remove_owned_container "$AGENT_NAME" || true
    remove_owned_container "$SERVER_NAME" || true
    remove_owned_network || true
  fi
  remove_temp_dir || true
  if (( original_status == 0 && CLEANUP_FAILED == 1 )); then
    original_status=1
  fi
  if (( original_status != 0 )); then
    printf 'local-k3s-smoke: cleanup finished with original exit=%d\n' "$original_status" >&2
  else
    printf 'local-k3s-smoke: cleanup complete\n'
  fi
  return "$original_status"
}

on_signal() {
  exit 130
}

wait_until() {
  local description=$1
  shift
  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  local last_output=''
  while (( SECONDS < deadline )); do
    if last_output=$("$@" 2>&1); then
      printf '%s: ready\n' "$description"
      return 0
    fi
    sleep 2
  done
  printf '%s: timed out after %ss\n' "$description" "$TIMEOUT_SECONDS" >&2
  [[ -n "$last_output" ]] && printf '%s\n' "$last_output" >&2
  return 1
}

server_api_ready() {
  local running
  running=$(run_docker inspect -f '{{.State.Running}}' "$SERVER_NAME" 2>/dev/null || true)
  [[ "$running" == true ]] || {
    run_docker logs --tail 40 "$SERVER_NAME" >&2 || true
    return 1
  }
  run_docker exec "$SERVER_NAME" /bin/kubectl get --raw=/readyz >/dev/null
}

read_server_kubeconfig() {
  local raw_file=$WORK_DIR/k3s.yaml
  local port_mapping
  port_mapping=$(run_docker port "$SERVER_NAME" 6443/tcp)
  API_PORT=${port_mapping##*:}
  [[ "$API_PORT" =~ ^[0-9]+$ && API_PORT -ge 1 && API_PORT -le 65535 ]] || die "could not determine the dynamically published API port: $port_mapping"
  run_docker exec "$SERVER_NAME" cat /etc/rancher/k3s/k3s.yaml >"$raw_file"
  [[ -s "$raw_file" ]] || die 'k3s did not produce a kubeconfig'
  sed -E "s#https://127\\.0\\.0\\.1:6443#https://127.0.0.1:${API_PORT}#g" "$raw_file" >"$KUBECONFIG_FILE"
  chmod 600 "$KUBECONFIG_FILE"
  grep -Fq "server: https://127.0.0.1:${API_PORT}" "$KUBECONFIG_FILE" || die 'generated kubeconfig did not contain the loopback API endpoint'
  run_kubectl_config config rename-context default "$CONTEXT_NAME" >/dev/null 2>&1 || die 'could not give the disposable kubeconfig a unique context'
  run_kubectl_config config use-context "$CONTEXT_NAME" >/dev/null
}

verify_cluster() {
  local node_count agent_nodes agent_node taints ready_count
  run_kubectl get --raw=/readyz >/dev/null
  run_kubectl get nodes -o wide >"$WORK_DIR/nodes.txt"
  node_count=$(run_kubectl get nodes --no-headers | awk 'NF { count++ } END { print count + 0 }')
  (( node_count == 2 )) || die "expected exactly two k3s nodes, found $node_count"
  run_kubectl wait --for=condition=Ready nodes --all --timeout="${TIMEOUT_SECONDS}s" >/dev/null
  ready_count=$(run_kubectl get nodes --no-headers | awk '$2 == "Ready" { count++ } END { print count + 0 }')
  (( ready_count == 2 )) || die "expected two Ready nodes, found $ready_count"

  agent_nodes=$(run_kubectl get nodes -l 'agw.astatide.com/agents=true' -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
  agent_node=$(printf '%s\n' "$agent_nodes" | awk 'NF { count++; value=$0 } END { if (count == 1) print value; else exit 1 }') || die 'expected exactly one node with agw.astatide.com/agents=true'
  AGENT_NODE="$agent_node"
  taints=$(run_kubectl get node "$agent_node" -o jsonpath='{range .spec.taints[*]}{.key}={.value}:{.effect}{"\n"}{end}')
  printf '%s\n' "$taints" | grep -Fxq 'agw.astatide.com/agents=true:NoSchedule' || die "agent node $agent_node is missing the required NoSchedule taint"
  printf 'run_id\t%s\napi_port\t%s\ncontext\t%s\nnode_count\t%s\nready_count\t%s\nagent_node\t%s\nagent_label\tagw.astatide.com/agents=true\nagent_taint\tagw.astatide.com/agents=true:NoSchedule\n' \
    "$RUN_ID" "$API_PORT" "$CONTEXT_NAME" "$node_count" "$ready_count" "$agent_node" >"$WORK_DIR/verification.tsv"
  printf 'verified: api=ready nodes=%s/%s agent=%s label=true taint=NoSchedule api_port=127.0.0.1:%s\n' \
    "$ready_count" "$node_count" "$agent_node" "$API_PORT"
}

run_agentgateway_chain() {
  local chain_script="$SCRIPT_DIR/../phase0/agentgateway-guard/scripts/local-chain-check.sh"
  [[ -x "$chain_script" ]] || die "agentgateway chain runner is missing or not executable: $chain_script"
  printf 'running optional provider-free agentgateway chain on worker=%s\n' "$AGENT_NODE"
  AGW_PHASE0_LOCAL_LIVE=1 \
  AGW_PHASE0_LIVE=1 \
  AGW_PHASE0_APPLY=1 \
  AGW_PHASE0_CONTEXT="$CONTEXT_NAME" \
  AGW_PHASE0_KUBECONFIG="$KUBECONFIG_FILE" \
  AGW_PHASE0_KUBECTL_BIN="$KUBECTL_BIN" \
  AGW_PHASE0_DOCKER_BIN="$DOCKER_BIN" \
  AGW_PHASE0_K3S_WORKER_CONTAINER="$AGENT_NAME" \
  AGW_PHASE0_WORKER_NODE="$AGENT_NODE" \
  AGW_PHASE0_RUN_ID="$RUN_ID" \
  bash "$chain_script"
}

two_nodes_present() {
  local node_count
  node_count=$(run_kubectl get nodes --no-headers | awk 'NF { count++ } END { print count + 0 }')
  [[ "$node_count" == 2 ]]
}

run_phase0_wrapper() {
  local phase0_script phase0_namespace phase0_run_id phase0_output status
  local -a phase0_args
  phase0_script="$SCRIPT_DIR/phase0-run.sh"
  [[ -f "$phase0_script" ]] || die "Phase-0 wrapper is missing: $phase0_script"
  phase0_namespace="agw-p0-${RUN_ID#agw-k3s-}"
  phase0_run_id="phase0-${RUN_ID#agw-k3s-}"
  phase0_output="$WORK_DIR/phase0-evidence"
  printf 'running Phase-0 wrapper in namespace=%s evidence=%s\n' "$phase0_namespace" "$phase0_output"
  phase0_args=(
    --apply --yes
    --context "$CONTEXT_NAME"
    --namespace "$phase0_namespace"
    --output-dir "$phase0_output"
    --run-id "$phase0_run_id"
  )
  if (( INSTALL_UPSTREAM == 1 )); then
    phase0_args+=(--install-upstream)
  fi
  set +e
  KUBECONFIG="$KUBECONFIG_FILE" bash "$phase0_script" "${phase0_args[@]}"
  status=$?
  set -e
  if (( status == 3 )); then
    warn 'Phase-0 completed with explicitly skipped optional checks; the disposable cluster will still be cleaned up'
    PHASE0_INCONCLUSIVE=1
  elif (( status != 0 )); then
    die "Phase-0 wrapper returned $status; the disposable cluster will still be cleaned up"
  fi
}

print_plan() {
  cat <<EOF
local-k3s-smoke plan
  image:       $K3S_IMAGE
  run id:      $RUN_ID
  network:     $NETWORK_NAME
  server:      $SERVER_NAME (1.5GiB, 1 CPU, 127.0.0.1:random -> 6443/tcp)
  agent:       $AGENT_NAME (1GiB, 0.75 CPU, no host port)
  worker:      label agw.astatide.com/agents=true
  worker:      taint agw.astatide.com/agents=true:NoSchedule
  timeout:     ${TIMEOUT_SECONDS}s
  phase0:      $([[ $RUN_PHASE0 == 1 ]] && printf enabled || printf disabled)
  phase0 upstream install: $([[ $INSTALL_UPSTREAM == 1 ]] && printf enabled || printf disabled)
  agentgateway chain: $([[ $RUN_AGENTGATEWAY_CHAIN == 1 ]] && printf enabled || printf disabled)
  cleanup:     exact containers, network, and temp kubeconfig via EXIT trap

No Docker or Kubernetes mutation was performed. Apply requires --apply --yes.
EOF
}

apply_cluster() {
  local server_token server_id agent_id
  docker_command_available || die "Docker binary is unavailable: $DOCKER_BIN"
  kubectl_command_available || die "kubectl binary is unavailable: $KUBECTL_BIN"
  WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/agw-k3s-smoke.XXXXXX")
  KUBECONFIG_FILE="$WORK_DIR/kubeconfig"
  trap cleanup EXIT
  trap on_signal INT TERM
  check_host_capacity
  assert_names_free
  RESOURCES_RESERVED=1

  if ! run_docker image inspect "$K3S_IMAGE" >/dev/null 2>&1; then
    printf 'pulling missing k3s image: %s\n' "$K3S_IMAGE"
    run_docker pull "$K3S_IMAGE" >/dev/null
  else
    printf 'using cached k3s image: %s\n' "$K3S_IMAGE"
  fi

  run_docker network create \
    --driver bridge \
    --label "com.astatide.agw.component=local-k3s-smoke" \
    --label "com.astatide.agw.run-id=$RUN_ID" \
    "$NETWORK_NAME" >/dev/null
  printf 'created network: %s\n' "$NETWORK_NAME"

  server_id=$(run_docker run -d \
    --name "$SERVER_NAME" \
    --hostname "$SERVER_NAME" \
    --network "$NETWORK_NAME" \
    --network-alias "$SERVER_NAME" \
    --publish 127.0.0.1::6443/tcp \
    --privileged \
    --cgroupns host \
    --tmpfs /run \
    --tmpfs /var/run \
    --cpus 1.0 \
    --memory 1536m \
    --memory-swap 1536m \
    --label "com.astatide.agw.component=local-k3s-smoke" \
    --label "com.astatide.agw.run-id=$RUN_ID" \
    --label 'com.astatide.agw.role=server' \
    "$K3S_IMAGE" server \
      --disable=traefik \
      --disable=servicelb \
      --tls-san "$SERVER_NAME" \
      --tls-san 127.0.0.1 \
      --write-kubeconfig-mode=600)
  [[ -n "$server_id" ]] || die 'Docker did not return a server container ID'
  printf 'created server: %s\n' "$SERVER_NAME"
  wait_until 'k3s server API' server_api_ready
  read_server_kubeconfig
  wait_until 'host-published k3s API' run_kubectl get --raw=/version >/dev/null

  server_token=$(run_docker exec "$SERVER_NAME" cat /var/lib/rancher/k3s/server/node-token | tr -d '\r\n')
  [[ -n "$server_token" && "$server_token" != *[[:space:]]* ]] || die 'k3s server token was empty or malformed'
  agent_id=$(run_docker run -d \
    --name "$AGENT_NAME" \
    --hostname "$AGENT_NAME" \
    --network "$NETWORK_NAME" \
    --network-alias "$AGENT_NAME" \
    --privileged \
    --cgroupns host \
    --tmpfs /run \
    --tmpfs /var/run \
    --cpus 0.75 \
    --memory 1024m \
    --memory-swap 1024m \
    --label "com.astatide.agw.component=local-k3s-smoke" \
    --label "com.astatide.agw.run-id=$RUN_ID" \
    --label 'com.astatide.agw.role=agent' \
    "$K3S_IMAGE" agent \
      --server "https://${SERVER_NAME}:6443" \
      --token "$server_token" \
      --node-label agw.astatide.com/agents=true \
      --node-taint agw.astatide.com/agents=true:NoSchedule)
  [[ -n "$agent_id" ]] || die 'Docker did not return an agent container ID'
  printf 'created agent: %s\n' "$AGENT_NAME"
  wait_until 'host-published k3s API readiness' run_kubectl get --raw=/readyz
  wait_until 'two-node k3s membership' two_nodes_present
  verify_cluster
  if (( RUN_PHASE0 == 1 )); then
    run_phase0_wrapper
  fi
  if (( RUN_AGENTGATEWAY_CHAIN == 1 )); then
    run_agentgateway_chain
  fi
  if (( PHASE0_INCONCLUSIVE == 1 )); then
    printf 'local-k3s-smoke: required checks passed; Phase-0 result is inconclusive because optional inputs/checks were skipped\n' >&2
    return 3
  fi
}

main() {
  parse_args "$@"
  make_identity
  validate_identity
  if [[ "$MODE" == plan ]]; then
    print_plan
    return 0
  fi
  apply_cluster
  printf 'local-k3s-smoke: smoke passed; leaving apply function for EXIT cleanup\n'
}

main "$@"
