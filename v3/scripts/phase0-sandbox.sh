#!/usr/bin/env bash
set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-sandbox
AGENT_SANDBOX_VERSION="${AGENT_SANDBOX_VERSION:-v0.5.4}"
AGENT_SANDBOX_INSTALL_URL="${AGENT_SANDBOX_INSTALL_URL:-https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox.yaml}"
AGENT_SANDBOX_MANIFEST_PATH="${AGENT_SANDBOX_MANIFEST_PATH:-}"
AGENT_SANDBOX_MANIFEST_SHA256="${AGENT_SANDBOX_MANIFEST_SHA256:-}"
AGENT_SANDBOX_CONTROLLER_IMAGE="${AGENT_SANDBOX_CONTROLLER_IMAGE:-}"
AGENT_SANDBOX_FIELD_MANAGER="${AGENT_SANDBOX_FIELD_MANAGER:-agw-phase0-agent-sandbox}"
PHASE0_BASE_IMAGE="${PHASE0_BASE_IMAGE:-docker.io/library/busybox:1.36.1}"
PHASE0_STORAGE_CLASS="${PHASE0_STORAGE_CLASS:-local-path}"
PHASE0_WORKSPACE_SIZE="${PHASE0_WORKSPACE_SIZE:-256Mi}"
PHASE0_NODE_SELECTOR_KEY="${PHASE0_NODE_SELECTOR_KEY:-agw.astatide.com/agents}"
PHASE0_NODE_SELECTOR_VALUE="${PHASE0_NODE_SELECTOR_VALUE:-true}"
PHASE0_TOLERATION_KEY="${PHASE0_TOLERATION_KEY:-agw.astatide.com/agents}"
PHASE0_TOLERATION_VALUE="${PHASE0_TOLERATION_VALUE:-true}"
PHASE0_SANDBOX_NAME="${PHASE0_SANDBOX_NAME:-agw-phase0-sandbox}"
PHASE0_SHUTDOWN_AFTER="${PHASE0_SHUTDOWN_AFTER:-90}"
PHASE0_EXPECT_PVC_DELETE="${PHASE0_EXPECT_PVC_DELETE:-true}"
PHASE0_SANDBOX_TIMEOUT="${PHASE0_SANDBOX_TIMEOUT:-90}"
PHASE0_INSTALL_UPSTREAM=0

usage() {
  cat <<'EOF'
Usage: phase0-sandbox.sh [common options] [--install-upstream]

Tests a direct agents.x-k8s.io/v1beta1 Sandbox against a pinned Agent Sandbox
release (default v0.5.4). It records readiness, generated Pod/Service/PVC/PV,
suspend/resume, shutdownTime/shutdownPolicy, and local-path cleanup. It never
removes the upstream controller or CRDs.

--install-upstream      Fetch/apply the pinned core release asset. In apply
                        mode this requires AGENT_SANDBOX_MANIFEST_SHA256 or a
                        local AGENT_SANDBOX_MANIFEST_PATH, plus an immutable
                        AGENT_SANDBOX_CONTROLLER_IMAGE reference.
EOF
  phase0_usage_common
}

phase0_parse_common_args "$@" || exit 2
remaining=()
for arg in "${PHASE0_REMAINING_ARGS[@]}"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    --install-upstream) PHASE0_INSTALL_UPSTREAM=1 ;;
    *) remaining+=("$arg") ;;
  esac
