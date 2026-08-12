#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

fail() {
  printf 'static verification failed: %s\n' "$*" >&2
  exit 1
}

require_file() {
  [[ -f "$root_dir/$1" ]] || fail "missing $1"
}

require_match() {
  local file="$1"
  local pattern="$2"
  rg -q -- "$pattern" "$root_dir/$file" || fail "$file does not contain: $pattern"
}

for file in \
  kustomization.yaml namespace.yaml rbac.yaml workflow-template.yaml workflow.yaml \
  versions.env bridge/waitbridge.go bridge/waitbridge_test.go bridge/Containerfile \
  cmd/waitbridge/main.go README.md; do
  require_file "$file"
done

require_match versions.env '^ARGO_WORKFLOWS_VERSION=v4\.1\.0$'
require_match versions.env '^AGENT_SANDBOX_VERSION=v0\.5\.4$'
require_match versions.env '^ARGO_WORKFLOWS_COMMIT=e5ed20d5cb54d4708d5aeb29148b3e49922f795c$'
require_match versions.env '^AGENT_SANDBOX_COMMIT=6e2b7617310e3bf084b6d1a1cffbeb141a5e37fe$'
require_match versions.env '^ARGO_WORKFLOWS_INSTALL_SHA256=[0-9a-f]{64}$'
require_match versions.env '^AGENT_SANDBOX_MANIFEST_SHA256=[0-9a-f]{64}$'
require_match versions.env '^AGENT_SANDBOX_CONTROLLER_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_match versions.env '^BRIDGE_BUILD_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_match versions.env '^WORKFLOW_ALPINE_IMAGE=.*@sha256:[0-9a-f]{64}$'

require_match kustomization.yaml 'resources:'
require_match kustomization.yaml 'workflow-template.yaml'
require_match kustomization.yaml 'workflow.yaml'
require_match workflow.yaml '^  name: phase0-argo-sandbox$'
require_match workflow-template.yaml '^  onExit: cleanup$'
require_match workflow-template.yaml '^  activeDeadlineSeconds: 900$'
require_match workflow-template.yaml 'shutdownTime:'
require_match workflow-template.yaml 'shutdownPolicy: Delete'
require_match workflow-template.yaml 'action: apply'
require_match workflow-template.yaml 'action: delete'
require_match workflow-template.yaml 'setOwnerReference: true'
require_match workflow-template.yaml 'name: "phase0-\{\{workflow.uid\}\}"'
require_match workflow-template.yaml 'name: "phase0-\{\{workflow.uid\}\}-workspace"'
require_match workflow-template.yaml '^                hostUsers: false$'
require_match workflow-template.yaml '^                  value: Ready$'
require_match workflow-template.yaml '^                  value: Finished$'
require_match workflow-template.yaml 'value: /workspace/.agw/ready.json'
require_match workflow-template.yaml 'outputs:'
require_match workflow-template.yaml 'artifacts:'
require_match workflow-template.yaml 'name: ready_marker'
require_match workflow-template.yaml 'name: finished_marker'
require_match workflow-template.yaml 'path: /workspace/.agw/ready.json'
require_match workflow-template.yaml 'path: /workspace/.agw/finished.json'
require_match workflow-template.yaml 'retryStrategy:'
require_match workflow-template.yaml 'imagePullPolicy: Never'

require_match scripts/live-check.sh 'AGW_PHASE0_KUBECONFIG'
require_match scripts/live-check.sh 'AGW_PHASE0_CONTEXT'
require_match scripts/live-check.sh 'status.phase'
require_match scripts/live-check.sh 'sandbox-artifact'

if rg -n --glob '*.yaml' --glob '*.yml' -- 'generateName:' "$root_dir"; then
  fail 'generated names are forbidden; Phase 0 uses deterministic object identities'
fi
if rg -n --glob '*.yaml' --glob '*.yml' -- 'workflow\.parameters\.(sandbox_name|workspace_pvc)' "$root_dir"; then
  fail 'child identities must derive from workflow.uid, not caller-controlled parameters'
fi
if rg -n -U -- 'kind: ClusterRole|kind: ClusterRoleBinding' "$root_dir/rbac.yaml"; then
  fail 'Phase 0 RBAC must remain namespaced'
fi
if rg -n -U -- '^[[:space:]]+- (pods|secrets|configmaps|jobs|nodes|namespaces)$' "$root_dir/rbac.yaml"; then
  fail 'workflow Role grants an unapproved resource'
fi
if rg -n --glob '*.yaml' --glob '*.yml' -- 'EventSource|Sensor|EventBus|argo-events' "$root_dir"; then
  fail 'Argo Events is intentionally out of Phase 0'
fi
if rg -n -- 'InsecureSkipVerify|insecure-skip|--insecure' "$root_dir/bridge"; then
  fail 'bridge TLS must not permit insecure transport'
fi
if rg -n -- 'image:[^#]*:latest([@[:space:]]|$)' "$root_dir"; then
  fail 'floating latest image tag found'
fi

while IFS= read -r image_line; do
  if [[ "$image_line" == *"agw-sandbox-wait-bridge:phase0"* ]]; then
    continue
  fi
  [[ "$image_line" == *"@sha256:"* ]] || fail "un-pinned image: $image_line"
done < <(rg --no-heading '^[[:space:]]+image:' "$root_dir")

if [[ "${ARGO_LINT:-0}" == "1" ]]; then
  command -v argo >/dev/null 2>&1 || fail 'ARGO_LINT=1 requires the pinned Argo CLI in PATH'
  argo lint --offline --strict --no-color \
    "$root_dir/workflow-template.yaml" "$root_dir/workflow.yaml"
else
  printf 'Argo schema lint skipped (set ARGO_LINT=1 with Argo CLI v4.1.0 in PATH)\n'
fi

printf 'static verification passed: manifests, pins, RBAC boundaries, bridge wiring, and no-live-cluster guardrails\n'
