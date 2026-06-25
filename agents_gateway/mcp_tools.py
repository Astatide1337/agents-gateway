"""MCP tools for Agents Gateway."""

from __future__ import annotations

import json
from typing import Any

from fastmcp import FastMCP

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.storage import TaskStorage


def create_mcp_server(config: GatewayConfig) -> FastMCP:
    mcp = FastMCP("Agents Gateway", instructions="Gateway tools for agent discovery and task management")
    catalog = AgentCatalog(config)
    storage = TaskStorage(config.storage.sqlite_path)

    @mcp.tool()
    def agents_list() -> str:
        agents = catalog.list_agents()
        entries = catalog.catalog_entries()
        return json.dumps([e.model_dump() for e in entries])

    @mcp.tool()
    def agents_search(query: str) -> str:
        results = catalog.search_agents(query)
        entries = [
            {
                "id": a.id, "name": a.name, "description": a.description,
                "version": a.version, "runtime": {"type": a.runtime.type},
                "risk_level": a.risk_level.value,
            }
            for a in results
        ]
        return json.dumps(entries)

    @mcp.tool()
    def agents_inspect(agent_id: str) -> str:
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return json.dumps({"error": f"Agent '{agent_id}' not found"})
        return json.dumps(agent.model_dump())

    @mcp.tool()
    def agent_task_create(agent_id: str, input_data: str = "") -> str:
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return json.dumps({"error": f"Agent '{agent_id}' not found or not available in active profile"})
        task = storage.create_task(agent_id, input_data)
        return json.dumps(task.model_dump())

    @mcp.tool()
    def agent_task_get(task_id: str) -> str:
        task = storage.get_task(task_id)
        if task is None:
            return json.dumps({"error": f"Task '{task_id}' not found"})
        return json.dumps(task.model_dump())

    @mcp.tool()
    def agent_task_events(task_id: str) -> str:
        events = storage.list_events(task_id)
        return json.dumps([e.model_dump() for e in events])

    @mcp.tool()
    def agent_task_artifacts(task_id: str) -> str:
        artifacts = storage.list_artifacts(task_id)
        return json.dumps([a.model_dump() for a in artifacts])

    @mcp.tool()
    def agent_task_cancel(task_id: str) -> str:
        try:
            task = storage.cancel_task(task_id)
            return json.dumps(task.model_dump())
        except Exception as e:
            return json.dumps({"error": str(e)})

    return mcp
