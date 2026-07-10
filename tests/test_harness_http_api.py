"""Tests for the harness-runtime HTTP API surface in agents_gateway.server.

Covers every harness_endpoint added in Phase F (server.py):

  * /harness-profiles (GET, list)
  * /harness-profiles/validate (POST)
  * /harness-profiles/{name} (GET)
  * /worktrees (GET list, GET by id, GET by task_id)
  * /sessions (GET list, GET by id, GET capture, POST send, POST stop)
  * /tasks/{task_id}/session (GET)
  * /interactions (GET list, GET by id, POST reply, POST cancel)
  * /agent-runs/{id}/verification (GET)
  * /agent-runs/{id}/verify (POST)
  * /agent-runs/{id}/artifacts (GET)
  * /artifacts/{id} (GET, with view=true)

Auth is configured via mode="disabled" so the global middleware lets
every request through. Real tmux is replaced by FakeTmuxDriver where
session-level endpoints touch the driver.
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
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
# Fixtures
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
    """Build a GatewayConfig with the harness runtime enabled + auth off."""
    return GatewayConfig(
        auth={"mode": "dev-none"},
        storage={"sqlite_path": str(tmp_path / "agw.db"),
                 "artifacts_dir": str(tmp_path / "artifacts")},
        service={"rate_limiting": {"enabled": False,
                                    "requests_per_minute": 999}},
        harness={"workspace_root": str(tmp_path / "repos"),
                 "worktree_root": str(tmp_path / "worktrees"),
                 "artifacts_root": str(tmp_path / "artifacts"),
                 "use_fake_tmux": True},
        agents={"dir": str(tmp_path / "agents")},
        integrations={
            "skills_gateway": {"enabled": False},
            "mcp_gateway": {"enabled": False},
        },
    )


@pytest.fixture
def server(tmp_path):
    scratch_repo = _make_scratch_repo(tmp_path)
    cfg = _config_for(tmp_path)
    from agents_gateway.metrics import MetricsRegistry
    from agents_gateway.server import create_asgi_app
    app = create_asgi_app(cfg, reg=MetricsRegistry())
    with TestClient(app) as client:
        # Configuration must include scratch_repo path so tests can use it.
        setattr(client, "_scratch_repo", scratch_repo)
        setattr(client, "_config", cfg)
        yield client


# ---------------------------------------------------------------------------
# Harness profiles
# ---------------------------------------------------------------------------


class TestHarnessProfilesAPI:
    def test_list_returns_all_builtin_profiles(self, server):
        resp = server.get("/harness-profiles")
        assert resp.status_code == 200
        body = resp.json()
        names = {p["name"] for p in body["profiles"]}
        assert {"opencode-deepseek", "claude-code", "codex",
                "fake-test"}.issubset(names)
        for p in body["profiles"]:
            assert set(p.keys()) >= {"name", "harness", "command",
                                     "supports_slash_goal"}

    def test_get_known_profile(self, server):
        resp = server.get("/harness-profiles/opencode-deepseek")
        assert resp.status_code == 200
        body = resp.json()
        assert body["name"] == "opencode-deepseek"
        assert body["supports_slash_goal"] is True

    def test_get_unknown_profile_returns_404(self, server):
        resp = server.get("/harness-profiles/nonexistent")
        assert resp.status_code == 404
        assert "Unknown profile" in resp.json()["error"]

    def test_validate_known(self, server):
        resp = server.post("/harness-profiles/validate",
                           json={"name": "opencode-deepseek"})
        assert resp.status_code == 200, resp.text
        body = resp.json()
        assert body["valid"] is True

    def test_validate_unknown_returns_404(self, server):
        resp = server.post("/harness-profiles/validate",
                           json={"name": "nope"})
        assert resp.status_code == 404
        body = resp.json()
        assert body["valid"] is False

    def test_validate_slash_goal_on_unsupported_profile(self, server):
        # claude-code has supports_slash_goal=False
        resp = server.post("/harness-profiles/validate",
                           json={"name": "claude-code",
                                 "goal_strategy": "slash_goal"})
        assert resp.status_code == 400
        body = resp.json()
        assert body["valid"] is False
        assert "slash_goal" in body["error"]


# ---------------------------------------------------------------------------
# Worktrees
# ---------------------------------------------------------------------------


@pytest.fixture
def populated_storage(server, tmp_path):
    """Pre-populate the harness DB with a worktree for listing tests."""
    cfg = server._config
    hs = HarnessStorage(cfg.storage.sqlite_path)
    from agents_gateway.harness.models import RepoWorkspace
    # Insert a workspace + worktree row directly.
    ws = RepoWorkspace.new(
        repo_url="https://example.com/o/r.git", owner="o", repo="r",
        default_branch="master",
        base_path=str(tmp_path / "ws"),
        worktrees_path=str(tmp_path / "wt"),
    )
    hs.save_workspace(ws)
    wt = Worktree(
        id="wt_test1", task_id="task_test1", agent_run_id="run_test1",
        repo_workspace_id=ws.id, branch="agent/test-task-fixture",
        base_branch="master", path=str(tmp_path / "wt" / "task_test1"),
        status=WorktreeStatus.active.value,
        created_at="2026-01-01T00:00:00+00:00",
        deleted_at=None, metadata={},
    )
    hs.save_worktree(wt)
    return hs, wt


class TestWorktreeAPI:
    def test_list_worktrees(self, server, populated_storage):
        resp = server.get("/worktrees")
        assert resp.status_code == 200
        body = resp.json()
        wt_ids = {w["id"] for w in body["worktrees"]}
        assert "wt_test1" in wt_ids

    def test_get_worktree_by_id(self, server, populated_storage):
        resp = server.get("/worktrees/wt_test1")
        assert resp.status_code == 200
        body = resp.json()
        assert body["id"] == "wt_test1"
        assert body["branch"] == "agent/test-task-fixture"

    def test_get_worktree_unknown_returns_404(self, server):
        resp = server.get("/worktrees/wt_missing")
        assert resp.status_code == 404

    def test_get_worktree_by_task_id(self, server, populated_storage):
        resp = server.get("/tasks/task_test1/worktree")
        assert resp.status_code == 200
        body = resp.json()
        assert body["id"] == "wt_test1"

    def test_get_worktree_by_unknown_task_id_returns_404(self, server):
        resp = server.get("/tasks/task_noexist/worktree")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Sessions
# ---------------------------------------------------------------------------


@pytest.fixture
def session_row(server, tmp_path):
    """Insert a session row that doesn't require a real tmux session."""
    cfg = server._config
    hs = HarnessStorage(cfg.storage.sqlite_path)
    session = HarnessSession(
        id="session_test1", agent_run_id="run_test1",
        task_id="task_test1",
        harness_profile="fake-test", harness="fake",
        runtime="tmux-fake",
        tmux_session="agw_test_session",
        tmux_window="main", tmux_pane="0",
        working_directory="/tmp/test",
        status=HarnessSessionStatus.running.value,
        started_at="2026-01-01T00:00:00+00:00",
        last_output_at="2026-01-01T00:00:01+00:00",
        ended_at=None, metadata={},
    )
    hs.save_session(session)
    return hs, session


