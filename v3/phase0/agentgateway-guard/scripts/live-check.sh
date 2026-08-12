#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
namespace=agw-phase0-guard
pod=agw-phase0-guard

[[ "${AGW_PHASE0_LIVE:-0}" == 1 ]] || { echo 'set AGW_PHASE0_LIVE=1 to enable the optional live checker' >&2; exit 64; }
kubectl_bin="${AGW_PHASE0_KUBECTL_BIN:-kubectl}"
command -v -- "$kubectl_bin" >/dev/null 2>&1 || { echo "live checker requires kubectl binary: $kubectl_bin" >&2; exit 2; }
kubeconfig="${AGW_PHASE0_KUBECONFIG:-${KUBECONFIG:-}}"
if [[ -n "$kubeconfig" ]]; then
  [[ -f "$kubeconfig" ]] || { echo "AGW_PHASE0_KUBECONFIG does not exist: $kubeconfig" >&2; exit 64; }
fi
expected_context="${AGW_PHASE0_CONTEXT:-}"
[[ -n "$expected_context" ]] || { echo 'AGW_PHASE0_CONTEXT must name the disposable context exactly' >&2; exit 64; }
kubectl_cmd() {
  if [[ -n "$kubeconfig" ]]; then
    "$kubectl_bin" --kubeconfig "$kubeconfig" --context "$expected_context" "$@"
  else
    "$kubectl_bin" --context "$expected_context" "$@"
  fi
}
if [[ -n "$kubeconfig" ]]; then
  current_context="$("$kubectl_bin" --kubeconfig "$kubeconfig" config current-context)"
else
  current_context="$("$kubectl_bin" config current-context)"
fi
[[ "$current_context" == "$expected_context" ]] || { echo "refusing context $current_context; expected $expected_context" >&2; exit 77; }

render_dir="$(mktemp -d /tmp/agw-phase0-guard-render.XXXXXX)"
cleanup_render() { rm -rf -- "$render_dir"; }
trap cleanup_render EXIT
AGENTGATEWAY_IMAGE="${AGENTGATEWAY_IMAGE:-}" "$root_dir/scripts/render.sh" "$render_dir" >/dev/null
kubectl_cmd kustomize "$render_dir" >/dev/null

if [[ "${AGW_PHASE0_APPLY:-0}" != 1 ]]; then
  cat <<EOF
PLAN ONLY — no cluster mutation performed.
context: $current_context
render:  $render_dir
would apply: kubectl apply -k $render_dir
would probe: agent -> guard:8081, agent -/-> gateway:8082, admin:15000,
             stats:15020, readiness:15021, recording:9090, and IPv6 loopback
would inspect: recording /evidence, pod logs for canary absence, and auth can-i
set AGW_PHASE0_APPLY=1 only after reviewing the rendered disposable manifest.
EOF
  exit 0
fi

kubectl_cmd apply -k "$render_dir"
if ! kubectl_cmd -n "$namespace" wait --for=jsonpath='{.status.phase}'=Running "pod/$pod" --timeout=120s; then
  echo 'live-check: pod did not reach Running; collecting exact disposable diagnostics' >&2
  kubectl_cmd -n "$namespace" describe pod "$pod" >&2 || true
  kubectl_cmd -n "$namespace" get events --sort-by=.lastTimestamp >&2 || true
  for init in credential-fixture airlock; do
    kubectl_cmd -n "$namespace" logs "$pod" -c "$init" >&2 || true
  done
  exit 1
fi

expected_worker_node="${AGW_PHASE0_WORKER_NODE:-}"
if [[ -n "$expected_worker_node" ]]; then
  observed_worker_node="$(kubectl_cmd -n "$namespace" get pod "$pod" -o jsonpath='{.spec.nodeName}')"
  [[ "$observed_worker_node" == "$expected_worker_node" ]] || {
    echo "pod scheduled on $observed_worker_node, expected disposable worker $expected_worker_node" >&2
    exit 1
  }
fi
pod_images="$(kubectl_cmd -n "$namespace" get pod "$pod" -o jsonpath='{range .spec.containers[*]}{.name}={.image}{"\n"}{end}')"
expected_agentgateway_image="${AGENTGATEWAY_IMAGE:-}"
[[ -n "$expected_agentgateway_image" ]] || { echo 'AGENTGATEWAY_IMAGE must be set for a live check' >&2; exit 64; }
printf '%s\n' "$pod_images" | grep -Fx "agentgateway=$expected_agentgateway_image" >/dev/null || {
  echo 'live pod does not contain the requested immutable agentgateway image' >&2
  exit 1
}
[[ "$(kubectl_cmd -n "$namespace" get pod "$pod" -o jsonpath='{.spec.hostUsers}')" == false ]] || {
  echo 'live pod hostUsers is not false' >&2
  exit 1
}
[[ "$(kubectl_cmd -n "$namespace" get pod "$pod" -o jsonpath='{.spec.automountServiceAccountToken}')" == false ]] || {
  echo 'live pod automountServiceAccountToken is not false' >&2
  exit 1
}

post_from_agent() {
  local payload="$1"
  kubectl_cmd -n "$namespace" exec "$pod" -c agent -- \
    sh -ceu 'wget -qO- --timeout=4 --header="content-type: application/json" --post-data="$1" http://127.0.0.1:8081/call' sh "$payload"
}

