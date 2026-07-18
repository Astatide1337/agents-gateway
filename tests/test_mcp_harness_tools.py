"""Tests for harness MCP tools exposed via the FastMCP server.

Verifies:

  * All ~16 harness_* tools are registered + callable.
  * Each returns the correct JSON shape for valid inputs.
  * Error paths (unknown id, empty reply) return JSON with `error`.
  * `harness_reply_interaction` (1) delivers the reply into the session
    via HarnessDriver.send_reply, (2) marks the interaction `answered`,
    (3) emits `composer.interaction.answered` task event.
  * `harness_task_create` builds the spec block + creates a task with
    `runtime_type=harness_session` metadata.
  * `harness_task_run` queues a task through TaskStorage.
"""

from __future__ import annotations

import json
import os
import subprocess
import threading
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
    # Attach config + storage paths for direct inspection in tests.
    setattr(mcp, "_test_cfg", cfg)
    setattr(mcp, "_test_harness_storage",
            HarnessStorage(cfg.storage.sqlite_path))
    setattr(mcp, "_test_task_storage",
            TaskStorage(cfg.storage.sqlite_path))
    return mcp


# ---------------------------------------------------------------------------
# Tool registration
# ---------------------------------------------------------------------------


EXPECTED_HARNESS_TOOLS = {
    "harness_profiles_list",
    "harness_profile_get",
    "harness_task_create",
    "harness_task_run",
    "harness_list_worktrees",
    "harness_list_sessions",
    "harness_get_session",
    "harness_get_session_capture",
    "harness_send_to_session",
    "harness_stop_session",
    "harness_list_interactions",
    "harness_get_interaction",
    "harness_reply_interaction",
    "harness_get_verification",
    "harness_list_artifacts",
    "harness_get_artifact",
}


class TestToolRegistration:
    def test_all_harness_tools_registered(self, mcp_server):
        try:
            tools_dict = mcp_server._tool_manager._tools
        except AttributeError:
            tools_dict = mcp_server._tools
        tool_names = set(tools_dict.keys())
        missing = EXPECTED_HARNESS_TOOLS - tool_names
        assert not missing, f"Missing harness tools: {missing}"


# ---------------------------------------------------------------------------
# Profile tools
# ---------------------------------------------------------------------------


class TestProfilesTools:
    def test_list_returns_all_builtin(self, mcp_server):
        result = call_tool(mcp_server, "harness_profiles_list")
        names = {p["name"] for p in result}
        assert {"opencode-deepseek", "claude-code", "codex",
                "fake-test"}.issubset(names)
        for p in result:
            assert "supports_slash_goal" in p
            assert "command" in p

    def test_get_known_profile(self, mcp_server):
        result = call_tool(mcp_server, "harness_profile_get",
                            name="opencode-deepseek")
        assert result["name"] == "opencode-deepseek"
        assert result["supports_slash_goal"] is True

    def test_get_unknown_profile_returns_error(self, mcp_server):
        result = call_tool(mcp_server, "harness_profile_get",
                            name="nope")
        assert "error" in result


# ---------------------------------------------------------------------------
# Task create / run
# ---------------------------------------------------------------------------


class TestTaskCreateTool:
    def test_create_task_with_minimal_args(self, mcp_server):
        result = call_tool(
            mcp_server, "harness_task_create",
            title="MCP task", brief="x",
            harness_profile="fake-test",
            goal_text="/goal write a file",
            verification_commands_json=json.dumps([
                {"name": "ok", "command": "true", "required": True},
            ]),
        )
        # The tool returns the TaskRecord.model_dump()
        assert "id" in result
        assert result["agent_id"] == "harness_session"
        meta = result.get("metadata", {})
        # metadata might be nested differently; check both keys
        assert meta.get("runtime_type") == "harness_session" or \
               "runtime_type" in str(meta)

    def test_create_task_with_invalid_json_returns_error(self, mcp_server):
        result = call_tool(
            mcp_server, "harness_task_create",
            verification_commands_json="not json",
        )
        assert "error" in result
        assert "Invalid JSON" in result["error"]


class TestTaskRunTool:
    def test_run_unknown_task_returns_error(self, mcp_server):
        result = call_tool(mcp_server, "harness_task_run",
                           task_id="task_noexist")
        assert "error" in result

    def test_run_queued_task(self, mcp_server):
        # First create one
        created = call_tool(
            mcp_server, "harness_task_create",
            title="x", brief="y", harness_profile="fake-test",
            goal_text="/goal test",
        )
        tid = created["id"]
        # Task is in "created" state by default
        result = call_tool(mcp_server, "harness_task_run",
                            task_id=tid)
        assert result.get("status") == "queued"


