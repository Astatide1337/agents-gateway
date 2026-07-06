"""Tests for HTTP management and task endpoints."""

import json
import tempfile
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.metrics import MetricsRegistry
from agents_gateway.server import create_app


@pytest.fixture
def app_client(tmp_path):
    agents_dir = tmp_path / "agents"
    agents_dir.mkdir()

    (agents_dir / "test-agent").mkdir()
    (agents_dir / "test-agent" / "agent.yaml").write_text(
        "id: test-agent\nname: Test Agent\ndescription: A test agent\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
    )

    config = GatewayConfig(
        agents={"dir": str(agents_dir)},
        storage={
            "sqlite_path": str(tmp_path / "test.db"),
            "artifacts_dir": str(tmp_path / "artifacts"),
        },
        observability={"log_level": "WARNING", "log_format": "json", "metrics_enabled": True},
    )
    fresh_registry = MetricsRegistry()
    app = create_app(config, reg=fresh_registry)
    with TestClient(app) as client:
        yield client


class TestManagementEndpoints:
    def test_health(self, app_client):
        resp = app_client.get("/health")
        assert resp.status_code == 200
        assert resp.json()["status"] == "ok"

    def test_ready(self, app_client):
        resp = app_client.get("/ready")
        assert resp.status_code == 200
        data = resp.json()
        assert data["ready"] is True

    def test_version(self, app_client):
        resp = app_client.get("/version")
        assert resp.status_code == 200
        assert resp.json()["version"] == "0.1.0"

    def test_inventory(self, app_client):
        resp = app_client.get("/inventory")
        assert resp.status_code == 200
        data = resp.json()
        assert "agent_count" in data
        assert "auth_mode" in data
        assert "tools" in data

    def test_metrics(self, app_client):
        resp = app_client.get("/metrics")
        assert resp.status_code == 200
        assert "agents_gateway_up" in resp.text

    def test_docs(self, app_client):
        resp = app_client.get("/docs")
        assert resp.status_code == 200

    def test_mcp_endpoint_is_mounted(self, app_client):
        resp = app_client.get("/mcp")
        assert resp.status_code != 404


class TestAgentEndpoints:
    def test_list_agents(self, app_client):
        resp = app_client.get("/agents")
        assert resp.status_code == 200
        data = resp.json()
        assert len(data["agents"]) >= 1

    def test_get_agent(self, app_client):
        resp = app_client.get("/agents/test-agent")
        assert resp.status_code == 200
        assert resp.json()["id"] == "test-agent"

    def test_get_agent_not_found(self, app_client):
        resp = app_client.get("/agents/nonexistent")
        assert resp.status_code == 404

    def test_validate_agents(self, app_client):
        resp = app_client.post("/agents/validate")
        assert resp.status_code == 200