allowed='{"runUID":"phase0-run-uid","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","server":"recording","tool":"record","arguments":{"message":"hello","mode":"safe"},"declaredEffect":"read"}'
if ! allowed_response="$(post_from_agent "$allowed")"; then
  echo 'guard allowed call failed before a successful response; collecting exact disposable diagnostics' >&2
  kubectl_cmd -n "$namespace" describe pod "$pod" >&2 || true
  kubectl_cmd -n "$namespace" get events --sort-by=.lastTimestamp >&2 || true
  for container in agent agw-guard agentgateway recording; do
    kubectl_cmd -n "$namespace" logs "$pod" -c "$container" >&2 || true
  done
  exit 1
fi
[[ "$allowed_response" == *'"allowed":true'* ]] || { echo "guard allowed call failed: $allowed_response" >&2; exit 1; }
[[ "$allowed_response" != *phase0-only-canary* ]] || { echo 'canary leaked to agent' >&2; exit 1; }

denied='{"runUID":"phase0-run-uid","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","server":"recording","tool":"unknown","arguments":{"message":"hello","mode":"safe"},"declaredEffect":"read"}'
if post_from_agent "$denied" >/dev/null 2>&1; then
  echo 'unknown tool was accepted by guard' >&2
  exit 1
fi

denied_arguments='{"runUID":"phase0-run-uid","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","server":"recording","tool":"record","arguments":{"message":"hello","mode":"safe","extra":true},"declaredEffect":"read"}'
if post_from_agent "$denied_arguments" >/dev/null 2>&1; then
  echo 'non-exact JSON arguments were accepted by guard' >&2
  exit 1
fi

denied_write='{"runUID":"phase0-run-uid","specDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","server":"recording","tool":"record","arguments":{"message":"write","mode":"safe"},"declaredEffect":"write"}'
if post_from_agent "$denied_write" >/dev/null 2>&1; then
  echo 'unapproved write was accepted by guard' >&2
  exit 1
fi

if ! kubectl_cmd -n "$namespace" exec "$pod" -c agent -- sh -ceu 'command -v nc >/dev/null 2>&1'; then
  echo 'agent image does not provide the required transport-level nc probe' >&2
  exit 1
fi
expect_direct_denied() {
  local host="$1"
  local port="$2"
  # Do not use wget here: BusyBox wget returns nonzero for an HTTP 4xx/5xx
  # after successfully connecting, which could turn a reachable listener into
  # a false denial. A successful TCP connect is the transport-level evidence
  # that the agent reached the forbidden destination.
  if kubectl_cmd -n "$namespace" exec "$pod" -c agent -- \
    sh -ceu 'nc -w 3 "$1" "$2" </dev/null >/dev/null 2>&1' sh "$host" "$port"; then
    echo "agent reached forbidden destination $host:$port" >&2
    exit 1
  fi
}
while read -r host port; do
  expect_direct_denied "$host" "$port"
done <<'EOF'
127.0.0.1 8082
127.0.0.2 8081
127.0.0.2 8082
127.0.0.2 9090
127.0.0.1 15000
127.0.0.1 15020
127.0.0.1 15021
127.0.0.1 9090
::1 8082
::1 15000
::1 15020
::1 15021
::1 9090
EOF

if kubectl_cmd -n "$namespace" exec "$pod" -c agent -- \
  sh -ceu '! env | grep -F "phase0-only-canary" >/dev/null && ! test -e /run/canary/token && ! test -e /run/agentgateway/canary/token'; then
  :
else
  echo 'credential canary is visible to the agent container' >&2
  exit 1
fi

if ! evidence="$(kubectl_cmd -n "$namespace" exec "$pod" -c evidence -- \
  wget -qO- --timeout=4 http://127.0.0.1:9090/evidence)"; then
  echo 'recording evidence probe failed; collecting exact disposable diagnostics' >&2
  kubectl_cmd -n "$namespace" describe pod "$pod" >&2 || true
  kubectl_cmd -n "$namespace" logs "$pod" -c recording >&2 || true
  kubectl_cmd -n "$namespace" logs "$pod" -c evidence >&2 || true
  exit 1
fi
[[ "$evidence" == *'"calls":1'* && "$evidence" == *'"canaryValid":1'* ]] || { echo "recording evidence failed: $evidence" >&2; exit 1; }
all_pod_logs="$(kubectl_cmd -n "$namespace" logs "$pod" --all-containers=true)" || {
  echo 'could not capture pod logs for canary scan' >&2
  exit 1
}
if rg -q 'phase0-only-canary' <<<"$all_pod_logs"; then
  echo 'credential canary appeared in pod logs' >&2
  exit 1
fi
if kubectl_cmd -n "$namespace" auth can-i get secrets --as="system:serviceaccount:$namespace:agw-phase0-guard"; then
  echo 'run service account can read Secrets' >&2
  exit 1
fi
if kubectl_cmd -n "$namespace" auth can-i get pods/exec --as="system:serviceaccount:$namespace:agw-phase0-guard"; then
  echo 'run service account can exec into pods' >&2
  exit 1
fi

printf 'LIVE PASS: routing, credential-canary, denied destinations, and least-privilege checks completed on context %s\n' "$current_context"
printf 'This evidence is valid only for the exact rendered image digest and disposable context above.\n'
