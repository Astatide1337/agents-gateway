#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Child runner for v3/scripts/local-k3s-smoke.sh. It deliberately does not
# create or delete a cluster. The parent owns the exact Docker containers,
# network, kubeconfig, and cleanup boundary; this script owns only its three
# fixture image tags and the named Phase-0 namespace.

root_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
repo_v3="$(CDPATH= cd -- "$root_dir/../.." && pwd)"
# shellcheck disable=SC1091
source "$root_dir/versions.env"

docker_bin="${AGW_PHASE0_DOCKER_BIN:-docker}"
kubectl_bin="${AGW_PHASE0_KUBECTL_BIN:-kubectl}"
kubeconfig="${AGW_PHASE0_KUBECONFIG:-${KUBECONFIG:-}}"
context="${AGW_PHASE0_CONTEXT:-}"
worker_container="${AGW_PHASE0_K3S_WORKER_CONTAINER:-}"
worker_node="${AGW_PHASE0_WORKER_NODE:-}"
parent_run_id="${AGW_PHASE0_RUN_ID:-}"

fail() {
  printf 'local k3s agentgateway chain failed: %s\n' "$*" >&2
  exit 1
}

die_usage() {
  printf '%s\n' "$*" >&2
  exit 64
}

validate_single_line() {
  local label=$1 value=$2
  [[ -n "$value" && "$value" != *$'\n'* && "$value" != *$'\r'* && "$value" != *[[:space:]]* ]] || die_usage "$label must be a non-empty single-line value"
}

[[ "${AGW_PHASE0_LOCAL_LIVE:-0}" == 1 ]] || die_usage 'set AGW_PHASE0_LOCAL_LIVE=1 to enable the optional local k3s chain'
command -v -- "$docker_bin" >/dev/null 2>&1 || die_usage "missing Docker binary: $docker_bin"
command -v -- "$kubectl_bin" >/dev/null 2>&1 || die_usage "missing kubectl binary: $kubectl_bin"
"$docker_bin" info >/dev/null 2>&1 || fail 'Docker daemon is unavailable'
[[ -n "$kubeconfig" && -f "$kubeconfig" ]] || die_usage 'AGW_PHASE0_KUBECONFIG must name the parent harness kubeconfig'
validate_single_line context "$context"
validate_single_line worker_container "$worker_container"
validate_single_line worker_node "$worker_node"
validate_single_line parent_run_id "$parent_run_id"

expected_agentgateway_image="$AGENTGATEWAY_IMAGE_REPOSITORY@sha256:$AGENTGATEWAY_IMAGE_DIGEST"
agentgateway_image="${AGENTGATEWAY_IMAGE:-$expected_agentgateway_image}"
[[ "$agentgateway_image" == "$expected_agentgateway_image" ]] || {
  fail "agentgateway image must equal the reviewed digest $expected_agentgateway_image"
}

kubectl_cmd() {
  "$kubectl_bin" --kubeconfig "$kubeconfig" --context "$context" "$@"
}

current_context="$("$kubectl_bin" --kubeconfig "$kubeconfig" config current-context)"
[[ "$current_context" == "$context" ]] || fail "refusing kubectl context $current_context; expected $context"

worker_ids="$("$docker_bin" ps -q --filter "name=^/${worker_container}$")"
[[ "$(printf '%s\n' "$worker_ids" | awk 'NF { count++ } END { print count + 0 }')" == 1 ]] || {
  fail "expected exactly one running disposable worker container named $worker_container"
}
worker_id="$(printf '%s\n' "$worker_ids" | awk 'NF { print; exit }')"
worker_state="$("$docker_bin" inspect -f '{{.State.Running}}' "$worker_id")"
[[ "$worker_state" == true ]] || fail "worker container is not running: $worker_container"
worker_owner="$("$docker_bin" inspect -f '{{ index .Config.Labels "com.astatide.agw.run-id" }}' "$worker_id")"
worker_component="$("$docker_bin" inspect -f '{{ index .Config.Labels "com.astatide.agw.component" }}' "$worker_id")"
worker_role="$("$docker_bin" inspect -f '{{ index .Config.Labels "com.astatide.agw.role" }}' "$worker_id")"
[[ "$worker_owner" == "$parent_run_id" ]] || fail "worker ownership label mismatch: $worker_owner"
[[ "$worker_component" == local-k3s-smoke && "$worker_role" == agent ]] || {
  fail 'worker ownership labels do not identify the parent local-k3s-smoke agent'
}

