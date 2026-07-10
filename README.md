# Agents Gateway

Agents Gateway is a self-hosted agent catalog and task runtime gateway. It exposes agent discovery and task lifecycle operations over HTTP/MCP, with Cloudflare Access authentication, persistent task state, runtime adapters, artifacts, metrics, and structured logs.

**Not** a general-purpose API gateway, not a Kubernetes operator, not an LLM proxy.

## Quick Start

```bash
uv sync
cp .env.example .env
agents-gateway run
```

Default auth is `dev-none` (no authentication). Access at `http://localhost:8092`.

## CLI Commands

```bash
agents-gateway run        # Start the gateway (default port 8092)
agents-gateway validate   # Validate agent manifests
agents-gateway list       # List available agents
agents-gateway inspect id # Inspect a specific agent
agents-gateway doctor     # Readiness checks
agents-gateway version    # Print version
```

## Architecture

```
              Cloudflare Access (edge)
                      │
                      ▼
              ┌─────────────────┐
              │  Agents Gateway  │
              │  (FastMCP/ASGI)  │
              └───────┬─────────┘
                      │
          ┌───────────┼───────────┐
          ▼           ▼           ▼
    Agent Catalog  Task Storage  Runtime Registry
     (manifests)   (SQLite/WAL)  (stub/process/docker)
                      │
                      ▼
              Background Worker
              (claims & executes)
```

### Components

| Component | Purpose |
|-----------|---------|
| `server.py` | ASGI app creation, auth middleware, custom routes |
| `auth.py` | AuthHandler (dev-none, internal-only, cloudflare-access) |
| `catalog.py` | Agent manifest scanning, profiles, search |
| `storage.py` | SQLite task state machine (validated transitions) |
| `runtime.py` | Runtime adapters: StubRuntime, DockerRuntime, ProcessRuntime |
| `worker.py` | Background thread claiming & executing tasks |
| `mcp_tools.py` | FastMCP tool registration |
| `logging.py` | Structured JSON/text logging, contextvars, header redaction |
| `metrics.py` | In-memory Prometheus-formatted metrics |
| `config.py` | GatewayConfig (YAML + env override layering) |

## Security Model

Agents Gateway supports two deployment postures:

### 1. Edge-Auth-Only Personal Mode

```
Internet → Cloudflare Access (identity/login gate) → Private Origin (127.0.0.1)
```

- Cloudflare Access protects the hostname at the edge. Users must authenticate with your identity provider before reaching the origin.
- The origin is bound to `127.0.0.1` — not directly reachable from the public internet.
- App auth can be `dev-none` or `internal-only` because the edge already enforces identity.
- Suitable for personal MCP tool usage where only the owner calls the gateway.

### 2. Defense-in-Depth Production Mode

```
Internet → Cloudflare Access → Origin validates CF JWT (RS256/JWKS)
```

- Cloudflare Access protects the hostname at the edge.
- The app also validates the Cloudflare Access JWT (`cloudflare-access` mode) for defense in depth.
- If the edge bypassed or misconfigured, the origin still rejects unauthenticated requests.
- Suitable for multi-user or zero-trust production deployments where you want defense in depth.

### Authentication Modes

| Mode | App-Level Protection | Recommended For |
|------|---------------------|-----------------|
| `dev-none` | No app auth (open) | Edge-auth-only personal mode (CF Access at edge). Refused in production (`AGW_ENV=production`) unless you understand this constraint. |
| `cloudflare-access` | Real CF Access JWT verification (RS256, JWKS, aud, iss, exp) | Defense-in-depth production mode. Requires CF Access at edge + app verification. |
| `internal-only` | Shared-secret header (`X-Auth-Internal-Token`) | Edge-auth-only mode with a lightweight app-level backup. Also used for service-to-service calls.

### Protected Paths

These paths require authentication in `cloudflare-access` and `internal-only` modes:

```
/mcp              (MCP protocol: initialize, tools/call, etc.)
/agents           (agent listing)
/agents/{id}      (agent detail)
/tasks            (create, list)
/tasks/{id}       (task detail)
/tasks/{id}/run   (enqueue for execution)
/tasks/{id}/events
/tasks/{id}/artifacts
/tasks/{id}/cancel
/inventory         (service inventory)
/metrics           (Prometheus metrics, sensitive in production)
```

### Public Paths (no auth)

```
/health
/ready
/version
/docs
/openapi.json
/.well-known/*     (OAuth discovery metadata)
```

### JWT Verification (cloudflare-access mode)

The AuthHandler performs real JWT verification:

- Signature verified using CF Access JWKS (`https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`)
- Algorithm restricted to RS256 (rejects `alg: none`)
- Audience (`aud`) must match configured `AGW_AUTH__CLOUDFLARE_AUD`
- Issuer (`iss`) must match `https://<team>.cloudflareaccess.com`
- Expiration (`exp`) enforced
- `sub` or `email` claim required
- Malformed / unsigned / wrong-aud / wrong-iss / expired JWTs rejected
- Arbitrary `Authorization: Bearer ...` headers ignored (not a bypass)

