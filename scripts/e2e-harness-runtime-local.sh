#!/usr/bin/env bash
# scripts/e2e-harness-runtime-local.sh
#
# Local end-to-end harness-runtime smoke test.
#
# Drives the complete Composer -> Agents Gateway -> harness session flow
# using the bundled fake-test harness profile and a scratch repo on disk.
# No real Claude/opencode/Codex binaries are required.
#
# Flow:
#   1. Boot Agents Gateway (auth=dev-none, fake tmux enabled).
#   2. Create a scratch repo with an initial commit.
#   3. POST /tasks with execution.mode=harness_session (fake-test profile,
#      goal text instructing the fake harness to write result.txt).
#   4. POST /tasks/{id}/run.
#   5. Poll GET /tasks/{id} until status terminal (completed|failed|blocked).
#   6. GET /agent-runs/{id}/verification — assert status == passed.
#   7. GET /agent-runs/{id}/artifacts — assert html_report exists on disk.
#   8. Print summary line: "Passed: N" 
#
# Exit code 0 on success, non-zero on failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

GATEWAY_PORT="${AGW_E2E_PORT:-18093}"
BASE_URL="http://127.0.0.1:${GATEWAY_PORT}"
WORK_DIR="${AGW_E2E_WORK_DIR:-$(mktemp -d -t agw-e2e-XXXXXX)}"
SCRATCH_REPO="${WORK_DIR}/scratch-repo"
GATEWAY_PID=""

PASS=0
FAIL=0

cleanup() {
    if [ -n "${GATEWAY_PID:-}" ]; then
        kill "$GATEWAY_PID" 2>/dev/null || true
        wait "$GATEWAY_PID" 2>/dev/null || true
    fi
    [ "${AGW_E2E_KEEP:-0}" = "1" ] || rm -rf "$WORK_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Wait helpers
# ---------------------------------------------------------------------------

wait_for_http() {
    local url="$1" tries="${2:-30}"
    for ((i=1; i<=tries; i++)); do
        if curl -sf -o /dev/null "$url" 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    return 1
}

json_extract() {
    python3 -c "
import json,sys
body=sys.stdin.read()
try:
    d=json.loads(body)
except Exception:
    print('', end=''); sys.exit(0)
key='$1'
def get(o, k):
    if not k: return o
    parts=k.split('.',1)
    if isinstance(o, dict) and parts[0] in o:
        return get(o[parts[0]], parts[1] if len(parts)>1 else '')
    return ''
print(get(d, key))
"
}

echo "=== Agents Gateway: harness runtime local E2E ==="
echo "Workdir: ${WORK_DIR}"

# ---------------------------------------------------------------------------
# 1. Bootstrap fake-test harness: agents/fake-test/agent.yaml + run.py
#    are already committed in the repo, so nothing to do.
# ---------------------------------------------------------------------------

# ---------------------------------------------------------------------------
# 2. Create a scratch repo with an initial commit
# ---------------------------------------------------------------------------
git init -q -b master "$SCRATCH_REPO" 2>/dev/null || (
    cd "$SCRATCH_REPO" && git init -q && git symbolic-ref HEAD refs/heads/master
)
printf '# Scratch\n' > "${SCRATCH_REPO}/README.md"
git -C "$SCRATCH_REPO" add README.md
git -C "$SCRATCH_REPO" -c user.email=t@local -c user.name=test \
    commit -q -m "Initial commit" 2>/dev/null || true
echo "Scratch repo: ${SCRATCH_REPO}"

# ---------------------------------------------------------------------------
# 3. Boot Agents Gateway with fake tmux enabled
# ---------------------------------------------------------------------------
export AGW_AGENTS__DIR="${PROJECT_DIR}/agents"
export AGW_STORAGE__SQLITE_PATH="${WORK_DIR}/agw.db"
export AGW_STORAGE__ARTIFACTS_DIR="${WORK_DIR}/artifacts"
export AGW_AUTH__MODE=dev-none
export AGW_OBSERVABILITY__LOG_LEVEL=WARNING
export AGW_OBSERVABILITY__LOG_FORMAT=json
# Enable fake-tmux mode so the harness driver does not need a real tmux.
export AGW_HARNESS__USE_FAKE_TMUX=false
export AGW_HARNESS__WORKSPACE_ROOT="${WORK_DIR}/repos"
export AGW_HARNESS__WORKTREE_ROOT="${WORK_DIR}/worktrees"
export AGW_HARNESS__ARTIFACTS_ROOT="${WORK_DIR}/artifacts"
export AGW_HARNESS__AUTO_COMMIT=false
export AGW_HARNESS__RELAY_MAX_TIME_SECONDS=60
export AGW_SERVICE__RATE_LIMITING__ENABLED=false

TERM="${TERM:-xterm-256color}"
echo "Starting Agents Gateway on port ${GATEWAY_PORT}..."
uv run agents-gateway run --port "${GATEWAY_PORT}" >/dev/null 2>&1 &
GATEWAY_PID="$!"
sleep 2

if ! wait_for_http "${BASE_URL}/health" 30; then
    echo "FAIL: gateway did not become healthy"
    exit 1
fi
echo "Gateway is healthy."

# ---------------------------------------------------------------------------
# 4. Create a harness_session task
# ---------------------------------------------------------------------------
TASK_BODY=$(cat <<EOF
{
  "title": "e2e local task",
  "brief": "Write result.txt to scratch repo and pass verification.",
  "repo": {"url": "file://${SCRATCH_REPO}", "owner": "o",
           "name": "r", "base_branch": "master"},
  "execution": {"mode": "harness_session", "harness_profile": "fake-test"},
  "goal": {"strategy": "auto",
           "text": "/goal Write result.txt. AGENT_SCRATCH_FILE:result.txt"},
  "verification": {"required": true, "commands": [
    {"name": "check file exists", "command": "ls result.txt", "required": true}
  ]},
  "artifacts": {"html_report": true}
}
EOF
)

echo "Creating harness task..."
TASK_RESP=$(curl -sf -X POST "${BASE_URL}/tasks" \
    -H "Content-Type: application/json" \
    -d "$TASK_BODY" 2>/dev/null) || {
    echo "FAIL: task creation returned non-2xx"
    exit 1
}
TASK_ID=$(echo "$TASK_RESP" | json_extract id)
if [ -z "$TASK_ID" ]; then
    echo "FAIL: could not parse task id"
    exit 1
fi
echo "Task created: ${TASK_ID}"

# ---------------------------------------------------------------------------
# 5. Trigger /tasks/{id}/run
# ---------------------------------------------------------------------------
echo "Triggering /tasks/${TASK_ID}/run..."
RUN_RESP=$(curl -sf -X POST "${BASE_URL}/tasks/${TASK_ID}/run" \
    -H "Content-Type: application/json" 2>/dev/null) || {
    echo "FAIL: run trigger returned non-2xx"
    exit 1
}

# ---------------------------------------------------------------------------
# 6. Poll for terminal status (limited iterations)
# ---------------------------------------------------------------------------
echo "Polling task status..."
FINAL_STATUS=""
for i in $(seq 1 60); do
    sleep 1
    TASK=$(curl -sf "${BASE_URL}/tasks/${TASK_ID}" 2>/dev/null) || continue
    STATUS=$(echo "$TASK" | json_extract status)
    case "$STATUS" in
        completed|failed|blocked_external|cancelled|stalled)
            FINAL_STATUS="$STATUS"
            break
            ;;
        *)
            # still pending
            ;;
    esac
