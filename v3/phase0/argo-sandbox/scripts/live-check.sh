#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'live Phase-0 check failed: %s\n' "$*" >&2
  exit 1
}

if [[ "${AGW_PHASE0_LIVE:-}" != "1" ]]; then
  printf 'refusing live checks: set AGW_PHASE0_LIVE=1 explicitly\n' >&2
  exit 2
fi

command -v kubectl >/dev/null 2>&1 || { printf 'kubectl is required\n' >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { printf 'jq is required for live evidence validation\n' >&2; exit 2; }

# A live check must never inherit the operator's ambient kubectl context. The
# explicit kubeconfig and context are part of the safety boundary, not merely
# convenience flags. The caller is responsible for choosing a disposable
# cluster and deleting it after evidence capture.
kubeconfig="${AGW_PHASE0_KUBECONFIG:-}"
context="${AGW_PHASE0_CONTEXT:-}"
[[ -n "$kubeconfig" && -f "$kubeconfig" ]] || fail 'AGW_PHASE0_KUBECONFIG must name an existing disposable kubeconfig'
[[ -n "$context" ]] || fail 'AGW_PHASE0_CONTEXT must name the disposable kube context'

kube() {
  kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"
}

namespace="${AGW_PHASE0_NAMESPACE:-agw-runs}"
workflow="${AGW_PHASE0_WORKFLOW:-phase0-argo-sandbox}"
service_account="system:serviceaccount:${namespace}:agw-phase0-workflow"
evidence_dir="$(mktemp -d "${TMPDIR:-/tmp}/agw-phase0-evidence.XXXXXX")"

namespace_json="$evidence_dir/namespace.json"
kube get namespace "$namespace" -o json >"$namespace_json"
jq -e '.metadata.labels["agw.astatide.com/phase"] == "0"' "$namespace_json" >/dev/null \
  || fail "namespace $namespace is not labelled as the disposable Phase-0 namespace"

workflow_json="$evidence_dir/workflow.json"
kube -n "$namespace" get workflow "$workflow" -o json >"$workflow_json"
workflow_uid="$(jq -r '.metadata.uid // empty' "$workflow_json")"
[[ "$workflow_uid" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] \
  || fail 'workflow UID is missing or unsafe'
sandbox="phase0-${workflow_uid}"

phase="$(jq -r '.status.phase // empty' "$workflow_json")"
[[ "$phase" == "Succeeded" ]] || fail "Workflow phase is $phase; expected Succeeded after onExit cleanup"

require_succeeded_node() {
  local template="$1"
  jq -e --arg template "$template" '
    [(.status.nodes // {}) | to_entries[] | .value
      | select(.templateName == $template and .phase == "Succeeded")]
    | length >= 1
  ' "$workflow_json" >/dev/null \
    || fail "Workflow has no Succeeded node for template $template"
}

for template in \
  create-workspace clone-setup create-sandbox wait-ready wait-finished \
  capture cleanup delete-sandbox delete-workspace; do
  require_succeeded_node "$template"
done

capture_node="$(jq -c --arg template capture '
  [(.status.nodes // {}) | to_entries[] | .value
    | select(.templateName == $template and .phase == "Succeeded")]
  | last // empty
' "$workflow_json")"
[[ -n "$capture_node" ]] || fail 'capture node output is missing'

capture_parameter() {
  local name="$1"
  jq -r --arg name "$name" '
    (.outputs.parameters // [])
    | map(select(.name == $name))
    | if length == 1 then .[0].value else empty end
  ' <<<"$capture_node"
}

artifact_digest="$(capture_parameter artifact_sha256)"
[[ "$artifact_digest" =~ ^[0-9a-f]{64}$ ]] \
  || fail 'capture did not expose a valid artifact SHA-256 parameter'
printf '%s\n' "$artifact_digest" >"$evidence_dir/artifact-sha256"

jq -e '(.outputs.artifacts // []) | any(.[]; .name == "sandbox-artifact")' <<<"$capture_node" >/dev/null \
  || fail 'capture did not expose the sandbox-artifact output'

ready_marker="$(capture_parameter ready_marker)"
finished_marker="$(capture_parameter finished_marker)"
[[ -n "$ready_marker" && -n "$finished_marker" ]] \
  || fail 'capture did not preserve both identity-bound Sandbox markers'
printf '%s\n' "$ready_marker" >"$evidence_dir/ready.json"
printf '%s\n' "$finished_marker" >"$evidence_dir/finished.json"

validate_marker() {
  local path="$1"
  local condition="$2"
  jq -e --arg namespace "$namespace" --arg name "$sandbox" --arg condition "$condition" '
    (keys | sort) == ["condition", "name", "namespace", "observedAt", "uid", "version"]
    and .version == "agents.astatide.com/phase0-marker/v1"
    and .namespace == $namespace
    and .name == $name
    and .condition == $condition
    and (.uid | type) == "string" and (.uid | length) > 0
    and (.observedAt | type) == "string"
    and (.observedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))
  ' "$path" >/dev/null || fail "invalid identity-bound $condition marker"
}

validate_marker "$evidence_dir/ready.json" Ready
validate_marker "$evidence_dir/finished.json" Finished
ready_uid="$(jq -r '.uid' "$evidence_dir/ready.json")"
finished_uid="$(jq -r '.uid' "$evidence_dir/finished.json")"
[[ "$ready_uid" == "$finished_uid" ]] || fail 'Ready and Finished markers identify different Sandbox UIDs'

assert_absent() {
	local kind="$1"
	local name="$2"
	local output
	if output="$(kube -n "$namespace" get "$kind" "$name" -o name 2>&1)"; then
		fail "$kind $namespace/$name remains after onExit cleanup"
	fi
	if ! grep -qiE 'not found|notfound|could not find the requested resource' <<<"$output"; then
		fail "could not prove $kind $namespace/$name was cleaned up: $output"
	fi
}

# The post-completion check intentionally expects the resources to be gone. A
# pre-cleanup Sandbox read here would race onExit and would not prove cleanup.
assert_absent sandbox "$sandbox"
assert_absent pvc "phase0-${workflow_uid}-workspace"

can() {
  local action="$1"
  local resource="$2"
  local scope_args=("$action" "$resource")
  shift 2
	local result
	if ! result="$(kube auth can-i --as="$service_account" "${scope_args[@]}" "$@" 2>&1)"; then
		fail "could not evaluate whether $service_account can $action $resource: $result"
	fi
	[[ "$result" == "yes" ]] || fail "$service_account cannot $action $resource"
}

cannot() {
  local action="$1"
  local resource="$2"
  shift 2
	local result
	if ! result="$(kube auth can-i --as="$service_account" "$action" "$resource" "$@" 2>&1)"; then
		fail "could not evaluate whether $service_account can $action $resource: $result"
	fi
	[[ "$result" == "no" ]] || fail "$service_account unexpectedly can $action $resource"
}

for permission in \
  "create sandboxes.agents.x-k8s.io" \
  "get sandboxes.agents.x-k8s.io" \
  "watch sandboxes.agents.x-k8s.io" \
  "patch sandboxes.agents.x-k8s.io" \
  "delete sandboxes.agents.x-k8s.io" \
  "create persistentvolumeclaims" \
  "get persistentvolumeclaims" \
  "watch persistentvolumeclaims" \
  "patch persistentvolumeclaims" \
  "delete persistentvolumeclaims" \
  "create workflowtaskresults.argoproj.io" \
  "patch workflowtaskresults.argoproj.io"; do
  read -r action resource <<<"$permission"
  can "$action" "$resource" -n "$namespace"
done

cannot get secrets -n "$namespace"
cannot list secrets -n "$namespace"
cannot get nodes
cannot create namespaces
cannot get pods -n "$namespace"

printf '%s\n' "$ready_uid" >"$evidence_dir/sandbox-uid"

printf 'live read-only checks completed; evidence directory: %s\n' "$evidence_dir"
