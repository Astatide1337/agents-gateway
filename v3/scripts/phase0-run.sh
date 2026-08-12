#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Safe orchestration wrapper for the individual Phase-0 probes. The probes
# remain the source of truth; this file supplies one run identity, evidence
# root, and explicit cluster boundary to them.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=phase0-common.sh
source "$SCRIPT_DIR/phase0-common.sh"

PHASES=(inventory userns airlock credentials sandbox objectstore)
MODE=plan
NAMESPACE=${PHASE0_NAMESPACE:-agw-phase0}
OUTPUT_DIR=${PHASE0_OUTPUT_DIR:-${PWD}/phase0-evidence}
CONTEXT=${PHASE0_CONTEXT:-}
KUBECTL_BIN=${PHASE0_KUBECTL_BIN:-kubectl}
RUN_ID=${PHASE0_RUN_ID:-}
YES=0
KEEP=0
REQUIRE_DIGESTS=${PHASE0_REQUIRE_DIGESTS:-0}
INSTALL_UPSTREAM=0

usage() {
  cat <<'EOF'
Usage: phase0-run.sh [options]

Runs all six Agents Gateway v3 Phase-0 probes with one disposable run
identity. The default is plan mode and performs no Kubernetes or object-store
mutation. The wrapper never changes the current kubectl context and never
deletes a namespace.

Actions (choose at most one; default: --plan):
  --plan                 Render all probes without contacting Kubernetes.
  --dry-run              Use bounded client-side kubectl dry-runs. Requires
                         --context and never creates live resources.
  --apply --yes          Run probes in the selected disposable context.
  --cleanup --yes        Delete only resources carrying the exact run identity
                         recorded by a prior wrapper run.

Boundary options:
  --context NAME         Exact kubectl context; required for dry-run/apply/
                         cleanup.
  --namespace NAME       Disposable namespace (default: agw-phase0).
  --output-dir DIR       Evidence root (default: ./phase0-evidence).
  --run-id ID            Kubernetes-label-safe identity. Generated for plan /
                         apply; required for cleanup.
  --require-digests      Require every probe image to use an immutable
                         image@sha256:<64-hex-digest> reference.
  --keep                 Keep successful live resources for inspection.
  --install-upstream     Pass --install-upstream to the Sandbox probe (apply only;
                         requires a manifest digest and immutable controller image).

The object-store probe remains explicitly configured through its existing
PHASE0_OBJECTSTORE_* environment variables; no bucket or credential is
invented by this wrapper.
EOF
}

die() {
  printf 'phase0-run: ERROR: %s\n' "$*" >&2
  exit 2
}

parse_args() {
  local action_count=0 arg
  while (($#)); do
    arg=$1
    case "$arg" in
      --plan) MODE=plan; action_count=$((action_count + 1)); shift ;;
      --dry-run) MODE=dry-run; action_count=$((action_count + 1)); shift ;;
      --apply) MODE=apply; action_count=$((action_count + 1)); shift ;;
      --cleanup) MODE=cleanup; action_count=$((action_count + 1)); shift ;;
      --yes) YES=1; shift ;;
      --keep) KEEP=1; shift ;;
      --require-digests) REQUIRE_DIGESTS=1; shift ;;
      --install-upstream) INSTALL_UPSTREAM=1; shift ;;
      --context)
        (($# >= 2)) || die '--context requires a value'
        CONTEXT=$2; shift 2 ;;
      --namespace)
        (($# >= 2)) || die '--namespace requires a value'
        NAMESPACE=$2; shift 2 ;;
      --output-dir)
        (($# >= 2)) || die '--output-dir requires a value'
        OUTPUT_DIR=$2; shift 2 ;;
      --run-id)
        (($# >= 2)) || die '--run-id requires a value'
        RUN_ID=$2; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      --) shift; (($# == 0)) || die "unexpected arguments after --: $*" ;;
      *) die "unknown argument: $arg" ;;
    esac
  done
  ((action_count <= 1)) || die 'choose exactly one of --plan, --dry-run, --apply, or --cleanup'
}

validate_inputs() {
  phase0_validate_dns_name namespace "$NAMESPACE" || exit 2
  phase0_validate_single_line context "$CONTEXT" || exit 2
  phase0_validate_single_line output-dir "$OUTPUT_DIR" || exit 2
  [[ "$REQUIRE_DIGESTS" == 0 || "$REQUIRE_DIGESTS" == 1 ]] || die 'require-digests must be 0 or 1'
  if [[ "$MODE" == dry-run || "$MODE" == apply || "$MODE" == cleanup ]]; then
    [[ -n "$CONTEXT" ]] || die "--$MODE requires an explicit --context; no ambient context is accepted"
    command -v -- "$KUBECTL_BIN" >/dev/null 2>&1 || die "kubectl binary is unavailable: $KUBECTL_BIN"
  fi
  if [[ "$MODE" == apply || "$MODE" == cleanup ]]; then
    ((YES == 1)) || die "--$MODE requires --yes explicitly"
  fi
  if (( INSTALL_UPSTREAM == 1 )) && [[ "$MODE" != apply ]]; then
    die '--install-upstream requires --apply --yes'
  fi
  if [[ "$MODE" == cleanup ]]; then
    [[ -n "$RUN_ID" ]] || die '--cleanup requires --run-id or PHASE0_RUN_ID from the original run'
  elif [[ -z "$RUN_ID" ]]; then
    RUN_ID="phase0-run-$(date -u +%Y%m%d%H%M%S)-$$"
  fi
  phase0_validate_run_id "$RUN_ID" || exit 2
}

prepare_evidence() {
  umask 077
  mkdir -p -- "$OUTPUT_DIR" || die "cannot create output directory: $OUTPUT_DIR"
  [[ -d "$OUTPUT_DIR" ]] || die "output path is not a directory: $OUTPUT_DIR"
  chmod 700 -- "$OUTPUT_DIR" 2>/dev/null || true
  if [[ "$MODE" == cleanup ]]; then
    local source_dir="$OUTPUT_DIR/$RUN_ID"
    [[ -d "$source_dir" ]] || die "cleanup requires prior evidence directory: $source_dir"
    [[ -f "$source_dir/orchestrator.lock" ]] || die "cleanup marker is missing: $source_dir/orchestrator.lock"
    grep -Fxq "run_id=$RUN_ID" "$source_dir/orchestrator.lock" || die 'cleanup run identity does not match its marker'
    grep -Fxq "namespace=$NAMESPACE" "$source_dir/orchestrator.lock" || die 'cleanup namespace does not match its marker'
    grep -Fxq "context=$CONTEXT" "$source_dir/orchestrator.lock" || die 'cleanup context does not match its marker'
    EVIDENCE_DIR="$OUTPUT_DIR/.cleanup-$RUN_ID-$(date -u +%Y%m%d%H%M%S)-$$"
  else
    EVIDENCE_DIR="$OUTPUT_DIR/$RUN_ID"
    [[ ! -e "$EVIDENCE_DIR" ]] || die "evidence directory already exists; choose a new --run-id"
  fi
  mkdir -p -- "$EVIDENCE_DIR" || die "cannot create evidence directory: $EVIDENCE_DIR"
  chmod 700 -- "$EVIDENCE_DIR" 2>/dev/null || true
  {
    printf 'run_id=%s\n' "$RUN_ID"
    printf 'namespace=%s\n' "$NAMESPACE"
    printf 'context=%s\n' "$CONTEXT"
    printf 'mode=%s\n' "$MODE"
    printf 'require_digests=%s\n' "$REQUIRE_DIGESTS"
    printf 'started_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } >"$EVIDENCE_DIR/orchestrator.lock"
  : >"$EVIDENCE_DIR/summary.tsv"
}

probe_for_phase() {
  case "$1" in
    inventory) printf '%s\n' phase0-inventory.sh ;;
    userns) printf '%s\n' phase0-userns.sh ;;
    airlock) printf '%s\n' phase0-airlock.sh ;;
    credentials) printf '%s\n' phase0-credentials.sh ;;
    sandbox) printf '%s\n' phase0-sandbox.sh ;;
    objectstore) printf '%s\n' phase0-objectstore.sh ;;
    *) die "unknown Phase-0 probe: $1" ;;
  esac
}

