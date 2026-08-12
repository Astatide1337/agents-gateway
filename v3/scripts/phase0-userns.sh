#!/usr/bin/env bash
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=phase0-common.sh
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-userns
PHASE0_BASE_IMAGE="${PHASE0_BASE_IMAGE:-docker.io/library/busybox:1.36.1}"
PHASE0_NODE_SELECTOR_KEY="${PHASE0_NODE_SELECTOR_KEY:-agw.astatide.com/agents}"
PHASE0_NODE_SELECTOR_VALUE="${PHASE0_NODE_SELECTOR_VALUE:-true}"
PHASE0_TOLERATION_KEY="${PHASE0_TOLERATION_KEY:-agw.astatide.com/agents}"
PHASE0_TOLERATION_VALUE="${PHASE0_TOLERATION_VALUE:-true}"
PHASE0_UIDMAP_POD="${PHASE0_UIDMAP_POD:-agw-phase0-uidmap}"

usage() {
  cat <<'EOF'
Usage: phase0-userns.sh [common options]

Proves PodSpec hostUsers:false with uid_map/gid_map, non-host namespace
identifiers, writable emptyDir behavior, and absent service-account tokens.
The live probe is never run unless --apply --yes is explicit.

Environment:
  PHASE0_BASE_IMAGE       Image containing sh/id/readlink (default: busybox:1.36.1)
  PHASE0_NODE_SELECTOR_KEY/VALUE
  PHASE0_TOLERATION_KEY/VALUE
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
  "uidmap-pod=$PHASE0_UIDMAP_POD"; do
  phase0_validate_single_line "${pair%%=*}" "${pair#*=}" || exit 2
done
phase0_validate_image_ref base-image "$PHASE0_BASE_IMAGE" || exit 2
phase0_validate_dns_name uidmap-pod "$PHASE0_UIDMAP_POD" || exit 2
phase0_init_evidence || exit 2

manifest="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-userns.XXXXXX.yaml")"
trap 'rm -f -- "$manifest"' EXIT
phase0_render_template \
  "$SCRIPT_DIR/../test/e2e/phase0/20-userns-uidmap.yaml" "$manifest" \
  "PHASE0_NAMESPACE=$PHASE0_NAMESPACE" \
  "PHASE0_RUN_ID=$PHASE0_RUN_ID" \
  "PHASE0_UIDMAP_POD=$PHASE0_UIDMAP_POD" \
  "PHASE0_BASE_IMAGE=$PHASE0_BASE_IMAGE" \
  "PHASE0_NODE_SELECTOR_KEY=$PHASE0_NODE_SELECTOR_KEY" \
  "PHASE0_NODE_SELECTOR_VALUE=$PHASE0_NODE_SELECTOR_VALUE" \
  "PHASE0_TOLERATION_KEY=$PHASE0_TOLERATION_KEY" \
  "PHASE0_TOLERATION_VALUE=$PHASE0_TOLERATION_VALUE"

phase0_manifest_action "$manifest" userns-uidmap

if [[ "$PHASE0_MODE" != apply || "$PHASE0_CLEANUP" == 1 ]]; then
  if (( PHASE0_CLEANUP )); then
    phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" userns-uidmap
  fi
  phase0_finish
  exit $?
fi

if ! phase0_cluster_ready; then
  phase0_finish
  exit $?
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$PHASE0_UIDMAP_POD" "pod/${PHASE0_UIDMAP_POD}-b" --timeout=60s >"$PHASE0_RUN_DIR/wait.txt" 2>&1; then
	phase0_result PASS userns-pod "both uid_map probes completed"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/wait.txt"
  phase0_result FAIL userns-pod "uid_map probe did not complete"
fi

log_file="$PHASE0_RUN_DIR/uidmap.log"
second_log="$PHASE0_RUN_DIR/uidmap-b.log"
if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "pod/$PHASE0_UIDMAP_POD" >"$log_file" 2>&1; then
  phase0_sanitize_file "$log_file"
    uid_line="$(awk '$1 == 0 { print $1, $2, $3; exit }' "$log_file")"
  if [[ "$uid_line" =~ ^0[[:space:]]+([1-9][0-9]+)[[:space:]]+([0-9]+)$ ]]; then
    mapped_start="${BASH_REMATCH[1]}"
    mapped_length="${BASH_REMATCH[2]}"
    if (( mapped_length >= 65536 )); then
      phase0_result PASS uid-map "uid 0 maps to a non-host range of length ${mapped_length}"
    else
      phase0_result FAIL uid-map "uid map range is shorter than 65536"
    fi
    printf 'uid_map_host_start=%s\nuid_map_length=%s\n' "$mapped_start" "$mapped_length" >>"$PHASE0_RUN_DIR/derived.txt"
  else
    phase0_result FAIL uid-map "uid_map does not prove a non-host mapping"
  fi
  if grep -q '^gid_map:' "$log_file" && grep -A1 '^gid_map:' "$log_file" | tail -1 | awk '$1 == 0 && $2 > 0 && $3 >= 65536 { found=1 } END { exit(found ? 0 : 1) }'; then
    phase0_result PASS gid-map "gid 0 maps to a non-host range"
  else
    phase0_result FAIL gid-map "gid_map does not prove a non-host mapping"
  fi
	if grep -q '^sa_mount_state=absent$' "$log_file"; then
    phase0_result PASS service-account-token "automatic service-account token is absent"
  else
    phase0_result FAIL service-account-token "service-account token was present"
	fi
	user_namespace="$(sed -n 's/^user_namespace=//p' "$log_file" | head -1)"
	if [[ -n "$user_namespace" && "${mapped_start:-}" =~ ^[1-9][0-9]+$ && "${mapped_length:-0}" -ge 65536 ]]; then
		phase0_result PASS user-namespace "pod user namespace is proven by its subordinate UID mapping"
	else
		phase0_result FAIL user-namespace "pod user namespace was not proven by a subordinate UID mapping"
	fi
else
  phase0_sanitize_file "$log_file"
  phase0_result FAIL uidmap-log "kubectl logs failed"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "pod/${PHASE0_UIDMAP_POD}-b" >"$second_log" 2>&1; then
	phase0_sanitize_file "$second_log"
	second_uid_line="$(awk '$1 == 0 { print $1, $2, $3; exit }' "$second_log")"
	if [[ "$second_uid_line" =~ ^0[[:space:]]+([1-9][0-9]+)[[:space:]]+([0-9]+)$ ]]; then
		second_mapped_start="${BASH_REMATCH[1]}"
		second_mapped_length="${BASH_REMATCH[2]}"
		if [[ -n "${mapped_start:-}" && "$second_mapped_start" != "$mapped_start" && "$second_mapped_length" -ge 65536 ]]; then
			phase0_result PASS unique-uid-map "two pods received distinct subordinate UID ranges"
		else
			phase0_result FAIL unique-uid-map "distinct subordinate UID ranges were not proven"
		fi
	else
		phase0_result FAIL unique-uid-map "second pod uid_map is malformed"
	fi
else
	phase0_sanitize_file "$second_log"
	phase0_result FAIL uidmap-b-log "second pod logs were unavailable"
fi

if (( ! PHASE0_KEEP )); then
  phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" userns-uidmap
fi
phase0_finish
