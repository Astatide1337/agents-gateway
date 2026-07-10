# Verification

Verification is what makes Agents Gateway more than "yet another harness starter". The runtime gates `completed` status behind mandatory verification commands. A harness claiming done only transitions the session to `verifying`; verification must pass before the runtime grants `completed`.

## What verification does

For each task that reaches the `verifying` phase, the runtime runs every configured verification command sequentially inside the worktree:

1. **Env-required gate**: if the command declares `env_required: [VAR1, VAR2]` (this is only set by the runtime for live_e2e commands), the runner first checks those env vars are present in `_safe_env()`. Missing → the command is marked `blocked=True`, `blocked_reason="missing_credentials: VAR1, VAR2"`, and the run halts immediately (no subprocess invocation).
2. **Subprocess invocation**: otherwise, the runner invokes `shlex.split(command)` via `subprocess.run(...)` with `cwd=worktree.path`, `timeout=command_timeout_seconds`, env=`_safe_env()`.
3. **Capture**: stdout+stderr are captured to a per-command log artifact under `artifacts/<agent_run_id>/logs/verification-<name>.txt`.
4. **Recording**: a `VerificationCommandResult` dataclass records `exit_code`, `passed`, `output_artifact` path, and any `blocked_reason`.
5. **Decision**:
   - **Pass** (all required commands returned 0 and none blocked): `VerificationRun.status = passed` → runtime emits `verification.passed`, captures git diff + artifacts + HTML report, marks session `completed`.
   - **Blocked** (any required command blocked): `VerificationRun.status = blocked` → runtime emits `verification.blocked`, creates `needs_credentials` Composer interactions for each blocked command, marks the session `blocked_external`, and exits.
   - **Fail** (any required command returned non-zero, no blocked commands): `VerificationRun.status = failed` → runtime calls `feed_failure_back` to push the failed command summary back into the tmux session, transitions the session back to `running`, and the loop continues. The harness sees a `VERIFICATION FEEDBACK:` block and can iterate.

## The `_safe_env` security boundary

Subprocesses NEVER receive the gateway's own secrets. Only this allow-list propagates into the child environment:

```
PATH, HOME, LANG, LC_ALL, LC_CTYPE, TERM, SHELL, USER, USERNAME,
PYTEST_DISABLE_PLUGIN_AUTOLOAD, UV_CACHE_DIR, PYTHONPATH,
VIRTUAL_ENV
```

Anything not in this list — including `CONDUCTOR_*_GATEWAY_INTERNAL_TOKEN`, GitHub tokens configured at the gateway level, the cloudflare JWKS client config, etc. — is NOT visible to the harness or verification subprocesses. If a verificarion command needs a token (e.g. for live E2E), the user must explicitly instruct Composable Composer to inject that secret into the task spec's env_required array; Agents Gateway itself never injects its own secrets.

## Required vs optional commands

The task spec's `verification.commands[].required` flag has the same meaning as in pytest:

- `required: true`: blocking — a failure or block here contributes to the run's overall status
- `required: false`: non-blocking — recorded but never blocks completion even if it fails

## Live E2E verification

Live E2E is treated as a separate verification command (the spec's `verification.live_e2e` block) that the runtime appends to the commands list:

```json
"verification": {
  "required": true,
  "commands": [...],
  "live_e2e": {
    "required": true,
    "command": "bash scripts/e2e-live-gateway-hub.sh",
    "env_required": ["GITHUB_TOKEN", "CONDUCTOR_BASE_URL"]
  }
}
```

The `env_required` array ensures the runtime surfaces missing credentials as a structured `needs_credentials` Composer interaction rather than fake-passing the test.

## Failed verification feedback

When verification fails, the runner sends a structured `VERIFICATION FEEDBACK:` block back into the tmux session via the driver's `send_reply` mechanism:

```
ASSISTANT REPLY (from Composer):
VERIFICATION FEEDBACK:
The following verification commands failed:
- uv run pytest -q (exit code 1)
- bash scripts/e2e-local.sh (exit code 127)
Please review the test output, fix the failure, and re-run verification.
```

The harness sees this block, can iterate, and eventually prints `DONE.` again. The supervisor re-runs verification. This loop continues until either verification passes (→ `completed`) or one of the safety nets trips (→ `stalled`).

`max_verify_iterations` defaults to `50` to give a real LLM-backed harness enough room to iterate without runaway cost. Per the milestone spec, failures are normal work and must not auto-fail the session.

## Verification artifacts

For every command executed, the runtime writes:

```
artifacts/<agent_run_id>/logs/verification-<name>.txt
```

The artifact contains the captured stdout+stderr along with exit-code and timing metadata. The `GET /artifacts/{id}?view=true` endpoint streams the raw bytes; the `GET /agent-runs/{agent_run_id}/artifacts` endpoint lists all artifacts for the run.

## Manual verification trigger

`POST /agent-runs/{agent_run_id}/verify` re-runs verification on demand using the commands stored in the task spec (it does NOT accept new commands in the request body — Composer already specified them at task-creation time).

`GET /agent-runs/{agent_run_id}/verification` returns the most recent verification run (most recent first via `ORDER BY started_at DESC LIMIT 1`).
