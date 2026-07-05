# Agents Gateway Standard

This document defines the production standard for `agents-gateway`.

It is intentionally standalone and does **not** assume Gateway Console.

---

## Goals

Agents Gateway provides:

- Agent discovery
- Agent manifest validation
- MCP tools for agent listing, inspection, and task lifecycle
- HTTP management endpoints
- Task creation, execution tracking, event logs, and artifacts
- Configurable auth modes
- Structured logs and metrics
- Docker-ready deployment

---

## Configuration

### Precedence

Configuration is loaded in this order:

1. CLI flags
2. Environment variables prefixed with `AGW_`
3. `agents-gateway.yaml`
4. Built-in defaults

### Default Config

```yaml
service:
  host: "0.0.0.0"
  port: 8092
  mcp_path: "/mcp"

auth:
  mode: "dev-none" # dev-none | cloudflare-access | internal-only

agents:
  dir: "./agents"

storage:
  sqlite_path: "./data/agents-gateway.db"
  artifacts_dir: "./data/artifacts"

observability:
  log_level: "INFO"
  log_format: "json"
  metrics_enabled: true

integrations:
  skills_gateway:
    enabled: false
    base_url: "http://localhost:8091"
    mcp_path: "/mcp"
    strict: false
    timeout_seconds: 5.0
```

### Environment Variables

Nested config uses double underscores:

```bash
AGW_SERVICE__PORT=8092
AGW_SERVICE__MCP_PATH=/mcp
AGW_AUTH__MODE=dev-none
AGW_AGENTS__DIR=./agents
AGW_STORAGE__SQLITE_PATH=./data/agents-gateway.db
AGW_STORAGE__ARTIFACTS_DIR=./data/artifacts
AGW_OBSERVABILITY__LOG_LEVEL=INFO
```

---

## Agent Manifest

Each agent lives in its own directory:

```text
agents/
  repo-reviewer/
    agent.yaml
    README.md
```

Required `agent.yaml` fields:

```yaml
id: repo-reviewer
name: Repo Reviewer
description: Reviews repository changes and produces structured feedback.
version: 0.1.0
runtime:
  type: local-stub
```

Recommended optional fields:

```yaml
skills:
  - code-review

tools:
  - github.fetch_pr
  - github.fetch_pr_diff

permissions:
  github:
    read: true
    write: false

risk_level: low # low | medium | high

tags:
  - github
  - code-review

author: Astatide
```

Validation rules:

- `id` should match the directory name.
- `name`, `description`, and `version` are required.
- `runtime.type` is required.
- Unknown runtime types should be reported clearly.
- Invalid agents are excluded from the active catalog but listed in validation results.

---

## Skills Gateway Integration

Agents Gateway may reference skills by ID in agent manifests. Skills remain owned by Skills Gateway; Agents Gateway should not duplicate the skill catalog.

### Config

```yaml
integrations:
  skills_gateway:
    enabled: true
    base_url: "http://skills-gateway:8091"
    mcp_path: "/mcp"
    strict: false
    timeout_seconds: 5.0
```

### Behavior

- If `enabled` is false, skill references remain local metadata on the agent manifest.
- If `enabled` is true and `strict` is false, missing or unreachable Skills Gateway references should produce warnings, not block catalog loading.
- If `enabled` is true and `strict` is true, missing skill references should be validation errors.
- Agents Gateway should consume Skills Gateway through stable MCP tools/resources, not by importing Skills Gateway implementation code.
- Task events for future skill-backed execution should record the referenced skill ID and version.

### Expected Skills Gateway contract

Minimum required capabilities:

- `skills_list` returns available skill summaries.
- `skills_inspect` returns complete metadata for a skill ID.
- `skill_read` reads a skill file by path.

---

## Profiles

Profiles define named subsets of agents:

```yaml
profiles:
  dev:
    agents:
      - repo-reviewer
      - test-runner
```

Active profile can be selected with:

```bash
AGW_PROFILE=dev
```

or CLI:

```bash
agents-gateway run --profile dev
```

If no profile is selected, all valid agents are available.

---

## Catalog Behavior

On startup:

1. Scan `agents.dir`
2. Load each `agent.yaml`
3. Validate schema
4. Exclude invalid agents from active catalog
5. Record validation errors
6. Apply active profile filter

