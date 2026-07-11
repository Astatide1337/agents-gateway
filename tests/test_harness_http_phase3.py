"""Tests for new HTTP endpoints added in Phases 3, 4, 8:

  * ``GET /agent-runs/{id}`` — unified task + harness + events view
  * ``GET /harness-profiles/{name}/availability`` — structured
    availability report
  * ``POST /cleanup/dry-run`` — retention preview
  * ``POST /cleanup/run`` — retention execution (honours dry_run flag)
  * ``POST /cleanup/run?force=true`` — override dry_run
  * Session send / cancel event emissions (``composer.session_send``,
    ``composer.interaction.cancelled``)
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest
from starlette.testclient import TestClient

from agents_gateway.config import GatewayConfig
from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionStatus,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
    Worktree,
    WorktreeStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import FakeTmuxDriver
from agents_gateway.storage import TaskStorage


# ---------------------------------------------------------------------------
# Fixtures + helpers (mirror of test_harness_http_api.py)
# ---------------------------------------------------------------------------


def _git(cwd: str, *args: str) -> subprocess.CompletedProcess:
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@local",
        "GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@local",
    }
    return subprocess.run(
        ["git", "-C", cwd, *args],
        capture_output=True, text=True, timeout=20, env=env,
    )


def _make_scratch_repo(tmp_path: Path) -> str:
    repo = tmp_path / "scratch-repo"
    repo.mkdir()
    proc = _git(str(repo), "init", "-b", "master")
    if proc.returncode != 0:
        _git(str(repo), "init")
        _git(str(repo), "symbolic-ref", "HEAD", "refs/heads/master")
    (repo / "README.md").write_text("# Scratch\n")
    _git(str(repo), "add", "README.md")
    _git(str(repo), "commit", "-m", "Initial")
    return str(repo)


def _config_for(tmp_path: Path) -> GatewayConfig:
    return GatewayConfig(
        auth={"mode": "dev-none"},
        storage={"sqlite_path": str(tmp_path / "agw.db"),
                 "artifacts_dir": str(tmp_path / "artifacts")},
        service={"rate_limiting": {"enabled": False,
                                    "requests_per_minute": 999}},
        harness={"workspace_root": str(tmp_path / "repos"),
                 "worktree_root": str(tmp_path / "worktrees"),
                 "artifacts_root": str(tmp_path / "artifacts"),
                 "use_fake_tmux": True,
                 # Configurable retention values for cleanup tests.
                 "artifact_retention_days": 14,
                 "worktree_retention_days": 7,
                 "max_artifact_bytes": 1_073_741_824,
                 "cleanup_dry_run": True},
        agents={"dir": str(tmp_path / "agents")},
        integrations={
            "skills_gateway": {"enabled": False},
            "mcp_gateway": {"enabled": False},
        },
    )


@pytest.fixture
def server(tmp_path):
    _make_scratch_repo(tmp_path)
    cfg = _config_for(tmp_path)
    from agents_gateway.metrics import MetricsRegistry
    from agents_gateway.server import create_asgi_app
    app = create_asgi_app(cfg, reg=MetricsRegistry())
    with TestClient(app) as client:
        setattr(client, "_test_cfg", cfg)
        setattr(client, "_harness_storage",
                HarnessStorage(cfg.storage.sqlite_path))
        setattr(client, "_task_storage",
                TaskStorage(cfg.storage.sqlite_path))
        yield client


# ---------------------------------------------------------------------------
# /agent-runs/{id} unified view
# ---------------------------------------------------------------------------


class TestAgentRunsUnified:
    def test_get_unknown_returns_404(self, server):
        resp = server.get("/agent-runs/nonexistent-task")
        assert resp.status_code == 404

    def test_get_known_task_includes_harness_block_and_events(
            self, server, tmp_path):
        ts: TaskStorage = server._task_storage
        hs: HarnessStorage = server._harness_storage
        # Create + queue + run a fake harness task (without actually dispatching).
        spec = {
            "title": "agent_run_view_test",
            "brief": "test",
            "execution": {"harness_profile": "fake-test",
                          "mode": "harness_session"},
            "goal": {"strategy": "auto", "text": "/goal nothing"},
            "verification": {"required": False, "commands": []},
        }
        task = ts.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session",
                      "objective_id": "obj_1"},
        )
        ts.update_task_status(task.id, "queued")
        ts.update_task_status(task.id, "running")
        # Create a harness session linked to this task.
        s = HarnessSession.new(
            agent_run_id=task.id, task_id=task.id,
            harness_profile="fake-test", harness="fake",
            tmux_session="agw_" + str(task.id)[:8],
            working_directory=str(tmp_path),
        )
        s.status = HarnessSessionStatus.running.value
        hs.save_session(s)
        # Create a worktree linked to the agent_run_id.
        wt = Worktree(
            id="wt_test", task_id=task.id, agent_run_id=task.id,
            repo_workspace_id="repo_ws", branch="agent/test",
            base_branch="master", path="/tmp/test-wt",
            status=WorktreeStatus.active.value,
            created_at="2024-01-01T00:00:00Z", deleted_at=None,
            metadata={},
        )
        hs.save_worktree(wt)

        resp = server.get(f"/agent-runs/{task.id}")
        assert resp.status_code == 200, resp.text
        body = resp.json()
        assert body["id"] == task.id
        # Harness block exists.
        assert "harness" in body
        # As this is a POST /tasks created harness task, harness block has
        # populated fields (session_id, status, harness_profile, worktree_id).
        assert body["harness"]["harness_profile"] == "fake-test"
        assert body["harness"]["session_id"] == s.id
        assert body["harness"]["worktree_id"] == "wt_test"
        # Events list included.
        assert "events" in body
        event_types = [e.get("event") for e in body["events"]]
        assert "task_created" in event_types
        assert "task_queued" in event_types


# ---------------------------------------------------------------------------
# /harness-profiles/{name}/availability
# ---------------------------------------------------------------------------


class TestAvailabilityEndpoint:
    def test_unknown_profile_returns_structured_response(self, server):
        resp = server.get("/harness-profiles/nonexistent/availability")
        # Spec: never raise — return runnable=false.
        assert resp.status_code in (200, 404)
        body = resp.json()
        assert body["runnable"] is False

    def test_known_profile_returns_availability_report(self, server):
        resp = server.get("/harness-profiles/fake-test/availability")
        assert resp.status_code == 200, resp.text
        body = resp.json()
        # fake-test uses python3, which should be present.
        assert body["profile"] == "fake-test"
        assert body["configured"] is True
        assert "binary_present" in body
        assert "credentials_present" in body
        assert "runnable" in body

    def test_known_missing_binary_profile(self, server):
        """Profile that's configured but binary is missing."""
        # opencode-deepseek opencode binary may or may not be present.
        # Just verify the endpoint returns a structured response.
        resp = server.get("/harness-profiles/opencode-deepseek/availability")
        assert resp.status_code == 200, resp.text
        body = resp.json()
        assert body["profile"] == "opencode-deepseek"
        assert body["configured"] is True
        # Don't assert specifics — depends on the host. Just check structure.
        assert isinstance(body["binary_present"], bool)
        assert isinstance(body["credentials_present"], (bool, type(None)))
        assert isinstance(body["runnable"], bool)


