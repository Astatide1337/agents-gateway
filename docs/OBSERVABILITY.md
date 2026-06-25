# Observability Reference

Agent Gateway provides structured logging and Prometheus-compatible metrics. This document covers the logging format, metric names, and the metrics endpoint.

## Structured Logging

The gateway emits structured logs in JSON format when `observability.log_format` is set to `json` (the default). Set to `text` for human-readable output during development.

### JSON Format

Each log line is a single JSON object:

```json
{
  "timestamp": "2025-01-15T10:30:00.123Z",
  "level": "info",
  "event": "request.completed",
  "method": "GET",
  "path": "/inventory",
  "status": 200,
  "duration_ms": 12,
  "request_id": "req_01HXYZ"
}
```

### Event Types

Events identify the kind of log entry. Known events include:

| Event | Description |
|---|---|
| `server.starting` | Gateway is initializing |
| `server.ready` | Gateway is accepting requests |
| `request.completed` | HTTP request finished |
| `task.created` | New task submitted |
| `task.running` | Task execution started |
| `task.completed` | Task finished successfully |
| `task.failed` | Task failed |
| `task.cancelled` | Task cancelled |
| `catalog.loaded` | Agent catalog built at startup |
| `auth.denied` | Authentication check failed |
| `config.loaded` | Configuration applied |

### Required Fields

Every structured log entry includes the following fields:

| Field | Type | Description |
|---|---|---|
| `timestamp` | string (ISO 8601) | UTC timestamp of the log entry |
| `level` | string | Log level: `debug`, `info`, `warning`, `error` |
| `event` | string | Identifier for the event type |

Additional fields are included based on the event type (e.g. `method`, `path`, `status` for HTTP events; `task_id`, `agent_id` for task events).

No secrets, tokens, or credentials are ever included in log output.

## Metrics

The gateway exposes metrics in Prometheus text format at the `/metrics` endpoint.

### Endpoint

```
GET /metrics
```

Returns Prometheus-formatted metrics with content type `text/plain; version=0.0.4; charset=utf-8`.

### Metric Names

| Metric | Type | Description |
|---|---|---|
| `agw_http_requests_total` | counter | Total number of HTTP requests processed |
| `agw_http_request_duration_seconds` | histogram | Request duration in seconds, labeled by method, path, and status |
| `agw_http_requests_in_progress` | gauge | Number of HTTP requests currently in flight |
| `agw_tasks_created_total` | counter | Total number of tasks created |
| `agw_tasks_completed_total` | counter | Total number of tasks completed successfully |
| `agw_tasks_failed_total` | counter | Total number of tasks that failed |
| `agw_tasks_cancelled_total` | counter | Total number of tasks cancelled |
| `agw_tasks_running` | gauge | Number of tasks currently in the `running` state |
| `agw_agents_loaded` | gauge | Number of agents in the catalog |
| `agw_auth_denied_total` | counter | Total number of denied authentication attempts |

### Example Output

```
# HELP agw_http_requests_total Total HTTP requests
# TYPE agw_http_requests_total counter
agw_http_requests_total{method="GET",path="/inventory",status="200"} 42

# HELP agw_http_request_duration_seconds Request duration
# TYPE agw_http_request_duration_seconds histogram
agw_http_request_duration_seconds_bucket{method="GET",path="/inventory",status="200",le="0.01"} 30
agw_http_request_duration_seconds_bucket{method="GET",path="/inventory",status="200",le="0.05"} 40
agw_http_request_duration_seconds_bucket{method="GET",path="/inventory",status="200",le="0.1"} 42
agw_http_request_duration_seconds_sum{method="GET",path="/inventory",status="200"} 1.23
agw_http_request_duration_seconds_count{method="GET",path="/inventory",status="200"} 42

# HELP agw_tasks_created_total Tasks created
# TYPE agw_tasks_created_total counter
agw_tasks_created_total{agent_id="echo-agent"} 10

# HELP agw_agents_loaded Agents in catalog
# TYPE agw_agents_loaded gauge
agw_agents_loaded 3
```

## Integration with Prometheus

Add the gateway as a scrape target in your Prometheus configuration:

```yaml
scrape_configs:
  - job_name: agent-gateway
    static_configs:
      - targets: ["localhost:8902"]
    metrics_path: /metrics
    scrape_interval: 15s
```