done
if ((${#remaining[@]})); then
  phase0_die "unknown argument: ${remaining[0]}"
  exit 2
fi
if [[ ! "$AGENT_SANDBOX_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  phase0_die "AGENT_SANDBOX_VERSION must be a concrete vX.Y.Z tag"
  exit 2
fi
expected_install_url="https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${AGENT_SANDBOX_VERSION}/sandbox.yaml"
if [[ -z "$AGENT_SANDBOX_MANIFEST_PATH" && "$AGENT_SANDBOX_INSTALL_URL" != "$expected_install_url" ]]; then
  phase0_die "remote Agent Sandbox installs must use the release asset on github.com/kubernetes-sigs/agent-sandbox"
  exit 2
fi
if [[ ! "$PHASE0_EXPECT_PVC_DELETE" =~ ^(true|false)$ ]]; then
  phase0_die "PHASE0_EXPECT_PVC_DELETE must be true or false"
  exit 2
fi
if ! [[ "$PHASE0_SHUTDOWN_AFTER" =~ ^[1-9][0-9]*$ ]]; then
  phase0_die "PHASE0_SHUTDOWN_AFTER must be a positive integer"
  exit 2
fi
if ! [[ "$PHASE0_SANDBOX_TIMEOUT" =~ ^[1-9][0-9]*$ ]]; then
  phase0_die "PHASE0_SANDBOX_TIMEOUT must be a positive integer"
  exit 2
fi
for pair in \
  "install-url=$AGENT_SANDBOX_INSTALL_URL" \
  "manifest-path=$AGENT_SANDBOX_MANIFEST_PATH" \
  "manifest-sha=$AGENT_SANDBOX_MANIFEST_SHA256" \
  "field-manager=$AGENT_SANDBOX_FIELD_MANAGER" \
  "base-image=$PHASE0_BASE_IMAGE" \
  "storage-class=$PHASE0_STORAGE_CLASS" \
  "workspace-size=$PHASE0_WORKSPACE_SIZE" \
  "node-selector-key=$PHASE0_NODE_SELECTOR_KEY" \
  "node-selector-value=$PHASE0_NODE_SELECTOR_VALUE" \
  "toleration-key=$PHASE0_TOLERATION_KEY" \
  "toleration-value=$PHASE0_TOLERATION_VALUE" \
  "sandbox-name=$PHASE0_SANDBOX_NAME"; do
  phase0_validate_single_line "${pair%%=*}" "${pair#*=}" || exit 2
done
phase0_validate_image_ref base-image "$PHASE0_BASE_IMAGE" || exit 2
if [[ ! "$AGENT_SANDBOX_FIELD_MANAGER" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]]; then
  phase0_die "AGENT_SANDBOX_FIELD_MANAGER must be a bounded field-manager name"
  exit 2
fi
phase0_validate_dns_name sandbox-name "$PHASE0_SANDBOX_NAME" || exit 2
if [[ -n "$AGENT_SANDBOX_MANIFEST_SHA256" && ! "$AGENT_SANDBOX_MANIFEST_SHA256" =~ ^[a-fA-F0-9]{64}$ ]]; then
  phase0_die "AGENT_SANDBOX_MANIFEST_SHA256 must be 64 hexadecimal characters"
  exit 2
fi
if (( PHASE0_INSTALL_UPSTREAM )) && [[ "$PHASE0_MODE" == apply ]] && [[ -z "$AGENT_SANDBOX_CONTROLLER_IMAGE" ]]; then
  phase0_die "AGENT_SANDBOX_CONTROLLER_IMAGE is required with --install-upstream"
  exit 2
fi
if [[ -n "$AGENT_SANDBOX_CONTROLLER_IMAGE" && ! "$AGENT_SANDBOX_CONTROLLER_IMAGE" =~ ^[^[:space:]@]+@sha256:[a-f0-9]{64}$ ]]; then
  phase0_die "AGENT_SANDBOX_CONTROLLER_IMAGE must be an immutable image@sha256:digest reference"
  exit 2
fi
phase0_init_evidence || exit 2

manifest="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-sandbox.XXXXXX.yaml")"
upstream="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-agent-sandbox.XXXXXX.yaml")"
trap 'rm -f -- "$manifest" "$upstream"' EXIT
shutdown_time="$(date -u -d "+${PHASE0_SHUTDOWN_AFTER} seconds" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"
[[ -n "$shutdown_time" ]] || { phase0_die "GNU date with UTC support is required"; exit 2; }
phase0_render_template \
  "$SCRIPT_DIR/../test/e2e/phase0/40-sandbox-lifecycle.yaml" "$manifest" \
  "PHASE0_NAMESPACE=$PHASE0_NAMESPACE" \
  "PHASE0_RUN_ID=$PHASE0_RUN_ID" \
  "PHASE0_SANDBOX_NAME=$PHASE0_SANDBOX_NAME" \
  "PHASE0_SHUTDOWN_TIME=$shutdown_time" \
  "PHASE0_BASE_IMAGE=$PHASE0_BASE_IMAGE" \
  "PHASE0_STORAGE_CLASS=$PHASE0_STORAGE_CLASS" \
  "PHASE0_WORKSPACE_SIZE=$PHASE0_WORKSPACE_SIZE" \
  "PHASE0_NODE_SELECTOR_KEY=$PHASE0_NODE_SELECTOR_KEY" \
  "PHASE0_NODE_SELECTOR_VALUE=$PHASE0_NODE_SELECTOR_VALUE" \
  "PHASE0_TOLERATION_KEY=$PHASE0_TOLERATION_KEY" \
  "PHASE0_TOLERATION_VALUE=$PHASE0_TOLERATION_VALUE"

delete_sandbox_resources() {
  local output="$PHASE0_RUN_DIR/sandbox-custom-resource-cleanup.txt"
  if phase0_kubectl -n "$PHASE0_NAMESPACE" delete sandboxes.agents.x-k8s.io \
    -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" --ignore-not-found --wait=true >"$output" 2>&1; then
    phase0_sanitize_file "$output"
    phase0_result PASS cleanup-sandbox-cr "owned Sandbox resources deleted"
  else
    phase0_sanitize_file "$output"
    phase0_result FAIL cleanup-sandbox-cr "owned Sandbox resources could not be deleted"
  fi
}

pin_controller_image() {
  local source="$1" target="$2" replacement="$3" pinned="$2.pinned.$$" count
  if ! awk -v version="$AGENT_SANDBOX_VERSION" -v image="$replacement" '
    BEGIN { found = 0 }
    $0 ~ "^[[:space:]]*image:[[:space:]]*registry[.]k8s[.]io/agent-sandbox/agent-sandbox-controller:" version "[[:space:]]*$" {
      print "        image: " image
      found++
      next
    }
    { print }
    END { print found > "/dev/stderr" }
  ' "$source" >"$pinned" 2>"$pinned.count"; then
    rm -f -- "$pinned" "$pinned.count"
    return 1
  fi
  count="$(<"$pinned.count")"
  if [[ "$count" != 1 ]]; then
    rm -f -- "$pinned" "$pinned.count"
    return 1
  fi
  if ! mv -- "$pinned" "$target"; then
    rm -f -- "$pinned" "$pinned.count"
    return 1
  fi
  rm -f -- "$pinned.count"
}

if [[ "$PHASE0_MODE" == plan || "$PHASE0_MODE" == dry-run ]]; then
  phase0_manifest_action "$manifest" sandbox-lifecycle
  phase0_result SKIP upstream-agent-sandbox "live installation not attempted; pinned version=${AGENT_SANDBOX_VERSION}"
  phase0_finish
  exit $?
fi

if [[ "$PHASE0_MODE" == cleanup ]]; then
  if phase0_cluster_ready; then
    delete_sandbox_resources
    phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" sandbox-lifecycle
  fi
  phase0_finish
  exit $?
fi

if ! phase0_cluster_ready; then
  phase0_finish
  exit $?
fi

if (( PHASE0_INSTALL_UPSTREAM )); then
	if [[ -z "$AGENT_SANDBOX_MANIFEST_SHA256" ]]; then
	  phase0_result FAIL upstream-agent-sandbox "a pinned manifest SHA-256 is required for both local and remote installation"
	elif [[ -n "$AGENT_SANDBOX_MANIFEST_PATH" ]]; then
    if [[ ! -f "$AGENT_SANDBOX_MANIFEST_PATH" ]]; then
      phase0_result FAIL upstream-agent-sandbox "AGENT_SANDBOX_MANIFEST_PATH does not exist"
    elif ! cp -- "$AGENT_SANDBOX_MANIFEST_PATH" "$upstream"; then
      phase0_result FAIL upstream-agent-sandbox "local Agent Sandbox manifest could not be copied"
    fi
  else
    if ! phase0_require_cmd curl; then
      :
    elif ! curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 "$AGENT_SANDBOX_INSTALL_URL" -o "$upstream" 2>"$PHASE0_RUN_DIR/upstream-fetch-error.txt"; then
      phase0_sanitize_file "$PHASE0_RUN_DIR/upstream-fetch-error.txt"
      phase0_result FAIL upstream-agent-sandbox "pinned release asset could not be fetched"
    fi
  fi
  if [[ -s "$upstream" ]]; then
    actual_sha="$(sha256sum "$upstream" | awk '{print $1}')"
    printf 'version=%s\nurl=%s\nsha256=%s\n' "$AGENT_SANDBOX_VERSION" "$AGENT_SANDBOX_INSTALL_URL" "$actual_sha" >"$PHASE0_RUN_DIR/upstream-manifest.txt"
    if [[ -n "$AGENT_SANDBOX_MANIFEST_SHA256" && "$actual_sha" != "$AGENT_SANDBOX_MANIFEST_SHA256" ]]; then
      phase0_result FAIL upstream-agent-sandbox "pinned manifest digest mismatch"
    elif ! pin_controller_image "$upstream" "$upstream" "$AGENT_SANDBOX_CONTROLLER_IMAGE"; then
      phase0_result FAIL upstream-agent-sandbox "pinned controller image was not found exactly once in the release manifest"
    # The v0.5.4 CRD is larger than Kubernetes' client-side
    # last-applied-configuration annotation limit. Server-side apply avoids
    # that annotation while keeping the field owner explicit and reviewable.
    elif phase0_kubectl apply --server-side --field-manager="$AGENT_SANDBOX_FIELD_MANAGER" -f "$upstream" >"$PHASE0_RUN_DIR/upstream-apply.txt" 2>&1; then
      phase0_sanitize_file "$PHASE0_RUN_DIR/upstream-apply.txt"
      if phase0_kubectl get crd/sandboxes.agents.x-k8s.io >"$PHASE0_RUN_DIR/upstream-crd.txt" 2>&1; then
        phase0_sanitize_file "$PHASE0_RUN_DIR/upstream-crd.txt"
        printf 'controller_image=%s\n' "$AGENT_SANDBOX_CONTROLLER_IMAGE" >"$PHASE0_RUN_DIR/controller-image.txt"
        phase0_result PASS upstream-agent-sandbox "pinned ${AGENT_SANDBOX_VERSION} core manifest applied with server-side ownership"
      else
        phase0_sanitize_file "$PHASE0_RUN_DIR/upstream-crd.txt"
        phase0_result FAIL upstream-agent-sandbox "manifest apply returned success but Sandbox CRD was not created"
      fi
    else
      phase0_sanitize_file "$PHASE0_RUN_DIR/upstream-apply.txt"
      phase0_result FAIL upstream-agent-sandbox "pinned Agent Sandbox manifest apply failed"
    fi
  fi
fi

if (( PHASE0_FAIL_COUNT > 0 )); then
  phase0_finish
  exit $?
fi

# A release manifest can create the CRD and controller in one apply, but the
# API discovery cache may not expose the Sandbox kind immediately. Wait for
# the CRD to be established before applying the first Sandbox resource so a
# live run does not race discovery and report a false lifecycle failure.
crd_wait="$PHASE0_RUN_DIR/sandbox-crd-wait.txt"
if phase0_kubectl wait --for=condition=Established crd/sandboxes.agents.x-k8s.io --timeout=60s >"$crd_wait" 2>&1; then
  phase0_sanitize_file "$crd_wait"
  phase0_result PASS sandbox-crd "Sandbox CRD is established and discoverable"
else
  phase0_sanitize_file "$crd_wait"
  phase0_result FAIL sandbox-crd "Sandbox CRD did not become established"
  phase0_finish
  exit $?
fi

controller_image="$(phase0_kubectl -n agent-sandbox-system get deployment agent-sandbox-controller -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
expected_controller_image="$AGENT_SANDBOX_CONTROLLER_IMAGE"
if [[ -z "$expected_controller_image" ]]; then
  expected_controller_image="registry.k8s.io/agent-sandbox/agent-sandbox-controller:${AGENT_SANDBOX_VERSION}"
fi
if [[ "$controller_image" == "$expected_controller_image" ]]; then
  printf 'expected_version=%s\ncontroller_image=%s\n' "$AGENT_SANDBOX_VERSION" "$controller_image" >"$PHASE0_RUN_DIR/controller-version.txt"
  phase0_result PASS upstream-version "Agent Sandbox controller identity was recorded"
else
  phase0_result FAIL upstream-version "Agent Sandbox controller version does not match ${AGENT_SANDBOX_VERSION}"
fi

phase0_manifest_action "$manifest" sandbox-lifecycle
if (( PHASE0_FAIL_COUNT > 0 )); then
  phase0_finish
  exit $?
fi

wait_for_pod() {
  local deadline=$((SECONDS + PHASE0_SANDBOX_TIMEOUT))
  local pod phase
  while (( SECONDS < deadline )); do
    pod="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pods -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID,agw.astatide.com/phase0-check=sandbox-pod" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "$pod" ]]; then
      phase="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
      if [[ "$phase" == Running ]]; then
        printf '%s\n' "$pod"
        return 0
      fi
      if [[ "$phase" == Failed ]]; then
        printf '%s\n' "$pod" >"$PHASE0_RUN_DIR/sandbox-pod-failed.txt"
        return 1
      fi
    fi
    sleep 2
  done
  return 1
}

wait_for_sandbox_condition() {
  local condition="$1" expected_reason="${2:-}" deadline=$((SECONDS + PHASE0_SANDBOX_TIMEOUT))
  local status reason
  while (( SECONDS < deadline )); do
    case "$condition" in
      Ready)
        status="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}' 2>/dev/null || true)"
        reason="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.reason}{"\n"}{end}' 2>/dev/null || true)"
        ;;
      Finished)
        status="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o jsonpath='{range .status.conditions[?(@.type=="Finished")]}{.status}{"\n"}{end}' 2>/dev/null || true)"
        reason="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o jsonpath='{range .status.conditions[?(@.type=="Finished")]}{.reason}{"\n"}{end}' 2>/dev/null || true)"
        ;;
      *)
        return 1
        ;;
    esac
    if [[ "$status" == True && ( -z "$expected_reason" || "$reason" == "$expected_reason" ) ]]; then
      return 0
    fi
    sleep 2
  done
  return 1
}

