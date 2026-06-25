# Configuration Reference

This document covers the full configuration system for Agent Gateway, including the primary config file, environment variable overrides, CLI flags, and precedence rules.

## Configuration File

Agent Gateway reads its configuration from `agents-gateway.yaml` in the working directory by default. An alternative path can be supplied via the `--config` CLI flag.

## Environment Variable Overrides

Every value in `agents-gateway.yaml` can be overridden with an environment variable using the `AGW_` prefix. Nested keys are separated by double underscores (`__`).

| YAML Path | Environment Variable |
|---|---|
| `service.port` | `AGW_SERVICE__PORT` |
| `service.host` | `AGW_SERVICE__HOST` |
| `auth.mode` | `AGW_AUTH__MODE` |
| `storage.dir` | `AGW_STORAGE__DIR` |
| `observability.log_level` | `AGW_OBSERVABILITY__LOG_LEVEL` |

Environment variables take precedence over the YAML file. This allows containerized deployments to inject configuration without mounting files.

## CLI Flags

| Flag | YAML Path | Description |
|---|---|---|
| `--config` | (file path) | Path to the configuration file |
| `--port` | `service.port` | HTTP server port |
| `--host` | `service.host` | HTTP server bind address |
| `--profile` | (profile selector) | Activate a named configuration profile |
| `--log-level` | `observability.log_level` | Set the log level |

CLI flags take precedence over both environment variables and the YAML file.

## Precedence Chain

From highest to lowest priority:

1. **CLI flags** -- explicit command-line arguments
2. **Environment variables** -- `AGW_` prefixed vars with `__` nesting
3. **Profile overrides** -- values from the active profile section
4. **YAML file** -- base configuration from `agents-gateway.yaml`
5. **Default values** -- built-in defaults

When multiple sources define the same key, the highest-precedence source wins.

## Default Values

| Key | Default |
|---|---|
| `service.port` | `8902` |
| `service.host` | `0.0.0.0` |
| `auth.mode` | `dev-none` |
| `storage.dir` | `./data` |
| `storage.sqlite_url` | `sqlite:///./data/agent-gateway.db` |
| `observability.log_level` | `info` |
| `observability.log_format` | `json` |

## Configuration Sections

### service

Controls the HTTP server binding.

```yaml
service:
  port: 8902
  host: "0.0.0.0"
```

| Field | Type | Default | Description |
|---|---|---|---|
| `port` | integer | `8902` | TCP port for the HTTP server |
| `host` | string | `0.0.0.0` | Bind address |

### auth

Controls authentication and authorization behavior.

```yaml
auth:
  mode: dev-none
```

| Field | Type | Default | Description |
|---|---|---|---|
| `mode` | string | `dev-none` | Auth mode. One of: `dev-none`, `cloudflare-access`, `internal-only` |

See [AUTH.md](./AUTH.md) for full details on each mode.

### agents

Controls agent discovery and catalog construction.

```yaml
agents:
  dirs:
    - ./agents
```

| Field | Type | Default | Description |
|---|---|---|---|
| `dirs` | list of strings | `["./agents"]` | Directories to scan for agent manifests |

### storage

Controls persistent storage backends.

```yaml
storage:
  dir: ./data
  sqlite_url: "sqlite:///./data/agent-gateway.db"
```

| Field | Type | Default | Description |
|---|---|---|---|
| `dir` | string | `./data` | Base directory for file-based storage |
| `sqlite_url` | string | `sqlite:///./data/agent-gateway.db` | SQLAlchemy-style SQLite connection URL |

### observability

Controls logging and metrics.

```yaml
observability:
  log_level: info
  log_format: json
```

| Field | Type | Default | Description |
|---|---|---|---|
| `log_level` | string | `info` | Minimum log level: `debug`, `info`, `warning`, `error` |
| `log_format` | string | `json` | Log output format: `json` or `text` |

## Full Example

```yaml
service:
  port: 8902
  host: "0.0.0.0"

auth:
  mode: dev-none

agents:
  dirs:
    - ./agents

storage:
  dir: ./data
  sqlite_url: "sqlite:///./data/agent-gateway.db"

observability:
  log_level: info
  log_format: json
```
