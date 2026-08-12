#!/usr/bin/env bash
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=phase0-common.sh
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-inventory
PHASE0_INVENTORY_IMAGE="${PHASE0_INVENTORY_IMAGE:-docker.io/library/alpine:3.20.3}"
PHASE0_KUBELET_ROOT="${PHASE0_KUBELET_ROOT:-/var/lib/rancher/k3s/agent/kubelet}"

usage() {
  cat <<'EOF'
Usage: phase0-inventory.sh [common options]

Runs a read-only Kubernetes/node runtime inventory probe. In --apply mode the
probe creates a privileged, host-root read-only DaemonSet in the selected
disposable namespace; this is intentionally never automatic.

Environment:
  PHASE0_INVENTORY_IMAGE  Probe image containing sh/stat/chroot (default: alpine:3.20.3)
  PHASE0_KUBELET_ROOT      Actual k3s kubelet root (default: /var/lib/rancher/k3s/agent/kubelet)
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
phase0_validate_image_ref inventory-image "$PHASE0_INVENTORY_IMAGE" || exit 2
phase0_validate_single_line kubelet-root "$PHASE0_KUBELET_ROOT" || exit 2
if [[ ! "$PHASE0_KUBELET_ROOT" =~ ^/[A-Za-z0-9._/-]+$ || "$PHASE0_KUBELET_ROOT" == *".."* || "$PHASE0_KUBELET_ROOT" == *"//"* ]]; then
  phase0_die "PHASE0_KUBELET_ROOT must be a normalized absolute host path"
  exit 2
fi
phase0_init_evidence || exit 2

manifest="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-inventory.XXXXXX.yaml")"
trap 'rm -f -- "$manifest"' EXIT
phase0_render_template \
  "$SCRIPT_DIR/../test/e2e/phase0/10-node-inventory.yaml" "$manifest" \
  "PHASE0_NAMESPACE=$PHASE0_NAMESPACE" \
  "PHASE0_RUN_ID=$PHASE0_RUN_ID" \
  "PHASE0_INVENTORY_IMAGE=$PHASE0_INVENTORY_IMAGE" \
  "PHASE0_KUBELET_ROOT=$PHASE0_KUBELET_ROOT"

phase0_manifest_action "$manifest" node-inventory

if [[ "$PHASE0_MODE" != apply || "$PHASE0_CLEANUP" == 1 ]]; then
  if (( PHASE0_CLEANUP )); then
    phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" node-inventory
  fi
  phase0_finish
  exit $?
fi

if ! phase0_cluster_ready; then
  phase0_finish
  exit $?
fi

pods=""
desired=0
pod_count=0
deadline=$((SECONDS + 60))
while (( SECONDS < deadline )); do
  desired="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get daemonset/agw-phase0-node-inventory -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || printf '0')"
  pods="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pods -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID,agw.astatide.com/phase0-check=node-inventory" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)"
  pod_count="$(printf '%s\n' "$pods" | awk 'NF { count++ } END { print count + 0 }')"
  if [[ "$desired" =~ ^[0-9]+$ ]] && (( desired > 0 && pod_count >= desired )); then
    phase0_result PASS node-inventory-scheduled "one completed probe pod was scheduled on each node"
    break
  fi
  sleep 2
done
if [[ ! "$desired" =~ ^[0-9]+$ ]] || (( desired == 0 )); then
  phase0_result FAIL node-inventory-scheduled "DaemonSet did not report a desired node count"
elif (( pod_count < desired )); then
  phase0_result FAIL node-inventory-scheduled "only ${pod_count} of ${desired} node probe pods were created"
fi

# DaemonSets require restartPolicy=Always, so a probe container that emits its
# completion marker and exits will remain in a Running/restarting Pod rather
# than reaching a terminal phase. Poll the marker in each Pod's current logs;
# this proves the probe ran without inventing an impossible terminal-state
# requirement for a DaemonSet.
logs_ready=0
logs_deadline=$((SECONDS + 60))
while (( SECONDS < logs_deadline )); do
  logs_ready=1
  pod_index=0
  while IFS= read -r pod; do
    [[ -n "$pod" ]] || continue
    pod_index=$((pod_index + 1))
    log_file="$PHASE0_RUN_DIR/node-${pod_index}.log"
    if ! phase0_kubectl -n "$PHASE0_NAMESPACE" logs "$pod" -c inventory >"$log_file" 2>&1 || ! grep -q '^inventory_complete=true$' "$log_file"; then
      logs_ready=0
    fi
  done <<<"$pods"
  (( logs_ready == 1 )) && break
  sleep 2
done
if (( logs_ready == 0 )); then
  phase0_result FAIL node-inventory-evidence "one or more inventory pods did not emit a completion marker before logs were read"
fi

if [[ -z "$pods" ]]; then
  phase0_result FAIL node-inventory-evidence "no inventory pod logs were available"
else
  pod_index=0
  while IFS= read -r pod; do
    [[ -n "$pod" ]] || continue
    pod_index=$((pod_index + 1))
    log_file="$PHASE0_RUN_DIR/node-${pod_index}.log"
    if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "$pod" -c inventory >"$log_file" 2>&1; then
      phase0_sanitize_file "$log_file"
      if grep -q '^inventory_complete=true$' "$log_file"; then
        phase0_result PASS "node-inventory:${pod}" "sanitized runtime facts captured"
      else
        phase0_result FAIL "node-inventory:${pod}" "probe did not emit completion marker"
      fi
    else
      phase0_sanitize_file "$log_file"
      phase0_result FAIL "node-inventory:${pod}" "kubectl logs failed"
    fi
  done <<<"$pods"
fi

if (( ! PHASE0_KEEP )); then
  phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" node-inventory
fi
phase0_finish
