#!/usr/bin/env bash
# Static and disposable-live checks for the Argo Workflows/Agent Sandbox seam.
#
# The default invocation is offline.  Live actions require all of:
#   AGW_ARGO_PREFLIGHT_LIVE=1
#   AGW_ARGO_PREFLIGHT_CONFIRM=AGW-V3-DISPOSABLE-K3S
#   AGW_ARGO_PREFLIGHT_KUBECONFIG=/absolute/path
#   AGW_ARGO_PREFLIGHT_CONTEXT=agw-v3-k3s-<run-id>
#
# This file is intentionally separate from phase0-sandbox.sh and the Go code.
# It installs only the exact upstream manifests recorded in versions.env,
# creates uniquely named probe objects, and deletes only those objects plus
# the exact upstream manifest objects.  It never changes the ambient context,
# deletes a label selector, or runs a broad cleanup command.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_V3="$(cd -- "$SCRIPT_DIR/.." && pwd)"
ARGO_DIR="$REPO_V3/phase0/argo-sandbox"
VERSIONS_FILE="$ARGO_DIR/versions.env"

# versions.env is a checked-in, assignment-only file.  Refuse unexpected
# syntax before sourcing it so a future edit cannot turn this test into an
# arbitrary command runner.
if rg -n --glob 'versions.env' -v '^$|^(#[^[:cntrl:]]*|[A-Z][A-Z0-9_]*=[^[:space:]]*)$' "$VERSIONS_FILE" >/dev/null; then
  printf 'argo live preflight: versions.env contains an unsafe assignment\n' >&2
  exit 1
fi
# shellcheck disable=SC1090
source "$VERSIONS_FILE"

KUBECTL_BIN="${AGW_ARGO_PREFLIGHT_KUBECTL_BIN:-kubectl}"
LIVE="${AGW_ARGO_PREFLIGHT_LIVE:-0}"
RUN_ID="${AGW_ARGO_PREFLIGHT_RUN_ID:-$(date -u +%Y%m%d%H%M%S)-$$}"
NAMESPACE="${AGW_ARGO_PREFLIGHT_NAMESPACE:-agw-runs}"
TIMEOUT="${AGW_ARGO_PREFLIGHT_TIMEOUT:-240}"
KEEP="${AGW_ARGO_PREFLIGHT_KEEP:-0}"
REQUIRE_HOST_USERS="${AGW_ARGO_PREFLIGHT_REQUIRE_HOST_USERS:-1}"
KUBECONFIG_PATH="${AGW_ARGO_PREFLIGHT_KUBECONFIG:-}"
CONTEXT="${AGW_ARGO_PREFLIGHT_CONTEXT:-}"
EVIDENCE_DIR="${AGW_ARGO_PREFLIGHT_EVIDENCE_DIR:-}"

ARGO_MANIFEST=""
SANDBOX_MANIFEST=""
CLEANUP_STATUS=0
WORKFLOW_SA="agw-argo-preflight-${RUN_ID}"
WORKFLOW_ROLE="$WORKFLOW_SA"
WORKFLOW_BINDING="$WORKFLOW_SA"
CONDITION_WORKFLOW="agw-conditions-${RUN_ID}"
CONDITION_PASS_CM="agw-condition-pass-${RUN_ID}"
CONDITION_FAIL_CM="agw-condition-fail-${RUN_ID}"
SANDBOX_WORKFLOW="agw-sandbox-workflow-${RUN_ID}"
SANDBOX_NAME="agw-sandbox-${RUN_ID}"

