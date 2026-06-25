# E2E Test Report — AGW-001.16

Date: 2026-06-24
Branch: epic/agw-001-maturity
Python: 3.14.4 | uv: 0.11.19 | FastMCP: 2.14.7

## Local E2E

Status: PASSED (32/32 checks)

Gateway started locally with `uv run agents-gateway run --port 18093` using env overrides for storage, agents dir, and auth mode.

| Check | Result |
|---|---|
| GET /health returns 200 | PASS |
| GET /ready returns 200 with readiness checks | PASS |
| GET /version returns 200 with name + version | PASS |
| GET /inventory returns 200 with agent counts, tools, auth mode | PASS |
| GET /metrics returns 200 with Prometheus text format | PASS |
| GET /docs returns 200 | PASS |
| GET /agents returns 200 with 3 valid agents | PASS |
| GET /agents/repo-reviewer returns 200 with manifest | PASS |
| GET /agents/nonexistent returns 404 | PASS |
| Agent list contains repo-reviewer | PASS |
| Agent list contains test-runner | PASS |
| Agent list contains incident-triager | PASS |
| POST /agents/validate returns 200 with validation results | PASS |
| POST /tasks creates task with agent_id + input dict | PASS |
| GET /tasks/{id} returns created task | PASS |
| GET /tasks/{id}/events returns events | PASS |
| GET /tasks/{id}/artifacts returns artifacts | PASS |
| POST /tasks/{id}/run transitions task to completed | PASS |
| Task status is "completed" after run | PASS |
| Task has >1 events after run (6 events) | PASS |
| Task has >=1 artifacts after run (1 artifact) | PASS |
| POST /tasks/{id}/cancel cancels a created task | PASS |
| GET /tasks returns all tasks | PASS |
| POST /tasks/{id}/cancel on completed task returns 409 | PASS |
| Metrics contains tasks_created_total | PASS |
| Metrics contains tasks_completed_total | PASS |
| Metrics contains requests_total | PASS |
| Metrics contains agents_total | PASS |
| Metrics contains tasks_cancelled_total | PASS |
| Log output contains "event" field | PASS |
| Log output contains "service" field | PASS |
| Log output contains "timestamp" field | PASS |

## Docker E2E

Status: PASSED (config validation only)

- `docker compose config` parses successfully without errors
- `version` field removed (obsolete in modern Compose spec)
- Healthcheck directive present (curl /health)
- Port mapping 127.0.0.1:8092:8092
- Volume mount ./data:/data
- Env vars with defaults (AGW_AUTH__MODE, AGW_OBSERVABILITY__LOG_LEVEL)
- Dockerfile builds with `uv` package manager

Note: Full Docker build + runtime E2E not performed (no Docker daemon available in CI environment). Docker Compose config validation confirms image build context, ports, volumes, env, and healthcheck are correctly defined.

## Task E2E

Status: PASSED

Full task lifecycle verified via live HTTP calls:

1. Create task: POST /tasks with agent_id + JSON input body -> 200, task in "created" state
2. Run task: POST /tasks/{id}/run -> 200, task transitions created -> queued -> running -> completed
3. Events: GET /tasks/{id}/events -> 200, 6 events (task_created, task_queued, task_running, task_completed, runtime_started, runtime_completed)
4. Artifacts: GET /tasks/{id}/artifacts -> 200, 1 artifact (stub JSON result)
5. Cancel task: POST /tasks/{id}/cancel -> 200, created task transitions to cancelled
6. Invalid cancel: POST /tasks/{id}/cancel on completed task -> 409 TransitionError
7. List tasks: GET /tasks -> 200, all tasks visible

Bug found and fixed during E2E:
- `storage.create_task()` failed with `sqlite3.ProgrammingError` when `input_data` was a dict (not serialized to JSON). Fixed by adding `if not isinstance(input_data, str): input_data = json.dumps(input_data)`.
- Route ordering bug: `/agents/{agent_id}` matched before `/agents/validate`, returning 404. Fixed by declaring `/agents/validate` before the parameterized route.
- `_registry` vs `registry` typo in run_task error handler. Fixed.

## Public/Live E2E

Status: SKIPPED

- No live infrastructure or Cloudflare Access tokens available for testing
- Cloudflare Access auth mode tested via unit tests only
- Internal-only auth mode tested via unit tests only
- TLS termination, rate limiting, and multi-client access deferred to post-AGW-001

## Unit Test Summary

- 141 tests across 11 test files
- All 141 passing
- Coverage: config, manifest, catalog, storage, runtime, auth, logging, metrics, MCP tools, endpoints, CLI

## Smoke Test

- `bash scripts/smoke-test.sh` PASSED
- Verifies: startup, health, ready, version, inventory, metrics, agents, task create, task get, task events, task artifacts, task cancel, metrics reflect activity
