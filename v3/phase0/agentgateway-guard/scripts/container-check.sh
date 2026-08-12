#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
repo_v3="$(cd -- "$root_dir/../.." && pwd)"
command -v docker >/dev/null 2>&1 || { echo 'CONTAINER_CHECK=1 requires docker' >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo 'CONTAINER_CHECK=1 requires curl' >&2; exit 2; }

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

guard_image="$(local_image AGW_PHASE0_GUARD_IMAGE agw-phase0-guard agw-phase0-guard:phase0)"
recording_image="$(local_image AGW_PHASE0_RECORDING_IMAGE agw-phase0-recording agw-phase0-recording:phase0)"
airlock_image="$(local_image AGW_PHASE0_AIRLOCK_IMAGE agw-phase0-airlock agw-phase0-airlock:phase0)"

# The disposable k3s worker imports exactly one linux/amd64 image stream. A
# Docker provenance attestation is an optional manifest-list member that is not
# needed for this local fixture; disable only that optional local attestation.
# Production image build/release settings are unchanged.
docker build --platform linux/amd64 --pull --provenance=false --progress=plain -f "$root_dir/Containerfile.guard" -t "$guard_image" "$repo_v3"
docker build --platform linux/amd64 --pull --provenance=false --progress=plain -f "$root_dir/Containerfile.recording" -t "$recording_image" "$repo_v3"
docker build --platform linux/amd64 --pull --provenance=false --progress=plain -f "$root_dir/Containerfile.airlock" -t "$airlock_image" "$repo_v3"

name="agw-phase0-recording-check-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker run -d --rm --name "$name" \
  --user 1339:1339 \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -e AGW_RECORDING_LISTEN=0.0.0.0:9090 \
  -e AGW_RECORDING_CANARY=phase0-only-canary \
  -p 127.0.0.1::9090 "$recording_image" >/dev/null
port="$(docker port "$name" 9090/tcp | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -n 1)"
[[ "$port" =~ ^[0-9]+$ ]] || { echo 'could not discover recording port' >&2; exit 1; }
base="http://127.0.0.1:$port"
curl -fsS "$base/healthz" >/dev/null
missing="$(curl -fsS -H 'content-type: application/json' -d '{"jsonrpc":"2.0","id":"missing","method":"tools/call","params":{"name":"record","arguments":{"message":"hello"}}}' "$base/mcp")"
[[ "$missing" != *phase0-only-canary* ]] || { echo 'recording error leaked canary' >&2; exit 1; }
evidence="$(curl -fsS "$base/evidence")"
[[ "$evidence" == *'"calls":0'* ]] || { echo "missing-canary request counted: $evidence" >&2; exit 1; }
valid="$(curl -fsS -H 'content-type: application/json' -H 'x-phase0-canary: phase0-only-canary' -d '{"jsonrpc":"2.0","id":"valid","method":"tools/call","params":{"name":"record","arguments":{"message":"hello"}}}' "$base/mcp")"
[[ "$valid" != *phase0-only-canary* ]] || { echo 'recording success leaked canary' >&2; exit 1; }
evidence="$(curl -fsS "$base/evidence")"
[[ "$evidence" == *'"calls":1'* && "$evidence" == *'"canaryValid":1'* ]] || { echo "canary evidence failed: $evidence" >&2; exit 1; }

printf 'container checks passed: pinned-base local images built and recording canary behavior verified\n'
printf 'This does not prove agentgateway routing or k3s UID airlock behavior.\n'