usage() {
  cat <<'EOF'
Usage: argo-live-preflight.test.sh

Default: run offline/static checks only.

Disposable live mode:
  AGW_ARGO_PREFLIGHT_LIVE=1 \
  AGW_ARGO_PREFLIGHT_CONFIRM=AGW-V3-DISPOSABLE-K3S \
  AGW_ARGO_PREFLIGHT_KUBECONFIG=/absolute/path/to/kubeconfig \
  AGW_ARGO_PREFLIGHT_CONTEXT=agw-v3-k3s-<run-id> \
  ./v3/scripts/argo-live-preflight.test.sh

Optional live variables:
  AGW_ARGO_PREFLIGHT_RUN_ID       lowercase run identifier (default: UTC+PID)
  AGW_ARGO_PREFLIGHT_NAMESPACE    disposable namespace (default: agw-runs)
  AGW_ARGO_PREFLIGHT_TIMEOUT      total polling budget in seconds (default: 240)
  AGW_ARGO_PREFLIGHT_EVIDENCE_DIR mode-0700 evidence directory
  AGW_ARGO_PREFLIGHT_KEEP=1       retain exact probe objects for inspection
  AGW_ARGO_PREFLIGHT_REQUIRE_HOST_USERS=0
                                  continue the functional probe when the
                                  upstream controller drops hostUsers=false;
                                  the run still fails its security gate

  The live path installs (or reuses) Argo and Agent Sandbox from the checksummed
  release assets, proves API/controller readiness, runs resource-template
  condition and Sandbox workload probes, then removes only objects carrying
  this run's exact label.  It never removes CRDs, controllers, namespaces, or
  other cluster-level resources; the main test owner tears down the cluster.
EOF
}

fail() {
  printf 'argo live preflight: FAIL: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"
}

require_pin() {
  local name="$1" value="$2" pattern="$3"
  [[ "$value" =~ $pattern ]] || fail "$name is not a concrete pinned value: $value"
}