### Internal-Only Mode

- `X-Auth-Internal-Token` compared with constant-time `secrets.compare_digest`
- Private-IP bypass is OFF by default and requires explicit `AGW_AUTH__ALLOW_UNSAFE_PRIVATE_IP_BYPASS=true` with documented risk
- No OAuth flow support

### OAuth Flow Constraints

- `/authorize` requires Cloudflare Access at the edge or in-app CF JWT
- `/token` exchanges valid authorization codes for registered clients only
- `/register` is open for MCP client DCR; does not create an unsafe token-vending flow

## Cloudflare Access Setup

1. Create a Cloudflare Access application for your gateway domain.
2. Set the policy to require authentication (email, GitHub, etc.).
3. Note the Application Audience (AUD) tag from the Cloudflare dashboard.
4. Set environment variables:

```bash
AGW_AUTH__MODE=cloudflare-access
AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN=<your-team>.cloudflareaccess.com
AGW_AUTH__CLOUDFLARE_AUD=<your-application-audience-tag>
AGW_AUTH__PUBLIC_BASE_URL=https://agents.yourdomain.com
AGW_ENV=production
```

5. Ensure Cloudflare Access injects the `Cf-Access-Jwt-Assertion` header.
6. Start the gateway; it will refuse to boot if required config is missing.

## Runtime Model

### Execution Flow

```
POST /tasks           → task created (status: created)
POST /tasks/{id}/run  → task enqueued (status: queued), returns 202
Background Worker     → atomically claims task (queued → running)
Runtime Adapter       → executes task (running → completed/failed)
GET /tasks/{id}       → current task state
GET /tasks/{id}/events → lifecycle event stream
GET /tasks/{id}/artifacts → artifact listing
```

The `/run` endpoint returns immediately (202). The background worker handles execution off the request path.

### State Transitions

```
created → queued → running → completed
                         → failed
                         → waiting → running
created → cancelled
queued → cancelled
running → cancelled
```

Terminal states (`completed`, `failed`, `cancelled`) have no valid outgoing transitions. Invalid transitions raise `TransitionError` (409 HTTP).

### Runtime Adapters

| Adapter | Sandbox | Use |
|---------|---------|-----|
| `StubRuntime` (`local-stub`) | Safe local stub, no external calls | Default, dev, testing |
| `DockerRuntime` (`docker`) | Hardened Docker container | Production agent execution |
| `ProcessRuntime` (`process`) | NO sandbox (trusted-only) | Local trusted scripts, dev workflows |

In addition to the legacy adapter family, the gateway supports a
**harness worktree runtime** for Composer-driven long-horizon agent
work. See [docs/harness-runtime.md](docs/harness-runtime.md) for the
full contract. In short: each task gets an isolated git worktree, a
tmux-backed harness session (Claude Code / opencode / Codex / fake
harness), Composer-controlled interactions, mandatory verification,
and an HTML review report generated when verification passes. The
harness runtime is purely additive — legacy dispatch paths continue
to work unchanged.

## DockerRuntime Sandboxing

Every `docker run` issued by DockerRuntime includes these mandatory flags:

```txt
--rm                          remove container on exit
-i                            interactive stdin
--network none                no network (default; opt-in via manifest)
--read-only                   root FS read-only
--cap-drop ALL                drop all Linux capabilities
--security-opt no-new-privileges
--user 65534:65534            non-root user
--memory 512m                 configurable via AGW_RUNTIME__DOCKER_MEMORY
--cpus 1.0                    configurable via AGW_RUNTIME__DOCKER_CPUS
--pids-limit 128              configurable via AGW_RUNTIME__DOCKER_PIDS_LIMIT
--tmpfs /tmp:rw,noexec,nosuid,size=64m
```

**Never mounted:** `/var/run/docker.sock`, host home, repo root, secret dirs, `.env` files, CF credentials, SSH keys.

Network access requires an explicit manifest-level permission AND `AGW_RUNTIME__DOCKER_NETWORK=true`.

## ProcessRuntime Warning

ProcessRuntime runs commands on the host/container process environment with NO sandbox. It inherits user, filesystem, network, environment, and secrets visible to the gateway process.

- **Not for production** unless explicitly allowed via `AGW_RUNTIME__ALLOW_PROCESS=true`.
- In production with `allow_process=false`, manifests using `runtime.type: process` are rejected at the registry level.
- A startup warning is logged if enabled in production.

## Request Logging

