#!/usr/bin/env bash
# scripts/e2e-harness-runtime-real.sh
#
# Real-harness smoke test for the Agents Gateway harness runtime.
#
# Pre-flight: this script MUST refuse to run if any required real harness
# binary is missing. Per milestone spec it exits with code 2 and prints:
#
#     REAL HARNESS E2E BLOCKED: missing <command(s)>
#
# When binaries are present, a tiny task is dispatched against a
# disposable scratch repo using the configured harness profile.
# Verification must pass before the script exits 0.
#
# Required binaries (any one suffices by default; set the
# AGW_E2E_REAL_PROFILE env var to choose explicitly):
#
#   - opencode
#   - claude
#   - codex
#
# Optional env vars:
#   AGW_E2E_REAL_PROFILE      harness profile name (default: opencode-deepseek)
#   AGW_E2E_BINARIES          space-separated list of commands required
#                             (default: derived from AGW_E2E_REAL_PROFILE)
#   AGW_E2E_PORT              gateway port (default 18094)
#   AGW_E2E_TIMEOUT_SECONDS   max wallclock seconds (default 600)
#
# Exit codes:
#   0  pass
#   1  infrastructure failure (gateway refused to boot, etc.)
#   2  one or more required harness binaries missing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

GATEWAY_PORT="${AGW_E2E_PORT:-18094}"
BASE_URL="http://127.0.0.1:${GATEWAY_PORT}"
REAL_PROFILE="${AGW_E2E_REAL_PROFILE:-opencode-deepseek}"
WALL_TIMEOUT_SECONDS="${AGW_E2E_TIMEOUT_SECONDS:-600}"
WORK_DIR="$(mktemp -d -t agw-real-e2e-XXXXXX)"
SCRATCH_REPO="${WORK_DIR}/scratch-repo"
GATEWAY_PID=""

# Mapping of harness profile -> required command. Keeps the venn diagram
# narrow: the script blocks until the explicit command is on PATH.
case "$REAL_PROFILE" in
    opencode-deepseek) REQUIRED_BINARIES="opencode" ;;
    claude-code)       REQUIRED_BINARIES="claude" ;;
    codex)             REQUIRED_BINARIES="codex" ;;
    fake-test)
        echo "REAL HARNESS E2E BLOCKED: "
        echo "AGW_E2E_REAL_PROFILE=fake-test is not a real harness"
        exit 2
        ;;
    *)
        if [ -n "${AGW_E2E_BINARIES:-}" ]; then
            REQUIRED_BINARIES="$AGW_E2E_BINARIES"
        else
            echo "REAL HARNESS E2E BLOCKED: "
            echo "unknown profile: ${REAL_PROFILE}"
            exit 2
        fi
        ;;
esac

# ---------------------------------------------------------------------------
# Pre-flight: required binaries
# ---------------------------------------------------------------------------

MISSING=""
for cmd in $REQUIRED_BINARIES; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        MISSING="${MISSING} ${cmd}"
    fi
done

# Optional explicit block requested? Useful to demonstrate the refusal
# mechanism without actually installing / configuring a real harness.
if [ "${AGW_E2E_FORCE_BLOCK:-0}" = "1" ]; then
    echo "REAL HARNESS E2E BLOCKED: missing${MISSING:- ${REQUIRED_BINARIES}}"
    exit 2
fi

if [ -n "$MISSING" ]; then
    echo "REAL HARNESS E2E BLOCKED: missing${MISSING}"
    exit 2
fi

# Warn (but proceed) when required binaries are present but credentials
# may not be configured. Real LLM-backed harnesses will need API keys
# for actual task completion.
echo "Required harness binaries present: ${REQUIRED_BINARIES}"
echo "Proceeding with real harness E2E (timeout=${WALL_TIMEOUT_SECONDS}s)"
echo "Note: this may take several minutes if the harness initiates real LLM calls."

# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------

