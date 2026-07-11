"""HTTP server for Agents Gateway using FastMCP custom routes.

Security model:
  * FastMCP's MCP endpoint (/mcp) is protected by FastMCP's auth middleware
    when auth.mode is cloudflare-access or internal-only (the AuthHandler
    is passed as a Starlette middleware in addition to the route-level
    guards).
  * All /mcp.custom_route routes are individually guarded by the same
    auth logic via a Starlette middleware that runs before each handler.
  * Public routes: /health, /ready, /version, /.well-known/*.
  * OAuth /authorize requires Cloudflare Access at the edge (documented).
  * /token and /register enforce basic flow integrity (codes + clients).
  * The /run endpoint enqueues the task and returns 202 immediately; a
    background worker (TaskWorker) executes the runtime off the request
    path.
"""

from __future__ import annotations

import contextvars
import os
import secrets
import time
import uuid
from typing import Any

from fastmcp import FastMCP
from starlette.middleware import Middleware
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import JSONResponse, RedirectResponse, Response

from agents_gateway import __version__
from agents_gateway.auth import (
    CF_JWT_HEADER,
    INTERNAL_AUTH_HEADER,
    AuthHandler,
    AuthResult,
)
from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig, load_config
from agents_gateway.logging import (
    bind_request_context,
    clear_request_context,
    log_event,
    setup_logging,
)
from agents_gateway.metrics import MetricsRegistry, init_gateway_metrics, registry
from agents_gateway.mcp_tools import create_mcp_server
from agents_gateway.runtime import create_default_registry
from agents_gateway.storage import TaskStorage, TransitionError
from agents_gateway.worker import TaskWorker

# Harness-runtime plane imports (kept lazy where possible to avoid
# import cost when handlers aren't called).
from agents_gateway.harness.models import (
    ComposerInteractionStatus,
    HarnessSessionStatus,
)
from agents_gateway.harness.profiles import (
    get_profile as _get_harness_profile,
    list_profiles as _list_harness_profiles,
    register_profile as _register_harness_profile,
)
from agents_gateway.harness.storage import HarnessStorage

# Paths that are intentionally public even when auth is enabled.
PUBLIC_PATHS = {
    "/health",
    "/ready",
    "/version",
    "/docs",
    "/openapi.json",
}

# Path prefixes that are always public (OAuth discovery).
PUBLIC_PREFIXES = (
    "/.well-known/",
)


# OAuth flow paths. /authorize must be Cloudflare-Access-protected at the
# edge (documented assumption). /token and /register are reachable by MCP
# clients but enforce flow integrity at the handler level. /register and
# /token are exempt from auth middleware because they are part of the
# OAuth handshake and implement their own flow integrity checks.
OAUTH_PATHS = {"/authorize", "/token", "/register"}


def _is_public(path: str) -> bool:
    if path in PUBLIC_PATHS:
        return True
    for p in PUBLIC_PREFIXES:
        if path.startswith(p):
            return True
    return False


def _enrich_task(task, harness_storage: "HarnessStorage") -> dict[str, Any]:
    """Augment a TaskRecord with harness-runtime state for /tasks{,/{id}}.

    Adds a ``harness`` block to the response when the task has a
    corresponding harness session — this is the bridge between the
    legacy task state machine (created/queued/running/waiting/
    completed/failed/cancelled) and the richer harness session state
    (created/starting/running/waiting_for_reply/verifying/
    completed/blocked_external/failed/cancelled/stalled).

    The legacy ``status`` field is still authoritative for the
    state-machine view; ``harness.status`` is the harness-runtime view.
    Conductor reconciles by reading both.
    """
    d = task.model_dump()
    try:
        session = harness_storage.get_session_by_task(task.id)
    except Exception:
        session = None
    if session is not None:
        d["harness"] = {
            "session_id": session.id,
            "status": session.status,
            "harness_profile": session.harness_profile,
            "tmux_session": session.tmux_session,
            "worktree_id": _worktree_id_for(harness_storage, task.id),
            "started_at": session.started_at,
            "ended_at": session.ended_at,
        }
    return d


def _worktree_id_for(harness_storage: "HarnessStorage", task_id: str) -> str | None:
    try:
        wt = harness_storage.get_worktree_by_task(task_id)
        return wt.id if wt else None
    except Exception:
        return None


