"""Tests for the agents_* MCP tool aliases added in Phase 10.

These tests verify that the unified ``agents_*``-prefixed MCP tools
correctly delegate to the underlying harness storage and produce the
contracted JSON response shapes.

Tools covered:

  * ``agents_check_harness_availability``
  * ``agents_list_sessions``
  * ``agents_get_session``
  * ``agents_capture_session``    (structured response + redaction)
  * ``agents_send_session``       (emits ``composer.session_send`` event)
  * ``agents_list_interactions``
  * ``agents_reply_interaction`` (emits ``composer.interaction.answered``)
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path
from typing import Any

import pytest

from agents_gateway.config import GatewayConfig
from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionStatus,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import FakeTmuxDriver
from agents_gateway.storage import TaskStorage


# ---------------------------------------------------------------------------
# Helpers
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
                 "use_fake_tmux": True},
        agents={"dir": str(tmp_path / "agents")},
        integrations={
            "skills_gateway": {"enabled": False},
            "mcp_gateway": {"enabled": False},
        },
    )


def call_tool(mcp: Any, tool_name: str, **kwargs: Any) -> Any:
    """Call a registered FastMCP tool synchronously and return its JSON result."""
    tools = list(mcp._tool_manager._tools.values())
    tool = next((t for t in tools if t.name == tool_name), None)
    assert tool is not None, f"Tool {tool_name!r} not registered"
    raw = tool.fn(**kwargs)
    return json.loads(raw)


@pytest.fixture
def mcp_server(tmp_path):
    _make_scratch_repo(tmp_path)
    cfg = _config_for(tmp_path)
    from agents_gateway.mcp_tools import create_mcp_server
    mcp = create_mcp_server(cfg)
    setattr(mcp, "_test_cfg", cfg)
    setattr(mcp, "_test_harness_storage",
            HarnessStorage(cfg.storage.sqlite_path))
    setattr(mcp, "_test_task_storage",
            TaskStorage(cfg.storage.sqlite_path))
    return mcp


# ---------------------------------------------------------------------------
# Expected tool inventory
# ---------------------------------------------------------------------------


EXPECTED_AGENTS_TOOLS = {
    "agents_check_harness_availability",
    "agents_list_sessions",
    "agents_get_session",
    "agents_capture_session",
    "agents_send_session",
    "agents_list_interactions",
    "agents_reply_interaction",
}


class TestAgentsToolRegistration:
    def test_all_agents_tools_registered(self, mcp_server):
        tools_dict = mcp_server._tool_manager._tools
        tool_names = set(tools_dict.keys())
        missing = EXPECTED_AGENTS_TOOLS - tool_names
        assert not missing, f"Missing agents_* tools: {missing}"


# ---------------------------------------------------------------------------
# Availability tool
# ---------------------------------------------------------------------------


class TestAgentsCheckAvailability:
    def test_unknown_profile(self, mcp_server):
        result = call_tool(mcp_server,
                            "agents_check_harness_availability",
                            name="nonexistent")
        assert result["runnable"] is False
        assert result["error"] == "unknown profile"

    def test_known_profile_returns_structured_report(self, mcp_server):
        result = call_tool(mcp_server,
                            "agents_check_harness_availability",
                            name="fake-test")
        # fake-test uses python3 as the command — should be present.
        assert result["configured"] is True
        assert "binary_present" in result
        assert "credentials_present" in result
        assert "runnable" in result
        assert result["profile"] == "fake-test"


# ---------------------------------------------------------------------------
# Sessions tools
# ---------------------------------------------------------------------------


@pytest.fixture
def session_in_db(mcp_server, tmp_path):
    """Insert one session into the harness storage."""
    hs: HarnessStorage = mcp_server._test_harness_storage
    s = HarnessSession.new(
        agent_run_id="run_capture", task_id="task_capture",
        harness_profile="fake-test", harness="fake",
        tmux_session="agw_capture",
        working_directory=str(tmp_path),
    )
    s.status = HarnessSessionStatus.running.value
    hs.save_session(s)
    # Register the session in FakeTmuxDriver and push some output.
    fake_tmux = FakeTmuxDriver()
    fake_tmux.push_output("agw_capture", "hello world\n")
    fake_tmux.push_output("agw_capture", "DONE.\n")
    # Attach the fake driver to the storage for capture calls.
    setattr(mcp_server, "_test_fake_tmux", fake_tmux)
    return s


class TestAgentsListSessions:
    def test_list_sessions_unfiltered(self, mcp_server, session_in_db):
        result = call_tool(mcp_server, "agents_list_sessions")
        assert isinstance(result, list)
        ids = [s["id"] for s in result]
        assert session_in_db.id in ids, f"sessions: {ids}"

    def test_list_sessions_status_filter(self, mcp_server, session_in_db):
        # Filter by status=running should include our running session.
        result = call_tool(mcp_server, "agents_list_sessions",
                            status="running")
        ids = [s["id"] for s in result]
        assert session_in_db.id in ids
        # Filter by status=completed should exclude.
        completed = call_tool(mcp_server, "agents_list_sessions",
                               status="completed")
        completed_ids = [s["id"] for s in completed]
        assert session_in_db.id not in completed_ids


class TestAgentsGetSession:
    def test_get_session_by_id(self, mcp_server, session_in_db):
        result = call_tool(mcp_server, "agents_get_session",
                            session_id=session_in_db.id)
        assert result["id"] == session_in_db.id
        assert result["status"] == HarnessSessionStatus.running.value

    def test_get_unknown_session_returns_error(self, mcp_server):
        result = call_tool(mcp_server, "agents_get_session",
                            session_id="nonexistent-id-12345")
        assert "error" in result