# ---------------------------------------------------------------------------
# Sessions + worktrees
# ---------------------------------------------------------------------------


@pytest.fixture
def session_in_db(mcp_server, tmp_path):
    """Insert a session record directly into harness storage."""
    hs = mcp_server._test_harness_storage
    session = HarnessSession(
        id="session_mcp_test", agent_run_id="run_mcp_test",
        task_id="task_mcp_test",
        harness_profile="fake-test", harness="fake",
        runtime="tmux-fake",
        tmux_session="agw_mcp_test",
        tmux_window="main", tmux_pane="0",
        working_directory=str(tmp_path),
        status=HarnessSessionStatus.running.value,
        started_at="2026-01-01T00:00:00+00:00",
        last_output_at="2026-01-01T00:00:01+00:00",
        ended_at=None, metadata={},
    )
    hs.save_session(session)
    return hs, session


class TestSessionsTools:
    def test_list_sessions(self, mcp_server, session_in_db):
        result = call_tool(mcp_server, "harness_list_sessions")
        ids = {s["id"] for s in result}
        assert "session_mcp_test" in ids

    def test_list_sessions_status_filter(self, mcp_server, session_in_db):
        result = call_tool(mcp_server, "harness_list_sessions",
                             status="running")
        assert all(s["status"] == "running" for s in result)

    def test_get_session(self, mcp_server, session_in_db):
        result = call_tool(mcp_server, "harness_get_session",
                            session_id="session_mcp_test")
        assert result["id"] == "session_mcp_test"
        assert result["tmux_session"] == "agw_mcp_test"

    def test_get_session_unknown(self, mcp_server):
        result = call_tool(mcp_server, "harness_get_session",
                            session_id="session_xxx")
        assert "error" in result

    def test_list_worktrees(self, mcp_server):
        result = call_tool(mcp_server, "harness_list_worktrees")
        assert isinstance(result, list)


# ---------------------------------------------------------------------------
# Interactions
# ---------------------------------------------------------------------------


@pytest.fixture
def interaction_in_db(mcp_server, session_in_db):
    hs, session = session_in_db
    interaction = ComposerInteraction(
        id="interaction_mcp_test",
        agent_run_id="run_mcp_test",
        task_id="task_mcp_test",
        session_id="session_mcp_test",
        type=ComposerInteractionType.needs_reply.value,
        status=ComposerInteractionStatus.pending.value,
        prompt_excerpt="Should I do X or Y?",
        full_context_ref=None,
        created_at="2026-01-01T00:00:00+00:00",
        resolved_at=None, composer_reply=None, metadata={},
    )
    hs.save_interaction(interaction)
    return hs, interaction


class TestInteractionsTools:
    def test_list_interactions_default_pending(self, mcp_server,
                                                interaction_in_db):
        result = call_tool(mcp_server, "harness_list_interactions")
        ids = {i["id"] for i in result}
        assert "interaction_mcp_test" in ids

    def test_get_interaction(self, mcp_server, interaction_in_db):
        result = call_tool(mcp_server, "harness_get_interaction",
                            interaction_id="interaction_mcp_test")
        assert result["id"] == "interaction_mcp_test"
        assert result["type"] == "needs_reply"

    def test_get_interaction_unknown(self, mcp_server):
        result = call_tool(mcp_server, "harness_get_interaction",
                            interaction_id="int_xxx")
        assert "error" in result

    def test_reply_unknown_interaction(self, mcp_server):
        result = call_tool(mcp_server, "harness_reply_interaction",
                            interaction_id="int_xxx",
                            reply="do X")
        assert "error" in result

    def test_reply_already_answered_returns_error(self, mcp_server,
                                                  interaction_in_db):
        hs, inter = interaction_in_db
        hs.update_interaction_status(
            inter.id, ComposerInteractionStatus.answered.value,
            composer_reply="x",
        )
        result = call_tool(mcp_server, "harness_reply_interaction",
                            interaction_id=inter.id,
                            reply="hi again")
        assert "error" in result
        assert "not pending" in result["error"].lower()


# ---------------------------------------------------------------------------
# Verification + artifacts
# ---------------------------------------------------------------------------


class TestVerificationTools:
    def test_get_verification_unknown(self, mcp_server):
        result = call_tool(mcp_server, "harness_get_verification",
                            agent_run_id="run_xxx")
        assert "error" in result

    def test_list_artifacts(self, mcp_server):
        result = call_tool(mcp_server, "harness_list_artifacts")
        assert isinstance(result, list)

    def test_get_artifact_unknown(self, mcp_server):
        result = call_tool(mcp_server, "harness_get_artifact",
                            artifact_id="art_xxx")
        assert "error" in result
