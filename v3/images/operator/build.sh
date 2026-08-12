#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../../.." && pwd)
engine=${CONTAINER_ENGINE:-docker}
image=${IMAGE:-agents-gateway-v3-operator:local}

if ! command -v "$engine" >/dev/null 2>&1; then
	echo "container engine not found: $engine" >&2
	exit 2
fi

vcs_ref=${VCS_REF:-unknown}
if [[ "$vcs_ref" == unknown ]] && git -C "$repo_root" rev-parse HEAD >/dev/null 2>&1; then
	vcs_ref=$(git -C "$repo_root" rev-parse HEAD)
fi
build_date=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

"$engine" build \
	--file "$script_dir/Containerfile" \
	--tag "$image" \
	--build-arg "VCS_REF=$vcs_ref" \
	--build-arg "BUILD_DATE=$build_date" \
	"$repo_root"

"$script_dir/validate.sh" --image "$image"
image_id=$("$engine" image inspect --format '{{.Id}}' "$image")
printf 'Built %s\n' "$image"
printf 'Image ID: %s\n' "$image_id"
printf '%s\n' 'Push the image and configure the Helm chart with its registry manifest digest.'
