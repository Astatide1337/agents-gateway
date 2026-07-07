"""Tests for HTTP management and task endpoints.

Covers:
  * dev-none auth mode (current default) — all endpoints return 200/201.
  * internal-only auth mode — protected routes 401 without X-Auth-Internal-Token.
  * cloudflare-access auth mode — protected routes 401 without Cf-Access-Jwt-Assertion.
  * The /run endpoint enqueues and returns 202; the background worker
    moves the task through queued -> running -> completed/failed.
  * State transition protection: cancel-on-completed returns 409.
"""

from __future__ import annotations

import json
import time
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.metrics import MetricsRegistry
from agents_gateway.server import create_asgi_app


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _make_agents_dir(tmp_path: Path) -> Path:
    agents_dir = tmp_path / "agents"
    agents_dir.mkdir()
    (agents_dir / "test-agent").mkdir()
    (agents_dir / "test-agent" / "agent.yaml").write_text(
        "id: test-agent\nname: Test Agent\ndescription: A test agent\n"
        "version: 0.1.0\nruntime:\n  type: local-stub\n"
    )
    return agents_dir


def _make_runtime_config(tmp_path: Path) -> GatewayConfig:
    return GatewayConfig(
        agents={"dir": str(_make_agents_dir(tmp_path))},
        storage={
            "sqlite_path": str(tmp_path / "test.db"),
            "artifacts_dir": str(tmp_path / "artifacts"),
        },
        observability={
            "log_level": "WARNING",
            "log_format": "json",
            "metrics_enabled": True,
        },
    )


@pytest.fixture
def app_client(tmp_path):
    config = _make_runtime_config(tmp_path)
    fresh_registry = MetricsRegistry()
    app = create_asgi_app(config, reg=fresh_registry)
    with TestClient(app) as client:
        yield client


@pytest.fixture
def internal_only_client(tmp_path):
    config = _make_runtime_config(tmp_path)
    config.auth = config.auth.model_copy(update={
        "mode": "internal-only",
        "internal_secret": "s3cr3t",
    })
    fresh_registry = MetricsRegistry()
    app = create_asgi_app(config, reg=fresh_registry)
    with TestClient(app) as client:
        yield client, config


@pytest.fixture
def cf_access_client(tmp_path):
    config = _make_runtime_config(tmp_path)
    config.auth = config.auth.model_copy(update={
        "mode": "cloudflare-access",
        "cloudflare_team_domain": "test-team.cloudflareaccess.com",
        "cloudflare_aud": "test-aud",
    })
    fresh_registry = MetricsRegistry()
    app = create_asgi_app(config, reg=fresh_registry)
    with TestClient(app) as client:
        yield client, config


# ---------------------------------------------------------------------------
# Public endpoints (no auth in any mode)
# ---------------------------------------------------------------------------


class TestPublicEndpoints:
    def test_health_no_auth(self, app_client):
        assert app_client.get("/health").status_code == 200

    def test_ready_no_auth(self, app_client):
        assert app_client.get("/ready").status_code == 200

    def test_version_no_auth(self, app_client):
        assert app_client.get("/version").status_code == 200


# ---------------------------------------------------------------------------
# Protected endpoints in dev-none mode (all open)
# ---------------------------------------------------------------------------


class TestDevNoneMode:
    def test_agents_open_in_dev_none(self, app_client):
        assert app_client.get("/agents").status_code == 200

    def test_inventory_open_in_dev_none(self, app_client):
        assert app_client.get("/inventory").status_code == 200

    def test_metrics_open_in_dev_none(self, app_client):
        assert app_client.get("/metrics").status_code == 200

    def test_create_task_open_in_dev_none(self, app_client):
        resp = app_client.post("/tasks",
                              json={"agent_id": "test-agent", "input": ""})
        assert resp.status_code == 201


# ---------------------------------------------------------------------------
# 401 in internal-only mode
# ---------------------------------------------------------------------------


