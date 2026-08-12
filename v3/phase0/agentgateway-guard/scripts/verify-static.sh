#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { printf 'agentgateway guard static verification failed: %s\n' "$*" >&2; exit 1; }
require_file() { [[ -f "$root_dir/$1" ]] || fail "missing $1"; }
require_match() { rg -q -- "$2" "$root_dir/$1" || fail "$1 missing: $2"; }

for file in \
  README.md versions.env kustomization.yaml namespace.yaml rbac.yaml networkpolicy.yaml \
  configmap.yaml pod.yaml Containerfile.guard Containerfile.recording Containerfile.airlock \
  guard/guard.go guard/guard_test.go recording/recording.go recording/recording_test.go \
  cmd/guard/main.go cmd/recording/main.go static/static_test.go \
  scripts/test.sh scripts/resolve-agentgateway-image.sh scripts/render.sh scripts/live-check.sh scripts/cleanup.sh scripts/container-check.sh scripts/local-chain-check.sh scripts/validate-agentgateway-config.sh; do
  require_file "$file"
done

require_match versions.env '^AGENTGATEWAY_VERSION=v1\.4\.1$'
require_match versions.env '^AGENTGATEWAY_COMMIT=163ea2146acb7b82082acea30ed691b29079095f$'
require_match versions.env '^AGENTGATEWAY_IMAGE_TAG=cr\.agentgateway\.dev/agentgateway:v1\.4\.1$'
require_match versions.env '^AGENTGATEWAY_IMAGE_DIGEST=efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29$'
require_match versions.env '^AGENTGATEWAY_RELEASE_URL=https://github\.com/agentgateway/agentgateway/releases/tag/v1\.4\.1$'
require_match versions.env '^GO_BUILD_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_match versions.env '^ALPINE_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_match pod.yaml '^  hostUsers: false$'
require_match pod.yaml '^  automountServiceAccountToken: false$'
require_match pod.yaml 'cr\.agentgateway\.dev/agentgateway@sha256:0000000000000000000000000000000000000000000000000000000000000000'
require_match pod.yaml 'runAsUser: 1000'
require_match pod.yaml 'runAsUser: 1337'
require_match pod.yaml 'runAsUser: 1338'
require_match pod.yaml 'runAsUser: 1339'
require_match pod.yaml 'add: \[CHOWN, DAC_OVERRIDE\]'
require_match pod.yaml 'chown 1338:1338 /run/agentgateway/runtime'
require_match pod.yaml 'chmod 0700 /run/agentgateway/runtime'
require_match configmap.yaml 'iptables -w -P OUTPUT DROP'
require_match configmap.yaml 'ip6tables -w -P OUTPUT DROP'
require_match configmap.yaml '--uid-owner 1000.*--dport 8081'
require_match configmap.yaml '--uid-owner 1337.*--dport 8082'
require_match configmap.yaml '--uid-owner 1338.*--dport 9090'
require_match configmap.yaml 'iptables -w -A INPUT -i lo -m conntrack --ctstate NEW.*--dports 8081,8082,9090'
require_match configmap.yaml 'ip6tables -w -A INPUT -i lo -m conntrack --ctstate NEW.*--dports 8081,8082,9090'
require_match configmap.yaml 'adminAddr: unix:///'
require_match configmap.yaml 'statsAddr: unix:///'
require_match configmap.yaml 'readinessAddr: unix:///'
require_match configmap.yaml '^    config:'
require_match configmap.yaml 'file: /run/agentgateway/canary/token'
require_match configmap.yaml 'level: warn'
require_match pod.yaml 'name: AGW_AIRLOCK_MODE'
require_match pod.yaml 'value: recording-only'
require_match pod.yaml 'agw.astatide.com/agents: "true"'
require_match pod.yaml 'key: agw.astatide.com'
require_match rbac.yaml 'rules: \[\]'
require_match networkpolicy.yaml 'ingress: \[\]'
require_match networkpolicy.yaml 'egress: \[\]'
require_match scripts/container-check.sh '--provenance=false'
require_match scripts/local-chain-check.sh 'images import --platform linux/amd64 --digests'
require_match scripts/local-chain-check.sh 'AGW_PHASE0_K3S_WORKER_CONTAINER'
require_match scripts/local-chain-check.sh 'AGW_PHASE0_KUBECONFIG'
require_match scripts/local-chain-check.sh 'containerd_ref="docker.io/library/\$image"'
require_match scripts/local-chain-check.sh 'containerd applies Docker'
require_match scripts/local-chain-check.sh 'runtime_agentgateway_image='
require_match scripts/local-chain-check.sh 'images tag "\$platform_ref" "\$runtime_agentgateway_image"'
require_match scripts/local-chain-check.sh 'validate-agentgateway-config.sh'
require_match scripts/validate-agentgateway-config.sh 'AGENTGATEWAY_IMAGE_REPOSITORY@sha256:\$AGENTGATEWAY_IMAGE_DIGEST'
require_match scripts/validate-agentgateway-config.sh '\-\-user 1338:1338'
require_match scripts/validate-agentgateway-config.sh '\-v "\$token_file:/run/agentgateway/canary/token:ro"'
require_match scripts/validate-agentgateway-config.sh '"\$image" --validate-only --file /dev/stdin'
require_match scripts/live-check.sh 'AGW_PHASE0_KUBECONFIG'
require_match scripts/live-check.sh 'transport-level evidence'
require_match scripts/live-check.sh 'command -v nc'
require_match scripts/live-check.sh 'all_pod_logs='
require_match scripts/cleanup.sh 'AGW_PHASE0_KUBECONFIG'
require_match pod.yaml '^    - name: evidence$'
require_match pod.yaml '^        runAsUser: 1338$'
require_match pod.yaml 'no canary volume or canary environment'