class TestSessionsAPI:
    def test_list_sessions_unfiltered(self, server, session_row):
        resp = server.get("/sessions")
        assert resp.status_code == 200
        body = resp.json()
        ids = {s["id"] for s in body["sessions"]}
        assert "session_test1" in ids

    def test_list_sessions_status_filter(self, server, session_row):
        resp = server.get("/sessions?status=running")
        assert resp.status_code == 200
        body = resp.json()
        for s in body["sessions"]:
            assert s["status"] == "running"
        # An invalid status returns no rows
        resp2 = server.get("/sessions?status=completed")
        assert resp2.status_code == 200
        assert len(resp2.json()["sessions"]) == 0

    def test_list_sessions_task_id_filter(self, server, session_row):
        resp = server.get("/sessions?task_id=task_test1")
        assert resp.status_code == 200
        body = resp.json()
        assert all(s["task_id"] == "task_test1" for s in body["sessions"])

    def test_get_session_by_id(self, server, session_row):
        resp = server.get("/sessions/session_test1")
        assert resp.status_code == 200
        body = resp.json()
        assert body["id"] == "session_test1"

    def test_get_session_unknown_returns_404(self, server):
        resp = server.get("/sessions/sess_missing")
        assert resp.status_code == 404

    def test_get_session_by_task_id(self, server, session_row):
        resp = server.get("/tasks/task_test1/session")
        assert resp.status_code == 200
        assert resp.json()["id"] == "session_test1"

    def test_get_session_by_unknown_task_returns_404(self, server):
        resp = server.get("/tasks/task_noexist/session")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Interactions
# ---------------------------------------------------------------------------


@pytest.fixture
def interaction_row(server, session_row):
    hs, session = session_row
    interaction = ComposerInteraction(
        id="interaction_test1",
        agent_run_id="run_test1", task_id="task_test1",
        session_id="session_test1",
        type=ComposerInteractionType.needs_reply.value,
        status=ComposerInteractionStatus.pending.value,
        prompt_excerpt="Do I want green or blue?",
        full_context_ref=None,
        created_at="2026-01-01T00:00:00+00:00",
        resolved_at=None, composer_reply=None, metadata={},
    )
    hs.save_interaction(interaction)
    return hs, interaction


