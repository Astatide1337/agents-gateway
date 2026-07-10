# Runbooks

Operational runbooks for Agents Gateway.

## 1. Boot the gateway locally

```bash
uv sync
cp .env.example .env
uv run agents-gateway run
```

Default port is `8092`. Auth defaults to `dev-none`. Override via env vars (see `README.md` and `docs/runtime.md`).

## 2. Run the local harness E2E

No real Claude/opencode/Codex binaries required.

```bash
bash scripts/e2e-harness-runtime-local.sh
```

Outputs a single pass/fail summary line. Exit code `0` on success.

The script:

1. Boots the gateway on a scratch port using the bundled `fake-test` harness profile (deterministic Python script that writes a file and prints `DONE.`).
2. Creates a scratch repo with an initial commit on `master`.
3. POSTs a harness-session task to `POST /tasks`.
4. Triggers `POST /tasks/{id}/run`.
5. Polls `GET /tasks/{id}` until terminal.
6. Fetches verification + artifacts.
7. Asserts verification passed and HTML report exists on disk.

Useful env vars:

- `AGW_E2E_PORT` — port (default 18093)
- `AGW_E2E_WORK_DIR` — override the scratch workdir
- `AGW_E2E_KEEP=1` — keep the scratch workdir for inspection afterward

## 3. Run the real harness E2E

Requires `opencode` (or `claude` / `codex`) on PATH, configured with LLM access.

```bash
bash scripts/e2e-harness-runtime-real.sh                       # opencode-deepseek profile
AGW_E2E_REAL_PROFILE=claude-code bash scripts/e2e-harness-runtime-real.sh
AGW_E2E_REAL_PROFILE=codex bash scripts/e2e-harness-runtime-real.sh
```

The script:

1. Pre-flights: verifies the required binary is on PATH. If not, it exits with **code 2** and prints `REAL HARNESS E2E BLOCKED: missing <command(s)>` — never fakes success.
2. Boots the gateway with `AGW_HARNESS__USE_FAKE_TMUX=false`.
3. Dispatches a tiny task (add a docstring + run a 2-line pytest) against a disposable scratch repo.
4. Polls for terminal status.
5. Asserts verification passed + artifacts exist.

Useful env vars:

- `AGW_E2E_REAL_PROFILE` — harness profile (default: `opencode-deepseek`)
- `AGW_E2E_BINARIES` — explicit required-binary list (default: derived from profile name)
- `AGW_E2E_PORT` — gateway port (default 18094)
- `AGW_E2E_TIMEOUT_SECONDS` — wall-clock cap (default 600)
- `AGW_E2E_FORCE_BLOCK=1` — force the exit-2 refusal path for testing the mechanism

## 4. Run the test suite

```bash
uv run pytest -q
```

Currently 495 tests passing. Tests use `FakeTmuxDriver` — no real tmux / no real harness dependency.

## 5. Inspect a specific run

Suppose a task failed in production. From the gateway host:

```bash
# Locate the task id (from logs, MCP client, etc.)
TASK_ID=...

# Status + recent events:
curl $AUTH_HEADERS "$BASE_URL/tasks/$TASK_ID"
curl $AUTH_HEADERS "$BASE_URL/tasks/$TASK_ID/events"

# Verification run + artifacts:
curl "$BASE_URL/agent-runs/$TASK_ID/verification"
curl "$BASE_URL/agent-runs/$TASK_ID/artifacts"

# Stream the HTML report raw bytes (open in browser):
curl "$BASE_URL/artifacts/<artifact_id>?view=true" -o report.html
xdg-open report.html

# Live capture of the tmux session (if still alive):
curl "$BASE_URL/sessions/<session_id>/capture"
```

The HTML report (`artifact_kind=html_report`) gives you a complete human-readable review of: task brief, verification commands + pass/fail, git diff summary, commit SHA + PR URL, timeline of the most recent 200 events (with redacted secrets), and links to the raw artifact files.

## 6. Diagnose a stalled session

A `stalled` status means:

