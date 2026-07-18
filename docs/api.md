# HTTP API

Agents Gateway exposes a hybrid HTTP + MCP surface. The MCP protocol is mounted at `/mcp` (FastMCP custom-route handler) and shares the same auth middleware as the HTTP routes.

## Auth

All non-public paths are protected by the global auth middleware (`AuthHandler`). Consult `SECURITY.md` and `README.md` for the full auth model. In short:

- `dev-none`: open (local dev only)
- `internal-only`: `X-Auth-Internal-Token` header matches configured shared secret
- `cloudflare-access`: `Cf-Access-Jwt-Assertion` header verified via CF JWKS

In tests and local dev, `dev-none` is the default.

## Legacy task/runtime plane

### Management

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness (no auth) |
| `GET` | `/ready` | Readiness (no auth) |
| `GET` | `/version` | Version string (no auth) |
| `GET` | `/agents` | Agent inventory list |
| `GET` | `/agents/{id}` | Agent inventory detail |
| `GET` | `/inventory` | Service inventory |
| `GET` | `/metrics` | Prometheus-style metrics |

### Tasks

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/tasks` | Create task (legacy `agent_id` + `input`) |
| `GET` | `/tasks` | List tasks (optional `status` filter) |
| `GET` | `/tasks/{id}` | Task detail (full record) |
| `POST` | `/tasks/{id}/run` | Enqueue task for execution (202 Accepted) |
| `GET` | `/tasks/{id}/events` | Event stream |
| `GET` | `/tasks/{id}/artifacts` | Artifact list |
| `POST` | `/tasks/{id}/cancel` | Cancel task |

State transitions required by the legacy task model: `created → queued → running → {completed, failed, waiting, cancelled}`. Invalid transitions return HTTP 409.

## Harness-runtime plane

### Harness profiles

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/harness-profiles` | List all registered profiles |
| `GET` | `/harness-profiles/{name}` | Get a single profile |
| `POST` | `/harness-profiles/validate` | Validate goal strategy compatibility |
| `GET` | `/harness-profiles/{name}/availability` | Structured availability report (binary_present, credentials_present, runnable) |

The validate endpoint accepts a JSON body:

```json
{"name": "opencode-deepseek", "goal_strategy": "slash_goal"}
```

Returns `{"valid": true, "profile": <profile dict>}` on success or HTTP 400/404 with a structured error.

### Worktrees

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/worktrees` | List all worktrees |
| `GET` | `/worktrees/{id}` | Get a single worktree |
| `GET` | `/tasks/{task_id}/worktree` | Get the worktree for a task |

Returned shape:

```json
{
  "id": "wt_...",
  "task_id": "task_...",
  "agent_run_id": "run_...",
  "repo_workspace_id": "repo_ws_...",
  "branch": "agent/<task_id>-<slug>",
  "base_branch": "master",
  "path": "/var/lib/agents-gateway/worktrees/.../task_...",
  "status": "created|active|dirty|committed|failed|cleaned_up",
  "created_at": "...",
  "deleted_at": null,
  "metadata": {}
}
```

### Sessions

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/sessions` | List sessions (filters: `status`, `task_id`) |
| `GET` | `/sessions/{id}` | Get a single session |
| `GET` | `/tasks/{task_id}/session` | Get the active session for a task |
| `GET` | `/sessions/{id}/capture` | Capture recent tmux output (redacted, returns structured response) |
| `POST` | `/sessions/{id}/send` | Send text into the session (emits `composer.session_send` event) |
| `POST` | `/sessions/{id}/stop` | Force-stop the session |

`GET /sessions/{id}/capture?lines=N` returns:

```json
{
  "session_id": "session_...",
  "status": "running",
  "capture": "<redacted tmux output text>",
  "captured_at": "2024-01-01T00:00:00Z",
  "lines": 42
}
```

The `capture` field is passed through `redact_text()` which applies
the same redaction patterns as the HTML review reports
(`Authorization` headers, GitHub tokens, URL credentials, etc.) so
no secrets leak to Composer clients.

### Interactions

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/interactions` | List interactions (filters: `status`, `task_id`, `agent_run_id`) |
| `GET` | `/interactions/{id}` | Get a single interaction |
| `POST` | `/interactions/{id}/reply` | Composer replies; text delivered into the session |
| `POST` | `/interactions/{id}/cancel` | Composer cancels the interaction (emits `composer.interaction.cancelled` event) |

`POST /interactions/{id}/reply` body:

```json
{"reply": "Proceed with lowercase. Document any assumption in the report."}
```

The reply is wrapped in an `ASSISTANT REPLY (from Composer):` header line before being sent into the tmux session, so the harness can distinguish it from user prompts. The interaction is marked `answered` and the session transitions to `running`.

### Verification

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/agent-runs/{id}/verification` | Get the latest verification run |
| `POST` | `/agent-runs/{id}/verify` | Trigger a fresh verification run |