Catalog operations:

- list agents
- search agents
- inspect agent
- validate all manifests

---

## Task Lifecycle

Task states:

```text
created -> queued -> running -> waiting -> running -> completed
                         |          |             |
                         |          |             -> failed
                         |          -> cancelled
                         -> cancelled
```

Terminal states:

- `completed`
- `failed`
- `cancelled`

Allowed transitions:

| From      | To                                      |
|-----------|------------------------------------------|
| created   | queued, cancelled                       |
| queued    | running, cancelled                      |
| running   | waiting, completed, failed, cancelled   |
| waiting   | running, cancelled                      |
| completed | none                                    |
| failed    | none                                    |
| cancelled | none                                    |

Every task has:

- `id`
- `agent_id`
- `status`
- `input`
- `output`
- `error`
- `created_at`
- `updated_at`

---

## Storage

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
POST /tasks/{id}/run        # Run a created/queued task with the configured runtime
```

`GET /tasks` supports optional query parameters:

- `status` — filter by task state.
- `agent_id` — filter by agent ID.
- `limit` — maximum number of tasks to return.
- `offset` — pagination offset.

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

Tool behavior:

- Tools respect active profile.
- Tools return JSON-serializable results.
- Missing agents/tasks return structured errors.
- Task-creating tools append task events.

---

## Runtime Contract

Initial runtime types:

- `local-stub`

Future runtime types:

- `process`
- `http`
- `docker`
- `skills-gateway`

Runtime responsibilities:

1. Accept task id and manifest runtime config
2. Move task through state transitions
3. Emit events
4. Write output and artifacts
5. Mark task as completed or failed

---

## Logs

Logs are structured JSON by default.

Required fields:

- timestamp
- level
- service
- environment
- event
- message

Recommended contextual fields:

- request_id
- agent_id
- task_id
- runtime_type
- duration_ms
- error

Example:

```json
{
  "timestamp": "2026-01-01T00:00:00Z",
  "level": "INFO",
  "service": "agents-gateway",
  "environment": "prod",
  "event": "task_created",
  "agent_id": "repo-reviewer",
  "task_id": "task_123",
  "message": "Task created"
}
```

---

## Metrics

Prometheus metrics:

```text
agents_gateway_up
agents_gateway_ready
agents_gateway_agents_total
agents_gateway_agents_invalid_total
agents_gateway_tasks_total
agents_gateway_tasks_created_total
agents_gateway_tasks_completed_total
agents_gateway_tasks_failed_total
agents_gateway_tasks_cancelled_total
agents_gateway_mcp_tool_calls_total
agents_gateway_mcp_tool_errors_total
```

---

## Auth

Auth modes:

### dev-none

No auth. Development only.

### internal-only

Accept requests only from localhost or trusted internal proxy.

### cloudflare-access

Validate Cloudflare Access JWTs.

Required config:

```yaml
auth:
  mode: cloudflare-access
  cloudflare:
    team_domain: example.cloudflareaccess.com
    audience: your-aud-tag
```

---

## CLI

Required commands:

```bash
agents-gateway run
agents-gateway validate
agents-gateway list
agents-gateway inspect <id>
agents-gateway doctor
agents-gateway version
```

---

## Docker

Required:

- `Dockerfile`
- `docker-compose.yml`
- `.env.example`
- healthcheck
- persistent volume for SQLite and artifacts

---

## Tests

Required test groups:

- config loading
- manifest validation
- catalog scanning
- profile filtering
- storage state machine
- MCP tools
- HTTP endpoints
- auth modes
- metrics
- logging
- Docker smoke test

---

## Smoke Test

A smoke test should verify:

1. Server starts
2. `/health` returns ok
3. `/ready` returns ready
4. `/version` returns version
5. `/inventory` returns agent/tool counts
6. `/metrics` returns Prometheus text
7. MCP tools are listed
8. A task can be created, read, cancelled, and inspected

---

## Live E2E Target

A complete live test should prove:

1. Gateway starts from Docker
2. Agent manifests are discovered
3. MCP client lists agents
4. MCP client creates task
5. Runtime executes stub task
6. Events are recorded
7. Artifact is produced
8. Metrics update
