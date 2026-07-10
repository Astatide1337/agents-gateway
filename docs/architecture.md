# Architecture

## Topology

```
User
  |
  v
ChatGPT / Claude / web API / any MCP-compatible cockpit
  - helps user flesh out intent/spec
  - hands off final objective/spec to Composer
  |
  v
Composer / Conductor
  - captain / orchestration brain
  - turns spec into task graph
  - assigns tasks to agents
  - chooses skills/tools/harness profiles
  - monitors agents
  - replies to agents when they need guidance
  - checks verification
  - produces final review packet
  |
  v
Agents Gateway
  - execution substrate
  - creates isolated worktrees
  - starts real harness sessions
  - runs Claude Code / opencode / Codex / other agents
  - supervises sessions
  - exposes session IO/events to Composer
  - runs verification commands
  - captures proof artifacts
  - returns branches/commits/artifacts/results
  |
  +--> Skills Gateway
  |     skills/methodology loaded by agents
  |
  +--> MCP Gateway
        external tools: GitHub, docs, browser, cloud, etc.
```

## Components

Agents Gateway splits into two execution planes that share a single SQLite store:

### Legacy task/runtime plane (`agents_gateway/{server,worker,storage,runtime}.py`)

- HTTP + MCP API for agent catalog and short-lived fat-task execution
- `AgentCatalog` scans `agents/*/agent.yaml` manifests
- `RuntimeRegistry` maps type strings to adapter implementations (`local-stub`, `process`, `docker`)
- `TaskWorker` background thread claims queued tasks via SQLite row-level claim
- `TaskStorage` defines the task state machine: `created→queued→running→{waiting,completed,failed,cancelled}`
- Tasks are submitted via `POST /tasks` with `agent_id` + `input`; payload shape is agent-defined

### Harness-runtime plane (`agents_gateway/harness/*.py`)

- HTTP + MCP API for Composer-driven long-horizon agent runs
- Bypasses `AgentCatalog` entirely — composer-controlled tasks have no fixed agent manifest
- Each task gets an isolated git worktree, a tmux-backed harness session, and an artifact tree
- Sessions are supervised by a background thread; composer replies keep the agent going
- Verification commands are mandatory before `completed` status is granted
- A structured `HarnessRunResult` with diff/commit/artifacts is returned to Composer

The legacy plane is preserved verbatim; the harness-runtime plane is purely additive.

## Module map

```
agents_gateway/
├── server.py             ASGI app, routes, auth middleware
├── auth.py               dev-none, internal-only, cloudflare-access
├── catalog.py            agent manifest scanning
├── storage.py            task state machine + harness task creation
├── runtime.py            StubRuntime / ProcessRuntime / DockerRuntime
├── worker.py             background task worker (legacy + harness)
├── mcp_tools.py          FastMCP tool registration
├── config.py             GatewayConfig + HarnessRuntimeConfig
├── logging.py            structured logging with header redaction
├── metrics.py            Prometheus-style counters
└── harness/
    ├── models.py         dataclasses for sessions, worktrees, verifications
    ├── profiles.py       builtin harness profiles (opencode-deepseek, claude-code, codex, fake-test)
    ├── tmux.py           TmuxDriver (real) + FakeTmuxDriver (tests)
    ├── driver.py         HarnessDriver: start+goal+classify+reply+stop
    ├── goal.py           GoalContext + inject_goal (slash, plain, file_modes)
    ├── classifier.py     classify_state from recent tmux capture
    ├── supervisor.py     SessionSupervisor background loop
    ├── workspace.py      RepoWorkspaceManager: clone/fetch/worktree
    ├── storage.py        HarnessStorage: SQLite schema for harness tables
    ├── verification.py   VerificationRunner: env-required gate + subprocess
    ├── artifacts.py      ArtifactStore: per-run logs/captures/reports/patches
    ├── reports.py        HTML review report generator (secrets redacted)
    ├── git.py            diff capture + commit / push / open PR
    ├── client_skills.py  Skills Gateway validation client
    ├── client_mcp.py     MCP Gateway tools-summary + TOOLS.md rendering
    └── runtime.py        HarnessRuntime.execute_task full lifecycle
```

## Data store

All state lives in a single SQLite database (`agents_gateway.db` by default; configurable via `AGW_STORAGE__SQLITE_PATH`). The harness-runtime plane adds these tables additively:

- `repo_workspaces` — cloned/cached base repos
- `worktrees` — isolated git worktrees (one per task)
- `harness_sessions` — running harness sessions
- `composer_interactions` — pending Composer replies from agents
- `verification_runs` — verification command pass/fail records
- `harness_artifacts` — captured proof artifacts (logs, diffs, reports)

All tables use `CREATE TABLE IF NOT EXISTS` so existing deployments upgrade in place.

## Concurrency

- The legacy `TaskWorker` thread serially claims queued tasks
- Each harness-session `execute_task` call runs in its own transient supervisor thread (the runtime loop is synchronous)
- Multiple tasks may execute in parallel if the worker is configured to spawn multiple worker threads (currently single-threaded)
-tmux sessions are independent OS processes; concurrent harness runs do not share state

## Known limitations / TODOs

### `harness_session` bypasses `RuntimeRegistry` and `AgentCatalog`

The `harness_session` execution mode routes directly from the worker to
`HarnessRuntime` by checking `task.metadata.runtime_type == "harness_session"`.
It does NOT go through the `RuntimeRegistry` or consult agent manifests.

**TODO**: Register a `HarnessRuntimeAdapter` in the `RuntimeRegistry` so that
`harness_session` tasks flow through the same `runtime_type` dispatch path
as `process` and `docker` runtimes. This would:
- Allow agent manifests to configure harness profiles and goal strategies.
- Let the worker dispatch generically via `registry.create("harness_session", ...)`.
- Enable legacy task-level safety checks (risk-level gating, manifest validation)
  before a harness session starts.

Until then, Composer is the trust boundary for which harness profile + repo
URL + skills to invoke — not Agents Gateway.

### Containerized harness sessions

The current `TmuxDriver` runs harness sessions on the host via tmux.
This is documented as "trusted personal deployment mode" — the harness
process has the same file system, network, and secret access as the
gateway user. Container isolation (via a future `ContainerDriver`) is the
long-term target.

## Security boundaries

- Verification subprocesses never receive gateway secrets
  (`_safe_env` allow-list): only `PATH`, `HOME`, `LANG`, `LC_*`, `TERM`,
  `SHELL`, `USER`, `USERNAME`, `PYTHONPATH`, `VIRTUAL_ENV`,
  `UV_CACHE_DIR`, `PYTEST_DISABLE_PLUGIN_AUTOLOAD` are propagated.
- HTML review reports apply redaction regexes for Bearer tokens,
  GitHub `ghp_/gho_/ghs_/ghu_/ghr_/gho_` tokens, and URL credentials
  (see `harness/reports.py:_REDACT_PATTERNS`).
- `cloudflare-access` mode verifies CF JWT signature via JWKS.
- Harness sessions today run on the host (tmux-on-host mode).
  Container isolation is the long-term target — the abstraction
  (`TmuxDriver`) is structured so a future `ContainerDriver` can
  slot in without touching the runtime contract.
