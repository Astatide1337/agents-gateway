"""FastAPI HTTP server for Agents Gateway."""

from __future__ import annotations

import collections
import json
import os
import secrets
import time
import uuid
from typing import Any

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, RedirectResponse, Response

from agents_gateway import __version__
from agents_gateway.auth import (
    CF_JWT_HEADER,
    INTERNAL_AUTH_HEADER,
    RISK_CONFIRM_HEADER,
    AuthHandler,
)
from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.logging import log_event, setup_logging
from agents_gateway.metrics import MetricsRegistry, registry, init_gateway_metrics
from agents_gateway.mcp_tools import create_mcp_server
from agents_gateway.runtime import create_default_registry
from agents_gateway.storage import TaskStorage, TransitionError

SENSITIVE_HEADERS = frozenset({
    "authorization", "cookie", "x-auth-internal-token",
    "cf-access-jwt-assertion", "x-confirm-high-risk",
})


def create_app(config: GatewayConfig, reg: MetricsRegistry | None = None) -> FastAPI:
    _registry = reg or registry
    logger = setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )

    auth_handler = AuthHandler(config.auth)
    storage = TaskStorage(config.storage.sqlite_path)
    runtime_registry = create_default_registry()

    if config.observability.metrics_enabled:
        init_gateway_metrics(_registry)

    catalog = AgentCatalog(config)
    _registry.set_gauge("agents_total", catalog.total_count)
    _registry.set_gauge("agents_invalid_total", catalog.invalid_count)

    log_event("service_start", "Agents Gateway starting")
    log_event("agent_scan_started", "Scanning agents directory")
    log_event("agent_scan_completed", f"Found {catalog.total_count} agents, {catalog.invalid_count} invalid")
    log_event("service_ready", "Agents Gateway ready")

    mcp_server = create_mcp_server(config)
    mcp_app = mcp_server.http_app(path="/")
    app = FastAPI(title="Agents Gateway", version=__version__, lifespan=mcp_app.lifespan)
    app.mount(config.service.mcp_path, mcp_app)

    _rate_limit_buckets: dict[str, list[float]] = collections.defaultdict(list)
    _rate_limit_enabled = config.service.rate_limiting.enabled
    _rate_limit_rpm = config.service.rate_limiting.requests_per_minute

    @app.middleware("http")
    async def rate_limit_middleware(request: Request, call_next):
        if _rate_limit_enabled and request.client:
            client_ip = request.client.host
            now = time.time()
            window = 60.0
            bucket = _rate_limit_buckets[client_ip]
            bucket[:] = [t for t in bucket if now - t < window]
            if len(bucket) >= _rate_limit_rpm:
                return JSONResponse(
                    status_code=429,
                    content={"error": "Rate limit exceeded", "retry_after_seconds": int(window)},
                )
            bucket.append(now)
        return await call_next(request)

    @app.middleware("http")
    async def request_middleware(request: Request, call_next):
        req_id = str(uuid.uuid4())
        os.environ["AGW_REQUEST_ID"] = req_id

        headers = dict(request.headers)
        auth_result = auth_handler.check(
            client_host=request.client.host if request.client else "",
            bearer_token=headers.get("authorization", ""),
            cf_jwt=headers.get(CF_JWT_HEADER, ""),
            internal_token=headers.get(INTERNAL_AUTH_HEADER, ""),
        )
        if not auth_result.allowed:
            return JSONResponse(
                status_code=401,
                content={"error": auth_result.error, "auth_mode": auth_handler.mode},
            )

        response = await call_next(request)
        _registry.inc_counter("requests_total")
        safe_headers = {k: v for k, v in headers.items() if k.lower() not in SENSITIVE_HEADERS}
        log_event(
            "request_completed",
            f"{request.method} {request.url.path}",
            request_id=req_id,
            user=auth_result.user,
        )
        return response

    _oauth_clients: dict[str, Any] = {}
    _oauth_codes: dict[str, Any] = {}

    @app.get("/.well-known/oauth-authorization-server")
    async def oauth_authorization_server():
        base = config.auth.public_base_url or f"http://{config.service.host}:{config.service.port}"
        return JSONResponse({
            "issuer": base,
            "authorization_endpoint": f"{base}/authorize",
            "token_endpoint": f"{base}/token",
            "registration_endpoint": f"{base}/register",
            "scopes_supported": ["mcp"],
            "response_types_supported": ["code"],
            "grant_types_supported": ["authorization_code", "refresh_token"],
            "token_endpoint_auth_methods_supported": ["client_secret_post", "client_secret_basic"],
            "code_challenge_methods_supported": ["S256"],
        }, headers={"Cache-Control": "public, max-age=3600"})

    @app.get("/.well-known/oauth-protected-resource/mcp")
    async def oauth_protected_resource():
        base = config.auth.public_base_url or f"http://{config.service.host}:{config.service.port}"
        return JSONResponse({
            "resource": f"{base}{config.service.mcp_path}",
            "authorization_servers": [base],
            "scopes_supported": ["mcp"],
            "bearer_methods_supported": ["header"],
        }, headers={"Cache-Control": "public, max-age=3600"})

    @app.get("/.well-known/oauth-protected-resource/{rest:path}")
    async def oauth_protected_resource_catch(rest: str):
        return await oauth_protected_resource()

    @app.post("/register")
    async def register_client(request: Request):
        ct = (request.headers.get("content-type") or "").lower()
        if "application/json" in ct:
            body = await request.json()
        else:
            form = await request.form()
            body = dict(form)
        raw_uris = body.get("redirect_uris", ["https://chatgpt.com/aip/mcp/oauth/callback", "https://claude.ai/api/mcp/auth_callback"])
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
        return _oauth_clients[client_id]

    @app.get("/authorize")
    async def authorize(client_id: str = "", redirect_uri: str = "", state: str = "", response_type: str = "code"):
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

    @app.post("/token")
    async def exchange_token(request: Request):
        form = await request.form()
        grant_type = form.get("grant_type", "authorization_code")
        if grant_type == "authorization_code":
            code = form.get("code", "")
            code_data = _oauth_codes.pop(code, None)
            if code_data is None:
                return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "authorization code does not exist or has expired"})
            token = secrets.token_urlsafe(48)
            refresh = secrets.token_urlsafe(48)
            return {
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "refresh_token": refresh,
                "scope": "mcp",
            }
        elif grant_type == "refresh_token":
            token = secrets.token_urlsafe(48)
            return {
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "scope": "mcp",
            }
        return JSONResponse(status_code=400, content={"error": "unsupported_grant_type"})

    @app.get("/.well-known/oauth-protected-resource/{rest:path}")
    async def oauth_protected_resource_catch(rest: str):
        return await oauth_protected_resource()

    @app.get("/health")
    async def health():
        return {"status": "ok"}

    @app.get("/ready")
    async def ready():
        checks: dict[str, Any] = {}
        checks["storage"] = True
        checks["agents_dir"] = os.path.isdir(config.agents.dir)
        checks["agent_scan"] = catalog.total_count >= 0
        checks["auth_mode"] = config.auth.mode
        checks["ready"] = all([
            checks["storage"], checks["agents_dir"], checks["agent_scan"],
        ])
        return checks

    @app.get("/version")
    async def version():
        return {"name": "agents-gateway", "version": __version__}

    @app.get("/inventory")
    async def inventory():
        return {
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
        }

    @app.get("/metrics")
    async def metrics():
        return Response(content=_registry.format_prometheus(), media_type="text/plain")

    @app.get("/docs")
    async def docs_redirect():
        return JSONResponse({"message": "See /docs for OpenAPI spec", "openapi": "/openapi.json"})

    @app.get("/agents")
    async def list_agents():
        agents = catalog.list_agents()
        log_event("agent_list", f"Listed {len(agents)} agents")
        return {"agents": [a.model_dump() for a in agents]}

    @app.post("/agents/validate")
    async def validate_agents():
        results = catalog.validate_all()
        return {"results": [r.model_dump() for r in results]}

    @app.get("/agents/{agent_id}")
    async def get_agent(agent_id: str):
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(status_code=404, content={"error": f"Agent '{agent_id}' not found"})
        log_event("agent_inspect", f"Inspected agent {agent_id}", agent_id=agent_id)
        return agent.model_dump()

    @app.post("/tasks")
    async def create_task(request: Request):
        try:
            body = await request.json()
        except Exception:
            return JSONResponse(status_code=400, content={"error": "Invalid JSON body"})
        agent_id = body.get("agent_id", "")
        input_data = body.get("input", "")
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(status_code=400, content={"error": f"Agent '{agent_id}' not found or not in active profile"})
        task = storage.create_task(agent_id, input_data)
        _registry.inc_counter("tasks_total")
        _registry.inc_counter("tasks_created_total")
        log_event("task_created", f"Task {task.id} created for agent {agent_id}", task_id=task.id, agent_id=agent_id)
        return task.model_dump()

    @app.get("/tasks")
    async def list_tasks(
        status: str | None = None,
        agent_id: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ):
        try:
            tasks = storage.list_tasks(
                status=status, agent_id=agent_id, limit=limit, offset=offset,
            )
        except ValueError as e:
            return JSONResponse(status_code=400, content={"error": str(e)})
        return {"tasks": [t.model_dump() for t in tasks]}

    @app.get("/tasks/{task_id}")
    async def get_task(task_id: str):
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404, content={"error": f"Task '{task_id}' not found"})
        return task.model_dump()

    @app.get("/tasks/{task_id}/events")
    async def get_task_events(task_id: str):
        events = storage.list_events(task_id)
        return {"events": [e.model_dump() for e in events]}

    @app.get("/tasks/{task_id}/artifacts")
    async def get_task_artifacts(task_id: str):
        artifacts = storage.list_artifacts(task_id)
        return {"artifacts": [a.model_dump() for a in artifacts]}

    @app.post("/tasks/{task_id}/cancel")
    async def cancel_task(task_id: str):
        try:
            task = storage.cancel_task(task_id)
            _registry.inc_counter("tasks_cancelled_total")
            log_event("task_cancelled", f"Task {task_id} cancelled", task_id=task_id)
            return task.model_dump()
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})

    @app.post("/tasks/{task_id}/run")
    async def run_task(request: Request, task_id: str):
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404, content={"error": f"Task '{task_id}' not found"})

        agent = catalog.get_agent(task.agent_id)
        if agent is None:
            return JSONResponse(status_code=400, content={"error": f"Agent '{task.agent_id}' not found"})

        if agent.risk_level.value == "high":
            headers = dict(request.headers)
            risk_check = auth_handler.check_high_risk(headers)
            if not risk_check.allowed:
                return JSONResponse(status_code=403, content={"error": risk_check.error})

        try:
            adapter = runtime_registry.create(
                agent.runtime.type,
                storage=storage,
                artifacts_dir=config.storage.artifacts_dir,
                command=agent.runtime.command,
            )
        except KeyError as e:
            return JSONResponse(status_code=400, content={"error": str(e)})

        if task.status in ("created",):
            try:
                storage.update_task_status(task_id, "queued")
            except TransitionError as e:
                return JSONResponse(status_code=409, content={"error": str(e)})

        try:
            result = adapter.execute(task_id)
            _registry.inc_counter("tasks_completed_total")
            log_event("task_completed", f"Task {task_id} completed", task_id=task_id)
            return result
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})
        except Exception as e:
            _registry.inc_counter("tasks_failed_total")
            log_event("task_failed", f"Task {task_id} failed", task_id=task_id)
            return JSONResponse(status_code=500, content={"error": str(e)})

    return app


def start_server(config: GatewayConfig) -> None:
    app = create_app(config)
    uvicorn.run(app, host=config.service.host, port=config.service.port)