- Request IDs and auth user identity propagated via `contextvars` (not env vars or thread-locals)
- Sensitive headers (`Authorization`, `Cookie`, `Cf-Access-Jwt-Assertion`, `X-Auth-Internal-Token`, `X-Confirm-High-Risk`) redacted
- JSON formatter has a fixed field whitelist (arbitrary kwargs excluded)

## Local Dev

```bash
uv sync
uv run agents-gateway validate
uv run agents-gateway list
uv run pytest -q
# Start:
uv run agents-gateway run
```

## Production Deploy Checklist

- [ ] `AGW_ENV=production` set
- [ ] `AGW_AUTH__MODE=cloudflare-access` (not dev-none)
- [ ] `AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN` set
- [ ] `AGW_AUTH__CLOUDFLARE_AUD` set
- [ ] `AGW_AUTH__PUBLIC_BASE_URL` set
- [ ] Cloudflare Access application created and policy applied
- [ ] `AGW_RUNTIME__ALLOW_PROCESS` reviewed and set intentionally
- [ ] Docker daemon available if using `docker` runtime type
- [ ] Agent manifests validated (`agents-gateway validate`)
- [ ] Health check endpoint accessible
- [ ] Metrics endpoint access controlled

## Testing

```bash
uv run pytest -q                        # 495 tests
uv run pytest tests/test_auth.py -v     # JWT verification proofs
uv run pytest tests/test_endpoints.py -v # HTTP auth + task lifecycle
uv run pytest tests/test_runtime.py -v  # Docker sandbox + ProcessRuntime gating
uv run pytest tests/test_harness_runtime_e2e.py -v  # fake-harness 3-flow E2E
uv run pytest tests/test_harness_http_api.py -v     # harness HTTP endpoints
uv run pytest tests/test_session_supervisor.py -v   # supervisor + classifier
```

### Local harness E2E

Drives the full harness flow end-to-end using the bundled `fake-test` harness profile. No real Claude/opencode/Codex required.

```bash
bash scripts/e2e-harness-runtime-local.sh
# Expected: "Passed: 3 / Failed: 0" + "[OK] Harness runtime local E2E passed"
```

### Real harness E2E (optional)

Requires `opencode` / `claude` / `codex` on PATH and configured with LLM credentials. Refuses with exit code 2 if the binary is missing — never fakes success.

```bash
bash scripts/e2e-harness-runtime-real.sh                 # opencode-deepseek profile
AGW_E2E_REAL_PROFILE=claude-code bash scripts/e2e-harness-runtime-real.sh
# Missing binary → "REAL HARNESS E2E BLOCKED: missing <command>" + exit 2
```

## Docker

```bash
cp .env.example .env
docker compose up -d --build
docker compose ps
curl http://localhost:8092/health
docker compose down
```

**Note:** DockerRuntime cannot execute Docker containers from inside a Compose container without Docker socket mounting (which DockerRuntime intentionally prohibits for sandboxing). For a deployment where DockerRuntime works, run the gateway on the Docker host or use a Docker-outside-of-Docker (DooD) setup with explicit socket access controls.

## Known Limitations

- Rate limiting is per-process (not distributed across replicas)
- No persistent OAuth client/token storage (in-memory)
- DockerRuntime requires host Docker daemon access (not usable inside a minimal Compose container)
- Single background worker thread (tasks queued sequentially; concurrent workers need multi-threading or separate processes)
- No task cancellation for in-flight Docker containers (docker rm is async best-effort)
- `stub-runtime` only; real Docker/Process execution needs Docker daemon
- No multi-replica coordination (SQLite single-writer; WAL mode helps but does not solve multi-process contention)
- Harness sessions today run on host via tmux (long-term containerization is roadmap)
- HTML review report redaction is regex-based and may miss novel token formats — report leaks at https://github.com/Astatide1337/agents-gateway/issues

## Documentation

| Document | Purpose |
|----------|---------|
| [README.md](README.md) | This file — overview, quick start, security model, runtime model |
| [SECURITY.md](SECURITY.md) | Threat model, the `_safe_env` boundary, redaction patterns, production checklist |
| [docs/architecture.md](docs/architecture.md) | Component map, module layout, data store, concurrency |
| [docs/api.md](docs/api.md) | Full HTTP + MCP API reference (legacy + harness-runtime planes) |
| [docs/runtime.md](docs/runtime.md) | Legacy adapters + harness worktree runtime configuration |
| [docs/harness-runtime.md](docs/harness-runtime.md) | Runtime contract — goal injection, supervision, verification, completion flow |
| [docs/verification.md](docs/verification.md) | Verification runner, env-required gate, failure feedback loop |
| [docs/composer-integration.md](docs/composer-integration.md) | Composer contract — endpoint map, task spec, reply protocol, terminal outcomes |
| [docs/runbooks.md](docs/runbooks.md) | Operational runbooks (boot, E2E, diagnose stall/blocked/missing artifacts) |

## License

See individual agent directories. Gateway code: repository license.