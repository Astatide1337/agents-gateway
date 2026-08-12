#!/usr/bin/env bash
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-credentials
PHASE0_BASE_IMAGE="${PHASE0_BASE_IMAGE:-docker.io/library/busybox:1.36.1}"
PHASE0_NODE_SELECTOR_KEY="${PHASE0_NODE_SELECTOR_KEY:-agw.astatide.com/agents}"
PHASE0_NODE_SELECTOR_VALUE="${PHASE0_NODE_SELECTOR_VALUE:-true}"
PHASE0_TOLERATION_KEY="${PHASE0_TOLERATION_KEY:-agw.astatide.com/agents}"
PHASE0_TOLERATION_VALUE="${PHASE0_TOLERATION_VALUE:-true}"
PHASE0_CANARY_SECRET="${PHASE0_CANARY_SECRET:-agw-phase0-canaries}"
PHASE0_CANARY_POD="${PHASE0_CANARY_POD:-agw-phase0-canary}"

usage() {
  cat <<'EOF'
Usage: phase0-credentials.sh [common options]

Generates synthetic, per-container canaries at runtime. The clone init
container and broker must see only their own canary; the agent must not see
either value in environment, /proc, workspace, or Git configuration. No real
credential is read or embedded. Values are sent to exec probes over stdin and
never placed in evidence or command arguments.

Environment:
  PHASE0_BASE_IMAGE, PHASE0_NODE_SELECTOR_KEY/VALUE
  PHASE0_TOLERATION_KEY/VALUE
  PHASE0_CANARY_SECRET, PHASE0_CANARY_POD
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
  "base-image=$PHASE0_BASE_IMAGE" \
  "node-selector-key=$PHASE0_NODE_SELECTOR_KEY" \
  "node-selector-value=$PHASE0_NODE_SELECTOR_VALUE" \
  "toleration-key=$PHASE0_TOLERATION_KEY" \
  "toleration-value=$PHASE0_TOLERATION_VALUE" \
  "canary-secret=$PHASE0_CANARY_SECRET" \
  "canary-pod=$PHASE0_CANARY_POD"; do
  phase0_validate_single_line "${pair%%=*}" "${pair#*=}" || exit 2
done
phase0_validate_image_ref base-image "$PHASE0_BASE_IMAGE" || exit 2
phase0_validate_dns_name canary-secret "$PHASE0_CANARY_SECRET" || exit 2
phase0_validate_dns_name canary-pod "$PHASE0_CANARY_POD" || exit 2
phase0_init_evidence || exit 2

if ! phase0_require_cmd openssl; then
  phase0_finish
  exit $?
fi
clone_canary="agw-clone-$(openssl rand -hex 24)"
broker_canary="agw-broker-$(openssl rand -hex 24)"
phase0_validate_single_line clone-canary "$clone_canary" || exit 2
phase0_validate_single_line broker-canary "$broker_canary" || exit 2

manifest="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-credentials.XXXXXX.yaml")"
trap 'rm -f -- "$manifest"' EXIT
phase0_render_template \
  "$SCRIPT_DIR/../test/e2e/phase0/50-credential-canaries.yaml" "$manifest" \
  "PHASE0_NAMESPACE=$PHASE0_NAMESPACE" \
  "PHASE0_RUN_ID=$PHASE0_RUN_ID" \
  "PHASE0_CANARY_SECRET=$PHASE0_CANARY_SECRET" \
  "PHASE0_CANARY_POD=$PHASE0_CANARY_POD" \
  "PHASE0_CLONE_CANARY=$clone_canary" \
  "PHASE0_BROKER_CANARY=$broker_canary" \
  "PHASE0_BASE_IMAGE=$PHASE0_BASE_IMAGE" \
  "PHASE0_NODE_SELECTOR_KEY=$PHASE0_NODE_SELECTOR_KEY" \
  "PHASE0_NODE_SELECTOR_VALUE=$PHASE0_NODE_SELECTOR_VALUE" \
  "PHASE0_TOLERATION_KEY=$PHASE0_TOLERATION_KEY" \
  "PHASE0_TOLERATION_VALUE=$PHASE0_TOLERATION_VALUE"

phase0_manifest_action "$manifest" credential-canaries
phase0_sanitize_file "$PHASE0_RUN_DIR/credential-canaries.yaml"

if [[ "$PHASE0_MODE" != apply || "$PHASE0_CLEANUP" == 1 ]]; then
  if (( PHASE0_CLEANUP )); then
    phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" credential-canaries
  fi
  phase0_finish
  exit $?
