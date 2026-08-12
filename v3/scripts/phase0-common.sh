#!/usr/bin/env bash
# Shared safety, evidence, and kubectl helpers for the Phase-0 probes.
# shellcheck shell=bash

if [[ -n "${AGW_PHASE0_COMMON_LOADED:-}" ]]; then
  return 0
fi
AGW_PHASE0_COMMON_LOADED=1

set -o pipefail

PHASE0_SCRIPT_NAME="${PHASE0_SCRIPT_NAME:-phase0}"
PHASE0_NAMESPACE="${PHASE0_NAMESPACE:-agw-phase0}"
PHASE0_OUTPUT_DIR="${PHASE0_OUTPUT_DIR:-${PWD}/phase0-evidence}"
PHASE0_MODE="${PHASE0_MODE:-dry-run}"
PHASE0_KUBECTL_BIN="${PHASE0_KUBECTL_BIN:-kubectl}"
PHASE0_CONTEXT="${PHASE0_CONTEXT:-}"
PHASE0_KUBECTL_TIMEOUT="${PHASE0_KUBECTL_TIMEOUT:-8s}"
PHASE0_RUN_ID="${PHASE0_RUN_ID:-}"
PHASE0_REQUIRE_DIGESTS="${PHASE0_REQUIRE_DIGESTS:-0}"
PHASE0_YES=0
PHASE0_CLEANUP=0
PHASE0_KEEP=0
PHASE0_FAIL_COUNT=0
PHASE0_SKIP_COUNT=0
PHASE0_PASS_COUNT=0
PHASE0_REMAINING_ARGS=()
PHASE0_RUN_DIR=""

phase0_die() {
  printf 'phase0: ERROR: %s\n' "$*" >&2
  return 2
}

phase0_usage_common() {
  cat <<'EOF'
Common options:
  --output-dir DIR     Evidence root (default: ./phase0-evidence)
  --namespace NAME     Disposable namespace (default: agw-phase0)
  --context NAME       kubectl context (never changes the current context)
  --plan               Render manifests and print planned actions only
  --dry-run            Render and use bounded kubectl client-side dry-run
                       (requires --context; use --plan for offline rendering)
  --apply --yes        Create disposable resources in the selected cluster
  --cleanup --yes      Delete only resources owned by this probe run
  --require-digests    Reject mutable image tags; every probe image must use
                       an image@sha256:<64-hex-digest> reference
  --keep               Keep successful live resources for inspection
  -h, --help           Show command-specific help

Live actions require an explicit --apply --yes or --cleanup --yes. The harness
never reads or prints secret values and never changes the current kubectl
context.
EOF
}