sandbox_pod="$(wait_for_pod || true)"
if [[ -n "$sandbox_pod" ]]; then
	phase0_result PASS sandbox-ready "Sandbox generated a Running pod"
	if wait_for_sandbox_condition Ready; then
		phase0_result PASS sandbox-ready-condition "Sandbox reported Ready=True"
	else
		phase0_result FAIL sandbox-ready-condition "Sandbox did not report Ready=True"
	fi
	phase0_kubectl -n "$PHASE0_NAMESPACE" get pod "$sandbox_pod" -o yaml >"$PHASE0_RUN_DIR/sandbox-pod.yaml" 2>&1 || true
	phase0_sanitize_file "$PHASE0_RUN_DIR/sandbox-pod.yaml"
	child_host_users="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pod "$sandbox_pod" -o jsonpath='{.spec.hostUsers}' 2>/dev/null || true)"
	if [[ "$child_host_users" == false ]]; then
		phase0_result PASS sandbox-host-users "generated pod preserves hostUsers=false"
	else
		phase0_result FAIL sandbox-host-users "generated pod did not preserve hostUsers=false"
	fi
	if phase0_kubectl -n "$PHASE0_NAMESPACE" exec "pod/$sandbox_pod" -- cat /proc/self/uid_map >"$PHASE0_RUN_DIR/sandbox-uid-map.txt" 2>&1; then
		phase0_sanitize_file "$PHASE0_RUN_DIR/sandbox-uid-map.txt"
		if awk '$1 == 0 && $2 > 0 && $3 >= 65536 { found=1 } END { exit(found ? 0 : 1) }' "$PHASE0_RUN_DIR/sandbox-uid-map.txt"; then
			phase0_result PASS sandbox-uid-map "generated pod uses a subordinate UID mapping"
		else
			phase0_result FAIL sandbox-uid-map "generated pod uid_map did not prove user-namespace isolation"
		fi
	else
		phase0_sanitize_file "$PHASE0_RUN_DIR/sandbox-uid-map.txt"
		phase0_result FAIL sandbox-uid-map "generated pod uid_map could not be inspected"
	fi
