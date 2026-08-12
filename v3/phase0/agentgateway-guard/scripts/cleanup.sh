#!/usr/bin/env bash
set -euo pipefail

namespace=agw-phase0-guard
pod=agw-phase0-guard
kubectl_bin="${AGW_PHASE0_KUBECTL_BIN:-kubectl}"
kubeconfig="${AGW_PHASE0_KUBECONFIG:-${KUBECONFIG:-}}"
context="${AGW_PHASE0_CONTEXT:-}"
[[ -n "$context" ]] || { echo 'AGW_PHASE0_CONTEXT must name the disposable context exactly' >&2; exit 64; }
command -v -- "$kubectl_bin" >/dev/null 2>&1 || { echo "cleanup requires kubectl binary: $kubectl_bin" >&2; exit 2; }
if [[ -n "$kubeconfig" ]]; then
  [[ -f "$kubeconfig" ]] || { echo "AGW_PHASE0_KUBECONFIG does not exist: $kubeconfig" >&2; exit 64; }
  current_context="$("$kubectl_bin" --kubeconfig "$kubeconfig" config current-context)"
else
  current_context="$("$kubectl_bin" config current-context)"
fi
[[ "$current_context" == "$context" ]] || { echo "refusing unexpected kubectl context $current_context" >&2; exit 77; }

kubectl_cmd() {
  if [[ -n "$kubeconfig" ]]; then
    "$kubectl_bin" --kubeconfig "$kubeconfig" --context "$context" "$@"
  else
    "$kubectl_bin" --context "$context" "$@"
  fi
}

if [[ "${AGW_PHASE0_CLEANUP:-0}" != 1 ]]; then
  cat <<EOF
PLAN ONLY — no deletion performed.
context: $context
exact targets: pod/$pod, ConfigMap/agw-phase0-guard-config, NetworkPolicy/agw-phase0-default-deny,
               ServiceAccount/agw-phase0-guard, Role/agw-phase0-no-api-access,
               RoleBinding/agw-phase0-no-api-access, Namespace/$namespace
set AGW_PHASE0_CLEANUP=1 to delete only those named disposable resources.
EOF
  exit 0
fi

kubectl_cmd -n "$namespace" delete pod "$pod" --ignore-not-found
kubectl_cmd -n "$namespace" delete configmap agw-phase0-guard-config --ignore-not-found
kubectl_cmd -n "$namespace" delete networkpolicy agw-phase0-default-deny --ignore-not-found
kubectl_cmd -n "$namespace" delete rolebinding agw-phase0-no-api-access --ignore-not-found
kubectl_cmd -n "$namespace" delete role agw-phase0-no-api-access --ignore-not-found
kubectl_cmd -n "$namespace" delete serviceaccount agw-phase0-guard --ignore-not-found
kubectl_cmd delete namespace "$namespace" --ignore-not-found
printf 'deleted only the named Phase 0 namespace and its resources on context %s\n' "$context"
