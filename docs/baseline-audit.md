# Baseline Audit — AGW-001.0

**Date:** 2026-06-23
**Auditor:** Autonomous Agent
**Repo:** Astatide1337/agents-gateway
**Branch:** epic/agw-001-maturity (from main at 5cccc76)

## Current Architecture

The repository contains a minimal prototype with two components:

1. **Gateway** (`gateway/server.py`) — A FastAPI application (727 lines) that implements:
   - OAuth 2.0 authorization server (authorize, token, register, JWKS, PKCE)
   - Cloudflare Access JWT validation
   - Auth middleware with Docker-internal and localhost bypass
   - A2A protocol proxy to backend agents (JSON-RPC over HTTP)
   - MCP protocol endpoint (`POST /mcp`) — hand-rolled SSE/JSON-RPC, not using FastMCP
   - Agent listing and agent-card proxy
   - Health endpoint (`GET /health`)
   - Single `agents.yaml` config for agent discovery

2. **Research Agent** (`agents/research-agent/server.py`) — A standalone FastAPI app (612 lines):
   - Multi-phase deep research (plan → research → synthesize) using NVIDIA NIM models
   - SQLite task storage with submitted/working/completed/canceled/failed statuses
   - SearXNG search integration and content scraping
   - A2A JSON-RPC handler (tasks/send, tasks/get, tasks/cancel, tasks/sendSubscribe with SSE)
   - Health and task list endpoints

3. **Docker Compose** — Two services: `agent-gateway` (port 8092) and `research-agent` (port 8093)

## Existing Infrastructure

| Component | Status |
|-----------|--------|
| `pyproject.toml` | **Missing** — no Python project config |
| `README.md` | **Missing** |
| `tests/` | **Missing** — no tests at all |
| CLI | **Missing** — no CLI, just bare `uvicorn server:app` |
| Config loading | Partial — hardcoded env vars, single `agents.yaml` YAML |
| Agent manifest schema | **Missing** — agents defined in flat YAML, no validation |
| Agent catalog/profiles | **Missing** |
| Task storage (gateway) | **Missing** — gateway delegates all tasks to research-agent |
| Task state machine | **Missing** — gateway has no task lifecycle |
| MCP tools (formal) | **Missing** — ad-hoc MCP handler, not using FastMCP |
| Structured logging | **Missing** — basic `logging.basicConfig` |
| Metrics | **Missing** — no Prometheus metrics |
| Auth modes | Partial — OAuth2 + CF Access + Docker-internal exist but not structured as modes |
| `Dockerfile` | Exists for gateway and research-agent |
| `docker-compose.yml` | Exists but minimal |
| `.gitignore` | Exists but minimal |
| `.env.example` | Exists at root and per-service |
| CI/CD | **Missing** — no `.github/workflows/` |
| `Makefile` | **Missing** |
| Smoke test | **Missing** |
| Documentation | **Missing** — no `docs/` directory |

## Greenfield Assessment

The repo is **not fully greenfield** — it contains working code for an OAuth gateway + A2A proxy + a research agent. However, the gateway lacks all production-grade infrastructure (CLI, config, manifests, task storage, MCP tools, logging, metrics, tests, CI, docs). The epic treats it as mostly greenfield for the *gateway maturity* aspects, preserving existing auth logic where feasible.

## Missing Pieces (Priority Order)

1. No `pyproject.toml` / `uv` project structure
2. No CLI (`agents-gateway` command)
3. No config file loading (`agents-gateway.yaml`) with precedence chain
4. No agent manifest schema / validation
5. No agent catalog or profiles
6. No gateway-level task storage, state machine, or lifecycle
7. No stub runtime adapter
8. No task HTTP API
9. No formal MCP tools (using FastMCP)
10. No structured logging
11. No metrics
12. Auth modes not formalized
13. No Docker Compose with gateway-only deployment
14. No CI, Makefile, or smoke test
15. No documentation
16. No live E2E verification

## Verification Commands

```bash
# Repo exists
git -C /home/ubuntu/agent-gateway status
# => On branch epic/agw-001-maturity

# Files present
ls -la /home/ubuntu/agent-gateway/gateway/server.py
ls -la /home/ubuntu/agent-gateway/agents/research-agent/server.py
ls -la /home/ubuntu/agent-gateway/docker-compose.yaml

# No tests
find /home/ubuntu/agent-gateway -name "test_*" -o -name "*_test.py" | wc -l
# => 0

# No pyproject.toml
ls /home/ubuntu/agent-gateway/pyproject.toml 2>&1
# => No such file or directory

# No docs
ls -d /home/ubuntu/agent-gateway/docs 2>&1
# => No such file or directory (now created for this audit)
```

## Conclusion

The repository has a functional prototype gateway with OAuth and A2A proxy, but it lacks every production-grade attribute required by the epic. The codebase should be restructured into an installable Python package with CLI, proper config, agent manifests, task lifecycle, MCP tools, and full observability. The existing auth and proxy logic can be adapted but must be refactored into the new architecture.