cleanup() {
    if [ -n "${GATEWAY_PID:-}" ]; then
        kill "$GATEWAY_PID" 2>/dev/null || true
        wait "$GATEWAY_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Scratch repo
# ---------------------------------------------------------------------------
git init -q -b master "$SCRATCH_REPO" 2>/dev/null || (
    cd "$SCRATCH_REPO" && git init -q && git symbolic-ref HEAD refs/heads/master
)
cat > "${SCRATCH_REPO}/add.py" <<'PY'
def add(a, b):
    return a + b
PY
cat > "${SCRATCH_REPO}/test_add.py" <<'PY'
from add import add

def test_add():
    assert add(2, 3) == 5
PY
git -C "$SCRATCH_REPO" add add.py test_add.py
git -C "$SCRATCH_REPO" -c user.email=t@local -c user.name=test commit -q -m "Initial"

# ---------------------------------------------------------------------------
# 2. Boot the gateway (DO NOT use fake tmux — we need the real harness)
# ---------------------------------------------------------------------------
export AGW_AGENTS__DIR="${PROJECT_DIR}/agents"
export AGW_STORAGE__SQLITE_PATH="${WORK_DIR}/agw.db"
export AGW_STORAGE__ARTIFACTS_DIR="${WORK_DIR}/artifacts"
export AGW_AUTH__MODE=dev-none
export AGW_OBSERVABILITY__LOG_LEVEL=INFO
export AGW_OBSERVABILITY__LOG_FORMAT=json
export AGW_HARNESS__USE_FAKE_TMUX=false
export AGW_HARNESS__WORKSPACE_ROOT="${WORK_DIR}/repos"
export AGW_HARNESS__WORKTREE_ROOT="${WORK_DIR}/worktrees"
export AGW_HARNESS__ARTIFACTS_ROOT="${WORK_DIR}/artifacts"
export AGW_HARNESS__AUTO_COMMIT=true
export AGW_HARNESS__RELAY_MAX_TIME_SECONDS="${WALL_TIMEOUT_SECONDS}"
export AGW_SERVICE__RATE_LIMITING__ENABLED=false

echo "Starting Agents Gateway on port ${GATEWAY_PORT}..."
uv run agents-gateway run --port "${GATEWAY_PORT}" \
    >"${WORK_DIR}/gateway.log" 2>&1 &
GATEWAY_PID="$!"

# Wait for healthy
ok=0
for i in $(seq 1 30); do
    sleep 1
    if curl -sf -o /dev/null "${BASE_URL}/health" 2>/dev/null; then
        ok=1; break
    fi
done
if [ "$ok" -ne 1 ]; then
    echo "FAIL: gateway did not become healthy"
    echo "----- gateway.log -----"
    tail -50 "${WORK_DIR}/gateway.log" || true
    exit 1
fi
echo "Gateway healthy."

# ---------------------------------------------------------------------------
# 3. Task body — small enough for a real harness to handle in seconds
# ---------------------------------------------------------------------------
TASK_BODY=$(cat <<EOF
{
  "title": "real harness E2E",
  "brief": "Add a docstring to add.py. Run pytest in the worktree before declaring done.",
  "repo": {"url": "file://${SCRATCH_REPO}", "owner": "o",
           "name": "r", "base_branch": "master"},
  "execution": {"mode": "harness_session", "harness_profile": "${REAL_PROFILE}"},
  "goal": {"strategy": "auto",
           "text": "Add a one-line docstring at the top of add.py. Then run 'python3 -m pytest test_add.py' and ensure it passes. Do not change test_add.py."},
  "verification": {"required": true, "commands": [
    {"name": "pytest", "command": "python3 -m pytest test_add.py", "required": true},
    {"name": "docstring present", "command": "grep -q '\"\"\"' add.py", "required": true}
  ]},
  "artifacts": {"html_report": true}
}
EOF
)

echo "Creating task (profile=${REAL_PROFILE})..."
TASK_RESP=$(curl -sf -X POST "${BASE_URL}/tasks" \
    -H "Content-Type: application/json" \
    -d "$TASK_BODY" 2>/dev/null) || {
    echo "FAIL: task creation returned non-2xx"
    exit 1
}
TASK_ID=$(echo "$TASK_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
if [ -z "$TASK_ID" ]; then
    echo "FAIL: could not parse task id"
    exit 1
fi
echo "Task id: ${TASK_ID}"

echo "Triggering /tasks/${TASK_ID}/run..."
curl -sf -X POST "${BASE_URL}/tasks/${TASK_ID}/run" \
    -H "Content-Type: application/json" 2>/dev/null >/dev/null \
    || true  # tolerate non-2xx on already-queued

# ---------------------------------------------------------------------------
# 4. Poll
# ---------------------------------------------------------------------------
echo "Polling status (max ${WALL_TIMEOUT_SECONDS}s)..."
DEADLINE=$(( $(date +%s) + WALL_TIMEOUT_SECONDS ))
FINAL_STATUS=""
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
    sleep 5
    TASK=$(curl -sf "${BASE_URL}/tasks/${TASK_ID}" 2>/dev/null) || continue
    STATUS=$(echo "$TASK" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
    case "$STATUS" in
        completed|failed|blocked_external|cancelled|stalled)
            FINAL_STATUS="$STATUS"
            break
            ;;
    esac
done

echo "Final status: ${FINAL_STATUS}"
if [ -z "$FINAL_STATUS" ]; then
    echo "FAIL: task did not reach terminal status within ${WALL_TIMEOUT_SECONDS} s"
    exit 1
fi

if [ "$FINAL_STATUS" != "completed" ]; then
    echo "Task ended with status ${FINAL_STATUS}"
    exit 1
fi

# ---------------------------------------------------------------------------
# 5. Verification + artifacts
# ---------------------------------------------------------------------------
VERIF=$(curl -sf "${BASE_URL}/agent-runs/${TASK_ID}/verification" 2>/dev/null || echo "")
VERIF_STATUS=$(echo "$VERIF" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
if [ "$VERIF_STATUS" != "passed" ]; then
    echo "FAIL: verification status=${VERIF_STATUS}"
    exit 1
fi
if [ ! -f "${WORK_DIR}/artifacts/${TASK_ID}/reports/review-report.html" ]; then
    echo "FAIL: HTML review report missing"
    exit 1
fi

echo "[OK] Real harness E2E passed (profile=${REAL_PROFILE})"
exit 0
