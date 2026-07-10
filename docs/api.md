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
| `GET` | `/sessions/{id}/capture` | Capture recent tmux output |
| `POST` | `/sessions/{id}/send` | Send text into the session |
| `POST` | `/sessions/{id}/stop` | Force-stop the session |

`POST /sessions/{id}/send` body:

```json
{"text": "Continue working on the spec.", "submit": true}
```

If `submit` is true (default), the gateway sends `<Enter>` after the text.

### Interactions

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/interactions` | List interactions (filters: `status`, `task_id`, `agent_run_id`) |
| `GET` | `/interactions/{id}` | Get a single interaction |
| `POST` | `/interactions/{id}/reply` | Composer replies; text delivered into the session |
| `POST` | `/interactions/{id}/cancel` | Composer cancels the interaction |

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

### Harness task creation

`POST /tasks` accepts harness-session tasks when any of these conditions holds:

- `body["execution"]["mode"] == "harness_session"`
- `body["agent_id"] == "harness_session"`
- `body["runtime_type"] == "harness_session"`

The full task body is preserved as-is in the `input` column. The legacy `agent_id` flow is unchanged.

### MCP protocol

The MCP protocol is served at `POST /mcp`. The tool inventory includes both legacy `agent_*` tools and new `harness_*` tools — see `docs/runtime.md` for the full tool inventory.
