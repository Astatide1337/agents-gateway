#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-}"
image="${AGENTGATEWAY_IMAGE:-}"

[[ -n "$output_dir" ]] || { echo 'usage: AGENTGATEWAY_IMAGE=repo@sha256:digest render.sh OUTPUT_DIR' >&2; exit 64; }
[[ "$image" =~ ^cr\.agentgateway\.dev/agentgateway@sha256:[0-9a-f]{64}$ ]] || {
  echo 'AGENTGATEWAY_IMAGE must be an immutable cr.agentgateway.dev/agentgateway@sha256:<64 hex> reference' >&2
  exit 64
}
if [[ -e "$output_dir" || -L "$output_dir" ]]; then
	[[ -d "$output_dir" && ! -L "$output_dir" ]] || {
		echo "render output must be a real directory: $output_dir" >&2
		exit 73
	}
	[[ -z "$(find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]] || {
		echo "refusing to overwrite non-empty output: $output_dir" >&2
		exit 73
	}
else
	mkdir -p "$output_dir"
fi
cp "$root_dir"/{kustomization.yaml,namespace.yaml,rbac.yaml,networkpolicy.yaml,configmap.yaml} "$output_dir/"

local_image() {
	local variable="$1"
	local expected_prefix="$2"
	local fallback="$3"
	local value="${!variable:-$fallback}"
	[[ "$value" =~ ^${expected_prefix}:[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || {
		echo "$variable must be a local ${expected_prefix}:<tag> image reference" >&2
		exit 64
	}
	printf '%s\n' "$value"
}

# The source pod keeps stable fixture references for static review. A
# disposable local runner may replace them with unique, locally built tags;
# it must never replace the real agentgateway reference with a mutable tag.
guard_image="$(local_image AGW_PHASE0_GUARD_IMAGE agw-phase0-guard agw-phase0-guard:phase0)"
recording_image="$(local_image AGW_PHASE0_RECORDING_IMAGE agw-phase0-recording agw-phase0-recording:phase0)"
airlock_image="$(local_image AGW_PHASE0_AIRLOCK_IMAGE agw-phase0-airlock agw-phase0-airlock:phase0)"
base_image='docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1'
if [[ -n "${AGW_PHASE0_BASE_IMAGE:-}" ]]; then
	base_image="$(local_image AGW_PHASE0_BASE_IMAGE agw-phase0-alpine agw-phase0-alpine:phase0)"
fi

sentinel='cr.agentgateway.dev/agentgateway@sha256:0000000000000000000000000000000000000000000000000000000000000000'
count_literal() {
	local needle="$1"
	local file="$2"
	awk -v needle="$needle" '
		{
			line = $0
			while ((offset = index(line, needle)) > 0) {
				count++
				line = substr(line, offset + length(needle))
			}
		}
		END { print count + 0 }
	' "$file"
}
[[ "$(count_literal "$sentinel" "$root_dir/pod.yaml")" == 1 ]] || {
	echo 'source pod must contain exactly one agentgateway digest sentinel' >&2
	exit 1
}
sed \
	-e "s#$sentinel#$image#" \
	-e "s#agw-phase0-guard:phase0#$guard_image#g" \
	-e "s#agw-phase0-recording:phase0#$recording_image#g" \
	-e "s#agw-phase0-airlock:phase0#$airlock_image#g" \
	-e "s#docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1#$base_image#g" \
	"$root_dir/pod.yaml" > "$output_dir/pod.yaml"

[[ "$(count_literal "$sentinel" "$output_dir/pod.yaml")" == 0 ]] || {
	echo 'render left the agentgateway digest sentinel in output' >&2
	exit 1
}
[[ "$(count_literal "$image" "$output_dir/pod.yaml")" == 1 ]] || {
	echo 'render failed to replace the agentgateway placeholder' >&2
	exit 1
}
printf '%s\n' "$output_dir"