else
  phase0_result FAIL sandbox-ready "Sandbox did not generate a Running pod"
fi

for resource in pods pvc; do
  file="$PHASE0_RUN_DIR/${resource}.txt"
  count="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get "$resource" -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d '[:space:]')"
  if [[ "$count" =~ ^[1-9][0-9]*$ ]] && phase0_kubectl -n "$PHASE0_NAMESPACE" get "$resource" -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" -o wide >"$file" 2>&1; then
	phase0_sanitize_file "$file"
	phase0_result PASS "generated-${resource}" "observed ${count} generated ${resource} resource(s)"
  else
	phase0_sanitize_file "$file"
	phase0_result FAIL "generated-${resource}" "required generated ${resource} resource was not observed"
  fi
done

# Agent Sandbox propagates user labels to the generated Pod and PVC, but the
# generated Service is identified authoritatively by status.service rather
# than by the user label set. Use that exact controller-owned name instead of
# treating a label-list miss as proof that the Service was not created.
service_name="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o jsonpath='{.status.service}' 2>/dev/null || true)"
service_file="$PHASE0_RUN_DIR/svc.txt"
if [[ -n "$service_name" ]] && phase0_kubectl -n "$PHASE0_NAMESPACE" get service "$service_name" -o wide >"$service_file" 2>&1; then
  phase0_sanitize_file "$service_file"
  phase0_result PASS generated-svc "observed controller-reported Service ${service_name}"