class TestInternalOnlyAuth:
    def test_health_open_with_auth(self, internal_only_client):
        client, _ = internal_only_client
        assert client.get("/health").status_code == 200

    def test_agents_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        assert client.get("/agents").status_code == 401

    def test_inventory_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        assert client.get("/inventory").status_code == 401

    def test_metrics_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        assert client.get("/metrics").status_code == 401

    def test_create_task_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.post("/tasks",
                           json={"agent_id": "test-agent", "input": ""})
        assert resp.status_code == 401

    def test_run_task_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        # Need to first create a task; but creating is also 401.
        # We assert that ANY POST to /tasks/{id}/run returns 401 without auth.
        resp = client.post("/tasks/any-id/run")
        assert resp.status_code == 401  # middleware fires before task lookup

    def test_agents_200_with_correct_token(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.get("/agents",
                          headers={"X-Auth-Internal-Token": "s3cr3t"})
        assert resp.status_code == 200, resp.text

    def test_agents_401_with_wrong_token(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.get("/agents",
                          headers={"X-Auth-Internal-Token": "wrong"})
        assert resp.status_code == 401

    def test_random_bearer_token_does_not_bypass(self, internal_only_client):
        """The old code accepted ANY bearer token; the new auth path must reject."""
        client, _ = internal_only_client
        resp = client.get("/agents",
                           headers={"Authorization": "Bearer made-up-token"})
        assert resp.status_code == 401

    def test_mcp_401_without_token(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.post("/mcp",
                           json={"jsonrpc": "2.0", "method": "initialize",
                                 "id": "1", "params": {}})
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# 401 in cloudflare-access mode
# ---------------------------------------------------------------------------


class TestCloudflareAccessAuth:
    def test_health_open_with_auth(self, cf_access_client):
        client, _ = cf_access_client
        assert client.get("/health").status_code == 200

    def test_agents_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        assert client.get("/agents").status_code == 401

    def test_inventory_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        assert client.get("/inventory").status_code == 401

    def test_metrics_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        assert client.get("/metrics").status_code == 401

    def test_create_task_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        resp = client.post("/tasks",
                           json={"agent_id": "test-agent", "input": ""})
        assert resp.status_code == 401

    def test_run_task_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        resp = client.post("/tasks/any-id/run")
        assert resp.status_code == 401

    def test_mcp_post_401_without_jwt(self, cf_access_client):
        client, _ = cf_access_client
        resp = client.post("/mcp",
                           json={"jsonrpc": "2.0", "method": "initialize",
                                 "id": "1", "params": {}})
        assert resp.status_code == 401

    def test_agents_401_with_unsigned_jwt(self, cf_access_client):
        client, _ = cf_access_client
        resp = client.get("/agents",
                          headers={"Cf-Access-Jwt-Assertion": "not.a.jwt"})
        assert resp.status_code == 401


# ---------------------------------------------------------------------------
# Task lifecycle (dev-none mode)
# ---------------------------------------------------------------------------


class TestTaskLifecycle:
    def test_create_task(self, app_client):
        resp = app_client.post("/tasks",
                              json={"agent_id": "test-agent", "input": "test"})
        assert resp.status_code == 201
        data = resp.json()
        assert data["status"] == "created"
        assert data["agent_id"] == "test-agent"

    def test_create_task_invalid_agent(self, app_client):
        resp = app_client.post("/tasks",
                              json={"agent_id": "nonexistent", "input": ""})
        assert resp.status_code == 400

    def test_list_tasks(self, app_client):
        app_client.post("/tasks",
                       json={"agent_id": "test-agent", "input": ""})
        resp = app_client.get("/tasks")
        assert resp.status_code == 200
        assert "tasks" in resp.json()

    def test_list_tasks_filters_by_status(self, app_client):
        app_client.post("/tasks",
                       json={"agent_id": "test-agent", "input": ""})
        resp = app_client.get("/tasks?status=created")
        assert resp.status_code == 200
        assert all(t["status"] == "created" for t in resp.json()["tasks"])

    def test_list_tasks_rejects_invalid_status(self, app_client):
        assert app_client.get("/tasks?status=not-a-status").status_code == 400

    def test_get_task(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        resp = app_client.get(f"/tasks/{task_id}")
        assert resp.status_code == 200
        assert resp.json()["id"] == task_id

    def test_get_task_not_found(self, app_client):
        assert app_client.get("/tasks/nonexistent").status_code == 404

    def test_task_events_initial(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        resp = app_client.get(f"/tasks/{task_id}/events")
        assert resp.status_code == 200
        events = resp.json()["events"]
        assert len(events) >= 1
        assert events[0]["event"] == "task_created"

    def test_task_no_artifacts_initially(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        resp = app_client.get(f"/tasks/{task_id}/artifacts")
        assert resp.status_code == 200
        assert resp.json()["artifacts"] == []


class TestRunEndpoint:
    def test_run_returns_202(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        resp = app_client.post(f"/tasks/{task_id}/run")
        assert resp.status_code == 202
        assert resp.json()["status"] == "queued"

    def test_run_eventually_completes(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        # Poll for up to 5 seconds.
        deadline = time.time() + 5.0
        final_status = "queued"
        while time.time() < deadline:
            resp = app_client.get(f"/tasks/{task_id}")
            assert resp.status_code == 200
            final_status = resp.json()["status"]
            if final_status in ("completed", "failed"):
                break
            time.sleep(0.05)
        assert final_status == "completed", final_status

    def test_run_produces_artifact(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        deadline = time.time() + 5.0
        while time.time() < deadline:
            arts = app_client.get(f"/tasks/{task_id}/artifacts").json()["artifacts"]
            if arts:
                break
            time.sleep(0.05)
        assert arts, "Expected at least one artifact after run completes"

    def test_run_task_unsupported_runtime_returns_202_then_worker_fails(
        self, tmp_path
    ):
        agents_dir = tmp_path / "bad-agents"
        agents_dir.mkdir()
        (agents_dir / "bad-agent").mkdir()
        (agents_dir / "bad-agent" / "agent.yaml").write_text(
            "id: bad-agent\nname: Bad\ndescription: bad\nversion: 0.1.0\n"
            "runtime:\n  type: some-unknown-runtime\n"
        )
        config = GatewayConfig(
            agents={"dir": str(agents_dir)},
            storage={
                "sqlite_path": str(tmp_path / "bad-runtime.db"),
                "artifacts_dir": str(tmp_path / "artifacts-bad"),
            },
            observability={
                "log_level": "WARNING", "log_format": "json", "metrics_enabled": False,
            },
        )
        app = create_asgi_app(config, reg=MetricsRegistry())
        with TestClient(app) as client:
            task_id = client.post("/tasks",
                                  json={"agent_id": "bad-agent", "input": ""}).json()["id"]
            run_resp = client.post(f"/tasks/{task_id}/run")
            # /run always returns 202 because the worker handles runtime errors.
            assert run_resp.status_code == 202
            # Worker eventually marks task failed.
            deadline = time.time() + 5.0
            final = "queued"
            while time.time() < deadline:
                final = client.get(f"/tasks/{task_id}").json()["status"]
                if final in ("failed", "completed"):
                    break
                time.sleep(0.05)
            assert final == "failed", final

    def test_cancel_completed_returns_409(self, app_client):
        task_id = app_client.post("/tasks",
                                  json={"agent_id": "test-agent", "input": ""}).json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        deadline = time.time() + 5.0
        while time.time() < deadline:
            if app_client.get(f"/tasks/{task_id}").json()["status"] == "completed":
                break
            time.sleep(0.05)
        # Now cancel: completed -> cancelled is not a valid transition.
        cancel_resp = app_client.post(f"/tasks/{task_id}/cancel")
        assert cancel_resp.status_code == 409

    def test_run_already_terminal_returns_409(self, app_client):
        task_id = app_client.post("/tasks",
                                   json={"agent_id": "test-agent", "input": ""}).json()["id"]
        app_client.post(f"/tasks/{task_id}/run")
        # wait for completion
        deadline = time.time() + 5.0
        while time.time() < deadline:
            if app_client.get(f"/tasks/{task_id}").json()["status"] == "completed":
                break
            time.sleep(0.05)
        # Already terminal
        resp = app_client.post(f"/tasks/{task_id}/run")
        assert resp.status_code == 409


class TestMcpEndpointAuth:
    """The /mcp endpoint (MCP protocol calls) must be auth-protected
    in internal-only and cloudflare-access modes, not just the HTTP
    custom routes (/agents, /tasks, etc.)."""

    def test_mcp_post_401_internal_only(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.post("/mcp",
                           json={"jsonrpc": "2.0", "method": "initialize",
                                 "id": "1", "params": {}})
        assert resp.status_code == 401, resp.text

    def test_mcp_post_401_cf_access(self, cf_access_client):
        client, _ = cf_access_client
        resp = client.post("/mcp",
                           json={"jsonrpc": "2.0", "method": "initialize",
                                 "id": "1", "params": {}})
        assert resp.status_code == 401, resp.text

    def test_mcp_post_200_internal_only_with_token(self, internal_only_client):
        client, _ = internal_only_client
        resp = client.post("/mcp",
                           json={"jsonrpc": "2.0", "method": "initialize",
                                 "id": "1", "params": {}},
                           headers={"X-Auth-Internal-Token": "s3cr3t"})
        # We expect a 200 (or 406 etc if MCP protocol specifics mismatch)
        # but NOT a 401, because the auth token is correct.
        assert resp.status_code != 401, (
            f"Expected non-401 with valid token, got {resp.status_code} {resp.text}"
        )


class TestRateLimiting:
    @pytest.fixture
    def rate_limited_client(self, tmp_path):
        from agents_gateway.config import RateLimitConfig
        config = _make_runtime_config(tmp_path)
        config.service = config.service.model_copy(update={
            "rate_limiting": RateLimitConfig(enabled=True, requests_per_minute=2),
        })
        app = create_asgi_app(config, reg=MetricsRegistry())
        with TestClient(app) as client:
            yield client

    def test_rate_limit_allows_first_requests(self, rate_limited_client):
        for _ in range(2):
            assert rate_limited_client.get("/health").status_code == 200

    def test_rate_limit_exceeded(self, rate_limited_client):
        for _ in range(2):
            rate_limited_client.get("/health")
        assert rate_limited_client.get("/health").status_code == 429
