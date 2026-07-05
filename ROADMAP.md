# Agents Gateway Roadmap

Agents Gateway is the task lifecycle and agent discovery layer for the gateway platform.

## Role

Agents Gateway answers:

- What agents exist?
- What can each agent do?
- What task was created?
- What is the task status?
- What events happened?
- What artifacts were produced?

Skills Gateway remains the capability registry. Agents Gateway may reference skills, but should not become the skill catalog itself.

## Principles

1. MCP-first: agent discovery and task lifecycle should be usable through MCP tools.
2. Task-scoped work: important activity should produce task records, events, and artifacts.
3. Stable contracts: agent manifests, task states, and tool outputs should be machine-readable.
4. Append-only events: task history should be auditable.
5. Observable by default: health, readiness, inventory, metrics, and logs should explain system state.

## Milestones

### AGW-M0: Reality audit

- [ ] Compare `docs/AGENTS_GATEWAY_STANDARD.md` with implementation.
- [ ] Inventory CLI commands, HTTP endpoints, MCP tools, config fields, storage tables, Docker files, and tests.
- [ ] Mark each standard requirement as implemented, partial, or missing.
- [ ] Identify code/spec mismatches.

Acceptance criteria:

- [ ] `docs/CURRENT_STATE.md` exists.
- [ ] Every standard requirement is accounted for.

### AGW-M1: MCP tool layer

Required MCP tools:

- [ ] `agents_list`
- [ ] `agents_search`
- [ ] `agents_inspect`
- [ ] `agent_task_create`
- [ ] `agent_task_get`
- [ ] `agent_task_events`
- [ ] `agent_task_artifacts`
- [ ] `agent_task_cancel`

Todo:

- [ ] Add MCP registration module.
- [ ] Mount the MCP endpoint at `config.service.mcp_path`.
- [ ] Reuse catalog and storage logic instead of duplicating behavior.
- [ ] Return stable JSON result/error shapes.
- [ ] Add MCP tests for each tool.

Acceptance criteria:

- [ ] MCP and HTTP behavior are consistent.
- [ ] Invalid agents are not exposed as usable.
- [ ] Active profile filtering is respected.

### AGW-M2: Config and profile correctness

- [ ] Move profile definitions fully into `GatewayConfig`.
- [ ] Stop reading config files directly inside catalog logic.
- [ ] Ensure `--config <path>` affects all config-dependent behavior.
- [ ] Support nested `AGW_` env vars consistently.
- [ ] Add config precedence tests.

Acceptance criteria:

- [ ] CLI > env > YAML > defaults is verified.
- [ ] Custom config paths work.
- [ ] Profiles work from custom config files.

### AGW-M3: Agent manifest hardening

- [ ] Define the canonical `agent.yaml` schema.
- [ ] Validate required fields.
- [ ] Validate `id` matches directory name.
- [ ] Validate type fields and risk level.
- [ ] Validate skills, tools, and permissions shape.
- [ ] Add valid and invalid fixtures.

Acceptance criteria:

- [ ] Valid agents appear in the catalog.
- [ ] Invalid agents are excluded from usable output.
- [ ] Validation errors are actionable.

### AGW-M4: Task engine hardening

- [ ] Enforce the full task state machine.
- [ ] Keep task events append-only.
- [ ] Add task filtering by status and agent ID.
- [ ] Add pagination for tasks and events.
- [ ] Add task output and error fields.
- [ ] Add artifact metadata tests.
- [ ] Decide and document `POST /tasks/{id}/run`.

Acceptance criteria:

- [ ] Impossible transitions fail clearly.
- [ ] Terminal tasks are protected from invalid updates.
- [ ] Events and artifacts survive process restart.

### AGW-M5: Skills Gateway integration

- [ ] Add Skills Gateway connection config.
- [ ] Add skill reference validation.
- [ ] Add optional strict mode for missing skill references.
- [ ] Record skill ID and version used in task events.
- [ ] Add two-gateway integration tests.

Acceptance criteria:

- [ ] Agents can reference skills by ID.
- [ ] Missing skills produce clear validation errors or warnings.
- [ ] Skill-backed tasks record which skill was used.

### AGW-M6: Auth and permissions

- [ ] Support `dev-none`, `internal-only`, and `cloudflare-access` modes.
- [ ] Fail production boot if auth mode is unsafe.
- [ ] Add permission checks before task activity.
- [ ] Add risk-level gates for high-risk agents.
- [ ] Ensure secrets are never logged.

Acceptance criteria:

- [ ] Unauthorized requests fail safely.
- [ ] High-risk activity is explicit.
- [ ] Auth failures are observable without leaking sensitive data.

### AGW-M7: Observability and smoke tests

- [ ] Add request IDs.
- [ ] Add structured lifecycle log events.
- [ ] Add task metrics.
- [ ] Ensure `/ready` gives detailed dependency status.
- [ ] Ensure `/inventory` reports agents, invalid agents, tools, active profile, auth mode, and storage mode.
- [ ] Add or update `scripts/smoke-test.sh`.

Acceptance criteria:

- [ ] Metrics change after task activity.
- [ ] Logs include request IDs, task IDs, and agent IDs where applicable.
- [ ] Smoke test exits nonzero on failure.

## North-star demo

1. MCP client connects to Agents Gateway.
2. Client calls `agents_list`.
3. Client inspects an agent.
4. Agent references one or more skills.
5. Agents Gateway resolves those skills through Skills Gateway.
6. Client creates a task.
7. Task records events and artifacts.
8. Client reads status, events, and artifacts through MCP.
