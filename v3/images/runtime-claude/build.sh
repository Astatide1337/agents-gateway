#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../../.." && pwd)
engine=${CONTAINER_ENGINE:-docker}
image=${IMAGE:-agents-gateway-v3-runtime-claude:local}

if ! command -v "$engine" >/dev/null 2>&1; then
	echo "container engine not found: $engine" >&2
	exit 2
fi
if ! command -v npm >/dev/null 2>&1; then
	echo "npm is required to resolve the current @anthropic-ai/claude-code version" >&2
	exit 2
fi

claude_version=${CLAUDE_CODE_VERSION:-}
if [[ -z "$claude_version" ]]; then
	claude_version=$(npm view @anthropic-ai/claude-code@latest version --json | sed -e 's/^"//' -e 's/"$//' -e 's/[[:space:]]//g')
fi
if [[ ! "$claude_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
	echo "invalid Claude Code version: $claude_version" >&2
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
	--build-arg "CLAUDE_CODE_VERSION=$claude_version" \
	--build-arg "VCS_REF=$vcs_ref" \
	--build-arg "BUILD_DATE=$build_date" \
	"$repo_root"

image_id=$("$engine" image inspect --format '{{.Id}}' "$image")
printf 'Built %s\n' "$image"
printf 'Image ID: %s\n' "$image_id"
printf 'Claude Code: %s\n' "$claude_version"
printf '%s\n' 'Push the image and pin the registry manifest digest in Agent.spec.runtime.image; do not deploy the local tag.'
