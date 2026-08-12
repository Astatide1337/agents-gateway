#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
repo_v3="$(cd -- "$root_dir/../.." && pwd)"

"$root_dir/scripts/verify-static.sh"
(
  cd "$repo_v3"
  go test -count=1 -timeout=90s ./phase0/agentgateway-guard/...
)
(
  cd "$repo_v3"
  go vet ./phase0/agentgateway-guard/...
)

if [[ "${RACE:-0}" == "1" ]]; then
  (
    cd "$repo_v3"
    go test -race -count=1 -timeout=120s ./phase0/agentgateway-guard/guard ./phase0/agentgateway-guard/recording
  )
else
  printf 'race tests skipped (set RACE=1)\n'
fi

if [[ "${CONTAINER_CHECK:-0}" == "1" ]]; then
  "$root_dir/scripts/container-check.sh"
else
  printf 'container checks skipped (set CONTAINER_CHECK=1)\n'
fi

if [[ "${RUN_LIVE:-0}" == "1" ]]; then
  "$root_dir/scripts/live-check.sh"
else
  printf 'live k3s checks skipped (set RUN_LIVE=1 and explicit live gates)\n'
fi

if [[ "${LOCAL_LIVE:-0}" == "1" ]]; then
  "$root_dir/scripts/local-chain-check.sh"
else
  printf 'local real-agentgateway chain skipped (set LOCAL_LIVE=1 and AGW_PHASE0_LOCAL_LIVE=1)\n'
fi
