"""FastAPI HTTP server for Agents Gateway."""

from __future__ import annotations

import json
import os
import uuid
from typing import Any

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, Response

from agents_gateway import __version__
from agents_gateway.auth import AuthHandler
from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.logging import log_event, setup_logging
from agents_gateway.metrics import MetricsRegistry, registry, init_gateway_metrics
from agents_gateway.runtime import StubRuntime
from agents_gateway.storage import TaskStorage, TransitionError


def create_app(config: GatewayConfig, reg: MetricsRegistry | None = None) -> FastAPI:
    _registry = reg or registry
    logger = setup_logging(
        log_level=config.observability.log_level,
        log_format=config.observability.log_format,
    )

    auth_handler = AuthHandler(config.auth)
    storage = TaskStorage(config.storage.sqlite_path)
    runtime = StubRuntime(storage, config.storage.artifacts_dir)

    if config.observability.metrics_enabled:
        init_gateway_metrics(_registry)

    catalog = AgentCatalog(config)
    _registry.set_gauge("agents_total", catalog.total_count)
    _registry.set_gauge("agents_invalid_total", catalog.invalid_count)

    log_event("service_start", "Agents Gateway starting")
    log_event("agent_scan_started", "Scanning agents directory")
    log_event("agent_scan_completed", f"Found {catalog.total_count} agents, {catalog.invalid_count} invalid")
    log_event("service_ready", "Agents Gateway ready")

    app = FastAPI(title="Agents Gateway", version=__version__)

    @app.middleware("http")
    async def request_middleware(request: Request, call_next):
        req_id = str(uuid.uuid4())
        os.environ["AGW_REQUEST_ID"] = req_id
        response = await call_next(request)
        _registry.inc_counter("requests_total")
        log_event("request_completed", f"{request.method} {request.url.path}", request_id=req_id)
        return response

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
            checks["storage"],
            checks["agents_dir"],
            checks["agent_scan"],
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
        body = await request.json()
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
    async def list_tasks():
        tasks = storage.list_tasks()
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
    async def run_task(task_id: str):
        task = storage.get_task(task_id)
        if task is None:
            return JSONResponse(status_code=404, content={"error": f"Task '{task_id}' not found"})

        if task.status in ("created",):
            try:
                storage.update_task_status(task_id, "queued")
            except TransitionError as e:
                return JSONResponse(status_code=409, content={"error": str(e)})

        try:
            result = runtime.execute(task_id)
            _registry.inc_counter("tasks_completed_total")
            log_event("task_completed", f"Task {task_id} completed", task_id=task_id)
            return result
        except TransitionError as e:
            return JSONResponse(status_code=409, content={"error": str(e)})
        except Exception as e:
            _registry.inc_counter("tasks_failed_total")
            log_event("task_failed", f"Task {task_id} failed: {e}", task_id=task_id, error=str(e))
            return JSONResponse(status_code=500, content={"error": str(e)})

    return app


def start_server(config: GatewayConfig) -> None:
    app = create_app(config)
    uvicorn.run(app, host=config.service.host, port=config.service.port)
