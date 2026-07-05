# Agents Gateway Current State

This audit records the current known state of Agents Gateway against `README.md`, `ROADMAP.md`, `docs/AGENTS_GATEWAY_STANDARD.md`, and `docs/GATEWAY_PLATFORM_STANDARD.md`.

Status labels:

- Implemented: verified in inspected files.
- Partial: present but needs tests or deeper inspection.
- Missing: not verified or not present in inspected files.
- Unclear: requires local run or additional file review.

## Verified repository facts

- Repository: `Astatide1337/agents-gateway`
- Default branch: `main`
- Package name: `agents-gateway`
- Python requirement: `>=3.12`
- CLI entry point: `agents-gateway = agents_gateway.cli:app`
- Config file: `agents-gateway.yaml`
- Storage config includes SQLite path and artifacts directory.

## CLI

Standard claims:

- `agents-gateway run`
- `agents-gateway validate`
- `agents-gateway list`
- `agents-gateway inspect <id>`
- `agents-gateway doctor`
- `agents-gateway version`

Status: Partial.

The CLI entry point exists. Individual command behavior still needs test confirmation.

## Config

Status: Partial.

Verified config fields include:

- `service.host`
- `service.port`
- `service.mcp_path`
- `auth.mode`
- `agents.dir`
- `storage.sqlite_path`
- `storage.artifacts_dir`
- `observability.log_level`
- `observability.log_format`
- `observability.metrics_enabled`
- `profile`

Verified behavior:

- YAML is loaded from `agents-gateway.yaml` by default.
- Environment overrides use `AGW_` and double underscores for nesting.
- `AGW_PROFILE` can set the active profile.

Known gap:

- Catalog profile loading should use the already-loaded config consistently.

## Agent manifest

Status: Partial.

Verified manifest fields include:

- `id`
- `name`
- `description`
- `version`
- `runtime.type`
- `skills`
- `tools`
- `permissions`
- `risk_level`
- `tags`
- `author`

Known gaps:

- ID/directory mismatch policy should be decided.
- Supported type validation should be tied to available adapters.

## Catalog and profiles

Status: Partial.

Verified catalog behavior:

- Scans agent directories.
- Loads `agent.yaml` files.
- Tracks validation errors.
- Lists, gets, and searches agents.
- Filters by active profile.

Known gap:

- Profile definitions appear to be loaded separately from the main config path.

## HTTP endpoints

Status: Partial/implemented.

Verified route names include:

- `GET /health`
- `GET /ready`
- `GET /version`
- `GET /inventory`
- `GET /metrics`
- `GET /docs`
- `GET /agents`
- `POST /agents/validate`
- `GET /agents/{agent_id}`
- `POST /tasks`
- `GET /tasks`
- `GET /tasks/{task_id}`
- `GET /tasks/{task_id}/events`
- `GET /tasks/{task_id}/artifacts`
- `POST /tasks/{task_id}/cancel`
- `POST /tasks/{task_id}/run`

Need smoke-test confirmation.

## MCP tools

Status: Missing/unclear.

The standard requires:

- `agents_list`
- `agents_search`
- `agents_inspect`
- `agent_task_create`
- `agent_task_get`
- `agent_task_events`
- `agent_task_artifacts`
- `agent_task_cancel`

The inventory response lists these tool names, but actual MCP registration still needs implementation or confirmation.

## Observability

Status: Partial.

Known behavior:

- Logging and metrics are initialized.
- Agent counts are reported.
- Request counts are incremented.
- Some task counters are incremented.

Known gap:

- Need complete metric coverage for MCP tools and task lifecycle.

## Priority gaps

1. Implement or confirm the MCP tool layer.
2. Fix profile loading to use loaded gateway config consistently.
3. Decide and document `POST /tasks/{task_id}/run`.
4. Harden task lifecycle tests.
5. Add Skills Gateway integration contract.
6. Add service checks and smoke tests.

## Follow-up issues

- AGW-002: Implement MCP tool layer.
- AGW-003: Fix profile loading.
- AGW-004: Harden task lifecycle behavior.
- AGW-005: Add Skills Gateway integration contract.