phase0_validate_name() {
  local kind="$1" value="$2"
  if [[ ! "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#value} > 63 )); then
    phase0_die "invalid ${kind}: ${value@Q}"
    return 1
  fi
}

phase0_validate_dns_name() {
  local kind="$1" value="$2"
  if [[ ! "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$ ]] || (( ${#value} > 253 )); then
    phase0_die "invalid ${kind}: ${value@Q}"
    return 1
  fi
}

phase0_validate_single_line() {
  local kind="$1" value="$2"
  if [[ "$value" == *$'\n'* || "$value" == *$'\r'* ]]; then
    phase0_die "${kind} must not contain a newline"
    return 1
  fi
}

phase0_validate_run_id() {
  local value="$1"
  # The run ID is both a Kubernetes label value and an evidence-directory
  # component. Keep it inside the label grammar and reject path separators so
  # an operator-supplied value cannot escape the evidence root or make cleanup
  # selectors ambiguous.
  if [[ ! "$value" =~ ^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$ ]]; then
    phase0_die "run-id must be a Kubernetes-label-safe value of at most 63 characters"
    return 1
  fi
}

phase0_parse_common_args() {
	local arg action_seen=0
  PHASE0_REMAINING_ARGS=()
  while (($#)); do
    arg="$1"
    case "$arg" in
      --output-dir)
        (($# >= 2)) || { phase0_die "--output-dir requires a value"; return 1; }
        PHASE0_OUTPUT_DIR="$2"; shift 2 ;;
      --namespace)
        (($# >= 2)) || { phase0_die "--namespace requires a value"; return 1; }
        PHASE0_NAMESPACE="$2"; shift 2 ;;
      --context)
        (($# >= 2)) || { phase0_die "--context requires a value"; return 1; }
        PHASE0_CONTEXT="$2"; shift 2 ;;
	  --plan) ((action_seen+=1)); PHASE0_MODE=plan; shift ;;
	  --dry-run) ((action_seen+=1)); PHASE0_MODE=dry-run; shift ;;
	  --apply) ((action_seen+=1)); PHASE0_MODE=apply; shift ;;
	  --cleanup) ((action_seen+=1)); PHASE0_MODE=cleanup; PHASE0_CLEANUP=1; shift ;;
      --yes) PHASE0_YES=1; shift ;;
      --require-digests) PHASE0_REQUIRE_DIGESTS=1; shift ;;
      --keep) PHASE0_KEEP=1; shift ;;
      -h|--help)
        PHASE0_REMAINING_ARGS+=("$arg"); shift ;;
      --)
        shift
        PHASE0_REMAINING_ARGS+=("$@")
        break ;;
      *) PHASE0_REMAINING_ARGS+=("$arg"); shift ;;
    esac
  done

	if (( action_seen > 1 )); then
	  phase0_die "choose exactly one of --plan, --dry-run, --apply, or --cleanup"
	  return 1
	fi
  case "$PHASE0_MODE" in plan|dry-run|apply|cleanup) ;; *)
    phase0_die "PHASE0_MODE must be plan, dry-run, apply, or cleanup"
    return 1
  esac
  if [[ "$PHASE0_REQUIRE_DIGESTS" != 0 && "$PHASE0_REQUIRE_DIGESTS" != 1 ]]; then
    phase0_die "PHASE0_REQUIRE_DIGESTS must be 0 or 1"
    return 1
  fi
  # PHASE0_MODE may be supplied through the environment for non-destructive
  # plan/dry-run use. Treat an environment-supplied cleanup mode exactly like
  # --cleanup so it cannot bypass the explicit confirmation requirement.
  if [[ "$PHASE0_MODE" == cleanup ]]; then
    PHASE0_CLEANUP=1
  fi
  phase0_validate_dns_name namespace "$PHASE0_NAMESPACE" || return 1
  if [[ -n "$PHASE0_CONTEXT" ]]; then
    phase0_validate_single_line context "$PHASE0_CONTEXT" || return 1
  fi
  if [[ "$PHASE0_MODE" == apply && "$PHASE0_YES" != 1 ]]; then
    phase0_die "--apply is destructive to a disposable namespace; add --yes explicitly"
    return 1
  fi
  if (( PHASE0_CLEANUP && PHASE0_YES != 1 )); then
    phase0_die "--cleanup requires --yes explicitly"
    return 1
  fi
  if [[ "$PHASE0_MODE" == apply || "$PHASE0_MODE" == cleanup ]] && [[ -z "$PHASE0_CONTEXT" ]]; then
    phase0_die "mutating Phase-0 actions require an explicit --context; no current context will be used"
    return 1
  fi
}