- The classifier saw silence for `AGW_HARNESS__SESSION_STALL_SECONDS` seconds, OR
- The runtime hit `AGW_HARNESS__MAX_VERIFY_ITERATIONS` verification rounds without progress, OR
- The runtime hit `AGW_HARNESS__RELAY_MAX_TIME_SECONDS` wall-clock

`talled` is intentionally NOT `failed` — the session may still be alive. From the gateway host:

1. Inspect the session: `GET /sessions/{id}` — check `last_output_at`, `tmux_session` name.
2. Try `GET /sessions/{id}/capture` — see what the harness is currently showing.
3. If you want to push the harness forward: `POST /sessions/{id}/send` with body `{"text": "Continue per spec. Surface remaining unknowns.", "submit": true}`.
4. If you want to terminate: `POST /sessions/{id}/stop`.

The task itself can be cancelled via `POST /tasks/{id}/cancel` only in `queued`/`running` states.

## 7. Diagnose a blocked_external run

`blocked_external` means a verification command reported missing credentials. Look at the `blockers` list in the task result or the `needs_credentials` Composer interaction.

The `missing_env` field tells you exactly which env vars the harness needs. To retry the task with the env vars in place, you'll need to redeploy the gateway with those env vars exported (or pipe them through a more privileged composer-orchestrated flow).

The reported env vars are NEVER auto-injected by Agents Gateway — that would defeat the security boundary.

## 8. Diagnose "the HTML report is missing"

If `GET /agent-runs/{id}/artifacts` returns `[]`:

- The task never reached the verification-pass branch of the runtime
- Check `GET /tasks/{id}/events` for `verification.passed` event — if it's missing, verification didn't pass
- If the event IS present but the artifact is missing, check `AGW_HARNESS__ARTIFACTS_ROOT` exists and is writable by the gateway process user

If the artifact exists but the path is wrong (per-target `path` field), check that `AGW_HARNESS__ARTIFACTS_ROOT` hasn't been changed mid-stack — yes, this is a common bug.

## 9. Diagnose "classifier says running but harness is dead"

If `tmux has-session -t agw_<task_id>` returns 1 (session is gone) but `GET /sessions/{id}` shows `running`, you have a race — the supervisor's `is_alive` check (`tmux.has-session`) happens once per poll. Common causes:

- `tmux` was killed outside the gateway (e.g. shell-triggered `tmux kill-server`)
- The harness process exited normally and tmux exited with it (when the spawn command completes, tmux's session terminates)
- The container rebooted while a session was alive

To fix manually: `POST /sessions/{id}/stop` to transition the record to `cancelled`. Optionally `POST /tasks/{id}/cancel`.

## 10. Diagnose slow verification runs

Per-command timeout defaults to `AGW_HARNESS__COMMAND_TIMEOUT_SECONDS=1800` (30 minutes). If you're seeing a single command take 30 minutes, the harness likely needs a different timeout. To diagnose:

1. Look at the `VerificationCommandResult.duration_seconds` field — this tells you how long the command actually ran.
2. If `duration_seconds == 1800.0` exactly, the timeout was hit (exit_code=124). The command does not run to completion.
3. Raise `AGW_HARNESS__COMMAND_TIMEOUT_SECONDS` if the command legitimately takes that long, or fix the test command to complete faster.

## 11. Containerize this gateway

Out of scope for this milestone — the harness runtime currently runs on host. See `docs/harness-runtime.md` for the long-term containerized roadmap. For now, the gateway runs as a process on the worker host with tmux + git installed. Containerizing the gateway process itself (Docker Compose) only helps the legacy task plane; harness sessions require host tmux. Plan accordingly.

## 12. Rotate the internal token

If you're using `internal-only` auth mode:

1. Generate a new secret: `python3 -c "import secrets; print(secrets.token_urlsafe(32))"`
2. Update the env var `AGW_AUTH__INTERNAL_SECRET` on every gateway replica + every MCP client (Composer, Conductor, etc.)
3. Restart the gateway. Old secrets stop working immediately.
4. Check your logs for unexpected `auth_failed` events — likely a Composer instance still using the old secret.
