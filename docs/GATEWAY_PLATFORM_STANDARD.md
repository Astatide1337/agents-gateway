# Gateway Platform Standard

This document defines shared conventions for Skills Gateway and Agents Gateway.

## Service roles

Skills Gateway is the capability registry. It discovers, validates, searches, inspects, and exposes skill files and resources.

Agents Gateway is the task lifecycle layer. It discovers agents, creates tasks, stores task state, records events, and exposes artifacts.

## Required management endpoints

Every gateway should expose:

```text
GET /health
GET /ready
GET /version
GET /inventory
GET /metrics
GET /docs
/mcp
```

## MCP-first rule

If an operation is important for an AI client, it should be available as an MCP tool or resource.

## Tool result shape

Successful result:

```json
{
  "ok": true,
  "data": {},
  "error": null,
  "meta": {
    "gateway": "agents-gateway",
    "version": "0.1.0",
    "request_id": "..."
  }
}
```

Error result:

```json
{
  "ok": false,
  "data": null,
  "error": {
    "code": "not_found",
    "message": "Readable error message",
    "details": {}
  },
  "meta": {
    "gateway": "agents-gateway",
    "version": "0.1.0",
    "request_id": "..."
  }
}
```

## Auth modes

Both gateways should support:

- `dev-none`
- `internal-only`
- `cloudflare-access`

Production mode must not silently run with `dev-none`.

## Environment variables

Use gateway-specific prefixes:

```text
SKG_ for Skills Gateway
AGW_ for Agents Gateway
```

Nested config should use double underscores:

```text
SKG_SERVICE__PORT=8091
AGW_SERVICE__PORT=8092
AGW_STORAGE__SQLITE_PATH=./data/agents-gateway.db
```

Precedence should be:

```text
CLI flags > environment variables > YAML config > built-in defaults
```

## Resource URI conventions

Skills Gateway:

```text
skill://{skill_id}/manifest
skill://{skill_id}/entrypoint
skill://{skill_id}/file/{path}
```

Agents Gateway:

```text
agent://{agent_id}/manifest
task://{task_id}
task://{task_id}/events
task://{task_id}/artifacts/{artifact_name}
```

## Observability

Logs should include:

```text
timestamp
level
service
environment
event
request_id
message
duration_ms
error
```

Agents Gateway should include `task_id` and `agent_id` where applicable.

## Metrics

Shared metrics:

```text
gateway_up
gateway_ready
requests_total
request_errors_total
request_duration_seconds
mcp_tool_calls_total
mcp_tool_errors_total
```

Skills Gateway metrics:

```text
skills_total
skills_invalid_total
skill_reads_total
```

Agents Gateway metrics:

```text
agents_total
agents_invalid_total
tasks_created_total
tasks_completed_total
tasks_failed_total
tasks_cancelled_total
active_runs
artifacts_total
```

## Versioning

Tool output schemas and manifest schemas are contracts. Breaking changes should require a version bump and migration notes.