phase0_validate_image_ref() {
  local kind="$1" value="$2"
  phase0_validate_single_line "$kind" "$value" || return 1
  if [[ -z "$value" || "$value" == *[[:space:]]* ]]; then
    phase0_die "${kind} must be a non-empty image reference without whitespace"
    return 1
  fi
  if [[ "$PHASE0_REQUIRE_DIGESTS" == 1 && "$PHASE0_MODE" != cleanup && ! "$value" =~ ^[^@[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]; then
    phase0_die "${kind} must be an immutable image@sha256:<64-hex-digest> reference"
    return 1
  fi
}

phase0_require_cmd() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    phase0_result SKIP "prerequisite:${cmd}" "${cmd} is not installed"
    return 1
  fi
  return 0
}

phase0_init_evidence() {
  local stamp
  umask 077
  mkdir -p "$PHASE0_OUTPUT_DIR" || { phase0_die "cannot create output directory"; return 1; }
  [[ -d "$PHASE0_OUTPUT_DIR" ]] || { phase0_die "output path is not a directory"; return 1; }
  stamp="$(date -u +%Y%m%dT%H%M%SZ)-$$"
  PHASE0_RUN_ID="${PHASE0_RUN_ID:-${PHASE0_SCRIPT_NAME}-${stamp}}"
  phase0_validate_run_id "$PHASE0_RUN_ID" || return 1
  PHASE0_RUN_DIR="$PHASE0_OUTPUT_DIR/$PHASE0_RUN_ID"
  mkdir -p "$PHASE0_RUN_DIR" || { phase0_die "cannot create run evidence directory"; return 1; }
  : >"$PHASE0_RUN_DIR/results.tsv"
  {
    printf 'script=%s\n' "$PHASE0_SCRIPT_NAME"
    printf 'run_id=%s\n' "$PHASE0_RUN_ID"
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'mode=%s\n' "$PHASE0_MODE"
    printf 'namespace=%s\n' "$PHASE0_NAMESPACE"
    printf 'kubectl=%s\n' "$PHASE0_KUBECTL_BIN"
    printf 'live_execution=not_started\n'
  } >"$PHASE0_RUN_DIR/metadata.txt"
}

phase0_sanitize_stream() {
  # Keep replacements syntactically valid when sanitized evidence is later
  # inspected as YAML or JSON (for example, a Secret canary manifest).
  sed -E \
    -e 's/(Authorization:[[:space:]]*Bearer[[:space:]]+)[^[:space:]]+/\1[REDACTED]/Ig' \
    -e 's/("(([A-Za-z0-9]+[-_])*(token|secret|password|passwd)|api[_-]?key|private[_-]?key)"[[:space:]]*:[[:space:]]*)"[^"]*"/\1"[REDACTED]"/Ig' \
    -e 's/((^|[^A-Za-z0-9_-])(([A-Za-z0-9]+[-_])*(token|secret|password|passwd)|api[_-]?key|private[_-]?key)[[:space:]]*:)[[:space:]]*[^[:space:]](.*)$/\1 "[REDACTED]"/Ig' \
    -e 's/((^|[[:space:]])[A-Za-z0-9_.-]*(token|secret|password|passwd|api[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*[[:space:]]*=[[:space:]]*)"[^"]*"/\1"[REDACTED]"/Ig' \
    -e 's/((^|[[:space:]])[A-Za-z0-9_.-]*(token|secret|password|passwd|api[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*[[:space:]]*=[[:space:]]*)[^[:space:]]+/\1[REDACTED]/Ig' \
    -e 's/(AWS4-HMAC-SHA256 Credential=)[^,[:space:]]+/\1[REDACTED]/Ig' \
    -e 's/([A-Za-z0-9_-]{20,}\.)[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/\1[REDACTED-JWT]/g'
}

phase0_sanitize_file() {
	local file="$1" tmp
	if [[ ! -e "$file" ]]; then
		# A failed kubectl read may not create its redirected output file. Keep
		# evidence processing fail-closed without turning that expected probe
		# failure into an unrelated shell error.
		: >"$file"
		return 0
	fi
	tmp="${file}.sanitized.$$"
	if ! phase0_sanitize_stream <"$file" >"$tmp"; then
	  rm -f -- "$tmp"
	  return 1
	fi
	if ! mv -- "$tmp" "$file"; then
	  rm -f -- "$tmp"
	  return 1
	fi
}

phase0_result() {
  local status="$1" check="$2" detail="${3:-}"
  local clean
  clean="$(printf '%s' "$detail" | phase0_sanitize_stream | tr '\r\n\t' '   ')"
  printf '%s\t%s\t%s\n' "$status" "$check" "$clean" >>"$PHASE0_RUN_DIR/results.tsv"
  case "$status" in
    PASS) ((PHASE0_PASS_COUNT+=1)) ;;
    FAIL) ((PHASE0_FAIL_COUNT+=1)) ;;
    SKIP) ((PHASE0_SKIP_COUNT+=1)) ;;
    *) phase0_die "invalid result status: $status"; return 1 ;;
  esac
  printf '%-4s %s%s\n' "$status" "$check" "${clean:+ — $clean}"
}

phase0_finish() {
	local final=PASS
	(( PHASE0_FAIL_COUNT > 0 )) && final=FAIL
	(( PHASE0_FAIL_COUNT == 0 && PHASE0_PASS_COUNT == 0 )) && final=SKIP
	if [[ "$PHASE0_MODE" == apply && "$PHASE0_SKIP_COUNT" -gt 0 && "$PHASE0_FAIL_COUNT" -eq 0 ]]; then
	  final=INCONCLUSIVE
	fi
  {
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'final=%s\n' "$final"
    printf 'pass=%d\n' "$PHASE0_PASS_COUNT"
    printf 'fail=%d\n' "$PHASE0_FAIL_COUNT"
    printf 'skip=%d\n' "$PHASE0_SKIP_COUNT"
  } >>"$PHASE0_RUN_DIR/metadata.txt"
	if [[ "$final" == FAIL ]]; then
	  return 1
	fi
	if [[ "$final" == INCONCLUSIVE ]]; then
	  return 3
	fi
  return 0
}

phase0_kubectl_args() {
  PHASE0_KUBECTL_ARGS=()
  [[ -n "$PHASE0_CONTEXT" ]] && PHASE0_KUBECTL_ARGS+=(--context "$PHASE0_CONTEXT")
}

phase0_kubectl() {
  phase0_kubectl_args
  "$PHASE0_KUBECTL_BIN" "${PHASE0_KUBECTL_ARGS[@]}" "$@"
}

phase0_have_kubectl() {
  if ! command -v "$PHASE0_KUBECTL_BIN" >/dev/null 2>&1; then
    phase0_result SKIP kubectl "${PHASE0_KUBECTL_BIN} is not installed"
    return 1
  fi
  return 0
}

