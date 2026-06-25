# Agent Worklog — AGW-001

## AGW-001.0 — Baseline Audit

**Goal:** Inspect repo structure, identify existing code, document baseline state

**Files changed:**
- `docs/baseline-audit.md` (created)
- `docs/agent-worklog.md` (created)

**Test output summary:** No tests exist in the repo.

**Known issues:**
- No `pyproject.toml`, CLI, tests, docs, CI, or structured config
- Gateway has no own task storage — delegates all task management to research-agent
- Auth logic is tightly coupled in a monolithic `server.py`

---

## AGW-001.1 — Agents Gateway Standard

**Goal:** Define the full specification for the gateway

**Files changed:**
- `docs/AGENTS_GATEWAY_STANDARD.md` (created)

---

## AGW-001.2 — Project Skeleton

**Goal:** Create project skeleton with pyproject.toml, package, CLI, tests dir

**Files changed:**
- `pyproject.toml` (created)
- `agents_gateway/__init__.py` (created)
- `agents_gateway/cli.py` (created)
- `agents_gateway/config.py` (created)
- `agents_gateway/manifest.py` (created)
- `agents_gateway/catalog.py` (created)
- `agents_gateway/storage.py` (created)
- `agents_gateway/runtime.py` (created)
- `agents_gateway/server.py` (created)
- `agents_gateway/mcp_tools.py` (created)
- `agents_gateway/logging.py` (created)
- `agents_gateway/metrics.py` (created)
- `agents_gateway/auth.py` (created)
- `tests/test_*.py` (created, 9 files)
- `README.md` (created)
- `agents/repo-reviewer/agent.yaml` (created)
- `agents/test-runner/agent.yaml` (created)
- `agents/incident-triager/agent.yaml` (created)
- `agents/bad-agent/agent.yaml` (created, intentionally invalid)

**Test output:** 141 tests passing

---

## AGW-001.3 through AGW-001.13 — Individual Features

Each ticket implemented one module:
- AGW-001.3: Config precedence chain (CLI > env > YAML > defaults), `AGW_` prefix
- AGW-001.4: Agent manifest schema (Pydantic, load_manifest with structured errors/warnings)
- AGW-001.5: AgentCatalog (scan, list, get, search, profiles, validate_all)
- AGW-001.6: HTTP management endpoints (/health, /ready, /version, /inventory, /metrics, /docs)
- AGW-001.7: Task storage (SQLite, 4 tables, state machine with VALID_TRANSITIONS)
- AGW-001.8: StubRuntime adapter (execute/fail, artifact creation, idempotent transitions)
- AGW-001.9: Task HTTP API (CRUD, events, artifacts, cancel, run)
- AGW-001.10: MCP tools (8 tools via FastMCP)
- AGW-001.11: Structured logging (JSONFormatter, HumanFormatter)
- AGW-001.12: Metrics (MetricsRegistry, Prometheus format)
- AGW-001.13: Auth modes (dev-none, cloudflare-access, internal-only)

**Test output:** 141 tests passing throughout

---

## AGW-001.14 — Docker/Compose/Smoke/CI

**Goal:** Dockerfile, docker-compose.yml, smoke test, CI workflow

**Files changed:**
- `Dockerfile` (created)
- `docker-compose.yml` (created, removed obsolete `version` field)
- `.dockerignore` (created)
- `.env.example` (created)
- `scripts/smoke-test.sh` (created, uses `uv run agents-gateway`)
- `Makefile` (created)
- `.github/workflows/ci.yml` (created)

**Smoke test:** PASSED

**Docker Compose config:** Valid

---

## AGW-001.15 — Documentation

**Goal:** Create comprehensive documentation

**Files created:**
- `docs/CONFIG.md`
- `docs/AGENTS.md`
- `docs/PROFILES.md`
- `docs/TASKS.md`
- `docs/RUNTIME.md`
- `docs/AUTH.md`
- `docs/DEPLOYMENT.md`
- `docs/OBSERVABILITY.md`
- `docs/TESTING.md`
- `docs/SECURITY.md`
- `docs/TROUBLESHOOTING.md`
- `docs/E2E_REPORT.md`

---

## AGW-001.16 — Live E2E Verification

**Goal:** Start gateway, hit all endpoints, verify task lifecycle, metrics, logs

**Bugs found and fixed:**
1. `storage.create_task()` crashed with `sqlite3.ProgrammingError` when `input_data` was a dict — fixed by serializing to JSON
2. Route ordering: `/agents/{agent_id}` matched before `/agents/validate` — fixed by declaring validate route first
3. `_registry` vs `registry` typo in run_task error handler — fixed

**E2E Results:** 32/32 checks PASSED
- Management endpoints: 6/6
- Agent endpoints: 7/7
- Task lifecycle: 12/12
- Metrics after activity: 5/5
- Structured logs: 3/3

**Unit tests:** 141/141 passing

**Smoke test:** PASSED
