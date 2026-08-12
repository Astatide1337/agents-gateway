#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$root_dir/versions.env"

docker_bin="${AGW_PHASE0_DOCKER_BIN:-docker}"
expected_image="$AGENTGATEWAY_IMAGE_REPOSITORY@sha256:$AGENTGATEWAY_IMAGE_DIGEST"
image="${AGENTGATEWAY_IMAGE:-$expected_image}"

[[ "$image" == "$expected_image" ]] || {
  echo "AGENTGATEWAY_IMAGE must equal the reviewed digest $expected_image" >&2
  exit 64
}
command -v -- "$docker_bin" >/dev/null 2>&1 || {
  echo "missing Docker binary: $docker_bin" >&2
  exit 2
}
"$docker_bin" image inspect "$image" >/dev/null 2>&1 || {
  echo "reviewed agentgateway image is not available locally: $image" >&2
  exit 69
}

token_file="$(mktemp /tmp/agw-phase0-config-token.XXXXXX)"
cleanup() { unlink "$token_file" >/dev/null 2>&1 || true; }
trap cleanup EXIT
printf '%s\n' 'phase0-only-canary' >"$token_file"
# The live pod tightens this to 0400 and assigns it to UID 1338. The local
# validator only needs the pinned binary (also run as 1338) to read the same
# file-backed form during deserialization.
chmod 0444 "$token_file"

awk '
  /^  agentgateway\.yaml: \|$/ { in_config = 1; next }
  /^  airlock\.sh: \|$/ { exit }
  in_config {
    sub(/^    /, "")
    print
  }
' "$root_dir/configmap.yaml" |
  "$docker_bin" run --rm -i --platform linux/amd64 --user 1338:1338 \
    -v "$token_file:/run/agentgateway/canary/token:ro" \
    "$image" --validate-only --file /dev/stdin

printf 'agentgateway config validation passed for %s\n' "$image"