phase0_cluster_ready() {
  if ! phase0_have_kubectl; then return 1; fi
  local output
  if output="$(phase0_kubectl get --raw=/version --request-timeout="$PHASE0_KUBECTL_TIMEOUT" 2>&1)"; then
    printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/cluster-version.json"
    printf 'live_execution=cluster-reachable\n' >>"$PHASE0_RUN_DIR/metadata.txt"
    phase0_result PASS cluster "kubectl reached the selected API server"
    return 0
  fi
  printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/cluster-error.txt"
  if [[ "$PHASE0_MODE" == apply || "$PHASE0_CLEANUP" == 1 ]]; then
    phase0_result FAIL cluster "selected cluster is not reachable; see cluster-error.txt"
  else
    phase0_result SKIP cluster "no reachable cluster; offline mode preserved"
  fi
  return 1
}

phase0_render_template() {
  local source="$1" destination="$2"
  shift 2
  local expression key value escaped
  local -a expressions=()
  while (($#)); do
    [[ "$1" == *=* ]] || { phase0_die "template replacement must be KEY=VALUE"; return 1; }
    key="${1%%=*}"
    value="${1#*=}"
    phase0_validate_single_line "$key" "$value" || return 1
    escaped="$(printf '%s' "$value" | sed 's/[\\&|]/\\&/g')"
    expression="s|__${key}__|${escaped}|g"
    expressions+=(-e "$expression")
    shift
  done
  sed "${expressions[@]}" "$source" >"$destination"
}

phase0_manifest_action() {
  local manifest="$1" label="$2"
  cp -- "$manifest" "$PHASE0_RUN_DIR/${label}.yaml"
	if [[ "$PHASE0_MODE" == plan ]]; then
    phase0_result SKIP "manifest:${label}" "plan mode; no kubectl action"
    return 0
	fi
	if [[ "$PHASE0_MODE" == cleanup ]]; then
	  phase0_result SKIP "manifest:${label}" "cleanup mode; manifest was not applied"
	  return 0
	fi
  if ! phase0_have_kubectl; then return 0; fi
  local output
  if [[ "$PHASE0_MODE" == dry-run ]]; then
    # kubectl apply --dry-run=client still performs API discovery for some
    # resource types. Never let a dry-run silently use the operator's current
    # context; --plan is the truly offline mode.
    if [[ -z "$PHASE0_CONTEXT" ]]; then
      phase0_result SKIP "manifest:${label}" "client dry-run requires an explicit --context; no current context was used"
      return 0
    fi
    if output="$(phase0_kubectl apply --dry-run=client --validate=false --request-timeout="$PHASE0_KUBECTL_TIMEOUT" -f "$manifest" 2>&1)"; then
      printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.kubectl.txt"
      phase0_result PASS "manifest:${label}" "kubectl client-side dry-run accepted the manifest"
    else
      printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.kubectl-error.txt"
      phase0_result FAIL "manifest:${label}" "kubectl client-side dry-run rejected the manifest"
    fi
    return 0
  fi
  if ! phase0_cluster_ready; then return 0; fi
  if output="$(phase0_kubectl apply -f "$manifest" 2>&1)"; then
    printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.kubectl.txt"
    phase0_result PASS "manifest:${label}" "resources applied"
  else
    printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.kubectl-error.txt"
    phase0_result FAIL "manifest:${label}" "kubectl apply failed"
  fi
}

phase0_delete_label_selector() {
	local selector="$1" label="$2"
	if [[ "$PHASE0_MODE" != apply && "$PHASE0_MODE" != cleanup ]]; then
	  phase0_result FAIL "cleanup:${label}" "cleanup is forbidden outside --apply or --cleanup"
	  return 1
	fi
  if ! phase0_cluster_ready; then return 0; fi
  local output
  if output="$(phase0_kubectl delete all,networkpolicy,secret,configmap,pvc -n "$PHASE0_NAMESPACE" -l "$selector" --ignore-not-found --wait=true 2>&1)"; then
    printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.cleanup.txt"
    phase0_result PASS "cleanup:${label}" "owned resources deleted"
  else
    printf '%s\n' "$output" | phase0_sanitize_stream >"$PHASE0_RUN_DIR/${label}.cleanup-error.txt"
    phase0_result FAIL "cleanup:${label}" "owned resources could not be deleted"
  fi
}

# Return 0 only when the selected object is positively reported absent, 1 when
# it still exists, and 2 when the API error was not a NotFound response. A
# transient auth, transport, or discovery failure must never be interpreted as
# successful cleanup.
phase0_resource_absent() {
  local kind="$1" name="$2" namespace="${3:-}" output
  local -a namespace_args=()
  [[ -n "$namespace" ]] && namespace_args=(-n "$namespace")
  if output="$(phase0_kubectl "${namespace_args[@]}" get "$kind" "$name" 2>&1)"; then
    return 1
  fi
  if grep -qiE 'not found|notfound|could not find the requested resource' <<<"$output"; then
    return 0
  fi
  printf '%s\n' "$output" >&2
  return 2
}

phase0_report_path() {
  printf '%s\n' "$PHASE0_RUN_DIR"
}
