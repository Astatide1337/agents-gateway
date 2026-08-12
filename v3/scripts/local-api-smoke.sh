#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Disposable, local-only Kubernetes API/schema smoke for Agents Gateway v3.
#
# This script deliberately does not invoke git, gh, GitHub APIs, Docker/Podman
# directly, an image push, an Argo submission, or a broad engine cleanup/prune
# command. Kind owns the disposable cluster lifecycle; the trap deletes only
# the generated cluster name and its private kubeconfig after creation has
# succeeded. A failed create is never treated as proof that this process owns
# an existing cluster with the same name.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
V3_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
REPO_ROOT=$(CDPATH= cd -- "$V3_DIR/.." && pwd)
CHART_DIR="$V3_DIR/charts/agw-operator"
CHART_VALUES="$CHART_DIR/tests/values-ci.yaml"
SAMPLES_DIR="$V3_DIR/config/samples"
CRD_DIR="$V3_DIR/config/crd"

KIND_BIN=${AGW_KIND_BIN:-kind}
KUBECTL_BIN=${AGW_KUBECTL_BIN:-kubectl}
HELM_BIN=${AGW_HELM_BIN:-helm}
ARGO_CRDS_FILE=${AGW_ARGO_CRDS_FILE:-}
FIELD_MANAGER=${AGW_API_SMOKE_FIELD_MANAGER:-agw-v3-local-api-smoke}
RUN_ARGO=1

CLUSTER_NAME=''
CLUSTER_CREATED=0
KUBECONFIG_DIR=''
KUBECONFIG_FILE=''
CURRENT_STEP=''

AGW_CRDS=(
  agentruns.agents.astatide.com
  agents.agents.astatide.com
  contextstrategies.agents.astatide.com
  gates.agents.astatide.com
  modelroutes.agents.astatide.com
  policies.agents.astatide.com
  toolsets.agents.astatide.com
)

ARGO_CRDS=(
  clusterworkflowtemplates.argoproj.io
  cronworkflows.argoproj.io
  workflowartifactgctasks.argoproj.io
  workfloweventbindings.argoproj.io
  workflows.argoproj.io
  workflowtaskresults.argoproj.io
  workflowtasksets.argoproj.io
  workflowtemplates.argoproj.io
)

usage() {
  cat <<'EOF'
Usage: ./v3/scripts/local-api-smoke.sh [options]

Creates one uniquely named disposable Kind cluster, checks the v3 Kubernetes
API contract, and deletes that exact cluster on exit. The complete smoke runs
the direct and Argo Helm server-side dry-runs. It is opt-in and separate from
validate-local.sh.

Options:
  --argo-crds-file PATH  Read a local Argo Workflows v4.1.0 full-install or
                         CRD-only YAML file. Only its eight CRD documents are
                         applied, server-side. No URL is accepted.
  --skip-argo            Skip Argo CRD installation and the Argo Helm check.
                         This is a reduced AGW-only smoke, not the complete
                         direct+Argo check.
  -h, --help             Show this help.

Environment overrides:
  AGW_KIND_BIN, AGW_KUBECTL_BIN, AGW_HELM_BIN
  AGW_ARGO_CRDS_FILE, AGW_API_SMOKE_FIELD_MANAGER
  AGW_API_SMOKE_CLUSTER_NAME  deterministic agw-v3-api-* name for local tests

The default complete mode requires a local Argo CRD bundle. This keeps the
command repeatable and guarantees it never fetches from GitHub or another
remote service.
EOF
}

die() {
  printf 'local-api-smoke: %s\n' "$*" >&2
  exit 2
}

