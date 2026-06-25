#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== Agents Gateway Smoke Test ==="

export AGW_AGENTS__DIR="$PROJECT_DIR/agents"
export AGW_STORAGE__SQLITE_PATH="/tmp/agw-smoke-test.db"
export AGW_STORAGE__ARTIFACTS_DIR="/tmp/agw-smoke-artifacts"
export AGW_OBSERVABILITY__LOG_LEVEL=WARNING
export AGW_OBSERVABILITY__LOG_FORMAT=json
export AGW_AUTH__MODE=dev-none

cleanup() {
    echo "Cleaning up..."
    if [ -n "${GATEWAY_PID:-}" ]; then
        kill "$GATEWAY_PID" 2>/dev/null || true
        wait "$GATEWAY_PID" 2>/dev/null || true
    fi
    rm -f /tmp/agw-smoke-test.db
    rm -rf /tmp/agw-smoke-artifacts
    echo "Cleanup complete."
}
trap cleanup EXIT

echo "Starting gateway..."
uv run agents-gateway run --port 18092 &
GATEWAY_PID=$!
sleep 2

BASE_URL="http://127.0.0.1:18092"
FAIL=0

check() {
    local name="$1" url="$2" expected="$3"
    local status
    status=$(curl -s -o /dev/null -w "%{http_code}" "$url" 2>/dev/null || echo "000")
    if [ "$status" = "$expected" ]; then
        echo "  PASS: $name ($status)"
    else
        echo "  FAIL: $name (expected $expected, got $status)"
        FAIL=1
    fi
}

echo "Checking management endpoints..."
check "health" "$BASE_URL/health" "200"
check "ready" "$BASE_URL/ready" "200"
check "version" "$BASE_URL/version" "200"
check "inventory" "$BASE_URL/inventory" "200"
check "metrics" "$BASE_URL/metrics" "200"
check "agents" "$BASE_URL/agents" "200"

echo "Checking task lifecycle..."
TASK_RESP=$(curl -s -X POST "$BASE_URL/tasks" -H "Content-Type: application/json" -d '{"agent_id":"repo-reviewer","input":"test"}')
TASK_ID=$(echo "$TASK_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null || echo "")
if [ -n "$TASK_ID" ]; then
    check "get task" "$BASE_URL/tasks/$TASK_ID" "200"
    check "task events" "$BASE_URL/tasks/$TASK_ID/events" "200"
    check "task artifacts" "$BASE_URL/tasks/$TASK_ID/artifacts" "200"

    CANCEL_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$BASE_URL/tasks/$TASK_ID/cancel" 2>/dev/null || echo "000")
    if [ "$CANCEL_STATUS" = "200" ] || [ "$CANCEL_STATUS" = "409" ]; then
        echo "  PASS: cancel task ($CANCEL_STATUS)"
    else
        echo "  FAIL: cancel task (expected 200 or 409, got $CANCEL_STATUS)"
        FAIL=1
    fi
else
    echo "  FAIL: could not create task"
    FAIL=1
fi

echo "Checking metrics reflect activity..."
METRICS=$(curl -s "$BASE_URL/metrics" 2>/dev/null || echo "")
if echo "$METRICS" | grep -q "tasks_created_total"; then
    echo "  PASS: metrics contains task counters"
else
    echo "  FAIL: metrics missing task counters"
    FAIL=1
fi

echo ""
if [ "$FAIL" = "0" ]; then
    echo "=== SMOKE TEST PASSED ==="
else
    echo "=== SMOKE TEST FAILED ==="
    exit 1
fi