class TestTaskEndpoints:
    def test_create_task(self, app_client):
        resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": "test"})
        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "created"
        assert data["agent_id"] == "test-agent"

    def test_create_task_invalid_agent(self, app_client):
        resp = app_client.post("/tasks", json={"agent_id": "nonexistent", "input": ""})
        assert resp.status_code == 400

    def test_list_tasks(self, app_client):
        app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        resp = app_client.get("/tasks")
        assert resp.status_code == 200
        assert "tasks" in resp.json()

    def test_list_tasks_filters_by_status(self, app_client):
        app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        resp = app_client.get("/tasks?status=created")
        assert resp.status_code == 200
        assert all(t["status"] == "created" for t in resp.json()["tasks"])

    def test_list_tasks_rejects_invalid_status(self, app_client):
        resp = app_client.get("/tasks?status=not-a-status")
        assert resp.status_code == 400

    def test_get_task(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        resp = app_client.get(f"/tasks/{task_id}")
        assert resp.status_code == 200
        assert resp.json()["id"] == task_id

    def test_get_task_not_found(self, app_client):
        resp = app_client.get("/tasks/nonexistent")
        assert resp.status_code == 404

    def test_task_events(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        resp = app_client.get(f"/tasks/{task_id}/events")
        assert resp.status_code == 200
        events = resp.json()["events"]
        assert len(events) >= 1

    def test_task_artifacts(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        resp = app_client.get(f"/tasks/{task_id}/artifacts")
        assert resp.status_code == 200

    def test_cancel_task(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        resp = app_client.post(f"/tasks/{task_id}/cancel")
        assert resp.status_code == 200
        assert resp.json()["status"] == "cancelled"

    def test_cancel_completed_fails(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        resp = app_client.post(f"/tasks/{task_id}/cancel")
        assert resp.status_code == 409

    def test_run_task(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        resp = app_client.post(f"/tasks/{task_id}/run")
        assert resp.status_code == 200
        assert resp.json()["status"] == "completed"

    def test_run_task_unsupported_runtime(self, app_client, tmp_path):
        agents_dir = tmp_path / "agents"
        (agents_dir / "bad-runtime-agent").mkdir(parents=True, exist_ok=True)
        (agents_dir / "bad-runtime-agent" / "agent.yaml").write_text(
            "id: bad-runtime-agent\nname: Bad Runtime\ndescription: Bad\nversion: 0.1.0\nruntime:\n  type: some-unknown-runtime\n"
        )
        from agents_gateway.config import GatewayConfig
        from agents_gateway.metrics import MetricsRegistry
        fresh_config = GatewayConfig(
            agents={"dir": str(agents_dir)},
            storage={
                "sqlite_path": str(tmp_path / "test2.db"),
                "artifacts_dir": str(tmp_path / "artifacts2"),
            },
            observability={"log_level": "WARNING", "log_format": "json", "metrics_enabled": False},
        )
        fresh_registry = MetricsRegistry()
        new_app = create_app(fresh_config, reg=fresh_registry)
        with TestClient(new_app) as client:
            create_resp = client.post("/tasks", json={"agent_id": "bad-runtime-agent", "input": ""})
            assert create_resp.status_code == 200
            task_id = create_resp.json()["id"]
            run_resp = client.post(f"/tasks/{task_id}/run")
            assert run_resp.status_code == 400
            assert "Unsupported runtime type" in run_resp.json()["error"]

    def test_run_task_produces_artifacts(self, app_client):
        create_resp = app_client.post("/tasks", json={"agent_id": "test-agent", "input": ""})
        task_id = create_resp.json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        art_resp = app_client.get(f"/tasks/{task_id}/artifacts")
        arts = art_resp.json()["artifacts"]
        assert len(arts) >= 1


class TestRateLimiting:
    @pytest.fixture
    def rate_limited_client(self, tmp_path):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        (agents_dir / "test-agent").mkdir()
        (agents_dir / "test-agent" / "agent.yaml").write_text(
            "id: test-agent\nname: Test Agent\ndescription: A test agent\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        config = GatewayConfig(
            agents={"dir": str(agents_dir)},
            storage={
                "sqlite_path": str(tmp_path / "test-rate.db"),
                "artifacts_dir": str(tmp_path / "artifacts-rate"),
            },
            service={"rate_limiting": {"enabled": True, "requests_per_minute": 2}},
            observability={"log_level": "WARNING", "log_format": "json", "metrics_enabled": False},
        )
        fresh_registry = MetricsRegistry()
        app = create_app(config, reg=fresh_registry)
        with TestClient(app) as client:
            yield client

    def test_rate_limit_allows_first_requests(self, rate_limited_client):
        for _ in range(2):
            resp = rate_limited_client.get("/health")
            assert resp.status_code == 200

    def test_rate_limit_exceeded(self, rate_limited_client):
        for _ in range(2):
            rate_limited_client.get("/health")
        resp = rate_limited_client.get("/health")
        assert resp.status_code == 429