run_probe() {
  local phase=$1 script status
  script="$SCRIPT_DIR/$(probe_for_phase "$phase")"
  [[ -f "$script" ]] || die "probe script is missing: $script"
  local child_output_dir="$EVIDENCE_DIR/$phase"
  mkdir -p -- "$child_output_dir"
  chmod 700 -- "$child_output_dir" 2>/dev/null || true
  local -a args=(--namespace "$NAMESPACE" --output-dir "$child_output_dir" --context "$CONTEXT")
  case "$MODE" in
    plan) args=(--namespace "$NAMESPACE" --output-dir "$child_output_dir" --plan) ;;
    dry-run) args+=(--dry-run) ;;
    apply)
      args+=(--apply --yes)
      ((KEEP == 1)) && args+=(--keep)
      [[ "$phase" == sandbox && "$INSTALL_UPSTREAM" == 1 ]] && args+=(--install-upstream)
      ;;
    cleanup) args+=(--cleanup --yes) ;;
  esac
  ((REQUIRE_DIGESTS == 1)) && args+=(--require-digests)
  printf '\n==> Phase-0 %s (%s)\n' "$phase" "$MODE"
  set +e
  env PHASE0_NAMESPACE="$NAMESPACE" PHASE0_OUTPUT_DIR="$child_output_dir" \
    PHASE0_RUN_ID="$RUN_ID" PHASE0_CONTEXT="$CONTEXT" \
    PHASE0_KUBECTL_BIN="$KUBECTL_BIN" bash "$script" "${args[@]}"
  status=$?
  set -e
  printf '%s\t%s\t%s\n' "$phase" "$status" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$EVIDENCE_DIR/summary.tsv"
  case "$status" in
    0) printf '<== Phase-0 %s: PASS\n' "$phase" ;;
    3) printf '<== Phase-0 %s: INCONCLUSIVE\n' "$phase" ;;
    *) printf '<== Phase-0 %s: FAIL (exit %d)\n' "$phase" "$status" >&2 ;;
  esac
  return "$status"
}

main() {
  parse_args "$@"
  validate_inputs
  prepare_evidence
  local overall=0 status phase
  for phase in "${PHASES[@]}"; do
    run_probe "$phase" || {
      status=$?
      if ((status != 3)); then overall=1; elif ((overall == 0)); then overall=3; fi
    }
  done
  {
    printf 'finished_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'overall_exit=%s\n' "$overall"
    printf 'evidence_dir=%s\n' "$EVIDENCE_DIR"
  } >>"$EVIDENCE_DIR/orchestrator.lock"
  printf '\nPhase-0 run complete: run_id=%s overall_exit=%s evidence=%s\n' "$RUN_ID" "$overall" "$EVIDENCE_DIR"
  return "$overall"
}

main "$@"
