"""Smoke tests for Agents Gateway (AGW-007).

Creates temporary agent fixtures, isolated sqlite/artifacts storage, verifies
REST /health /inventory /agents, exercises task lifecycle create/run/get/events/artifacts,
and uses FastMCP Client against the in-process server to call agents_list,
agents_search, and agents_inspect.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from fastapi.testclient import TestClient
from fastmcp.client import Client

from agents_gateway.config import GatewayConfig
from agents_gateway.mcp_tools import create_mcp_server
from agents_gateway.metrics import MetricsRegistry
from agents_gateway.server import create_app

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
def agents_dir(tmp_path: Path) -> Path:
    d = tmp_path / "agents"
    d.mkdir()

    d1 = d / "smoke-test-agent"
    d1.mkdir()
    (d1 / "agent.yaml").write_text(
        "id: smoke-test-agent\n"
        "name: Smoke Test Agent\n"
        "description: Primary agent for AGW-007 smoke tests\n"
        "version: 0.1.0\n"
        "runtime:\n  type: local-stub\n"
        "tags: [smoke, primary]\n"
    )

    d2 = d / "search-agent"
    d2.mkdir()
    (d2 / "agent.yaml").write_text(
        "id: search-agent\n"
        "name: Search Agent\n"
        "description: Agent for testing search and inspect\n"
        "version: 0.2.0\n"
        "runtime:\n  type: local-stub\n"
        "tags: [search, discovery]\n"
    )

    return d


@pytest.fixture
def config(agents_dir: Path, tmp_path: Path) -> GatewayConfig:
    return GatewayConfig(
        agents={"dir": str(agents_dir)},
        storage={
            "sqlite_path": str(tmp_path / "smoke-test.db"),
            "artifacts_dir": str(tmp_path / "artifacts"),
        },
        observability={
            "log_level": "WARNING",
            "log_format": "json",
            "metrics_enabled": False,
        },
    )


@pytest.fixture
def app_client(config: GatewayConfig) -> TestClient:
    fresh_registry = MetricsRegistry()
    app = create_app(config, reg=fresh_registry)
    with TestClient(app) as client:
        yield client


@pytest.fixture
def mcp_server(config: GatewayConfig):
    return create_mcp_server(config)


# ---------------------------------------------------------------------------
# REST management endpoints
# ---------------------------------------------------------------------------


class TestSmokeRESTEndpoints:
    def test_health(self, app_client: TestClient) -> None:
        resp = app_client.get("/health")
        assert resp.status_code == 200
        assert resp.json() == {"status": "ok"}

    def test_inventory(self, app_client: TestClient) -> None:
        resp = app_client.get("/inventory")
        assert resp.status_code == 200
        data = resp.json()
        assert data["agent_count"] >= 2
        assert data["auth_mode"] == "dev-none"
        assert "agents_list" in data["tools"]
        assert "agent_task_create" in data["tools"]

    def test_list_agents(self, app_client: TestClient) -> None:
        resp = app_client.get("/agents")
        assert resp.status_code == 200
        data = resp.json()
        agent_ids = {a["id"] for a in data["agents"]}
        assert "smoke-test-agent" in agent_ids
        assert "search-agent" in agent_ids

    def test_get_agent_found(self, app_client: TestClient) -> None:
        resp = app_client.get("/agents/smoke-test-agent")
        assert resp.status_code == 200
        data = resp.json()
        assert data["id"] == "smoke-test-agent"
        assert data["name"] == "Smoke Test Agent"

    def test_get_agent_not_found(self, app_client: TestClient) -> None:
        resp = app_client.get("/agents/nonexistent")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Task lifecycle
# ---------------------------------------------------------------------------


class TestSmokeTaskLifecycle:
    def test_create_task(self, app_client: TestClient) -> None:
        resp = app_client.post(
            "/tasks",
            json={"agent_id": "smoke-test-agent", "input": "hello from smoke test"},
        )
        assert resp.status_code == 200
        task = resp.json()
        assert task["status"] == "created"
        assert task["agent_id"] == "smoke-test-agent"
        assert "id" in task

    def test_create_task_invalid_agent(self, app_client: TestClient) -> None:
        resp = app_client.post("/tasks", json={"agent_id": "nonexistent", "input": ""})
        assert resp.status_code == 400

    def test_full_lifecycle(self, app_client: TestClient) -> None:
        # 1. Create
        create_resp = app_client.post(
            "/tasks",
            json={"agent_id": "smoke-test-agent", "input": "full lifecycle"},
        )
        assert create_resp.status_code == 200
        task_id: str = create_resp.json()["id"]

        # 2. Run
        run_resp = app_client.post(f"/tasks/{task_id}/run")
        assert run_resp.status_code == 200
        assert run_resp.json()["status"] == "completed"

        # 3. Get
        get_resp = app_client.get(f"/tasks/{task_id}")
        assert get_resp.status_code == 200
        assert get_resp.json()["status"] == "completed"
        assert get_resp.json()["id"] == task_id

        # 4. Events
        events_resp = app_client.get(f"/tasks/{task_id}/events")
        assert events_resp.status_code == 200
        events = events_resp.json()["events"]
        event_names = {e["event"] for e in events}
        assert "task_created" in event_names
        assert "task_completed" in event_names
        assert "runtime_started" in event_names
        assert "artifact_created" in event_names

        # 5. Artifacts
        arts_resp = app_client.get(f"/tasks/{task_id}/artifacts")
        assert arts_resp.status_code == 200
        artifacts = arts_resp.json()["artifacts"]
        assert len(artifacts) >= 1
        assert artifacts[0]["name"] == "result.json"

    def test_cancel_task(self, app_client: TestClient) -> None:
        create_resp = app_client.post(
            "/tasks",
            json={"agent_id": "smoke-test-agent", "input": "cancel me"},
        )
        task_id = create_resp.json()["id"]

        cancel_resp = app_client.post(f"/tasks/{task_id}/cancel")
        assert cancel_resp.status_code == 200
        assert cancel_resp.json()["status"] == "cancelled"

    def test_get_nonexistent_task(self, app_client: TestClient) -> None:
        resp = app_client.get("/tasks/nonexistent-task-id")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# MCP tools via FastMCP Client
# ---------------------------------------------------------------------------


class TestSmokeMCPTools:
    @pytest.mark.asyncio
    async def test_agents_list(self, mcp_server) -> None:
        async with Client(mcp_server) as client:
            result = await client.call_tool("agents_list")
        data = json.loads(result.content[0].text)
        agent_ids = {a["id"] for a in data}
        assert "smoke-test-agent" in agent_ids
        assert "search-agent" in agent_ids

    @pytest.mark.asyncio
    async def test_agents_search(self, mcp_server) -> None:
        async with Client(mcp_server) as client:
            result = await client.call_tool("agents_search", {"query": "smoke"})
        data = json.loads(result.content[0].text)
        assert len(data) >= 1
        found = {a["id"] for a in data}
        assert "smoke-test-agent" in found

    @pytest.mark.asyncio
    async def test_agents_search_no_results(self, mcp_server) -> None:
        async with Client(mcp_server) as client:
            result = await client.call_tool(
                "agents_search", {"query": "nonexistent_term_xyz"}
            )
        data = json.loads(result.content[0].text)
        assert data == []

    @pytest.mark.asyncio
    async def test_agents_inspect(self, mcp_server) -> None:
        async with Client(mcp_server) as client:
            result = await client.call_tool(
                "agents_inspect", {"agent_id": "smoke-test-agent"}
            )
        data = json.loads(result.content[0].text)
        assert data["id"] == "smoke-test-agent"
        assert data["name"] == "Smoke Test Agent"
        assert data["description"] == "Primary agent for AGW-007 smoke tests"

    @pytest.mark.asyncio
    async def test_agents_inspect_not_found(self, mcp_server) -> None:
        async with Client(mcp_server) as client:
            result = await client.call_tool(
                "agents_inspect", {"agent_id": "does-not-exist"}
            )
        data = json.loads(result.content[0].text)
        assert "error" in data
