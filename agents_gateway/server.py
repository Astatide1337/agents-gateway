"""HTTP server for Agents Gateway using FastMCP custom routes."""

from __future__ import annotations

import os
import secrets
import time
import uuid
from typing import Any

from fastmcp import FastMCP
from starlette.requests import Request
from starlette.responses import JSONResponse, RedirectResponse, Response

from agents_gateway import __version__
from agents_gateway.auth import (
    CF_JWT_HEADER,
    INTERNAL_AUTH_HEADER,
    AuthHandler,
)
from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig, load_config
from agents_gateway.logging import log_event, setup_logging
from agents_gateway.metrics import MetricsRegistry, init_gateway_metrics, registry
from agents_gateway.mcp_tools import create_mcp_server
from agents_gateway.runtime import create_default_registry
from agents_gateway.storage import TaskStorage, TransitionError


def create_app(config: GatewayConfig, reg: MetricsRegistry | None = None) -> FastMCP:
    _registry = reg or registry
    setup_logging(
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

    mcp = create_mcp_server(config)

    base_url = (config.auth.public_base_url or f"http://{config.service.host}:{config.service.port}").rstrip("/")
    mcp_path = config.service.mcp_path

    # OAuth well-known metadata
    @mcp.custom_route("/.well-known/oauth-authorization-server", methods=["GET"])
    async def oauth_authorization_server(request):
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
    async def oauth_protected_resource(request):
        return JSONResponse({
            "resource": f"{base_url}{mcp_path}",
            "authorization_servers": [base_url],
            "scopes_supported": ["mcp"],
            "bearer_methods_supported": ["header"],
        }, headers={"Cache-Control": "public, max-age=3600"})

    @mcp.custom_route("/.well-known/oauth-protected-resource/{rest:path}", methods=["GET"])
    async def oauth_protected_resource_catch(request):
        return await oauth_protected_resource(request)

    _oauth_clients: dict[str, Any] = {}
    _oauth_codes: dict[str, Any] = {}

    @mcp.custom_route("/register", methods=["POST"])
    async def register_client(request):
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
        return JSONResponse(_oauth_clients[client_id])

    @mcp.custom_route("/authorize", methods=["GET"])
    async def authorize(request):
        client_id = request.query_params.get("client_id", "")
        redirect_uri = request.query_params.get("redirect_uri", "")
        state = request.query_params.get("state", "")
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
    async def exchange_token(request):
        form = await request.form()
        grant_type = form.get("grant_type", "authorization_code")
        if grant_type == "authorization_code":
            code = form.get("code", "")
            code_data = _oauth_codes.pop(code, None)
            if code_data is None:
                return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "authorization code does not exist or has expired"})
            token = secrets.token_urlsafe(48)
            refresh = secrets.token_urlsafe(48)
            return JSONResponse({
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "refresh_token": refresh,
                "scope": "mcp",
            })
        elif grant_type == "refresh_token":
            token = secrets.token_urlsafe(48)
            return JSONResponse({
                "access_token": token,
                "token_type": "Bearer",
                "expires_in": 3600,
                "scope": "mcp",
            })
        return JSONResponse(status_code=400, content={"error": "unsupported_grant_type"})

    # Health / readiness / version
    @mcp.custom_route("/health", methods=["GET"])
    async def health(request):
        return JSONResponse({"status": "ok"})

    @mcp.custom_route("/ready", methods=["GET"])
    async def ready(request):
        checks = {
            "storage": True,
            "agents_dir": os.path.isdir(config.agents.dir),
            "agent_scan": catalog.total_count >= 0,
            "auth_mode": config.auth.mode,
        }
        checks["ready"] = all([checks["storage"], checks["agents_dir"], checks["agent_scan"]])
        return JSONResponse(checks)

    @mcp.custom_route("/version", methods=["GET"])
    async def version(request):
        return JSONResponse({"name": "agents-gateway", "version": __version__})

    @mcp.custom_route("/inventory", methods=["GET"])
    async def inventory(request):
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
    async def metrics(request):
        return Response(content=_registry.format_prometheus(), media_type="text/plain")

    @mcp.custom_route("/docs", methods=["GET"])
    async def docs(request):
        return JSONResponse({"message": "See /docs for OpenAPI spec", "openapi": "/openapi.json"})

    # Agent catalog HTTP API
    @mcp.custom_route("/agents", methods=["GET"])
    async def list_agents(request):
        agents = catalog.list_agents()
        log_event("agent_list", f"Listed {len(agents)} agents")
        return JSONResponse({"agents": [a.model_dump() for a in agents]})

    @mcp.custom_route("/agents/validate", methods=["POST"])
    async def validate_agents(request):
        results = catalog.validate_all()
        return JSONResponse({"results": [r.model_dump() for r in results]})

    @mcp.custom_route("/agents/{agent_id}", methods=["GET"])
    async def get_agent(request):
        agent_id = request.path_params["agent_id"]
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return JSONResponse(status_code=404, content={"error": f"Agent '{agent_id}' not found"})
        log_event("agent_inspect", f"Inspected agent {agent_id}", agent_id=agent_id)
        return JSONResponse(agent.model_dump())

    # Tasks HTTP API
    @mcp.custom_route("/tasks", methods=["POST"])
    async def create_task(request):
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
        return JSONResponse(task.model_dump())

    @mcp.custom_route("/tasks", methods=["GET"])
    async def list_tasks(request):
        status = request.query_params.get("status")
        agent_id = request.query_params.get("agent_id")
        limit = int(request.query_params.get("limit", "50"))
        offset = int(request.query_params.get("offset", "0"))
        try:
            tasks = storage.list_tasks(status=status, agent_id=agent_id, limit=limit, offset=offset)
        except ValueError as e:
            return JSONResponse(status_code=400, content={"error": str(e)})
        return JSONResponse({"tasks": [t.model_dump() for t in tasks]})

    @mcp.custom_route("/tasks/{task_id}", methods=["GET"])
    async def get_task(request):
        task_id = request.path_params["task_id"]
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404, content={"error": f"Task '{task_id}' not found"})
        return JSONResponse(task.model_dump())

    @mcp.custom_route("/tasks/{task_id}/events", methods=["GET"])
    async def get_task_events(request):
        task_id = request.path_params["task_id"]
        events = storage.list_events(task_id)
        return JSONResponse({"events": [e.model_dump() for e in events]})

    @mcp.custom_route("/tasks/{task_id}/artifacts", methods=["GET"])
    async def get_task_artifacts(request):
        task_id = request.path_params["task_id"]
        artifacts = storage.list_artifacts(task_id)
        return JSONResponse({"artifacts": [a.model_dump() for a in artifacts]})

    @mcp.custom_route("/tasks/{task_id}/cancel", methods=["POST"])
    async def cancel_task(request):
        task_id = request.path_params["task_id"]
        try:
            task = storage.cancel_task(task_id)
            _registry.inc_counter("tasks_cancelled_total")
            log_event("task_cancelled", f"Task {task_id} cancelled", task_id=task_id)
            return JSONResponse(task.model_dump())
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})

    @mcp.custom_route("/tasks/{task_id}/run", methods=["POST"])
    async def run_task(request):
        task_id = request.path_params["task_id"]
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
            return JSONResponse(result)
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})
        except Exception as e:
            _registry.inc_counter("tasks_failed_total")
            log_event("task_failed", f"Task {task_id} failed", task_id=task_id)
            return JSONResponse(status_code=500, content={"error": str(e)})

    return mcp


def run_with_config(config: GatewayConfig):
    setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )

    log_event("service_start", "Agents Gateway starting", host=config.service.host, port=config.service.port, auth_mode=config.auth.mode)
    log_event("service_ready", "Agents Gateway ready to serve requests")

    mcp = create_app(config)

    mcp.run(
        transport="streamable-http",
        host=config.service.host,
        port=config.service.port,
        path=config.service.mcp_path,
    )


def start_server(config: GatewayConfig) -> None:
    run_with_config(config)


def main():
    cfg = load_config()
    run_with_config(cfg)


if __name__ == "__main__":
    main()