validate_name() {
  local name="$1" value="$2"
  [[ "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || fail "$name is not DNS-label safe: $value"
  (( ${#value} <= 63 )) || fail "$name is longer than 63 characters"
}

validate_run_id() {
  [[ "$RUN_ID" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || fail "run ID is not DNS-label safe: $RUN_ID"
  (( ${#RUN_ID} <= 40 )) || fail "run ID is longer than 40 characters"
}

static_checks() {
  require_cmd bash
  require_cmd rg
  require_cmd sha256sum
  require_pin ARGO_WORKFLOWS_VERSION "$ARGO_WORKFLOWS_VERSION" '^v[0-9]+\.[0-9]+\.[0-9]+$'
  require_pin AGENT_SANDBOX_VERSION "$AGENT_SANDBOX_VERSION" '^v[0-9]+\.[0-9]+\.[0-9]+$'
  require_pin ARGO_WORKFLOWS_COMMIT "$ARGO_WORKFLOWS_COMMIT" '^[0-9a-f]{40}$'
  require_pin AGENT_SANDBOX_COMMIT "$AGENT_SANDBOX_COMMIT" '^[0-9a-f]{40}$'
  require_pin ARGO_WORKFLOWS_INSTALL_SHA256 "$ARGO_WORKFLOWS_INSTALL_SHA256" '^[0-9a-f]{64}$'
  require_pin AGENT_SANDBOX_MANIFEST_SHA256 "$AGENT_SANDBOX_MANIFEST_SHA256" '^[0-9a-f]{64}$'
  [[ "$ARGO_WORKFLOWS_INSTALL_URL" == "https://github.com/argoproj/argo-workflows/releases/download/${ARGO_WORKFLOWS_VERSION}/namespace-install.yaml" ]] \
    || fail 'Argo install URL is not the pinned GitHub release asset'
  [[ "$AGENT_SANDBOX_MANIFEST_URL" == "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox.yaml" ]] \
    || fail 'Agent Sandbox URL is not the pinned GitHub release asset'

  for file in \
    "$ARGO_DIR/versions.env" \
    "$ARGO_DIR/kustomization.yaml" \
    "$ARGO_DIR/workflow-template.yaml" \
    "$ARGO_DIR/workflow.yaml" \
    "$ARGO_DIR/scripts/live-check.sh" \
    "$ARGO_DIR/scripts/verify-static.sh"; do
    [[ -f "$file" ]] || fail "missing required Phase-0 file: $file"
  done

  bash -n "$SCRIPT_DIR/argo-live-preflight.test.sh" \
    "$ARGO_DIR/scripts/live-check.sh" \
    "$ARGO_DIR/scripts/verify-static.sh"
  (
    cd "$REPO_V3"
    bash phase0/argo-sandbox/scripts/verify-static.sh
    go test -count=1 -timeout=90s ./phase0/argo-sandbox/...
  )

  rg -q -- '--server-side' "$SCRIPT_DIR/argo-live-preflight.test.sh" || fail 'live install must use server-side apply'
  rg -q -- 'sha256sum -c' "$SCRIPT_DIR/argo-live-preflight.test.sh" || fail 'live install must verify downloaded assets'
  rg -q -- 'AGW-V3-DISPOSABLE-K3S' "$SCRIPT_DIR/argo-live-preflight.test.sh" || fail 'live confirmation guard is missing'
  rg -q -- 'agw\.astatide\.com/preflight=' "$SCRIPT_DIR/argo-live-preflight.test.sh" || fail 'exact run-label cleanup is missing'
  if rg -n -- 'delete (namespace|crd|clusterrole|clusterrolebinding|priorityclass)' "$SCRIPT_DIR/argo-live-preflight.test.sh"; then
    fail 'cluster-level deletion is forbidden in the shared-cluster preflight'
  fi
  if rg -n -- 'delete[[:space:]]+-f' "$SCRIPT_DIR/argo-live-preflight.test.sh"; then
    fail 'manifest deletion is forbidden in the shared-cluster preflight'
  fi
  if rg -n -- 'delete (all|[[:space:]]+-A|[[:space:]]+--all|\*)' "$SCRIPT_DIR/argo-live-preflight.test.sh"; then
    fail 'broad kubectl deletion is forbidden'
  fi
  printf 'argo live preflight: static checks passed (no cluster contacted)\n'
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
[[ -z "${1:-}" ]] || fail "unknown argument: $1"

static_checks
[[ "$LIVE" == 1 ]] || exit 0

require_cmd "$KUBECTL_BIN"
require_cmd curl
require_cmd jq
require_cmd sha256sum
[[ "${AGW_ARGO_PREFLIGHT_CONFIRM:-}" == AGW-V3-DISPOSABLE-K3S ]] \
  || fail 'live mode requires AGW_ARGO_PREFLIGHT_CONFIRM=AGW-V3-DISPOSABLE-K3S'
[[ -n "$KUBECONFIG_PATH" && "$KUBECONFIG_PATH" == /* && -f "$KUBECONFIG_PATH" ]] \
  || fail 'live mode requires an existing absolute AGW_ARGO_PREFLIGHT_KUBECONFIG'
[[ -n "$CONTEXT" && "$CONTEXT" =~ ^agw-v3-k3s-[a-z0-9-]+$ ]] \
  || fail 'live mode requires a uniquely named agw-v3-k3s-* context'
validate_run_id
validate_name namespace "$NAMESPACE"
[[ "$NAMESPACE" == agw-* ]] || fail 'live namespace must start with agw-'
if ! [[ "$TIMEOUT" =~ ^[1-9][0-9]+$ ]]; then
  fail 'AGW_ARGO_PREFLIGHT_TIMEOUT must be a positive integer'
fi
(( TIMEOUT >= 120 && TIMEOUT <= 1800 )) || fail 'live timeout must be between 120 and 1800 seconds'
[[ "$KEEP" == 0 || "$KEEP" == 1 ]] || fail 'AGW_ARGO_PREFLIGHT_KEEP must be 0 or 1'
[[ "$REQUIRE_HOST_USERS" == 0 || "$REQUIRE_HOST_USERS" == 1 ]] || fail 'AGW_ARGO_PREFLIGHT_REQUIRE_HOST_USERS must be 0 or 1'

if [[ -z "$EVIDENCE_DIR" ]]; then
  EVIDENCE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/agw-argo-live.XXXXXX")"
else
  mkdir -p "$EVIDENCE_DIR"
  chmod 700 "$EVIDENCE_DIR"
fi
chmod 700 "$EVIDENCE_DIR"

kube() {
  "$KUBECTL_BIN" --kubeconfig "$KUBECONFIG_PATH" --context "$CONTEXT" "$@"
}

cleanup_exact() {
	local status=0 resource object resources objects attempt remaining
	local -a run_objects=()
	set +e
	if [[ "$KEEP" == 1 ]]; then
		printf 'argo live preflight: KEEP=1; exact probe resources retained in %s\n' "$EVIDENCE_DIR" >&2
		printf 'status=retained\n' >"$EVIDENCE_DIR/cleanup-status.txt"
		set -e
		return 0
	fi
  # Enumerate by this run's label, then delete each exact resource identity.
  # The selector is used only for read-side discovery; no selector deletion is
  # ever issued.  CRDs, controllers, namespaces, and unlabeled events remain
  # for the owner of the shared disposable cluster to tear down.
	if ! resources="$(kube api-resources --verbs=list --namespaced -o name 2>>"$EVIDENCE_DIR/cleanup-errors.txt")"; then
		printf 'argo live preflight: could not enumerate namespaced API resources for cleanup\n' >&2
		status=1
	else
		while IFS= read -r resource; do
			[[ -n "$resource" ]] || continue
			if ! objects="$(kube -n "$NAMESPACE" get "$resource" -l "agw.astatide.com/preflight=$RUN_ID" -o name --ignore-not-found 2>>"$EVIDENCE_DIR/cleanup-errors.txt")"; then
				status=1
				continue
			fi
			while IFS= read -r object; do
				[[ -n "$object" ]] && run_objects+=("$object")
			done <<<"$objects"
		done <<<"$resources"
	fi
	for object in "${run_objects[@]}"; do
		[[ -n "$object" ]] || continue
		resource="${object%%/*}"
		kube -n "$NAMESPACE" delete "$object" --ignore-not-found=true --wait=true --timeout=30s >/dev/null 2>>"$EVIDENCE_DIR/cleanup-errors.txt" || status=1
	done
  # A Sandbox-owned Pod can still be terminating when its parent Sandbox is
  # deleted. Give exact run-labelled objects a bounded grace period to finish
  # deletion before declaring cleanup incomplete.
	remaining=''
	for ((attempt=0; attempt<90; attempt++)); do
		run_objects=()
		if ! resources="$(kube api-resources --verbs=list --namespaced -o name 2>>"$EVIDENCE_DIR/cleanup-errors.txt")"; then
			status=1
			break
		fi
		while IFS= read -r resource; do
			[[ -n "$resource" ]] || continue
			if ! objects="$(kube -n "$NAMESPACE" get "$resource" -l "agw.astatide.com/preflight=$RUN_ID" -o name --ignore-not-found 2>>"$EVIDENCE_DIR/cleanup-errors.txt")"; then
				status=1
				continue
			fi
			while IFS= read -r object; do
				[[ -n "$object" ]] && run_objects+=("$object")
			 done <<<"$objects"
		done <<<"$resources"
		remaining="$(printf '%s\n' "${run_objects[@]}" | sort -u)"
		[[ -z "$remaining" ]] && break
		sleep 1
  done
  if [[ -n "$remaining" ]]; then
    printf '%s\n' "$remaining" >"$EVIDENCE_DIR/cleanup-remaining.txt"
    printf 'argo live preflight: exact run-labelled objects remain\n' >&2
		status=1
	fi
	set -e
	CLEANUP_STATUS=$status
	printf 'status=%s\n' "$status" >"$EVIDENCE_DIR/cleanup-status.txt"
	return "$status"
}

on_exit() {
	local exit_status=$?
	if ! cleanup_exact; then
		exit_status=1
	fi
	exit "$exit_status"
}
trap on_exit EXIT

server_url="$(kube config view --raw --minify --context "$CONTEXT" -o jsonpath='{.clusters[0].cluster.server}')"
[[ "$server_url" =~ ^https://(127\.0\.0\.1|localhost)(:[0-9]+)?$ ]] \
  || fail "refusing non-loopback Kubernetes API server: $server_url"
printf 'context=%s\nserver=%s\nrun_id=%s\nnamespace=%s\n' "$CONTEXT" "$server_url" "$RUN_ID" "$NAMESPACE" >"$EVIDENCE_DIR/context.txt"

kube version -o json >"$EVIDENCE_DIR/version.json"
kube cluster-info >"$EVIDENCE_DIR/cluster-info.txt"
kube get nodes -o wide >"$EVIDENCE_DIR/nodes.txt"
kube get storageclass >"$EVIDENCE_DIR/storageclasses.txt"
if kube get namespace "$NAMESPACE" --ignore-not-found -o name | grep -q .; then
  phase_label="$(kube get namespace "$NAMESPACE" -o jsonpath='{.metadata.labels.agw\.astatide\.com/phase}')"
  [[ "$phase_label" == 0 ]] || fail "existing namespace is not the disposable Phase-0 namespace: $NAMESPACE"
  printf 'reusing existing disposable namespace=%s\n' "$NAMESPACE" >"$EVIDENCE_DIR/namespace-create.txt"
else
  kube create namespace "$NAMESPACE" >"$EVIDENCE_DIR/namespace-create.txt"
  kube label namespace "$NAMESPACE" "agw.astatide.com/phase=0" --overwrite >"$EVIDENCE_DIR/namespace-label.txt"
fi

download_pinned() {
  local url="$1" expected="$2" output="$3"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --retry 3 --connect-timeout 10 --max-time 180 "$url" -o "$output"
  printf '%s  %s\n' "$expected" "$output" | sha256sum -c -
}

ARGO_MANIFEST="$EVIDENCE_DIR/argo-namespace-install.yaml"
SANDBOX_MANIFEST="$EVIDENCE_DIR/agent-sandbox.yaml"
download_pinned "$ARGO_WORKFLOWS_INSTALL_URL" "$ARGO_WORKFLOWS_INSTALL_SHA256" "$ARGO_MANIFEST"
download_pinned "$AGENT_SANDBOX_MANIFEST_URL" "$AGENT_SANDBOX_MANIFEST_SHA256" "$SANDBOX_MANIFEST"
printf 'argo=%s commit=%s sha256=%s\nagent-sandbox=%s commit=%s sha256=%s\n' \
  "$ARGO_WORKFLOWS_VERSION" "$ARGO_WORKFLOWS_COMMIT" "$ARGO_WORKFLOWS_INSTALL_SHA256" \
  "$AGENT_SANDBOX_VERSION" "$AGENT_SANDBOX_COMMIT" "$AGENT_SANDBOX_MANIFEST_SHA256" \
  >"$EVIDENCE_DIR/pins.txt"

# Argo's namespace install is applied into the disposable run namespace only
# when the shared cluster does not already expose its controllers. Agent
# Sandbox is handled the same way. Applying an exact release manifest is
# idempotent; cleanup deliberately never deletes these cluster-level objects.
argo_controller="$(kube -n "$NAMESPACE" get deployment workflow-controller --ignore-not-found -o name)"
argo_server="$(kube -n "$NAMESPACE" get deployment argo-server --ignore-not-found -o name)"
sandbox_controller="$(kube -n agent-sandbox-system get deployment agent-sandbox-controller --ignore-not-found -o name)"
if [[ -z "$argo_controller" || -z "$argo_server" ]]; then
  kube -n "$NAMESPACE" apply --server-side --field-manager=agw-v3-preflight -f "$ARGO_MANIFEST" >"$EVIDENCE_DIR/argo-apply.txt"
else
  printf 'reusing existing Argo controller/server\n' >"$EVIDENCE_DIR/argo-apply.txt"
fi
if [[ -z "$sandbox_controller" ]]; then
  kube apply --server-side --field-manager=agw-v3-preflight -f "$SANDBOX_MANIFEST" >"$EVIDENCE_DIR/sandbox-apply.txt"
else
  printf 'reusing existing Agent Sandbox controller\n' >"$EVIDENCE_DIR/sandbox-apply.txt"
fi

argo_crds=(
  clusterworkflowtemplates.argoproj.io
  cronworkflows.argoproj.io
  workflowartifactgctasks.argoproj.io
  workfloweventbindings.argoproj.io
  workflows.argoproj.io
  workflowtaskresults.argoproj.io
  workflowtasksets.argoproj.io
  workflowtemplates.argoproj.io
)
for crd in "${argo_crds[@]}"; do
  kube wait --for=condition=Established --timeout=180s "crd/$crd"
done
kube wait --for=condition=Established --timeout=180s crd/sandboxes.agents.x-k8s.io
printf '%s\n' "${argo_crds[@]}" sandboxes.agents.x-k8s.io >"$EVIDENCE_DIR/crds-established.txt"

kube -n "$NAMESPACE" rollout status deployment/workflow-controller --timeout=180s
kube -n "$NAMESPACE" rollout status deployment/argo-server --timeout=180s
kube -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s
kube -n "$NAMESPACE" get deployment workflow-controller argo-server -o wide >"$EVIDENCE_DIR/argo-deployments.txt"
kube -n agent-sandbox-system get deployment agent-sandbox-controller -o wide >"$EVIDENCE_DIR/sandbox-controller.txt"
kube -n agent-sandbox-system get endpoints agent-sandbox-webhook-service -o yaml >"$EVIDENCE_DIR/sandbox-webhook-endpoints.yaml"
kube -n "$NAMESPACE" get deployment workflow-controller -o json >"$EVIDENCE_DIR/workflow-controller.json"
jq -e '.spec.template.spec.containers[0].args | any(. == "--namespaced")' "$EVIDENCE_DIR/workflow-controller.json" >/dev/null \
  || fail 'Argo controller is not running in namespaced mode'

# The workflow ServiceAccount is intentionally narrower than the upstream
# Argo controller account. It can manipulate only the two resource kinds used
# by these probes in the disposable namespace.
kube -n "$NAMESPACE" apply --server-side --field-manager=agw-v3-preflight -f - >"$EVIDENCE_DIR/probe-rbac.txt" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $WORKFLOW_SA
  namespace: $NAMESPACE
  labels:
    agw.astatide.com/preflight: "$RUN_ID"
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: $WORKFLOW_ROLE
  namespace: $NAMESPACE
  labels:
    agw.astatide.com/preflight: "$RUN_ID"
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["create", "get", "patch", "delete"]
- apiGroups: ["agents.x-k8s.io"]
  resources: ["sandboxes"]
  verbs: ["create", "get", "watch", "patch", "delete"]
- apiGroups: ["argoproj.io"]
  resources: ["workflowtaskresults"]
  verbs: ["create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $WORKFLOW_BINDING
  namespace: $NAMESPACE
  labels:
    agw.astatide.com/preflight: "$RUN_ID"
subjects:
- kind: ServiceAccount
  name: $WORKFLOW_SA
  namespace: $NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: $WORKFLOW_ROLE
EOF

# API-only/controller evidence ends above.  The following two workflows are
# workload evidence: they require the running Argo controller to execute a
# resource template, not merely accept a CRD object.
kube -n "$NAMESPACE" apply --server-side --field-manager=agw-v3-preflight -f - >"$EVIDENCE_DIR/condition-workflow-apply.txt" <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: $CONDITION_WORKFLOW
  namespace: $NAMESPACE
  labels:
    agw.astatide.com/preflight: "$RUN_ID"
spec:
  serviceAccountName: $WORKFLOW_SA
  entrypoint: main
  templates:
  - name: main
    steps:
    - - name: positive
        template: positive
    - - name: negative
        template: negative
        continueOn:
          failed: true
  - name: positive
    resource:
      action: apply
      setOwnerReference: true
      manifest: |
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: $CONDITION_PASS_CM
          namespace: $NAMESPACE
          labels:
            agw.astatide.com/preflight: "$RUN_ID"
            phase: ready
        data:
          probe: positive
      successCondition: metadata.labels.phase == ready
      failureCondition: metadata.labels.phase == failed
  - name: negative
    resource:
      action: apply
      setOwnerReference: true
      manifest: |
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: $CONDITION_FAIL_CM
          namespace: $NAMESPACE
          labels:
            agw.astatide.com/preflight: "$RUN_ID"
            phase: failed
        data:
          probe: negative
      successCondition: metadata.labels.phase == ready
      failureCondition: metadata.labels.phase == failed
EOF

wait_workflow_phase() {
	local workflow="$1" deadline=$((SECONDS + TIMEOUT)) phase
	while (( SECONDS < deadline )); do
		if ! phase="$(kube -n "$NAMESPACE" get workflow "$workflow" -o jsonpath='{.status.phase}' 2>>"$EVIDENCE_DIR/workflow-poll-errors.txt")"; then
			return 2
		fi
		case "$phase" in
      Succeeded|Failed|Error) printf '%s\n' "$phase"; return 0 ;;
    esac
    sleep 2
  done
  return 1
}

condition_phase="$(wait_workflow_phase "$CONDITION_WORKFLOW")" || fail 'resource-template condition workflow timed out'
printf 'phase=%s\n' "$condition_phase" >"$EVIDENCE_DIR/condition-workflow-phase.txt"
[[ "$condition_phase" == Succeeded ]] || fail "condition workflow ended in $condition_phase"
kube -n "$NAMESPACE" get workflow "$CONDITION_WORKFLOW" -o json >"$EVIDENCE_DIR/condition-workflow.json"
jq -e --arg template positive '[.status.nodes[]? | select(.templateName == $template and .phase == "Succeeded")] | length == 1' "$EVIDENCE_DIR/condition-workflow.json" >/dev/null \
  || fail 'positive successCondition did not produce a Succeeded resource node'
jq -e --arg template negative '[.status.nodes[]? | select(.templateName == $template and .phase == "Failed")] | length == 1' "$EVIDENCE_DIR/condition-workflow.json" >/dev/null \
  || fail 'negative failureCondition did not produce a Failed resource node'
[[ "$(kube -n "$NAMESPACE" get configmap "$CONDITION_PASS_CM" -o jsonpath='{.metadata.labels.phase}')" == ready ]] \
  || fail 'positive condition ConfigMap was not observed'
[[ "$(kube -n "$NAMESPACE" get configmap "$CONDITION_FAIL_CM" -o jsonpath='{.metadata.labels.phase}')" == failed ]] \
  || fail 'negative condition ConfigMap was not observed'

shutdown_time="$(date -u -d '+120 seconds' +%Y-%m-%dT%H:%M:%SZ)"
kube -n "$NAMESPACE" apply --server-side --field-manager=agw-v3-preflight -f - >"$EVIDENCE_DIR/sandbox-workflow-apply.txt" <<EOF
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  name: $SANDBOX_WORKFLOW
  namespace: $NAMESPACE
  labels:
    agw.astatide.com/preflight: "$RUN_ID"
spec:
  serviceAccountName: $WORKFLOW_SA
  entrypoint: create-sandbox
  templates:
  - name: create-sandbox
    resource:
      action: apply
      setOwnerReference: true
      manifest: |
        apiVersion: agents.x-k8s.io/v1beta1
        kind: Sandbox
        metadata:
          name: $SANDBOX_NAME
          namespace: $NAMESPACE
          labels:
            agw.astatide.com/preflight: "$RUN_ID"
        spec:
          shutdownTime: "$shutdown_time"
          shutdownPolicy: Delete
          service: false
          podTemplate:
            metadata:
              labels:
                agw.astatide.com/preflight: "$RUN_ID"
            spec:
              hostUsers: false
              automountServiceAccountToken: false
              restartPolicy: Never
              securityContext:
                runAsNonRoot: true
                runAsUser: 65532
                runAsGroup: 65532
                seccompProfile:
                  type: RuntimeDefault
              containers:
              - name: fixture
                image: $WORKFLOW_ALPINE_IMAGE
                imagePullPolicy: IfNotPresent
                command: [sh, -ceu]
                args:
                - |
                  # Leave a Ready=True observation window before the terminal
                  # transition changes Ready to False/PodSucceeded.
                  sleep 30
                securityContext:
                  allowPrivilegeEscalation: false
                  readOnlyRootFilesystem: true
                  capabilities:
                    drop: [ALL]
                resources:
                  requests:
                    cpu: 10m
                    memory: 16Mi
                  limits:
                    cpu: 100m
                    memory: 64Mi
EOF

sandbox_workflow_phase="$(wait_workflow_phase "$SANDBOX_WORKFLOW")" || fail 'Sandbox resource-template workflow timed out'
printf 'phase=%s\n' "$sandbox_workflow_phase" >"$EVIDENCE_DIR/sandbox-workflow-phase.txt"
[[ "$sandbox_workflow_phase" == Succeeded ]] || fail "Sandbox resource-template workflow ended in $sandbox_workflow_phase"
kube -n "$NAMESPACE" get workflow "$SANDBOX_WORKFLOW" -o json >"$EVIDENCE_DIR/sandbox-workflow.json"
jq -e --arg template create-sandbox '[.status.nodes[]? | select(.templateName == $template and .phase == "Succeeded")] | length == 1' "$EVIDENCE_DIR/sandbox-workflow.json" >/dev/null \
  || fail 'Sandbox resource template did not complete successfully'

pod=""
pod_deadline=$((SECONDS + TIMEOUT))
while (( SECONDS < pod_deadline )); do
  pod="$(kube -n "$NAMESPACE" get pods -l "agw.astatide.com/preflight=$RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [[ -n "$pod" ]] && break
  sleep 2
done
[[ -n "$pod" ]] || fail 'Agent Sandbox did not create a pod'
kube -n "$NAMESPACE" get pod "$pod" -o json >"$EVIDENCE_DIR/sandbox-pod.json"
# jq's `//` treats boolean false as empty and would misreport the required
# security setting as unset. Distinguish JSON null from the legitimate false
# value explicitly.
host_users="$(jq -r 'if .spec.hostUsers == null then "unset" else (.spec.hostUsers | tostring) end' "$EVIDENCE_DIR/sandbox-pod.json")"
printf 'pod=%s\nhostUsers=%s\n' "$pod" "$host_users" >"$EVIDENCE_DIR/sandbox-pod-summary.txt"
security_gate_failed=0
if [[ "$host_users" != false ]]; then
  security_gate_failed=1
  printf 'hostUsers gate failed: generated Sandbox pod observed hostUsers=%s\n' "$host_users" >&2
  [[ "$REQUIRE_HOST_USERS" == 0 ]] || fail "generated Sandbox pod did not preserve hostUsers=false (observed $host_users)"
fi

ready=0
finished=0
ready_observed=0
sandbox_deadline=$((SECONDS + TIMEOUT))
while (( SECONDS < sandbox_deadline )); do
  kube -n "$NAMESPACE" get sandbox "$SANDBOX_NAME" -o json >"$EVIDENCE_DIR/sandbox.json" 2>/dev/null || true
  if [[ -s "$EVIDENCE_DIR/sandbox.json" ]]; then
    ready="$(jq -r '[.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length' "$EVIDENCE_DIR/sandbox.json")"
    finished="$(jq -r '[.status.conditions[]? | select(.type == "Finished" and .status == "True")] | length' "$EVIDENCE_DIR/sandbox.json")"
    (( ready > 0 )) && ready_observed=1
    (( ready_observed > 0 && finished > 0 )) && break
  fi
  sleep 2
done
(( ready_observed > 0 )) || fail 'Agent Sandbox never reported Ready=True before terminal state'
(( finished > 0 )) || fail 'Agent Sandbox never reported Finished=True'
printf 'ready_observed=%s\nfinished_true=%s\n' "$ready_observed" "$finished" >"$EVIDENCE_DIR/sandbox-conditions.txt"
kube -n "$NAMESPACE" get sandbox "$SANDBOX_NAME" -o yaml >"$EVIDENCE_DIR/sandbox.yaml"

if (( security_gate_failed )); then
  fail 'functional workload evidence completed, but the hostUsers security gate failed'
fi
printf 'argo live preflight: PASS\n'
printf 'evidence=%s\n' "$EVIDENCE_DIR"