else
  phase0_sanitize_file "$service_file"
  phase0_result FAIL generated-svc "Sandbox did not report an existing generated Service"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" patch sandbox "$PHASE0_SANDBOX_NAME" --type=merge -p '{"spec":{"operatingMode":"Suspended"}}' >"$PHASE0_RUN_DIR/suspend.txt" 2>&1; then
  phase0_sanitize_file "$PHASE0_RUN_DIR/suspend.txt"
  phase0_result PASS sandbox-suspend "Sandbox accepted operatingMode=Suspended"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/suspend.txt"
  phase0_result FAIL sandbox-suspend "Sandbox did not accept suspend operation"
fi
if phase0_kubectl -n "$PHASE0_NAMESPACE" patch sandbox "$PHASE0_SANDBOX_NAME" --type=merge -p '{"spec":{"operatingMode":"Running"}}' >"$PHASE0_RUN_DIR/resume.txt" 2>&1; then
  phase0_sanitize_file "$PHASE0_RUN_DIR/resume.txt"
  phase0_result PASS sandbox-resume "Sandbox accepted operatingMode=Running"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/resume.txt"
  phase0_result FAIL sandbox-resume "Sandbox did not accept resume operation"
fi

if wait_for_sandbox_condition Finished PodSucceeded; then
  phase0_result PASS sandbox-finished "Sandbox reported Finished=True/PodSucceeded"