done
echo "Final status: ${FINAL_STATUS}"

if [ -z "$FINAL_STATUS" ]; then
    (echo "FAIL: task did not reach terminal status within 60 s")
    FAIL=$((FAIL+1))
elif [ "$FINAL_STATUS" = "completed" ]; then
    PASS=$((PASS+1))
    echo "Task completed."
else
    FAIL=$((FAIL+1))
    echo "Task ended with status ${FINAL_STATUS} (expected completed)."
fi

# ---------------------------------------------------------------------------
# 7. Fetch verification run + artifacts
# ---------------------------------------------------------------------------
# The agent_run_id == task_id in the harness_session path.
AGENT_RUN_ID="$TASK_ID"
echo "Fetching verification for agent_run ${AGENT_RUN_ID}..."
VERIF=$(curl -sf "${BASE_URL}/agent-runs/${AGENT_RUN_ID}/verification" \
    2>/dev/null) || echo "(no verification run persisted)"
echo "Verification: ${VERIF}"

VERIF_STATUS=$(echo "$VERIF" | json_extract status)
if [ "$VERIF_STATUS" = "passed" ]; then
    PASS=$((PASS+1))
    echo "Verification passed."
else
    FAIL=$((FAIL+1))
    echo "Verification status: ${VERIF_STATUS} (expected passed)."
fi

echo "Listing artifacts..."
ARTIFACTS=$(curl -sf "${BASE_URL}/agent-runs/${AGENT_RUN_ID}/artifacts" \
    2>/dev/null) || echo "(no artifacts persisted)"
echo "Artifacts: ${ARTIFACTS}"

# Check the html report file exists
REPORTS_DIR="${WORK_DIR}/artifacts/${AGENT_RUN_ID}/reports"
if [ -f "${REPORTS_DIR}/review-report.html" ]; then
    PASS=$((PASS+1))
    echo "HTML report present."
else
    FAIL=$((FAIL+1))
    echo "HTML report missing at ${REPORTS_DIR}/review-report.html"
fi

# ---------------------------------------------------------------------------
# 8. Summary
# ---------------------------------------------------------------------------
echo ""
echo "Passed: ${PASS}"
echo "Failed: ${FAIL}"
if [ "$FAIL" -eq 0 ]; then
    echo "[OK] Harness runtime local E2E passed"
    exit 0
else
    echo "[FAIL] Harness runtime local E2E failed"
    exit 1
fi
