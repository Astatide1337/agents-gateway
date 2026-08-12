#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
repo_v3="$(cd -- "$root_dir/../.." && pwd)"

"$root_dir/scripts/verify-static.sh"

(
  cd "$repo_v3"
  go test -count=1 -timeout=90s ./phase0/argo-sandbox/...
)

if [[ "${BUILD_BRIDGE_IMAGE:-0}" == "1" ]]; then
  (
    cd "$repo_v3"
    docker build --pull --progress=plain \
      -f phase0/argo-sandbox/bridge/Containerfile \
      -t agw-sandbox-wait-bridge:phase0 .
  )
fi

if [[ "${RUN_LIVE:-0}" == "1" ]]; then
  "$root_dir/scripts/live-check.sh"
else
  printf 'live-cluster checks skipped (set RUN_LIVE=1 only with an explicitly disposable context)\n'
fi
