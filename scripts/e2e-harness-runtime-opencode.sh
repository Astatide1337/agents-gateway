#!/usr/bin/env bash
# scripts/e2e-harness-runtime-opencode.sh
#
# Thin wrapper around e2e-harness-runtime-real.sh pinned to the
# opencode profile. Useful as a named entry point for CI and the
# "must pass real opencode" milestone gate.
#
# Honours:
#   AGW_E2E_TIMEOUT_SECONDS  (default 900)
#   AGW_REAL_HARNESS_REPO    (override scratch repo location)
#
# Exit codes mirror the underlying script.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export AGW_REAL_HARNESS_PROFILE="${AGW_REAL_HARNESS_PROFILE:-opencode-deepseek}"
export AGW_REAL_HARNESS_COMMAND="${AGW_REAL_HARNESS_COMMAND:-opencode}"
export AGW_E2E_TIMEOUT_SECONDS="${AGW_E2E_TIMEOUT_SECONDS:-900}"

exec bash "${SCRIPT_DIR}/e2e-harness-runtime-real.sh"