else
  phase0_result FAIL sandbox-finished "Sandbox did not report Finished=True/PodSucceeded"
fi
phase0_kubectl -n "$PHASE0_NAMESPACE" get sandbox "$PHASE0_SANDBOX_NAME" -o yaml >"$PHASE0_RUN_DIR/sandbox.yaml" 2>&1 || true
phase0_sanitize_file "$PHASE0_RUN_DIR/sandbox.yaml"

node_name=""
if [[ -n "$sandbox_pod" ]]; then
  node_name="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pod "$sandbox_pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
fi
pvc_name="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pvc -l "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
pv_name=""
host_path=""
if [[ -n "$pvc_name" ]]; then
  pv_name="$(phase0_kubectl -n "$PHASE0_NAMESPACE" get pvc "$pvc_name" -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)"
  if [[ -n "$pv_name" ]]; then
    host_path="$(phase0_kubectl get pv "$pv_name" -o jsonpath='{.spec.local.path}' 2>/dev/null || true)"
    if [[ -z "$host_path" ]]; then
      phase0_result SKIP local-path "PV is not a local-path PV; host-directory check is not applicable"
    else
      phase0_result PASS local-path "local PV path recorded without reading its contents"
      printf 'node=%s\npvc=%s\npv=%s\nhost_path=%s\n' "$node_name" "$pvc_name" "$pv_name" >"$PHASE0_RUN_DIR/storage.txt"
    fi
  fi