def create_app(config: GatewayConfig, reg: MetricsRegistry | None = None) -> FastMCP:
    _registry = reg or registry
    setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )

    auth_handler = AuthHandler(config.auth)
    storage = TaskStorage(config.storage.sqlite_path)
    # Build the harness runtime config once so it can be shared with
    # the HarnessSessionRuntimeAdapter via the RuntimeRegistry.
    config.runtime._environment = config.environment
    from agents_gateway.harness.runtime import HarnessRuntimeConfig
    harness_runtime_cfg = HarnessRuntimeConfig(
        workspace_root=config.harness.workspace_root,
        worktree_root=config.harness.worktree_root,
        artifacts_root=config.harness.artifacts_root,
        session_poll_interval_seconds=config.harness.session_poll_interval_seconds,
        session_stall_seconds=config.harness.session_stall_seconds,
        auto_commit=config.harness.auto_commit,
        auto_push=config.harness.auto_push,
        auto_pr=config.harness.auto_pr,
        use_fake_tmux=config.harness.use_fake_tmux,
        command_timeout_seconds=config.harness.command_timeout_seconds,
        completion_wait_seconds=config.harness.completion_wait_seconds,
        relay_max_time_seconds=config.harness.relay_max_time_seconds,
        max_verify_iterations=config.harness.max_verify_iterations,
    )
    runtime_registry = create_default_registry(
        config.runtime, harness_config=harness_runtime_cfg)
    harness_storage = HarnessStorage(config.storage.sqlite_path)

    # Single shared tmux driver so all session-level endpoints
    # (send/capture/stop) honor the gateway ``use_fake_tmux`` flag
    # instead of forcing a real TmuxDriver() instance.
    from agents_gateway.harness.tmux import (FakeTmuxDriver,
                                              TmuxDriver)
    _shared_tmux_driver = (FakeTmuxDriver()
                           if config.harness.use_fake_tmux
                           else TmuxDriver())

    if config.observability.metrics_enabled:
        init_gateway_metrics(_registry)

    catalog = AgentCatalog(config)
    _registry.set_gauge("agents_total", catalog.total_count)
    _registry.set_gauge("agents_invalid_total", catalog.invalid_count)

    log_event("service_start", "Agents Gateway starting",
              host=config.service.host, port=config.service.port,
              auth_mode=config.auth.mode, environment=config.environment)
    log_event("agent_scan_started", "Scanning agents directory")
    log_event("agent_scan_completed",
              f"Found {catalog.total_count} agents, {catalog.invalid_count} invalid")
    log_event("service_ready", "Agents Gateway ready")

    mcp = create_mcp_server(config)
    base_url = (config.auth.public_base_url
                or f"http://{config.service.host}:{config.service.port}").rstrip("/")
    mcp_path = config.service.mcp_path

    # Background worker for off-request-path task execution. Pass the
    # environment onto the runtime config so ProcessRuntime can self-gate
    # in production. The harness_config is already set on the registry
    # above and the HarnessSessionRuntimeAdapter reads it from there.
    worker = TaskWorker(storage=storage, catalog=catalog,
                        runtime_registry=runtime_registry,
                        runtime_config=config.runtime,
                        artifacts_dir=config.storage.artifacts_dir,
                        harness_config=harness_runtime_cfg)
    worker.start()

    # Reconcile any harness tmux sessions that survived a gateway
    # process restart. Cheap: one ``tmux has-session`` per
    # recoverable session. Runs synchronously at boot so by the time
    # the gateway advertises ready the picture is consistent.
    try:
        from agents_gateway.harness.reconcile import reconcile_harness_sessions
        rr = reconcile_harness_sessions(harness_storage)
        log_event("harness_reconcile",
                  f"recovered={len(rr.recovered)}, "
                  f"missing={len(rr.missing)}, "
                  f"skipped={len(rr.skipped)}")
    except Exception as e:
        log_event("harness_reconcile_error",
                  f"reconciliation failed: {e}",
                  level="WARNING")

    # Auth middleware that runs for every Starlette route (including
    # custom_route handlers and /mcp). We attach it to the FastMCP instance
    # and pass it to mcp.http_app(middleware=[...]) when building the ASGI
    # app. The mcp.add_middleware() method only wraps the /mcp path; passing
    # the middleware to http_app() is the only way to cover custom_route.
    auth_handler_ref = auth_handler
    cfg_ref = config
    reg_ref = _registry

    # Rate limit state (in-memory buckets keyed by client IP). This is a
    # coarse-grained per-process limit; not designed for multi-replica
    # deployments.
    _rate_buckets: dict[str, list[float]] = {}
    _rate_enabled = config.service.rate_limiting.enabled
    _rate_rpm = config.service.rate_limiting.requests_per_minute

    def _make_middleware_cls():
        import time as _time
        class _AuthMiddleware(BaseHTTPMiddleware):
            async def dispatch(self, request: Request, call_next):
                path = request.url.path
                request_id = str(uuid.uuid4())
                bind_request_context(request_id, "",
                                     request.method, path, str(request.url))
                try:
                    # Rate limit applies to ALL paths including public ones
                    # (health checks can be hammered by misconfigured probes
                    # and rate limiting them protects the gateway).
                    if _rate_enabled and request.client:
                        ip = request.client.host
                        now = _time.time()
                        bucket = _rate_buckets.setdefault(ip, [])
                        bucket[:] = [t for t in bucket if now - t < 60.0]
                        if len(bucket) >= _rate_rpm:
                            return JSONResponse(
                                status_code=429,
                                content={"error": "Rate limit exceeded",
                                         "retry_after_seconds": 60},
                            )
                        bucket.append(now)
                    if _is_public(path) or (path in OAUTH_PATHS and path != "/authorize"):
                        return await call_next(request)
                    # /authorize must be CF-Access edge-protected; we
                    # also accept it from in-app caller if CF JWT is provided.
                    if path == "/authorize":
                        if path == "/authorize" and cfg_ref.auth.mode == "cloudflare-access":
                            cf_jwt = request.headers.get(CF_JWT_HEADER, "")
                            auth_result = auth_handler_ref.check(
                                client_host=request.client.host if request.client else "",
                                cf_jwt=cf_jwt,
                                internal_token=request.headers.get(INTERNAL_AUTH_HEADER, ""),
                            )
                            if not auth_result.allowed:
                                return JSONResponse(
                                    status_code=401,
                                    content={"error": "Cloudflare Access JWT required at /authorize"},
                                )
                        return await call_next(request)

                    # All other custom routes (/agents, /tasks, /inventory, /metrics, etc.)
                    auth_result = auth_handler_ref.check(
                        client_host=request.client.host if request.client else "",
                        bearer_token="",
                        cf_jwt=request.headers.get(CF_JWT_HEADER, ""),
                        internal_token=request.headers.get(INTERNAL_AUTH_HEADER, ""),
                    )
                    if not auth_result.allowed:
                        return JSONResponse(
                            status_code=401,
                            content={"error": auth_result.error, "auth_mode": auth_handler_ref.mode},
                        )
                    bind_request_context(request_id, auth_result.user,
                                         request.method, path, str(request.url))
                    reg_ref.inc_counter("requests_total")
                    return await call_next(request)
                finally:
                    clear_request_context()
        return _AuthMiddleware

    mcp._auth_middleware_cls = _make_middleware_cls()  # type: ignore[attr-defined]



    # OAuth well-known metadata
    @mcp.custom_route("/.well-known/oauth-authorization-server", methods=["GET"])
    async def oauth_authorization_server(request: Request):
        return JSONResponse({
            "issuer": base_url,
            "authorization_endpoint": f"{base_url}/authorize",
            "token_endpoint": f"{base_url}/token",
            "registration_endpoint": f"{base_url}/register",
            "scopes_supported": ["mcp"],
            "response_types_supported": ["code"],
            "grant_types_supported": ["authorization_code", "refresh_token"],
            "token_endpoint_auth_methods_supported": ["client_secret_post", "client_secret_basic"],
            "code_challenge_methods_supported": ["S256"],
        }, headers={"Cache-Control": "public, max-age=3600"})

    @mcp.custom_route("/.well-known/oauth-protected-resource/mcp", methods=["GET"])
    async def oauth_protected_resource(request: Request):
        return JSONResponse({
            "resource": f"{base_url}{mcp_path}",
            "authorization_servers": [base_url],
            "scopes_supported": ["mcp"],
            "bearer_methods_supported": ["header"],
        }, headers={"Cache-Control": "public, max-age=3600"})

    @mcp.custom_route("/.well-known/oauth-protected-resource/{rest:path}", methods=["GET"])
    async def oauth_protected_resource_catch(request: Request):
        return await oauth_protected_resource(request)

    _oauth_clients: dict[str, Any] = {}
    _oauth_codes: dict[str, Any] = {}

    @mcp.custom_route("/register", methods=["POST"])
    async def register_client(request: Request):
        ct = (request.headers.get("content-type") or "").lower()
        if "application/json" in ct:
            body = await request.json()
        else:
            form = await request.form()
            body = dict(form)
        raw_uris = body.get("redirect_uris", [
            "https://chatgpt.com/aip/mcp/oauth/callback",
            "https://claude.ai/api/mcp/auth_callback",
        ])
        if isinstance(raw_uris, str):
            redirect_uris = [raw_uris]
        else:
            redirect_uris = list(raw_uris)
        client_id = body.get("client_id") or secrets.token_urlsafe(32)
        client_secret = secrets.token_urlsafe(48)
        _oauth_clients[client_id] = {
            "client_id": client_id,
            "client_secret": client_secret,
            "client_id_issued_at": int(time.time()),
            "redirect_uris": redirect_uris,
            "grant_types": ["authorization_code", "refresh_token"],
            "response_types": ["code"],
            "token_endpoint_auth_method": "client_secret_post",
            "scope": "mcp",
            "client_name": body.get("client_name", ""),
        }
        return JSONResponse(_oauth_clients[client_id])

    @mcp.custom_route("/authorize", methods=["GET"])
    async def authorize(request: Request):
        client_id = request.query_params.get("client_id", "")
        redirect_uri = request.query_params.get("redirect_uri", "")
        state = request.query_params.get("state", "")
        # NOTE: this endpoint requires Cloudflare Access at the edge because
        # the in-app middleware only allows it through if a CF JWT is also
        # presented. In normal usage the user is auth'd by Cloudflare
        # before reaching here; the JWT is forwarded in Cf-Access-Jwt-Assertion.
        code = secrets.token_urlsafe(32)
        _oauth_codes[code] = {
            "code": code,
            "client_id": client_id or "dev",
            "expires_at": time.time() + 300,
            "redirect_uri": redirect_uri,
            "subject": "mcp-user",
        }
        sep = "&" if "?" in redirect_uri else "?"
        location = f"{redirect_uri}{sep}code={code}"
        if state:
            location += f"&state={state}"
        return RedirectResponse(url=location, status_code=302)

    @mcp.custom_route("/token", methods=["POST"])
    async def exchange_token(request: Request):
        form = await request.form()
        grant_type = form.get("grant_type", "authorization_code")
        if grant_type == "authorization_code":
            code = form.get("code", "")
            code_data = _oauth_codes.pop(code, None)
            if code_data is None or code_data.get("expires_at", 0) < time.time():
                return JSONResponse(
                    status_code=400,
                    content={"error": "invalid_grant",
                             "error_description": "authorization code does not exist or has expired"},
                )
            token = secrets.token_urlsafe(48)
            refresh = secrets.token_urlsafe(48)
            return JSONResponse({
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "refresh_token": refresh,
                "scope": "mcp",
            })
        if grant_type == "refresh_token":
            token = secrets.token_urlsafe(48)
            return JSONResponse({
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "scope": "mcp",
            })
        return JSONResponse(status_code=400, content={"error": "unsupported_grant_type"})

    # Health / readiness / version (PUBLIC)
    @mcp.custom_route("/health", methods=["GET"])
    async def health(request: Request):
        return JSONResponse({"status": "ok"})

    @mcp.custom_route("/ready", methods=["GET"])
    async def ready(request: Request):
        checks = {
            "storage": True,
            "agents_dir": os.path.isdir(config.agents.dir),
            "agent_scan": catalog.total_count >= 0,
            "auth_mode": config.auth.mode,
            "worker": worker.is_alive(),
        }
        checks["ready"] = all([checks["storage"], checks["agents_dir"],
                               checks["agent_scan"], checks["worker"]])
        return JSONResponse(checks)

    @mcp.custom_route("/version", methods=["GET"])
    async def version(request: Request):
        return JSONResponse({"name": "agents-gateway", "version": __version__,
                             "environment": config.environment})

    @mcp.custom_route("/inventory", methods=["GET"])
    async def inventory(request: Request):
        return JSONResponse({
            "agent_count": catalog.total_count,
            "invalid_count": catalog.invalid_count,
            "profiles": catalog.profiles,
            "auth_mode": config.auth.mode,
            "storage_mode": "sqlite",
            "tools": [
                "agents_list", "agents_search", "agents_inspect",
                "agent_task_create", "agent_task_get", "agent_task_events",
                "agent_task_artifacts", "agent_task_cancel",
            ],
            "active_profile": config.profile,
        })

    @mcp.custom_route("/metrics", methods=["GET"])
    async def metrics(request: Request):
        return Response(content=_registry.format_prometheus(), media_type="text/plain")

    @mcp.custom_route("/docs", methods=["GET"])
    async def docs(request: Request):
        return JSONResponse({"message": "See /docs for OpenAPI spec",
                             "openapi": "/openapi.json"})

    # Agent catalog HTTP API (PROTECTED)
    @mcp.custom_route("/agents", methods=["GET"])
    async def list_agents(request: Request):
        # Manifest-backed agents.
        agents = catalog.list_agents()
        manifest_entries = [a.model_dump() for a in agents]
        # Harness-profile entries (harness_session runtime type).
        harness_entries = [e.model_dump() for e in catalog.list_harness_profiles()]
        log_event("agent_list",
                  f"Listed {len(manifest_entries)} manifest agents + "
                  f"{len(harness_entries)} harness profiles")
        return JSONResponse({
            "agents": manifest_entries,
            "harness_profiles": harness_entries,
        })

    @mcp.custom_route("/agents/validate", methods=["POST"])
    async def validate_agents(request: Request):
        results = catalog.validate_all()
        return JSONResponse({"results": [r.model_dump() for r in results]})

    @mcp.custom_route("/agents/{agent_id}", methods=["GET"])
    async def get_agent(request: Request):
        agent_id = request.path_params["agent_id"]
        # First check the manifest catalog.
        agent = catalog.get_agent(agent_id)
        if agent is not None:
            log_event("agent_inspect",
                      f"Inspected agent {agent_id}", agent_id=agent_id)
            return JSONResponse(agent.model_dump())
        # Then the harness-profile catalog so /agents/opencode-deepseek
        # returns the harness entry instead of 404.
        harness_entry = catalog.get_harness_profile_entry(agent_id)
        if harness_entry is not None:
            return JSONResponse(harness_entry.model_dump())
        return JSONResponse(status_code=404,
                            content={"error": f"Agent '{agent_id}' not found"})

    # Tasks HTTP API (PROTECTED)
    @mcp.custom_route("/tasks", methods=["POST"])
    async def create_task(request: Request):
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "Invalid JSON body"})
        agent_id = body.get("agent_id", "")
        # Harness-session path: a task is composer-controlled when
        #   * the body declares execution.mode == "harness_session"
        #   * the body declares runtime_type == "harness_session"
        #   * agent_id is literally "harness_session"
        #   * agent_id matches a known harness profile (so callers can
        #     create a task with agent_id="opencode-deepseek" and zero
        #     additional ceremony — the worker dispatches through the
        #     harness_session RuntimeRegistry entry)
        is_harness_explicit = (
            body.get("execution", {}).get("mode") == "harness_session"
            or agent_id == "harness_session"
            or body.get("runtime_type") == "harness_session"
        )
        matches_harness_profile = bool(agent_id and agent_id != "harness_session"
                                       and _get_harness_profile(agent_id) is not None)
        if is_harness_explicit or matches_harness_profile:
            # Stamp harness_profile into the spec so the
            # HarnessSessionRuntimeAdapter finds it deterministically.
            if matches_harness_profile:
                body.setdefault("execution", {})
                body.setdefault("execution", {}).setdefault(
                    "harness_profile", agent_id)
                body.setdefault("execution", {})["mode"] = "harness_session"
            task = storage.create_harness_task(
                agent_id="harness_session",
                task_spec=body,
                metadata={"composer_task_id": body.get("composer_task_id"),
                          "objective_id": body.get("objective_id"),
                          "title": body.get("title", "")[:120]},
            )
            _registry.inc_counter("tasks_total")
            _registry.inc_counter("tasks_created_total")
            log_event("task_created",
                      f"Harness session task {task.id} created",
                      task_id=task.id, agent_id=task.agent_id,
                      runtime_type="harness_session")
            return JSONResponse(task.model_dump(), status_code=201)
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(
                status_code=400,
                content={"error": f"Agent '{agent_id}' not found or not in active profile"},
            )
        task = storage.create_task(agent_id, body.get("input", ""))
        _registry.inc_counter("tasks_total")
        _registry.inc_counter("tasks_created_total")
        log_event("task_created", f"Task {task.id} created for agent {agent_id}",
                  task_id=task.id, agent_id=agent_id)
        return JSONResponse(task.model_dump(), status_code=201)

    @mcp.custom_route("/tasks", methods=["GET"])
    async def list_tasks(request: Request):
        status = request.query_params.get("status")
        agent_id = request.query_params.get("agent_id")
        try:
            limit = int(request.query_params.get("limit", "50"))
            offset = int(request.query_params.get("offset", "0"))
        except ValueError:
            return JSONResponse(status_code=400, content={"error": "limit/offset must be integers"})
        try:
            tasks = storage.list_tasks(status=status, agent_id=agent_id,
                                       limit=limit, offset=offset)
        except ValueError as e:
            return JSONResponse(status_code=400, content={"error": str(e)})
        return JSONResponse({"tasks": [_enrich_task(t, harness_storage)
                                       for t in tasks]})

    @mcp.custom_route("/tasks/{task_id}", methods=["GET"])
    async def get_task(request: Request):
        task_id = request.path_params["task_id"]
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Task '{task_id}' not found"})
        return JSONResponse(_enrich_task(task, harness_storage))

    @mcp.custom_route("/tasks/{task_id}/events", methods=["GET"])
    async def get_task_events(request: Request):
        task_id = request.path_params["task_id"]
        events = storage.list_events(task_id)
        return JSONResponse({"events": [e.model_dump() for e in events]})

    @mcp.custom_route("/tasks/{task_id}/artifacts", methods=["GET"])
    async def get_task_artifacts(request: Request):
        task_id = request.path_params["task_id"]
        artifacts = storage.list_artifacts(task_id)
        return JSONResponse({"artifacts": [a.model_dump() for a in artifacts]})

    @mcp.custom_route("/tasks/{task_id}/cancel", methods=["POST"])
    async def cancel_task(request: Request):
        task_id = request.path_params["task_id"]
        try:
            task = storage.cancel_task(task_id)
            _registry.inc_counter("tasks_cancelled_total")
            log_event("task_cancelled", f"Task {task_id} cancelled", task_id=task_id)
            return JSONResponse(task.model_dump())
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})

    @mcp.custom_route("/tasks/{task_id}/run", methods=["POST"])
    async def run_task(request: Request):
        task_id = request.path_params["task_id"]
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Task '{task_id}' not found"})

        # Harness-session tasks bypass the agent catalog; they're
        # composer-controlled and the harness runtime is responsible
        # for starting the harness inside the worktree.
        is_harness = bool(getattr(task, "metadata", {}) and
                          task.metadata.get("runtime_type") == "harness_session")
        if not is_harness:
            agent = catalog.get_agent(task.agent_id)
            if agent is None:
                return JSONResponse(
                    status_code=400,
                    content={"error": f"Agent '{task.agent_id}' not found"},
                )

            if agent.risk_level.value == "high":
                headers = dict(request.headers)
                risk_check = auth_handler.check_high_risk(headers)
                if not risk_check.allowed:
                    return JSONResponse(status_code=403,
                                        content={"error": risk_check.error})

        # Enqueue: this transitions created -> queued. The background worker
        # owns queued -> running -> terminal.
        try:
            if task.status == "created":
                storage.update_task_status(task_id, "queued")
            elif task.status in ("completed", "failed", "cancelled"):
                return JSONResponse(
                    status_code=409,
                    content={"error": f"Task already in terminal state '{task.status}'"},
                )
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})

        log_event("task_enqueued", f"Task {task_id} enqueued for run",
                  task_id=task_id, agent_id=task.agent_id,
                  runtime_type=("harness_session" if is_harness
                                 else (agent.runtime.type if not is_harness else "")))
        return JSONResponse(
            {"task_id": task_id, "status": "queued", "agent_id": task.agent_id},
            status_code=202,
        )

    # ===================================================================
    # Harness worktree runtime HTTP API
    # ===================================================================
    #
    # All endpoints below require the same auth as /tasks. Public paths
    # set (health/ready/version/docs/oauth) remain unchanged.

    # -- Harness profiles -------------------------------------------------

    @mcp.custom_route("/harness-profiles", methods=["GET"])
    async def list_harness_profiles(request: Request):
        return JSONResponse(
            {"profiles": [p.to_dict() for p in _list_harness_profiles()]},
        )

    @mcp.custom_route("/harness-profiles/validate", methods=["POST"])
    async def validate_harness_profile(request: Request):
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400,
                                content={"error": "Invalid JSON body"})
        name = body.get("name", "")
        profile = _get_harness_profile(name)
        if profile is None:
            return JSONResponse(
                status_code=404,
                content={"valid": False, "error": f"Unknown profile: {name}"},
            )
        # Validate goal-strategy compatibility: if user requests
        # slash_goal but profile doesn't support it, fail validation.
        strat = body.get("goal_strategy", profile.goal_strategy)
        if strat == "slash_goal" and not profile.supports_slash_goal:
            return JSONResponse(
                status_code=400,
                content={"valid": False,
                         "error": (f"Profile '{name}' does not support "
                                   "slash_goal goal_strategy")},
            )
        return JSONResponse({"valid": True, "profile": profile.to_dict()})

    @mcp.custom_route("/harness-profiles/{name}", methods=["GET"])
    async def get_harness_profile(request: Request):
        name = request.path_params["name"]
        profile = _get_harness_profile(name)
        if profile is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Unknown profile: {name}"})
        return JSONResponse(profile.to_dict())

    @mcp.custom_route("/harness-profiles/{name}/availability", methods=["GET"])
    async def get_harness_availability(request: Request):
        """Return a structured availability report for one harness profile.

        Never raises and never leaks secrets — only binary/credential
        *presence* is reported, not their values.
        """
        name = request.path_params["name"]
        report = catalog.check_harness_availability(name)
        status_code = 200 if report.get("configured") else 404
        _registry.inc_counter("harness_availability_checks_total")
        return JSONResponse(status_code=status_code, content=report)

    # -- Worktrees -------------------------------------------------------

    @mcp.custom_route("/worktrees", methods=["GET"])
    async def list_worktrees(request: Request):
        return JSONResponse(
            {"worktrees": [w.__dict__ for w in harness_storage.list_worktrees()]}
        )

    @mcp.custom_route("/worktrees/{wt_id}", methods=["GET"])
    async def get_worktree(request: Request):
        wt_id = request.path_params["wt_id"]
        wt = harness_storage.get_worktree(wt_id)
        if wt is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Worktree '{wt_id}' not found"})
        return JSONResponse(wt.__dict__)

    @mcp.custom_route("/tasks/{task_id}/worktree", methods=["GET"])
    async def get_task_worktree(request: Request):
        task_id = request.path_params["task_id"]
        wt = harness_storage.get_worktree_by_task(task_id)
        if wt is None:
            return JSONResponse(status_code=404,
                                content={"error": f"No worktree for task {task_id}"})
        return JSONResponse(wt.__dict__)

    # -- Sessions ---------------------------------------------------------

    @mcp.custom_route("/sessions", methods=["GET"])
    async def list_sessions(request: Request):
        status = request.query_params.get("status")
        task_id = request.query_params.get("task_id")
        sessions = harness_storage.list_sessions(status=status, task_id=task_id)
        return JSONResponse(
            {"sessions": [s.__dict__ for s in sessions]}
        )

    @mcp.custom_route("/sessions/{session_id}", methods=["GET"])
    async def get_session(request: Request):
        session_id = request.path_params["session_id"]
        session = harness_storage.get_session(session_id)
        if session is None:
            return JSONResponse(
                status_code=404,
                content={"error": f"Session '{session_id}' not found"})
        return JSONResponse(session.__dict__)

    @mcp.custom_route("/tasks/{task_id}/session", methods=["GET"])
    async def get_task_session(request: Request):
        task_id = request.path_params["task_id"]
        session = harness_storage.get_session_by_task(task_id)
        if session is None:
            return JSONResponse(
                status_code=404,
                content={"error": f"No active session for task {task_id}"})
        return JSONResponse(session.__dict__)

    @mcp.custom_route("/sessions/{session_id}/capture", methods=["GET"])
    async def capture_session(request: Request):
        """Capture recent session output.

        Returns the latest ``lines`` lines of tmux capture plus the
        session status and a redacted capture. The redaction regexes
        are the same ones used by the HTML review report (see
        ``harness/reports.py``) — they scrub Bearer tokens, GitHub
        PATs, and URL credentials. This means Composer never needs to
        post-process captures to safely log them.
        """
        session_id = request.path_params["session_id"]
        lines_requested = 2000
        try:
            lines_requested = max(1, int(request.query_params.get("lines", "2000")))
        except (TypeError, ValueError):
            lines_requested = 2000
        from agents_gateway.harness.driver import HarnessDriver
        from agents_gateway.harness.reports import redact_text as _redact_secrets
        session = harness_storage.get_session(session_id)
        if session is None:
            return JSONResponse(
                status_code=404,
                content={"error": f"Session '{session_id}' not found"})
        # Construct a transient driver bound to the session's tmux
        # session.
        driver = HarnessDriver(storage=harness_storage, tmux_driver=_shared_tmux_driver)
        try:
            output = driver.capture_output(session, lines=lines_requested)
        except Exception as e:
            return JSONResponse(
                status_code=500,
                content={"error": f"capture failed: {e}"})
        safe_output = _redact_secrets(output) if output else output
        _registry.inc_counter("harness_session_captures_total")
        return JSONResponse({
            "session_id": session_id,
            "status": session.status,
            "capture": safe_output,
            "captured_at": session.last_output_at,
            "lines": lines_requested,
        })

    @mcp.custom_route("/sessions/{session_id}/send", methods=["POST"])
    async def send_to_session(request: Request):
        """Send text to a session (intended for Composer/system use only)."""
        session_id = request.path_params["session_id"]
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "Invalid JSON body"})
        text = str(body.get("text", ""))
        submit = bool(body.get("submit", True))
        if not text:
            return JSONResponse(status_code=400, content={"error": "text required"})
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(session_id)
        if session is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Session '{session_id}' not found"})
        driver = HarnessDriver(storage=harness_storage, tmux_driver=_shared_tmux_driver)
        try:
            driver.tmux.send_text(driver._ref(session), text)
            if submit:
                driver.tmux.send_enter(driver._ref(session))
        except Exception as e:
            return JSONResponse(status_code=500,
                                content={"error": f"send_text failed: {e}"})
        _registry.inc_counter("harness_session_send_total")
        # Log the send as a task event so Composer's audit trail is
        # self-contained.
        try:
            storage.append_event(session.task_id,
                                 "composer.session_send",
                                 {"session_id": session_id,
                                  "text_chars": len(text),
                                  "submitted": bool(submit)})
        except Exception:
            pass
        return JSONResponse({"session_id": session_id,
                             "status": "sent",
                             "text_chars": len(text)})

    @mcp.custom_route("/sessions/{session_id}/stop", methods=["POST"])
    async def stop_session(request: Request):
        session_id = request.path_params["session_id"]
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(session_id)
        if session is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Session '{session_id}' not found"})
        driver = HarnessDriver(storage=harness_storage, tmux_driver=_shared_tmux_driver)
        try:
            driver.stop_session(session)
        except Exception as e:
            return JSONResponse(status_code=500,
                                content={"error": f"stop failed: {e}"})
        return JSONResponse({"session_id": session_id,
                             "status": session.status})

    # -- Composer interactions --------------------------------------------

    @mcp.custom_route("/interactions", methods=["GET"])
    async def list_interactions(request: Request):
        status = request.query_params.get("status")
        task_id = request.query_params.get("task_id")
        agent_run_id = request.query_params.get("agent_run_id")
        interactions = harness_storage.list_interactions(
            status=status, task_id=task_id, agent_run_id=agent_run_id,
        )
        return JSONResponse(
            {"interactions": [i.__dict__ for i in interactions]}
        )

    @mcp.custom_route("/interactions/{interaction_id}", methods=["GET"])
    async def get_interaction(request: Request):
        interaction_id = request.path_params["interaction_id"]
        interaction = harness_storage.get_interaction(interaction_id)
        if interaction is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Interaction '{interaction_id}' not found"})
        return JSONResponse(interaction.__dict__)

    @mcp.custom_route("/interactions/{interaction_id}/reply", methods=["POST"])
    async def reply_to_interaction(request: Request):
        """Composer sends a reply which is then injected into the session."""
        interaction_id = request.path_params["interaction_id"]
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "Invalid JSON body"})
        reply_text = str(body.get("reply", ""))
        if not reply_text:
            return JSONResponse(status_code=400, content={"error": "reply required"})
        interaction = harness_storage.get_interaction(interaction_id)
        if interaction is None:
            return JSONResponse(
                status_code=404,
                content={"error": f"Interaction '{interaction_id}' not found"})
        if interaction.status != ComposerInteractionStatus.pending.value:
            return JSONResponse(
                status_code=409,
                content={"error": f"Interaction is not pending (status={interaction.status})"})
        # Deliver into the session via the harness driver.
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(interaction.session_id)
        if session is None:
            # Session was cleaned up; we still mark interaction answered
            # so Composer gets an ack, and surface the missing session
            # via the metadata.
            harness_storage.update_interaction_status(
                interaction_id, ComposerInteractionStatus.answered.value,
                composer_reply=reply_text,
            )
            return JSONResponse(
                {"interaction_id": interaction_id,
                 "status": "answered",
                 "warning": "session_missing_ delivery_skipped"},
            )
        driver = HarnessDriver(storage=harness_storage, tmux_driver=_shared_tmux_driver)
        try:
            driver.send_reply(session, reply_text)
        except Exception as e:
            return JSONResponse(status_code=500,
                                content={"error": f"send_reply failed: {e}"})
        # Mark answered.
        harness_storage.update_interaction_status(
            interaction_id, ComposerInteractionStatus.answered.value,
            composer_reply=reply_text,
        )
        _registry.inc_counter("harness_composer_interactions_answered_total")
        # Emit events into the task timeline.
        storage.append_event(interaction.task_id,
                             "composer.interaction.answered",
                             {"interaction_id": interaction_id})
        storage.append_event(interaction.task_id,
                             "agent.resumed",
                             {"interaction_id": interaction_id,
                              "session_id": interaction.session_id})
        return JSONResponse(
            {"interaction_id": interaction_id,
             "status": "answered",
             "session_id": interaction.session_id}
        )

    @mcp.custom_route("/interactions/{interaction_id}/cancel", methods=["POST"])
    async def cancel_interaction(request: Request):
        interaction_id = request.path_params["interaction_id"]
        interaction = harness_storage.get_interaction(interaction_id)
        if interaction is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Interaction '{interaction_id}' not found"})
        if interaction.status != ComposerInteractionStatus.pending.value:
            return JSONResponse(
                status_code=409,
                content={"error": f"Interaction is not pending (status={interaction.status})"})
        harness_storage.update_interaction_status(
            interaction_id, ComposerInteractionStatus.cancelled.value,
        )
        # Log the cancellation as a task event so Composer has a
        # complete audit trail without polling the interaction table.
        try:
            storage.append_event(interaction.task_id,
                                 "composer.interaction.cancelled",
                                 {"interaction_id": interaction_id})
        except Exception:
            pass
        return JSONResponse(
            {"interaction_id": interaction_id, "status": "cancelled"}
        )

    # -- Verification + artifacts ----------------------------------------

    @mcp.custom_route("/agent-runs/{agent_run_id}/verification",
                      methods=["GET"])
    async def get_verification(request: Request):
        agent_run_id = request.path_params["agent_run_id"]
        vr = harness_storage.get_verification_run_by_agent_run(agent_run_id)
        if vr is None:
            return JSONResponse(status_code=404,
                                content={"error": "No verification run found"})
        return JSONResponse({
            "id": vr.id,
            "agent_run_id": vr.agent_run_id,
            "task_id": vr.task_id,
            "status": vr.status,
            "started_at": vr.started_at,
            "completed_at": vr.completed_at,
            "commands": [c.__dict__ for c in vr.commands],
            "metadata": vr.metadata,
        })

    @mcp.custom_route("/agent-runs/{agent_run_id}/verify", methods=["POST"])
    async def trigger_verification(request: Request):
        """Trigger a fresh verification run for the agent_run.

        The runtime orchestrator runs verification automatically, but
        Composer can also trigger an explicit re-verification via this
        endpoint. Returns the resulting VerificationRun as JSON.
        """
        agent_run_id = request.path_params["agent_run_id"]
        worktree = harness_storage.get_worktree_by_run(agent_run_id)
        if worktree is None:
            return JSONResponse(
                status_code=404,
                content={"error": f"No worktree found for agent_run {agent_run_id}"},
            )
        session = harness_storage.get_session(
            harness_storage.get_session_by_task(worktree.task_id).id
            if harness_storage.get_session_by_task(worktree.task_id)
            else ""
        ) if harness_storage.get_session_by_task(worktree.task_id) else None
        # Pull existing verification commands from the task spec
        task = storage.get_task(worktree.task_id)
        if task is None:
            return JSONResponse(status_code=404, content={"error": "task not found"})
        import json as _json
        try:
            spec = _json.loads(task.input) if task.input else {}
        except (ValueError, TypeError):
            spec = {}
        from agents_gateway.harness.verification import (
            VerificationCommand, VerificationRunner,
        )
        vrunner = VerificationRunner(
            storage=harness_storage,
            artifacts_root=config.harness.artifacts_root,
        )
        cmds = []
        for c in spec.get("verification", {}).get("commands", []):
            cmds.append(VerificationCommand(
                name=str(c.get("name", "cmd")),
                command=str(c.get("command", "")),
                required=bool(c.get("required", True)),
                live_e2e=False, env_required=[],
            ))
        live_e2e = spec.get("verification", {}).get("live_e2e") or {}
        if live_e2e.get("required"):
            cmds.append(VerificationCommand(
                name=str(live_e2e.get("name", "live_e2e")),
                command=str(live_e2e.get("command", "")),
                required=bool(live_e2e.get("required", False)),
                live_e2e=True,
                env_required=list(live_e2e.get("env_required", []) or []),
            ))
        if not cmds:
            return JSONResponse(
                status_code=400,
                content={"error": "No verification commands configured"})
        session_obj = None
        existing_sessions = harness_storage.list_sessions(task_id=worktree.task_id)
        if existing_sessions:
            session_obj = existing_sessions[0]
        vr = vrunner.run(agent_run_id, worktree.task_id, worktree.path,
                         cmds, session=session_obj)
        return JSONResponse({
            "id": vr.id, "agent_run_id": vr.agent_run_id,
            "task_id": vr.task_id, "status": vr.status,
            "commands": [c.__dict__ for c in vr.commands],
        })

    @mcp.custom_route("/agent-runs/{agent_run_id}/artifacts", methods=["GET"])
    async def list_run_artifacts(request: Request):
        agent_run_id = request.path_params["agent_run_id"]
        artifacts = harness_storage.list_harness_artifacts(agent_run_id=agent_run_id)
        return JSONResponse({"artifacts": artifacts})

    @mcp.custom_route("/agent-runs/{agent_run_id}", methods=["GET"])
    async def get_agent_run(request: Request):
        """Return the unified agent-run view for one agent_run (== task).

        Combines the legacy TaskRecord, the harness session(s), the
        worktree, the latest verification run, and the enriched
        artifacts list — so Conductor can reconcile a run through a
        single GET instead of stitching multiple endpoints.
        """
        agent_run_id = request.path_params["agent_run_id"]
        task = storage.get_task(agent_run_id)
        if task is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Run '{agent_run_id}' not found"})
        result = _enrich_task(task, harness_storage)
        result["events"] = [e.model_dump() for e in storage.list_events(task.id)]
        return JSONResponse(result)

    @mcp.custom_route("/artifacts/{artifact_id}", methods=["GET"])
    async def get_artifact(request: Request):
        artifact_id = request.path_params["artifact_id"]
        artifact = harness_storage.get_harness_artifact(artifact_id)
        if artifact is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Artifact '{artifact_id}' not found"})
        # If viewer=true and the artifact is on the local filesystem,
        # return the content streaming from disk.
        if request.query_params.get("view") == "true":
            from pathlib import Path
            try:
                p = Path(artifact["path"])
                if not p.exists():
                    return JSONResponse(status_code=410,
                                        content={"error": "artifact file missing"})
                content = p.read_bytes()
                return Response(content, media_type=artifact.get("mime_type",
                                                                "application/octet-stream"))
            except Exception as e:
                return JSONResponse(status_code=500,
                                    content={"error": f"read failed: {e}"})
        return JSONResponse(artifact)

    # -- Cleanup / retention --------------------------------------------
    @mcp.custom_route("/cleanup/dry-run", methods=["POST"])
    async def cleanup_dry_run(request: Request):
        """Report what *would* be deleted under the current retention
        policy. Does not touch disk."""
        from agents_gateway.harness.cleanup import run_cleanup
        report = run_cleanup(
            harness_storage,
            artifact_retention_days=config.harness.artifact_retention_days,
            worktree_retention_days=config.harness.worktree_retention_days,
            max_artifact_bytes=config.harness.max_artifact_bytes,
            dry_run=True,
        )
        _registry.inc_counter("harness_cleanup_dry_run_total")
        return JSONResponse(report.to_dict())

    @mcp.custom_route("/cleanup/run", methods=["POST"])
    async def cleanup_run(request: Request):
        """Actually run the retention cleanup pass.

        Honours ``config.harness.cleanup_dry_run`` — when the operator
        hasn't flipped it to false via
        ``AGW_HARNESS__CLEANUP_DRY_RUN=false`` this endpoint returns
        the dry-run report with a ``dry_run`` flag so callers don't
        silently believe work was deleted.

        ``?force=true`` overrides the dry-run gate so an operator with
        explicit intent can run a one-shot live cleanup without
        reconfiguring the gateway.
        """
        from agents_gateway.harness.cleanup import run_cleanup
        force = request.query_params.get("force", "").lower() in ("1", "true", "yes")
        dry = config.harness.cleanup_dry_run and not force
        report = run_cleanup(
            harness_storage,
            artifact_retention_days=config.harness.artifact_retention_days,
            worktree_retention_days=config.harness.worktree_retention_days,
            max_artifact_bytes=config.harness.max_artifact_bytes,
            dry_run=dry,
        )
        _registry.inc_counter("harness_cleanup_run_total")
        return JSONResponse(report.to_dict())

    return mcp  # end of create_app


