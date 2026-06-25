# Agents Gateway Standard

This document defines the architecture, interfaces, and conventions for Agents Gateway.
**This is not Gateway Console.** Agents Gateway is a standalone, production-grade agent gateway that is useful without any future console.

---

## CLI Commands

```bash
agents-gateway run              # Start the gateway server
agents-gateway validate         # Validate all agent manifests and config
agents-gateway list             # List available agents
agents-gateway inspect <id>     # Show details for a specific agent
agents-gateway doctor           # Diagnose gateway health and configuration
agents-gateway version          # Print version
```

All CLI commands accept `--config <path>` to override the default config file location.

---

## Config Format

Config is loaded from `agents-gateway.yaml` with the following precedence (highest to lowest):

1. CLI flags
2. Environment variables (prefixed `AGW_`, double-underscore for nesting: `AGW_SERVICE__PORT=8092`)
3. `agents-gateway.yaml`
4. Built-in defaults

### Default Config

```yaml
service:
  host: "0.0.0.0"
  port: 8092
  mcp_path: "/mcp"

auth:
  mode: "dev-none"

agents:
  dir: "./agents"

storage:
  sqlite_path: "./data/agents-gateway.db"
  artifacts_dir: "./data/artifacts"

observability:
  log_level: "INFO"
  log_format: "json"
  metrics_enabled: true
```

---

## Agent Manifest Format

Each agent directory under `agents.dir` must contain `agent.yaml`.

### Required Fields

```yaml
id: string            # Unique identifier, must match directory name
name: string          # Human-readable name
description: string   # What this agent does
version: string       # Semver
runtime:
  type: string        # e.g. "local-stub", "docker", "process"
```

### Recommended Fields

```yaml
skills: []            # List of skill IDs the agent provides
tools: []             # List of tool IDs the agent exposes
permissions: {}       # Permission mapping
risk_level: low | medium | high
tags: []              # Freeform tags
author: string        # Author or team
```

---

## Profiles

Profiles define named subsets of agents for different environments or use cases.

```yaml
profiles:
  development:
    agents:
      - repo-reviewer
      - test-runner

  operations:
    agents:
      - incident-triager
      - log-analyst
```

The active profile is set via `AGW_PROFILE` env var or `--profile` CLI flag. Only agents in the active profile are visible and runnable. If no profile is set, all valid agents are available.

---

## Catalogs

The catalog is built at startup by scanning the agents directory, validating each manifest, and producing a list of agent entries.

### Catalog Entry Shape

```json
{
  "id": "repo-reviewer",
  "name": "Repo Reviewer",
  "description": "...",
  "version": "0.1.0",
  "path": "repo-reviewer",
  "runtime": {"type": "local-stub"},
  "risk_level": "medium"
}
```

Invalid agents are excluded from the catalog but tracked in metrics and reported via `agents-gateway validate` and the `/inventory` endpoint.

---

## Task State Machine

### States

```
created → queued → running → waiting → running → completed
                                           ↘ failed
```

Any pre-terminal state can transition to `cancelled`.

### Allowed Transitions

```
created   -> queued
created   -> cancelled
queued    -> running
queued    -> cancelled
running   -> waiting
running   -> completed
running   -> failed
running   -> cancelled
waiting   -> running
waiting   -> cancelled
```

### Operations

- Create task
- Get task
- List tasks
- Update task status (with transition validation)
- Cancel task
- Append event (append-only log per task)
- Create run (a run is one execution attempt of a task)
- List task events
- List task artifacts

---

## Storage Model

For AGW-001, SQLite is used. Tables:

- `tasks` — id, agent_id, status, input, output, error, created_at, updated_at
- `task_events` — id, task_id, event, data_json, created_at
- `task_runs` — id, task_id, status, started_at, completed_at
- `task_artifacts` — id, task_id, name, path, size_bytes, created_at

All events are append-only. Artifacts are stored on disk under `storage.artifacts_dir` with metadata in SQLite.

---

## HTTP Endpoints

### Management Endpoints

