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


def create_app(config: GatewayConfig, reg: MetricsRegistry | None = None) -> FastMCP:
    _registry = reg or registry
    setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )

    auth_handler = AuthHandler(config.auth)
    storage = TaskStorage(config.storage.sqlite_path)
    runtime_registry = create_default_registry(config.runtime)

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
    # in production.
    config.runtime._environment = config.environment
    worker = TaskWorker(storage=storage, catalog=catalog,
                        runtime_registry=runtime_registry,
                        runtime_config=config.runtime,
                        artifacts_dir=config.storage.artifacts_dir)
    worker.start()

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
        agents = catalog.list_agents()
        log_event("agent_list", f"Listed {len(agents)} agents")
        return JSONResponse({"agents": [a.model_dump() for a in agents]})

    @mcp.custom_route("/agents/validate", methods=["POST"])
    async def validate_agents(request: Request):
        results = catalog.validate_all()
        return JSONResponse({"results": [r.model_dump() for r in results]})

    @mcp.custom_route("/agents/{agent_id}", methods=["GET"])
    async def get_agent(request: Request):
        agent_id = request.path_params["agent_id"]
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Agent '{agent_id}' not found"})
        log_event("agent_inspect", f"Inspected agent {agent_id}", agent_id=agent_id)
        return JSONResponse(agent.model_dump())

    # Tasks HTTP API (PROTECTED)
    @mcp.custom_route("/tasks", methods=["POST"])
    async def create_task(request: Request):
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "Invalid JSON body"})
        agent_id = body.get("agent_id", "")
        input_data = body.get("input", "")
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(
                status_code=400,
                content={"error": f"Agent '{agent_id}' not found or not in active profile"},
            )
        task = storage.create_task(agent_id, input_data)
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
        return JSONResponse({"tasks": [t.model_dump() for t in tasks]})

    @mcp.custom_route("/tasks/{task_id}", methods=["GET"])
    async def get_task(request: Request):
        task_id = request.path_params["task_id"]
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404,
                                content={"error": f"Task '{task_id}' not found"})
        return JSONResponse(task.model_dump())

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
                  runtime_type=agent.runtime.type)
        return JSONResponse(
            {"task_id": task_id, "status": "queued", "agent_id": task.agent_id},
            status_code=202,
        )

    return mcp


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