def create_asgi_app(config: GatewayConfig, reg: MetricsRegistry | None = None):
    """Return a Starlette ASGI app suitable for TestClient / uvicorn.

    This is the same wiring as create_app but exposes the ASGI surface so
    tests and embedding code that needs a callable ASGI app can use it.
    The auth middleware is passed via the middleware= kwarg so it covers
    all custom_route handlers, not just the /mcp endpoint.
    """
    mcp = create_app(config, reg=reg)
    auth_middleware_cls = getattr(mcp, "_auth_middleware_cls", None)
    middleware = ([Middleware(auth_middleware_cls)] if auth_middleware_cls is not None
                  else [])
    return mcp.http_app(path=config.service.mcp_path, middleware=middleware)


def run_with_config(config: GatewayConfig):
    setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )
    log_event("service_start", "Agents Gateway starting",
              host=config.service.host, port=config.service.port,
              auth_mode=config.auth.mode, environment=config.environment)

    # Production boot assertions
    if config.environment == "production":
        from agents_gateway.auth import AuthHandler
        AuthHandler(config.auth).require_production_safe()

    if config.runtime.allow_process and config.environment == "production":
        log_event("runtime_warning",
                  "runtime.allow_process=true in production: ProcessRuntime is unsandboxed",
                  level="WARNING")

    log_event("service_ready", "Agents Gateway ready to serve requests")
    mcp = create_app(config)
    auth_middleware_cls = getattr(mcp, "_auth_middleware_cls", None)
    middleware = ([Middleware(auth_middleware_cls)] if auth_middleware_cls is not None
                  else [])
    mcp.run(
        transport="streamable-http",
        host=config.service.host,
        port=config.service.port,
        path=config.service.mcp_path,
        middleware=middleware,
    )


def start_server(config: GatewayConfig) -> None:
    run_with_config(config)


def main():
    cfg = load_config()
    run_with_config(cfg)


if __name__ == "__main__":
    main()