The `POST /verify` endpoint reads the verification commands from the stored task spec (`task.input` JSON) — not from the request body. Returns:

```json
{
  "id": "verif_...",
  "agent_run_id": "run_...",
  "task_id": "task_...",
  "status": "running|passed|failed|blocked",
  "commands": [
    {
      "name": "unit tests",
      "command": "uv run pytest -q",
      "required": true,
      "exit_code": 0,
      "passed": true,
      "output_artifact": "<path>",
      "blocked": false,
      "blocked_reason": ""
    }
  ]
}
```

### Artifacts

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/agent-runs/{id}/artifacts` | List artifacts for a run |
| `GET` | `/artifacts/{id}` | Get artifact metadata (or raw bytes with `?view=true`) |

`GET /artifacts/{id}?view=true` streams the raw file bytes with the
correct `Content-Type` (defaults to
`application/octet-stream` if the artifact has no recorded mime type).

### Unified agent-run view

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/agent-runs/{id}` | Unified view: task record + harness block (session_id, status, harness_profile, worktree_id) + full event stream |

Returns:

```json
{
  "id": "task_...",
  "agent_id": "opencode-deepseek",
  "status": "completed",
  "input": "{...}",
  "created_at": "...",
  "updated_at": "...",
  "harness": {
    "session_id": "session_...",
    "status": "completed",
    "harness_profile": "opencode-deepseek",
    "worktree_id": "wt_..."
  },
  "events": [
    {"task_id": "task_...", "type": "task.received", "data": {...}, "created_at": "..."},
    {"task_id": "task_...", "type": "runtime_selected", "data": {...}, "created_at": "..."},
    ...
    {"task_id": "task_...", "type": "agent_run.completed", "data": {...}, "created_at": "..."}
  ]
}
```

### Cleanup / retention

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/cleanup/dry-run` | Preview what would be deleted under the retention policy (no disk changes) |
| `POST` | `/cleanup/run` | Execute retention cleanup (honours `cleanup_dry_run`; use `?force=true` to override) |

Retention config env vars:

| Env var | Default | Purpose |
|---------|---------|---------|
| `AGW_HARNESS__ARTIFACT_RETENTION_DAYS` | `14` | Artifacts older than this are pruning candidates |
| `AGW_HARNESS__WORKTREE_RETENTION_DAYS` | `7` | Worktrees older than this are pruning candidates |
| `AGW_HARNESS__MAX_ARTIFACT_BYTES` | `1073741824` (1 GB) | Total artifact budget — oldest artifacts over budget are pruned |
| `AGW_HARNESS__CLEANUP_DRY_RUN` | `true` | When `true`, `/cleanup/run` acts as a dry-run unless `?force=true` |

Cleanup **never** touches sessions that are still active (`status in
(created, starting, running, waiting_for_reply, verifying)`) or their
worktrees. Pruned artifacts are removed from disk but the DB row is
retained for audit trail.

### Harness task creation

`POST /tasks` accepts harness-session tasks when any of these conditions holds:

- `body["execution"]["mode"] == "harness_session"`
- `body["agent_id"] == "harness_session"`
- `body["runtime_type"] == "harness_session"`

The full task body is preserved as-is in the `input` column. The legacy `agent_id` flow is unchanged.

### MCP protocol

The MCP protocol is served at `POST /mcp`. The tool inventory includes:

**Legacy task tools:**
- `agents_list` — list agent catalog entries
- `agents_search` — search agents by keyword
- `agents_inspect` — get one agent's manifest
- `agent_task_create` — create a legacy task
- `agent_task_get` — get a task record
- `agent_task_events` — list task events

**Harness tools (`harness_*` prefix):**
- `harness_task_create` — create a harness_session task
- `harness_task_run` — enqueue a harness task
- `harness_list_worktrees` — list all worktrees
- `harness_list_sessions` — list harness sessions
- `harness_get_session` — get one harness session
- `harness_get_session_capture` — capture tmux output
- `harness_send_to_session` — send text into a session
- `harness_stop_session` — force-stop a session
- `harness_list_interactions` — list Composer interactions
- `harness_get_interaction` — get one interaction
- `harness_reply_interaction` — Composer reply to an interaction
- `harness_get_verification` — get latest verification run
- `harness_list_artifacts` — list proof artifacts
- `harness_get_artifact` — get artifact metadata or content

**Unified tools (`agents_*` prefix — recommended for new clients):**
- `agents_check_harness_availability` — structured availability report for a profile
- `agents_list_sessions` — list harness sessions (task_id/status filter)
- `agents_get_session` — get one harness session
- `agents_capture_session` — redacted tmux capture (structured response)
- `agents_send_session` — send text into a session + emit `composer.session_send` event
- `agents_list_interactions` — list Composer interactions
- `agents_reply_interaction` — Composer reply (delivers to session + emits event)