run_id="${AGW_PHASE0_LOCAL_TAG_SUFFIX:-${parent_run_id#agw-k3s-}-${BASHPID:-$$}}"
[[ "$run_id" =~ ^[a-z0-9][a-z0-9.-]{0,45}$ ]] || die_usage 'local fixture tag suffix must be lowercase and at most 46 characters'

guard_image="agw-phase0-guard:local-$run_id"
recording_image="agw-phase0-recording:local-$run_id"
airlock_image="agw-phase0-airlock:local-$run_id"
base_image="agw-phase0-alpine:local-$run_id"
fixture_images=("$guard_image" "$recording_image" "$airlock_image" "$base_image")
created_images=()
cleanup_status=0
runtime_agentgateway_image=""

for image in "${fixture_images[@]}"; do
  if "$docker_bin" image inspect "$image" >/dev/null 2>&1; then
    fail "refusing to overwrite an existing exact fixture tag: $image"
  fi
done

cleanup() {
  local status=$?
  trap - EXIT INT TERM

  # The parent will remove the cluster even if this deletion is interrupted;
  # try the exact named resources first so the namespace is gone before that.
  if [[ -f "$kubeconfig" ]]; then
    AGW_PHASE0_CLEANUP=1 \
      AGW_PHASE0_CONTEXT="$context" \
      AGW_PHASE0_KUBECONFIG="$kubeconfig" \
      AGW_PHASE0_KUBECTL_BIN="$kubectl_bin" \
      bash "$root_dir/scripts/cleanup.sh" >/dev/null 2>&1 || cleanup_status=1
  fi

  for image in "${created_images[@]}"; do
    "$docker_bin" image rm "$image" >/dev/null 2>&1 || cleanup_status=1
  done

  if (( status == 0 && cleanup_status != 0 )); then
    status=1
  fi
  if (( status != 0 )); then
    printf 'local k3s agentgateway chain cleanup completed with original exit=%d\n' "$status" >&2
  else
    printf 'local k3s agentgateway chain cleanup complete\n'
  fi
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

build_fixture() {
  local name=$1 file=$2
  printf 'building local linux/amd64 fixture: %s\n' "$name"
  "$docker_bin" build \
    --platform linux/amd64 \
    --pull \
    --provenance=false \
    --progress=plain \
    -f "$root_dir/$file" \
    -t "$name" \
    "$repo_v3"
  created_images+=("$name")
}

printf 'pulling reviewed agentgateway digest and pinned fixture base\n'
"$docker_bin" pull --platform linux/amd64 "$agentgateway_image" >/dev/null
"$docker_bin" pull --platform linux/amd64 "$ALPINE_IMAGE" >/dev/null

AGENTGATEWAY_IMAGE="$agentgateway_image" \
AGW_PHASE0_DOCKER_BIN="$docker_bin" \
  bash "$root_dir/scripts/validate-agentgateway-config.sh"

build_fixture "$guard_image" Containerfile.guard
build_fixture "$recording_image" Containerfile.recording
build_fixture "$airlock_image" Containerfile.airlock
"$docker_bin" tag "$ALPINE_IMAGE" "$base_image"
created_images+=("$base_image")

assert_linux_amd64() {
  local image=$1 platform
  platform="$("$docker_bin" image inspect -f '{{.Os}}/{{.Architecture}}' "$image")"
  [[ "$platform" == linux/amd64 ]] || fail "image $image has platform $platform, expected linux/amd64"
}
for image in "${fixture_images[@]}" "$agentgateway_image"; do
  assert_linux_amd64 "$image"
done

load_image() {
  local image=$1 containerd_ref repository platform_ref
  printf 'loading linux/amd64 image into k3s worker containerd: %s\n' "$image"
  "$docker_bin" save --platform linux/amd64 "$image" | \
    "$docker_bin" exec --privileged -i "$worker_container" \
      ctr --namespace=k8s.io images import --platform linux/amd64 --digests --snapshotter=overlayfs -
	# containerd applies Docker's default registry to an unqualified reference
	# during import (for example, agw-phase0-alpine:tag becomes
	# docker.io/library/agw-phase0-alpine:tag). Keep local tags exact while
	# accepting the platform manifest digest selected from a digest-pinned
	# multi-platform index: its digest necessarily differs from the index
	# digest that was pulled and reviewed.
  if [[ "$image" == "$agentgateway_image" ]]; then
    # Docker saves an index-only digest pull without RepoTags. containerd
    # imports the selected platform under an import-<date>@sha256:<digest>
    # name, so give that exact platform manifest a repository-qualified,
    # immutable name for the pod to resolve. The source image was pulled by
    # the reviewed index digest above; this is only its local amd64 child.
    platform_ref=$("$docker_bin" exec "$worker_container" ctr --namespace=k8s.io images list | awk '$1 ~ /^import-[^@]+@sha256:[0-9a-f]{64}$/ && $0 ~ /linux\/amd64/ && result == "" { result = $1 } END { print result }')
    [[ -n "$platform_ref" ]] || fail "worker containerd did not expose a linux/amd64 platform manifest for source=$image"
    runtime_agentgateway_digest="${platform_ref##*@}"
    runtime_agentgateway_image="$AGENTGATEWAY_IMAGE_REPOSITORY@$runtime_agentgateway_digest"
    "$docker_bin" exec "$worker_container" ctr --namespace=k8s.io images tag "$platform_ref" "$runtime_agentgateway_image" >/dev/null
    "$docker_bin" exec "$worker_container" ctr --namespace=k8s.io images list -q | grep -Fx -- "$runtime_agentgateway_image" >/dev/null || {
      fail "worker containerd did not retain the immutable platform alias: $runtime_agentgateway_image"
    }
  elif [[ "$image" == *@sha256:* ]]; then
		repository="${image%@*}"
		"$docker_bin" exec "$worker_container" \
			ctr --namespace=k8s.io images list -q | \
			awk -v repository="$repository" 'index($0, repository) == 1 { found=1 } END { exit !found }' || {
			fail "worker containerd did not retain the imported digest-pinned repository: source=$image"
		}
	else
		if [[ "$image" == */* ]]; then
			containerd_ref="$image"
		else
			containerd_ref="docker.io/library/$image"
		fi
		"$docker_bin" exec "$worker_container" \
			ctr --namespace=k8s.io images list -q | grep -Fx -- "$containerd_ref" >/dev/null || {
			fail "worker containerd did not retain the imported image reference: source=$image stored=$containerd_ref"
		}
	fi
}

for image in "${fixture_images[@]}" "$agentgateway_image"; do
  load_image "$image"
done

[[ -n "$runtime_agentgateway_image" ]] || fail 'local agentgateway platform alias was not created'
printf 'source agentgateway index: %s\n' "$agentgateway_image"
printf 'local agentgateway platform: %s\n' "$runtime_agentgateway_image"

printf 'running existing agentgateway guard live-check against disposable context=%s worker=%s\n' "$context" "$worker_node"
AGENTGATEWAY_IMAGE="$runtime_agentgateway_image" \
AGW_PHASE0_GUARD_IMAGE="$guard_image" \
AGW_PHASE0_RECORDING_IMAGE="$recording_image" \
AGW_PHASE0_AIRLOCK_IMAGE="$airlock_image" \
AGW_PHASE0_BASE_IMAGE="$base_image" \
AGW_PHASE0_WORKER_NODE="$worker_node" \
AGW_PHASE0_LIVE=1 \
AGW_PHASE0_APPLY=1 \
AGW_PHASE0_CONTEXT="$context" \
AGW_PHASE0_KUBECONFIG="$kubeconfig" \
AGW_PHASE0_KUBECTL_BIN="$kubectl_bin" \
bash "$root_dir/scripts/live-check.sh"

printf 'LOCAL PASS: real agentgateway digest routed the provider-free recording chain on worker=%s\n' "$worker_node"
printf 'fixture images are local-only and the parent local-k3s-smoke trap owns cluster cleanup\n'