# ---------------------------------------------------------------------------
# Cleanup endpoints
# ---------------------------------------------------------------------------


class TestCleanupEndpoints:
    def test_dry_run_returns_structured_report(self, server):
        resp = server.post("/cleanup/dry-run")
        assert resp.status_code == 200, resp.text
        body = resp.json()
        # All expected keys present.
        expected = {"dry_run", "deleted_artifacts", "deleted_worktrees",
                    "skipped_active_artifacts",
                    "skipped_active_worktrees",
                    "bytes_freed", "total_artifact_bytes_before"}
        assert expected.issubset(set(body.keys()))
        # Dry-run flag is true.
        assert body["dry_run"] is True

    def test_run_without_force_respects_dry_run_default(self, server):
        """Default config has cleanup_dry_run=true, so /cleanup/run
        without ?force=true acts as a dry-run."""
        resp = server.post("/cleanup/run")
        assert resp.status_code == 200
        body = resp.json()
        assert body["dry_run"] is True

    def test_run_with_force_overrides_dry_run(self, server):
        """?force=true causes /cleanup/run to actually execute cleanup."""
        resp = server.post("/cleanup/run?force=true")
        assert resp.status_code == 200
        body = resp.json()
        # dry_run=False because ?force=true overrode the dry-run default.
        assert body["dry_run"] is False, (
            f"force=true should override dry_run default; "
            f"got: {body['dry_run']}"
        )

    def test_cleanup_dry_run_empty_storage_no_deletions(self, server):
        resp = server.post("/cleanup/dry-run")
        body = resp.json()
        assert body["deleted_artifacts"] == []
        assert body["deleted_worktrees"] == []
        assert body["bytes_freed"] == 0