fi
if ! phase0_cluster_ready; then
  phase0_finish
  exit $?
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" wait --for=condition=Ready "pod/$PHASE0_CANARY_POD" --timeout=60s >"$PHASE0_RUN_DIR/ready.txt" 2>&1; then
  phase0_result PASS canary-pod "credential isolation pod is ready"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/ready.txt"
  phase0_result FAIL canary-pod "credential isolation pod did not become ready"
fi

clone_log="$PHASE0_RUN_DIR/clone-init.log"
if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "pod/$PHASE0_CANARY_POD" -c clone-setup >"$clone_log" 2>&1 && grep -q '^clone_setup_received=true$' "$clone_log"; then
  phase0_sanitize_file "$clone_log"
  phase0_result PASS clone-container "clone init container received its scoped canary"
else
  phase0_sanitize_file "$clone_log"
  phase0_result FAIL clone-container "clone init container did not complete with its scoped canary"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$PHASE0_CANARY_POD" -c broker -- sh -ceu 'test -n "$BROKER_TOKEN"' >"$PHASE0_RUN_DIR/broker-canary.txt" 2>&1; then
  phase0_result PASS broker-container "broker received its scoped canary"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/broker-canary.txt"
  phase0_result FAIL broker-container "broker did not receive its scoped canary"
fi

agent_exec_stdin() {
  local label="$1"
  shift
  local output="$PHASE0_RUN_DIR/${label}.txt"
  local rc=0
  if printf '%s\n' "$clone_canary" | phase0_kubectl -n "$PHASE0_NAMESPACE" exec -i "pod/$PHASE0_CANARY_POD" -c agent -- "$@" >"$output" 2>&1; then
    rc=0
  else
    rc=$?
  fi
  phase0_sanitize_file "$output"
  return "$rc"
}

if agent_exec_stdin agent-no-canary sh -ceu 'read -r needle; ! printf "%s\n" "$needle" | grep -R -a -q -F -f - /workspace /run /proc/*/environ /proc/*/cmdline 2>/dev/null'; then
  phase0_result PASS agent-no-clone-canary "agent could not find the clone canary in visible state"
else
  phase0_result FAIL agent-no-clone-canary "agent-visible state contained the clone canary"
fi

if printf '%s\n' "$broker_canary" | phase0_kubectl -n "$PHASE0_NAMESPACE" exec -i "pod/$PHASE0_CANARY_POD" -c agent -- sh -ceu 'read -r needle; ! printf "%s\n" "$needle" | grep -R -a -q -F -f - /workspace /run /proc/*/environ /proc/*/cmdline 2>/dev/null' >"$PHASE0_RUN_DIR/agent-no-broker-canary.txt" 2>&1; then
  phase0_sanitize_file "$PHASE0_RUN_DIR/agent-no-broker-canary.txt"
  phase0_result PASS agent-no-broker-canary "agent could not find the broker canary in visible state"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/agent-no-broker-canary.txt"
  phase0_result FAIL agent-no-broker-canary "agent-visible state contained the broker canary"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$PHASE0_CANARY_POD" -c agent -- sh -ceu 'test -z "${CLONE_TOKEN:-}" && test -z "${BROKER_TOKEN:-}" && ! env | grep -E "^(CLONE_TOKEN|BROKER_TOKEN)="' >"$PHASE0_RUN_DIR/agent-env.txt" 2>&1; then
  phase0_result PASS agent-environment "agent environment has no scoped credential variables"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/agent-env.txt"
  phase0_result FAIL agent-environment "agent environment exposed a scoped credential variable"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$PHASE0_CANARY_POD" -c agent -- sh -ceu 'test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token && test ! -e /run/secrets/broker-token && test ! -e /run/secrets/clone-token' >"$PHASE0_RUN_DIR/agent-mounts.txt" 2>&1; then
  phase0_result PASS agent-mounts "agent has no Secret or service-account token mount"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/agent-mounts.txt"
  phase0_result FAIL agent-mounts "agent can see a Secret or service-account token mount"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$PHASE0_CANARY_POD" -c agent -- sh -ceu 'test ! -s /workspace/.git/config || ! grep -Eiq "(token|password|secret|authorization|api[_-]?key)" /workspace/.git/config' >"$PHASE0_RUN_DIR/git-config.txt" 2>&1; then
  phase0_result PASS git-config "workspace Git configuration contains no credential-shaped value"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/git-config.txt"
  phase0_result FAIL git-config "workspace Git configuration contains a credential-shaped value"
fi

if (( ! PHASE0_KEEP )); then
  phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" credential-canaries
fi
phase0_finish