else
	phase0_result FAIL local-path "PVC was not generated before shutdown"
fi

if phase0_kubectl -n "$PHASE0_NAMESPACE" patch sandbox "$PHASE0_SANDBOX_NAME" --type=merge -p "{\"spec\":{\"shutdownTime\":\"$(date -u -d '+8 seconds' +%Y-%m-%dT%H:%M:%SZ)\"}}" >"$PHASE0_RUN_DIR/shutdown-patch.txt" 2>&1; then
  phase0_sanitize_file "$PHASE0_RUN_DIR/shutdown-patch.txt"
  phase0_result PASS sandbox-shutdown-time "shutdownTime update accepted"
else
  phase0_sanitize_file "$PHASE0_RUN_DIR/shutdown-patch.txt"
  phase0_result FAIL sandbox-shutdown-time "shutdownTime update was rejected"
fi

deadline=$((SECONDS + PHASE0_SANDBOX_TIMEOUT))
deleted=0
delete_probe_error=0
while (( SECONDS < deadline )); do
  if phase0_resource_absent sandbox "$PHASE0_SANDBOX_NAME" "$PHASE0_NAMESPACE"; then
    deleted=1
    break
  elif [[ $? -eq 2 ]]; then
    delete_probe_error=1
    break
  fi
  sleep 2
done
if (( delete_probe_error )); then
  phase0_result FAIL sandbox-shutdown "could not prove Sandbox deletion because the API read failed"
elif (( deleted )); then
  phase0_result PASS sandbox-shutdown "Sandbox was deleted at shutdownTime with shutdownPolicy=Delete"
else
  phase0_result FAIL sandbox-shutdown "Sandbox remained after shutdownTime"
fi

children_deadline=$((SECONDS + PHASE0_SANDBOX_TIMEOUT))
pod_deleted=0
pvc_deleted=0
child_probe_error=0
while (( SECONDS < children_deadline )); do
  if [[ -n "$sandbox_pod" ]]; then
    if phase0_resource_absent pod "$sandbox_pod" "$PHASE0_NAMESPACE"; then
      pod_deleted=1
    elif [[ $? -eq 2 ]]; then
      child_probe_error=1
      break
    fi
  fi
  if [[ -n "$pvc_name" ]]; then
    if phase0_resource_absent pvc "$pvc_name" "$PHASE0_NAMESPACE"; then
      pvc_deleted=1
    elif [[ $? -eq 2 ]]; then
      child_probe_error=1
      break
    fi
  fi
  pod_done=0
  pvc_done=0
  [[ -z "$sandbox_pod" || "$pod_deleted" == 1 ]] && pod_done=1
  [[ -z "$pvc_name" || "$pvc_deleted" == 1 ]] && pvc_done=1
  (( pod_done == 1 && pvc_done == 1 )) && break
  sleep 2
done
if (( child_probe_error )); then
  phase0_result FAIL cleanup-pod "could not prove generated Pod deletion because the API read failed"
elif [[ -z "$sandbox_pod" || "$pod_deleted" == 1 ]]; then
  phase0_result PASS cleanup-pod "generated Pod was deleted"
else
  phase0_result FAIL cleanup-pod "generated Pod remained after shutdown cleanup"
fi
if (( child_probe_error )); then
  phase0_result FAIL cleanup-pvc "could not prove PVC deletion because the API read failed"
elif [[ "$PHASE0_EXPECT_PVC_DELETE" == true ]]; then
	if [[ -n "$pvc_name" && "$pvc_deleted" == 1 ]]; then
	  phase0_result PASS cleanup-pvc "PVC was deleted under shutdownPolicy=Delete"
	elif [[ -z "$pvc_name" ]]; then
	  phase0_result FAIL cleanup-pvc "PVC was never observed before shutdown"
	else
	  phase0_result FAIL cleanup-pvc "PVC remained after shutdown cleanup"
	fi
