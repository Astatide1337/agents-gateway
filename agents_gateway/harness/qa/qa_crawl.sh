#!/usr/bin/env bash
# Runs the interactive QA crawl against a freshly-integrated app.
# Copied into every task worktree's .agent-task/ directory by
# VerificationRunner before verification commands execute — see
# agents_gateway/harness/verification.py.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${1:-.}"

uvx --with playwright python3 "$SCRIPT_DIR/qa_crawl.py" --repo-root "$REPO_ROOT"
exit_code=$?

if [ "$exit_code" -eq 2 ]; then
  echo "[qa_crawl.sh] Playwright's chromium may not be installed — attempting install and retry" >&2
  uvx --with playwright playwright install --with-deps chromium >&2
  uvx --with playwright python3 "$SCRIPT_DIR/qa_crawl.py" --repo-root "$REPO_ROOT"
  exit_code=$?
fi

exit "$exit_code"