resolve_tool() {
  local requested=$1
  local label=$2
  local resolved

  if [[ "$requested" == */* ]]; then
    [[ -x "$requested" ]] || die "required local $label tool is not executable: $requested"
    printf '%s\n' "$requested"
    return
  fi

  resolved=$(command -v -- "$requested" 2>/dev/null || true)
  [[ -n "$resolved" ]] || die "required local $label tool not found: $requested (set AGW_${label^^}_BIN to its path)"
  printf '%s\n' "$resolved"
}

normalize_local_path() {
  local path=$1
  if [[ "$path" == /* ]]; then
    printf '%s\n' "$path"
  else
    printf '%s/%s\n' "$PWD" "$path"
  fi
}

validate_cluster_name() {
  local name=$1
  [[ "$name" == agw-v3-api-* ]] || die 'cluster name must start with agw-v3-api-'
  (( ${#name} <= 63 )) || die "cluster name is longer than 63 characters: $name"
  [[ "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
    die "cluster name must be a lowercase DNS label: $name"
}

parse_args() {
  while (($#)); do
    case "$1" in
      --argo-crds-file)
        [[ $# -ge 2 ]] || die '--argo-crds-file requires a local path'
        ARGO_CRDS_FILE=$2
        shift
        ;;
      --skip-argo|--skip-argo-crds)
        RUN_ARGO=0
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
        die "unknown option: $1"
        ;;
    esac
    shift
  done
}

print_command() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
}

run_step() {
  local label=$1
  shift
  CURRENT_STEP=$label
  printf '\n==> %s\n' "$label"
  print_command "$@"
  "$@"
  printf '<== %s: PASS\n' "$label"
  CURRENT_STEP=''
}

kube() {
  "$KUBECTL_BIN" \
    --kubeconfig "$KUBECONFIG_FILE" \
    --context "kind-$CLUSTER_NAME" \
    "$@"
}

cleanup() {
  local status=$?
  local cleanup_status=0

  if (( CLUSTER_CREATED )); then
    printf '\n==> delete disposable Kind cluster: %s\n' "$CLUSTER_NAME"
    print_command "$KIND_BIN" delete cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_FILE"
    "$KIND_BIN" delete cluster \
      --name "$CLUSTER_NAME" \
      --kubeconfig "$KUBECONFIG_FILE" || cleanup_status=$?
    if (( cleanup_status == 0 )); then
      printf '<== delete disposable Kind cluster: PASS (%s)\n' "$CLUSTER_NAME"
    else
      printf '<== delete disposable Kind cluster: FAIL (exit %d)\n' "$cleanup_status" >&2
    fi
  fi

  if [[ -n "$KUBECONFIG_DIR" && -d "$KUBECONFIG_DIR" ]]; then
    rm -rf -- "$KUBECONFIG_DIR"
  fi

  if (( status != 0 )); then
    printf '\nlocal-api-smoke: FAILED%s (exit %d)\n' \
      "${CURRENT_STEP:+ during $CURRENT_STEP}" "$status" >&2
    exit "$status"
  fi
  if (( cleanup_status != 0 )); then
    printf '\nlocal-api-smoke: FAILED during exact-cluster cleanup (exit %d)\n' "$cleanup_status" >&2
    exit "$cleanup_status"
  fi
  exit 0
}
trap cleanup EXIT
trap 'exit 130' INT TERM

extract_argo_crds() {
  local source=$1
  local output=$2

  # Official Argo release bundles may be a CRD-only file or a larger
  # namespace-install file. Keep only the eight CustomResourceDefinition
  # documents. This uses no YAML dependency and the exact names are checked
  # again after the API server establishes them.
  python3 - "$source" "${ARGO_CRDS[@]}" >"$output" <<'PY'
import re
import sys
from pathlib import Path

source = Path(sys.argv[1])
required = set(sys.argv[2:])
text = source.read_text(encoding="utf-8")
documents = re.split(r"(?m)^---[ \t]*(?:#.*)?$", text)
selected = []
names = []

for document in documents:
    if not re.search(r"(?m)^kind:[ \t]*CustomResourceDefinition[ \t]*$", document):
        continue
    match = re.search(
        r"(?ms)^metadata:\s*\n(?:^[ \t].*\n)*?^  name:[ \t]*([^\s#]+)",
        document,
    )
    if match is None:
        raise SystemExit("Argo CRD document has no metadata.name")
    names.append(match.group(1))
    selected.append(document.strip() + "\n")

if set(names) != required or len(names) != len(required):
    raise SystemExit(
        "local Argo manifest did not contain exactly the eight expected v4.1.0 "
        f"CRDs; found {sorted(names)!r}"
    )

sys.stdout.write("---\n".join(selected))
PY
}

parse_args "$@"

KIND_BIN=$(resolve_tool "$KIND_BIN" kind)
KUBECTL_BIN=$(resolve_tool "$KUBECTL_BIN" kubectl)
HELM_BIN=$(resolve_tool "$HELM_BIN" helm)
command -v python3 >/dev/null 2>&1 || die 'required local helper is missing: python3 (needed to filter a local Argo manifest)'

[[ -d "$CRD_DIR" ]] || die "AGW CRD directory is missing: $CRD_DIR"
[[ -d "$SAMPLES_DIR" ]] || die "AGW sample directory is missing: $SAMPLES_DIR"
[[ -f "$CHART_VALUES" ]] || die "chart smoke values are missing: $CHART_VALUES"

if (( RUN_ARGO )); then
  [[ -n "$ARGO_CRDS_FILE" ]] || die 'complete direct+Argo smoke requires --argo-crds-file PATH or AGW_ARGO_CRDS_FILE; use --skip-argo for AGW-only mode'
  [[ "$ARGO_CRDS_FILE" != *://* ]] || die 'Argo CRD input must be a local file; remote URLs are intentionally rejected'
  ARGO_CRDS_FILE=$(normalize_local_path "$ARGO_CRDS_FILE")
  [[ -f "$ARGO_CRDS_FILE" ]] || die "local Argo CRD input does not exist: $ARGO_CRDS_FILE"
fi

printf 'local-api-smoke: using kind=%s\n' "$KIND_BIN"
printf 'local-api-smoke: using kubectl=%s\n' "$KUBECTL_BIN"
printf 'local-api-smoke: using helm=%s\n' "$HELM_BIN"
if [[ -n "${AGW_API_SMOKE_CLUSTER_NAME:-}" ]]; then
  CLUSTER_NAME=$AGW_API_SMOKE_CLUSTER_NAME
else
  CLUSTER_NAME="agw-v3-api-$(date -u +%Y%m%d%H%M%S)-$$-${RANDOM}"
fi
validate_cluster_name "$CLUSTER_NAME"
"$KIND_BIN" version
"$KUBECTL_BIN" version --client=true --output=yaml | sed -n '1,12p'
"$HELM_BIN" version --short

KUBECONFIG_DIR=$(mktemp -d "${TMPDIR:-/tmp}/agw-v3-api-smoke.XXXXXX")
KUBECONFIG_FILE="$KUBECONFIG_DIR/kubeconfig"
existing_clusters=$("$KIND_BIN" get clusters 2>/dev/null || die 'could not inspect existing Kind clusters before create')
# The exact-name preflight is deliberately separate from creation. If a
# concurrent creator wins a race after this check, CLUSTER_CREATED remains
# false on create failure, so the EXIT trap cannot delete the other cluster.
if grep -Fxq -- "$CLUSTER_NAME" <<<"$existing_clusters"; then
  die "refusing to reuse existing disposable cluster name: $CLUSTER_NAME"
fi
run_step "create disposable Kind cluster: $CLUSTER_NAME" \
  "$KIND_BIN" create cluster \
  --name "$CLUSTER_NAME" \
  --kubeconfig "$KUBECONFIG_FILE" \
  --wait 90s
CLUSTER_CREATED=1

run_step 'verify Kubernetes API connectivity' kube get --raw=/version

run_step 'install seven AGW CRDs server-side' \
  kube apply \
  --server-side \
  --force-conflicts \
  --field-manager "$FIELD_MANAGER" \
  -k "$CRD_DIR"

for crd in "${AGW_CRDS[@]}"; do
  run_step "wait for AGW CRD Established: $crd" \
    kube wait --for=condition=Established --timeout=60s "crd/$crd"
done

if (( RUN_ARGO )); then
  ARGO_FILTERED="$KUBECONFIG_DIR/argo-v4.1.0-crds.yaml"
  run_step 'extract exactly eight local Argo v4.1.0 CRD documents' \
    extract_argo_crds "$ARGO_CRDS_FILE" "$ARGO_FILTERED"
  run_step 'install official Argo v4.1.0 full CRDs server-side' \
    kube apply \
    --server-side \
    --force-conflicts \
    --field-manager "$FIELD_MANAGER" \
    -f "$ARGO_FILTERED"
  for crd in "${ARGO_CRDS[@]}"; do
    run_step "wait for Argo CRD Established: $crd" \
      kube wait --for=condition=Established --timeout=60s "crd/$crd"
  done
fi

run_step 'establish agw-system and agw-runs namespaces server-side' \
  kube apply \
  --server-side \
  --force-conflicts \
  --field-manager "$FIELD_MANAGER" \
  -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: agw-system
  labels:
    agents.astatide.com/role: system
---
apiVersion: v1
kind: Namespace
metadata:
  name: agw-runs
  labels:
    agents.astatide.com/role: runs
EOF

run_step 'server-side dry-run AGW samples' \
  kube apply \
  --server-side \
  --dry-run=server \
  --field-manager "$FIELD_MANAGER" \
  -k "$SAMPLES_DIR"

run_step 'server-side create disposable AGW samples for admission checks' \
  kube apply \
  --server-side \
  --force-conflicts \
  --field-manager "$FIELD_MANAGER" \
  -k "$SAMPLES_DIR"

mutation_output=''
mutation_status=0
CURRENT_STEP='immutable AgentRun mutation rejection'
printf '\n==> %s\n' "$CURRENT_STEP"
print_command kube patch agentrun jobmark-fix-427 --namespace agw-runs --type=merge --patch '{"spec":{"task":{"inline":"local-api-smoke mutation"}}}'
set +e
mutation_output=$(kube patch agentrun jobmark-fix-427 \
  --namespace agw-runs \
  --type=merge \
  --patch '{"spec":{"task":{"inline":"local-api-smoke mutation"}}}' 2>&1)
mutation_status=$?
set -e
if (( mutation_status == 0 )); then
  printf '%s\n' "$mutation_output" >&2
  die 'immutable AgentRun mutation was unexpectedly accepted'
fi
if ! grep -qi 'immutable' <<<"$mutation_output"; then
  printf '%s\n' "$mutation_output" >&2
  die 'AgentRun mutation was rejected, but not by the immutable-spec validation'
fi
printf 'API rejected the immutable AgentRun mutation as expected:\n%s\n' "$mutation_output"
CURRENT_STEP=''

if ! kube get agentrun jobmark-fix-427 --namespace agw-runs -o jsonpath='{.spec.task.inline}' | grep -Fq 'nil dereference'; then
  die 'immutable AgentRun mutation changed the stored task unexpectedly'
fi
printf '<== immutable AgentRun mutation rejection: PASS\n'

run_step 'set disposable AgentRun cancellation request' \
  kube patch agentrun jobmark-fix-427 \
  --namespace agw-runs \
  --type=merge \
  --patch '{"spec":{"cancelRequested":true}}'

cancel_output=''
cancel_status=0
CURRENT_STEP='monotonic cancellation rejection'
printf '\n==> %s\n' "$CURRENT_STEP"
print_command kube patch agentrun jobmark-fix-427 --namespace agw-runs --type=merge --patch '{"spec":{"cancelRequested":false}}'
set +e
cancel_output=$(kube patch agentrun jobmark-fix-427 \
  --namespace agw-runs \
  --type=merge \
  --patch '{"spec":{"cancelRequested":false}}' 2>&1)
cancel_status=$?
set -e
if (( cancel_status == 0 )); then
  printf '%s\n' "$cancel_output" >&2
  die 'cancelRequested was unexpectedly cleared'
fi
if ! grep -qi 'cancelRequested cannot be cleared' <<<"$cancel_output"; then
  printf '%s\n' "$cancel_output" >&2
  die 'cancel clear was rejected, but not by the monotonic cancellation validation'
fi
printf 'API rejected cancellation reversal as expected:\n%s\n' "$cancel_output"
CURRENT_STEP=''

if ! kube get agentrun jobmark-fix-427 --namespace agw-runs -o jsonpath='{.spec.cancelRequested}' | grep -Fxq 'true'; then
  die 'cancelRequested did not remain true after the rejected reversal'
fi
printf '<== monotonic cancellation rejection: PASS\n'

run_step 'server-side dry-run direct Helm output' \
  bash -c '
    set -Eeuo pipefail
    helm_bin=$1
    chart_dir=$2
    values_file=$3
    kubectl_bin=$4
    kubeconfig_file=$5
    context=$6
    field_manager=$7
    "$helm_bin" template agw-operator "$chart_dir" --namespace agw-system --values "$values_file" --set orchestrationBackend=direct |
      "$kubectl_bin" --kubeconfig "$kubeconfig_file" --context "$context" apply --server-side --dry-run=server --field-manager "$field_manager-direct" -f -
  ' _ "$HELM_BIN" "$CHART_DIR" "$CHART_VALUES" "$KUBECTL_BIN" "$KUBECONFIG_FILE" "kind-$CLUSTER_NAME" "$FIELD_MANAGER"

if (( RUN_ARGO )); then
  run_step 'server-side dry-run Argo Helm output' \
    bash -c '
      set -Eeuo pipefail
      helm_bin=$1
      chart_dir=$2
      values_file=$3
      kubectl_bin=$4
      kubeconfig_file=$5
      context=$6
      field_manager=$7
      "$helm_bin" template agw-operator "$chart_dir" --namespace agw-system --values "$values_file" --set orchestrationBackend=argo |
        "$kubectl_bin" --kubeconfig "$kubeconfig_file" --context "$context" apply --server-side --dry-run=server --field-manager "$field_manager-argo" -f -
    ' _ "$HELM_BIN" "$CHART_DIR" "$CHART_VALUES" "$KUBECTL_BIN" "$KUBECONFIG_FILE" "kind-$CLUSTER_NAME" "$FIELD_MANAGER"
fi

printf '\nlocal-api-smoke: PASS\n'
if (( RUN_ARGO )); then
  printf 'local-api-smoke: AGW seven-CRD + Argo eight-CRD + direct/Argo Helm API contract verified\n'
else
  printf 'local-api-smoke: AGW seven-CRD + direct Helm API contract verified (Argo skipped)\n'
fi
printf 'local-api-smoke: cluster %s will be deleted by the EXIT trap\n' "$CLUSTER_NAME"
