# Testing Reference

This document covers how to run the test suite, the test structure, and what is covered by the tests.

## Running Tests

### Unit and Integration Tests

```bash
uv run pytest tests/ -v
```

This runs the full test suite using `pytest` with verbose output. The test runner is invoked through `uv` to ensure the correct Python environment and dependencies are used.

### Smoke Test

A smoke test script exercises the running gateway over HTTP to verify end-to-end behavior:

```bash
bash scripts/smoke-test.sh
```

The smoke test assumes the gateway is already running on `localhost:8902`. It performs a sequence of HTTP requests against key endpoints and validates the responses.

## Test Structure

Tests are organized under the `tests/` directory, typically mirroring the application module structure:

```
tests/
  test_config.py
  test_manifest.py
  test_catalog.py
  test_storage.py
  test_runtime.py
  test_auth.py
  test_logging.py
  test_metrics.py
  test_mcp_tools.py
  test_endpoints.py
```

Each test file focuses on a specific subsystem. Tests use `pytest` fixtures for setup and teardown where needed.

## What Is Tested

### Configuration (test_config.py)

- YAML file parsing
- Environment variable overrides with `AGW_` prefix and `__` nesting
- CLI flag parsing
- Precedence chain enforcement (CLI > env > profile > YAML > defaults)
- Profile activation and unknown profile errors
- Default value application

### Manifest Validation (test_manifest.py)

- Required field validation (`id`, `name`, `description`, `version`, `runtime.type`)
- Agent ID format enforcement
- Duplicate ID detection
- Unknown runtime type rejection
- Invalid `risk_level` values
- Empty and malformed YAML handling

### Catalog (test_catalog.py)

- Agent discovery from configured directories
- Catalog construction from valid manifests
- Catalog API response format
- Agent filtering by tags and skills

### Storage (test_storage.py)

- SQLite database initialization
- Task persistence and retrieval
- Artifact storage and retrieval
- Concurrent access handling

### Runtime (test_runtime.py)

- Stub runtime execution and artifact production
- Stub output JSON structure
- Unknown runtime type handling
- Runtime adapter registration

### Auth (test_auth.py)

- `dev-none` mode behavior (all requests allowed)
- `cloudflare-access` token validation (valid and invalid tokens)
- `internal-only` IP range enforcement
- Auth mode reflection in `/inventory` and `/ready`

### Logging (test_logging.py)

- Structured JSON log format
- Required field presence (timestamp, level, event)
- Event type emission
- No secrets in log output

### Metrics (test_metrics.py)

- `/metrics` endpoint availability
- Prometheus text format output
- Counter and gauge metric presence
- Label correctness

### MCP Tools (test_mcp_tools.py)

- MCP tool registration and discovery
- Tool invocation and response format
- Error handling for unknown tools

### Endpoints (test_endpoints.py)

- `/ready` healthcheck response
- `/inventory` response structure
- Task CRUD operations via HTTP
- Task state transition enforcement
- 404 and 409 error responses
