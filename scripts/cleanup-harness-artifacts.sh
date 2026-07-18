#!/usr/bin/env bash
# scripts/cleanup-harness-artifacts.sh
#
# CLI front-end for the harness-artifact / worktree retention cleanup.
# Either runs the in-process cleanup function directly (no HTTP
# required — useful from cron), or POSTs to a running gateway's
# /cleanup/dry-run or /cleanup/run endpoint.
#
# Usage:
#   scripts/cleanup-harness-artifacts.sh --dry-run
#   scripts/cleanup-harness-artifacts.sh --run
#   scripts/cleanup-harness-artifacts.sh --http --run --gateway http://127.0.0.1:8092
#
# Honours the AGW_HARNESS__ARTIFACT_RETENTION_DAYS,
# AGW_HARNESS__WORKTREE_RETENTION_DAYS,
# AGW_HARNESS__MAX_ARTIFACT_BYTES, and
# AGW_HARNESS__CLEANUP_DRY_RUN env vars when invoked in --in-process
# mode (default).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

MODE="dry-run"
USE_HTTP=false
GATEWAY_URL="http://127.0.0.1:8092"

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run) MODE="dry-run"; shift ;;
        --run)     MODE="run"; shift ;;
        --force)   MODE="run"; FORCE="--force"; shift ;;
        --http)    USE_HTTP=true; shift ;;
        --gateway) GATEWAY_URL="$2"; shift 2 ;;
        -h|--help)
            sed -n '3,20p' "$0"
            exit 0
            ;;
        *) echo "unknown flag: $1"; exit 1 ;;
    esac
done

if [ "$USE_HTTP" = "true" ]; then
    if [ "$MODE" = "dry-run" ]; then
        echo "POST ${GATEWAY_URL}/cleanup/dry-run"
        curl -sf -X POST "${GATEWAY_URL}/cleanup/dry-run" | python3 -m json.tool
    else
        echo "POST ${GATEWAY_URL}/cleanup/run?${FORCE:-}"
        curl -sf -X POST "${GATEWAY_URL}/cleanup/run${FORCE:+?force=true}" \
            | python3 -m json.tool
    fi
    exit $?
fi

# In-process mode: invoke the cleanup function directly. Useful when
# the gateway isn't running or for cron jobs.
cd "$PROJECT_DIR"
uv run python -c "
from agents_gateway.config import load_config
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.cleanup import run_cleanup
import json, sys

cfg = load_config()
hs = HarnessStorage(cfg.storage.sqlite_path)
dry = (('$MODE' == 'dry-run') and cfg.harness.cleanup_dry_run)
print('cleanup dry-run:', dry, file=sys.stderr)
report = run_cleanup(
    hs,
    artifact_retention_days=cfg.harness.artifact_retention_days,
    worktree_retention_days=cfg.harness.worktree_retention_days,
    max_artifact_bytes=cfg.harness.max_artifact_bytes,
    dry_run=dry,
)
print(json.dumps(report.to_dict(), indent=2))
"