else
  phase0_result SKIP cleanup-pvc "PVC deletion expectation disabled by configuration"
fi
if [[ -n "$pv_name" ]]; then
  if phase0_resource_absent pv "$pv_name"; then
    phase0_result PASS cleanup-pv "PV was deleted"
  elif [[ $? -eq 2 ]]; then
    phase0_result FAIL cleanup-pv "could not determine PV cleanup state because the API read failed"
  else
    phase0_result SKIP cleanup-pv "PV may be retained by the storage class"
  fi
else
  phase0_result SKIP cleanup-pv "PV may be retained by the storage class"
fi

if [[ -n "$node_name" && -n "$host_path" && "$host_path" =~ ^/var/lib/[A-Za-z0-9._/-]+$ && "$host_path" != *..* && "$host_path" != *//* ]]; then
  host_probe="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-host-probe.XXXXXX.yaml")"
  trap 'rm -f -- "$manifest" "$upstream" "$host_probe"' EXIT
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Pod' \
    'metadata:' \
    "  name: ${PHASE0_SANDBOX_NAME}-hostcheck" \
    "  namespace: ${PHASE0_NAMESPACE}" \
    '  labels:' \
    "    agw.astatide.com/phase0-run: ${PHASE0_RUN_ID}" \
    '    agw.astatide.com/phase0-check: host-path-cleanup' \
    'spec:' \
    "  nodeName: ${node_name}" \
    '  restartPolicy: Never' \
    '  automountServiceAccountToken: false' \
    '  containers:' \
    '    - name: check' \
    "      image: ${PHASE0_BASE_IMAGE}" \
    '      securityContext:' \
    '        privileged: true' \
    '        runAsUser: 0' \
    '        capabilities:' \
    '          drop: [ALL]' \
    '          add: [DAC_READ_SEARCH]' \
    '      command: ["/bin/sh", "-ceu"]' \
    "      args: [\"test ! -e /host${host_path}; printf 'host_path_absent=true\\\\n'\"]" \
    '      volumeMounts:' \
    '        - name: host-root' \
    '          mountPath: /host' \
    '          readOnly: true' \
    '  volumes:' \
    '    - name: host-root' \
    '      hostPath:' \
    '        path: /' \
    '        type: Directory' >"$host_probe"
  if phase0_kubectl apply -f "$host_probe" >"$PHASE0_RUN_DIR/hostcheck-apply.txt" 2>&1; then
    phase0_kubectl -n "$PHASE0_NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${PHASE0_SANDBOX_NAME}-hostcheck" --timeout=30s >"$PHASE0_RUN_DIR/hostcheck-wait.txt" 2>&1 || true
    if phase0_kubectl -n "$PHASE0_NAMESPACE" logs "pod/${PHASE0_SANDBOX_NAME}-hostcheck" >"$PHASE0_RUN_DIR/hostcheck.log" 2>&1 && grep -q '^host_path_absent=true$' "$PHASE0_RUN_DIR/hostcheck.log"; then
      phase0_result PASS cleanup-host-directory "local-path host directory is absent"
    else
      phase0_sanitize_file "$PHASE0_RUN_DIR/hostcheck.log"
      phase0_result FAIL cleanup-host-directory "local-path host directory remains or probe failed"
    fi
    phase0_kubectl -n "$PHASE0_NAMESPACE" delete pod "${PHASE0_SANDBOX_NAME}-hostcheck" --ignore-not-found --wait=true >"$PHASE0_RUN_DIR/hostcheck-cleanup.txt" 2>&1 || true
  else
    phase0_sanitize_file "$PHASE0_RUN_DIR/hostcheck-apply.txt"
    phase0_result SKIP cleanup-host-directory "privileged node-side host path probe could not be created"
  fi
else
  phase0_result SKIP cleanup-host-directory "no safe local-path host path was available for a node-side check"
fi

if (( ! PHASE0_KEEP )); then
  delete_sandbox_resources
  phase0_delete_label_selector "agw.astatide.com/phase0-run=$PHASE0_RUN_ID" sandbox-lifecycle
fi
phase0_finish