```
GET  /health              # Liveness probe
GET  /ready                # Readiness probe (checks agents dir, storage, auth)
GET  /version              # Version info
GET  /inventory            # Gateway inventory (agent counts, auth mode, etc.)
GET  /metrics              # Prometheus metrics
GET  /docs                 # API documentation redirect

GET  /agents               # List agents (respecting active profile)
GET  /agents/{id}          # Get single agent
POST /agents/validate      # Validate agent manifests
```

### Task Endpoints

```
POST /tasks                # Create a task
GET  /tasks                 # List tasks
GET  /tasks/{id}            # Get task
GET  /tasks/{id}/events     # Get task events
GET  /tasks/{id}/artifacts  # Get task artifacts
POST /tasks/{id}/cancel     # Cancel a task
```

All endpoints return JSON except `/metrics` (Prometheus text format).

---

## MCP Tools

The gateway exposes MCP tools via the MCP protocol at `service.mcp_path` (default: `/mcp`).

### Required Tools

| Tool Name              | Description                              |
|------------------------|------------------------------------------|
| `agents_list`          | List available agents                    |
| `agents_search`        | Search agents by keyword                 |
| `agents_inspect`       | Get details for a specific agent         |
| `agent_task_create`    | Create a task for an agent               |
| `agent_task_get`       | Get task status                          |
| `agent_task_events`    | Get events for a task                    |
| `agent_task_artifacts` | Get artifacts for a task                 |
| `agent_task_cancel`    | Cancel a task                            |

All tools return JSON strings. Tools respect the active profile. Tools do not expose invalid agents as runnable.

---

## Logs

### Structured Log Events

```
service_start
service_ready
agent_scan_started
agent_scan_completed
agent_invalid
agent_list
agent_search
agent_inspect
task_created
task_queued
task_started
task_completed
task_failed
task_cancelled
artifact_created
auth_success
auth_failure
request_completed
```

### Required Log Fields

```
timestamp       # ISO 8601 UTC
level           # DEBUG, INFO, WARNING, ERROR, CRITICAL
service         # "agents-gateway"
environment     # dev, staging, production
event           # One of the structured event names above
request_id      # UUID for request correlation
message         # Human-readable summary
duration_ms     # Where applicable
task_id         # Where applicable
agent_id        # Where applicable
error           # Error detail where applicable
```

Production mode uses JSON format. Development mode may use human-readable format. No secrets are ever logged.

---

## Metrics

Prometheus-compatible metrics exposed at `/metrics`:

```
agents_gateway_up
agents_gateway_ready
agents_total
agents_invalid_total
tasks_total
tasks_created_total
tasks_completed_total
tasks_failed_total
tasks_cancelled_total
active_runs
artifacts_total
requests_total
request_errors_total
request_duration_seconds
```

---

## Auth Modes

| Mode                | Description                                              |
|---------------------|----------------------------------------------------------|
| `dev-none`          | No authentication. Explicit and unsafe. Not for production. |
| `cloudflare-access` | Validate Cloudflare Access JWTs. May adapt Skills Gateway auth code. |
| `internal-only`     | Only Docker-internal or localhost access allowed.         |

- Auth mode appears in `/inventory` and `/ready`.
- Missing production auth config (`dev-none` in production) must fail clearly.
- No secrets are logged.

---

## Docker Deployment

- `Dockerfile` at repo root, building the `agents_gateway` package.
- `docker-compose.yml` at repo root for single-command local deployment.
- `.env.example` for required environment variables.
- Gateway must start with `docker compose up` after `cp .env.example .env`.

---

## Test Requirements

- Unit tests for config loading, manifest validation, state machine, storage, runtime
- CLI tests for all commands
- HTTP endpoint tests for all endpoints
- MCP tool tests where practical
- All tests runnable via `uv run pytest tests/ -v`

---

## Smoke Test Requirements

- `scripts/smoke-test.sh` must:
  - Start the gateway in background
  - Hit all management endpoints
  - Create and retrieve a task
  - Cancel a task
  - Verify metrics changed
  - Clean up with `trap`

---

## Live E2E Requirements

Before opening a PR:

1. Docker Compose deployment must start cleanly
2. All management endpoints must respond
3. Task create/lifecycle/cancel must work end-to-end
4. Metrics must reflect task activity
5. Logs must contain lifecycle events

If live E2E cannot be performed due to missing environment, document the blocker in `docs/E2E_REPORT.md` and do not open a PR.