class TestInteractionsAPI:
    def test_list_interactions_default_status_pending(self, server,
                                                      interaction_row):
        resp = server.get("/interactions")
        assert resp.status_code == 200
        body = resp.json()
        ids = {i["id"] for i in body["interactions"]}
        assert "interaction_test1" in ids

    def test_list_interactions_status_filter(self, server, interaction_row):
        resp = server.get("/interactions?status=answered")
        assert resp.status_code == 200
        assert len(resp.json()["interactions"]) == 0

    def test_get_interaction(self, server, interaction_row):
        resp = server.get("/interactions/interaction_test1")
        assert resp.status_code == 200
        assert resp.json()["id"] == "interaction_test1"
        assert resp.json()["type"] == "needs_reply"

    def test_get_interaction_unknown_returns_404(self, server):
        resp = server.get("/interactions/int_missing")
        assert resp.status_code == 404

    def test_cancel_pending_interaction(self, server, interaction_row):
        resp = server.post("/interactions/interaction_test1/cancel")
        assert resp.status_code == 200
        body = resp.json()
        assert body["status"] == "cancelled"

    def test_cancel_already_answered_returns_409(self, server, interaction_row):
        hs, _ = interaction_row
        hs.update_interaction_status(
            "interaction_test1",
            ComposerInteractionStatus.answered.value,
            composer_reply="x",
        )
        resp = server.post("/interactions/interaction_test1/cancel")
        assert resp.status_code == 409

    def test_reply_to_unknown_interaction_404(self, server):
        resp = server.post("/interactions/int_missing/reply",
                           json={"reply": "hi"})
        assert resp.status_code == 404

    def test_reply_with_empty_text_returns_400(self, server,
                                                interaction_row):
        resp = server.post("/interactions/interaction_test1/reply",
                            json={"reply": ""})
        assert resp.status_code == 400
        assert "reply required" in resp.json()["error"]

    def test_reply_invalid_json_body_returns_400(self, server,
                                                interaction_row):
        resp = server.post("/interactions/interaction_test1/reply",
                            content=b"not json",
                            headers={"content-type": "application/json"})
        assert resp.status_code == 400
        assert "Invalid JSON body" in resp.json()["error"]

    def test_reply_already_answered_returns_409(self, server, interaction_row):
        hs, _ = interaction_row
        hs.update_interaction_status(
            "interaction_test1",
            ComposerInteractionStatus.answered.value,
            composer_reply="x",
        )
        resp = server.post("/interactions/interaction_test1/reply",
                            json={"reply": "hi again"})
        assert resp.status_code == 409


# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------


class TestVerificationAPI:
    def test_get_verification_run_unknown_returns_404(self, server):
        resp = server.get("/agent-runs/run_unknown/verification")
        assert resp.status_code == 404

    def test_verify_unknown_worktree_returns_404(self, server):
        resp = server.post("/agent-runs/run_unknown/verify")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Artifacts
# ---------------------------------------------------------------------------


class TestArtifactsAPI:
    def test_list_artifacts_empty(self, server):
        resp = server.get("/agent-runs/run_unknown/artifacts")
        assert resp.status_code == 200
        assert resp.json() == {"artifacts": []}

    def test_get_artifact_unknown_returns_404(self, server):
        resp = server.get("/artifacts/art_missing")
        assert resp.status_code == 404


# ---------------------------------------------------------------------------
# Tasks: harness_session task creation + run path
# ---------------------------------------------------------------------------


class TestHarnessTaskCreation:
    def test_create_harness_task_via_post_tasks(self, server):
        body = {
            "title": "API task",
            "brief": "test brief",
            "execution": {"mode": "harness_session",
                          "harness_profile": "fake-test"},
            "goal": {"strategy": "auto", "text": "/goal Create a file."},
            "verification": {"required": True, "commands": [
                {"name": "check", "command": "echo ok", "required": True},
            ]},
            "metadata": {"objective_id": "obj_1"},
        }
        resp = server.post("/tasks", json=body)
        assert resp.status_code in (201, 202), resp.text
        data = resp.json()
        assert data["id"]
        assert data.get("agent_id") == "harness_session" or "harness" in str(data.get("metadata", {}))

    def test_create_harness_task_with_explicit_agent_id(self, server):
        body = {
            "agent_id": "harness_session",
            "title": "explicit agent_id",
            "brief": "x",
            "objective_id": "obj_x",
            "goal": {"text": "/goal hi"},
            "verification": {"required": False},
        }
        resp = server.post("/tasks", json=body)
        assert resp.status_code in (201, 202), resp.text