# ---------------------------------------------------------------------------
# Session send + interaction cancel event emissions
# ---------------------------------------------------------------------------


@pytest.fixture
def session_in_db(server, tmp_path):
    """Insert one harness session into the underlying storage."""
    hs: HarnessStorage = server._harness_storage
    ts: TaskStorage = server._task_storage
    # Create a task first (so events can be attached to it).
    spec = {"title": "events-test-session", "brief": "test"}
    task = ts.create_harness_task(
        agent_id="harness_session", task_spec=spec,
        metadata={"runtime_type": "harness_session"},
    )
    ts.update_task_status(task.id, "queued")
    ts.update_task_status(task.id, "running")
    s = HarnessSession.new(
        agent_run_id=task.id, task_id=task.id,
        harness_profile="fake-test", harness="fake",
        tmux_session="agw_test_events",
        working_directory=str(tmp_path),
    )
    s.status = HarnessSessionStatus.running.value
    hs.save_session(s)
    return {"session": s, "task_id": task.id}


class TestEventEmissions:
    def test_session_send_emits_composer_session_send_event(
            self, server, session_in_db):
        """POST /sessions/{id}/send emits a ``composer.session_send``
        task event."""
        s_id = session_in_db["session_id" if "session_id" in session_in_db
                              else "session"].id if "session" in session_in_db else None
        s = session_in_db["session"]
        task_id = session_in_db["task_id"]
        resp = server.post(f"/sessions/{s.id}/send",
                            json={"text": "continue working",
                                   "submit": True})
        assert resp.status_code in (200, 202), resp.text
        ts: TaskStorage = server._task_storage
        events = ts.list_events(task_id)
        event_names = [e.event for e in events]
        assert "composer.session_send" in event_names, (
            f"Expected composer.session_send event, got: {event_names}"
        )

    def test_interaction_cancel_emits_composer_interaction_cancelled_event(
            self, server, session_in_db):
        """POST /interactions/{id}/cancel emits the
        ``composer.interaction.cancelled`` event."""
        hs: HarnessStorage = server._harness_storage
        ts: TaskStorage = server._task_storage
        s = session_in_db["session"]
        task_id = session_in_db["task_id"]
        # Create a pending interaction.
        inter = ComposerInteraction.new(
            agent_run_id=task_id, task_id=task_id, session_id=s.id,
            type_=ComposerInteractionType.needs_reply.value,
            prompt_excerpt="I need clarification",
            full_context_ref="",
        )
        hs.save_interaction(inter)
        # Cancel the interaction.
        resp = server.post(f"/interactions/{inter.id}/cancel")
        assert resp.status_code in (200, 202), resp.text
        # Verify the cancel event was emitted.
        events = ts.list_events(task_id)
        event_names = [e.event for e in events]
        assert "composer.interaction.cancelled" in event_names, (
            f"Expected composer.interaction.cancelled event, got: {event_names}"
        )