if rg -n --glob '*.yaml' --glob '*.yml' -U '^kind: ClusterRole|^kind: ClusterRoleBinding|^[[:space:]]+secretRef:|^kind: EventSource|^kind: Sensor|^kind: EventBus' "$root_dir"; then
  fail 'cluster-scoped authority, Secret reference, or Argo Events resource found'
fi
if rg -n --glob '*.yaml' --glob '*.yml' 'agentgateway:v1\.4\.1|:latest' "$root_dir"; then
  fail 'mutable agentgateway tag or latest tag found in an applied manifest'
fi
# Scan the runner surface, but exclude this verifier: its own guard pattern
# necessarily contains the words it is checking for.
if rg -n --glob '!verify-static.sh' -- 'kindest/node|AGW_PHASE0_KIND|\bkind (create|delete|load)' "$root_dir/scripts" "$root_dir/README.md"; then
  fail 'agentgateway live runner must use the parent disposable k3s harness, not Kind'
fi
if rg -n -- 'public-test|0\.0\.0\.0/0|-d ::/0' "$root_dir/configmap.yaml" "$root_dir/pod.yaml" "$root_dir/networkpolicy.yaml"; then
  fail 'recording-only probe contains public wildcard egress mode'
fi
if awk '/-A OUTPUT -o lo/ && $0 !~ /-m owner/ { print }' "$root_dir/configmap.yaml" | rg -n .; then
  fail 'loopback OUTPUT allow is not owner-scoped'
fi
if rg -n --glob '*.yaml' --glob '*.yml' 'adminAddr: (127\.|0\.0\.0\.0)|statsAddr: (127\.|0\.0\.0\.0)|readinessAddr: (127\.|0\.0\.0\.0)' "$root_dir"; then
  fail 'management listener is TCP-addressable in the fixture'
fi
for script in "$root_dir"/scripts/*.sh; do
  [[ -x "$script" ]] || fail "script is not executable: $script"
done

# Exercise the same empty mktemp directory shape used by live-check.sh. This
# catches accidental sentinel replacement regressions without contacting a
# registry or applying anything to Kubernetes.
render_check_dir="$(mktemp -d /tmp/agw-phase0-guard-render-check.XXXXXX)"
cleanup_render_check() { rm -rf -- "$render_check_dir"; }
trap cleanup_render_check EXIT
AGENTGATEWAY_IMAGE='cr.agentgateway.dev/agentgateway@sha256:1111111111111111111111111111111111111111111111111111111111111111' \
  "$root_dir/scripts/render.sh" "$render_check_dir" >/dev/null || fail 'digest-sentinel render failed'
if rg -n -- '0000000000000000000000000000000000000000000000000000000000000000|cr\.agentgateway\.dev/agentgateway:v1\.4\.1' "$render_check_dir"; then
  fail 'digest sentinel or mutable image tag survived rendering'
fi
[[ "$(rg -o -F 'cr.agentgateway.dev/agentgateway@sha256:1111111111111111111111111111111111111111111111111111111111111111' "$render_check_dir/pod.yaml" | wc -l)" == 1 ]] || {
  fail 'rendered pod does not contain exactly one immutable image reference'
}

if command -v kubectl >/dev/null 2>&1; then
  kubectl kustomize "$root_dir" >/dev/null || fail 'kubectl kustomize rejected the source manifests'
else
  printf 'kubectl kustomize skipped: kubectl is not installed (Go YAML tests still run)\n'
fi

printf 'static verification passed: version sentinel, security fields, UIDs, owner airlock, RBAC, no Secrets, and no mutable gateway tag\n'
