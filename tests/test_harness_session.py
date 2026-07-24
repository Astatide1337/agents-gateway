"""Tests for the HarnessDriver + Composer interaction flow."""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from agents_gateway.harness.driver import HarnessDriver, HarnessDriverError
from agents_gateway.harness.goal import GoalContext
from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionStatus,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
)
from agents_gateway.harness.profiles import (
    HarnessProfile,
    get_profile,
)
from agents_gateway.harness.models import GoalStrategy
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import FakeTmuxDriver


@pytest.fixture
def storage(tmp_path):
    return HarnessStorage(str(tmp_path / "harness.db"))


@pytest.fixture
def fake_tmux():
    return FakeTmuxDriver()


@pytest.fixture
def driver(storage, fake_tmux):
    return HarnessDriver(storage=storage, tmux_driver=fake_tmux)


@pytest.fixture
def worktree_path(tmp_path):
    wt = tmp_path / "wt"
    wt.mkdir()
    return str(wt)


# ---------------------------------------------------------------------------
# start_session / inject_goal / capture_output / send_reply / stop
# ---------------------------------------------------------------------------


class TestStartSession:
    def test_start_session_returns_session_with_running_status(self, driver,
                                                                worktree_path):
        session = driver.start_session(
            task_id="task_1", agent_run_id="run_1",
            worktree_path=worktree_path,
            profile=get_profile("fake-test"),
            goal_context=GoalContext(goal_text="hello world"),
        )
        assert session.status == HarnessSessionStatus.running.value
        assert session.tmux_session.startswith("agw_")
        assert (Path(worktree_path) / ".agent-task" / "GOAL.md").exists()
        captured = driver.tmux.spawn_commands  # type: ignore[attr-defined]
        assert any("python3" in c for c in captured[session.tmux_session])
        inputs = driver.tmux.inputs.get(session.tmux_session, [])  # type: ignore[attr-defined]
        assert any("hello world" in i or "/goal" in i for i in inputs)

    def test_start_session_with_unknown_profile_uses_default(self, driver, worktree_path):
        session = driver.start_session(
            task_id="task_2", agent_run_id="run_2",
            worktree_path=worktree_path,
            profile="nonexistent",
        )
        assert session.harness_profile == "pi-coding-agent"

    def test_start_session_records_in_storage(self, driver, worktree_path):
        session = driver.start_session(
            task_id="task_3", agent_run_id="run_3",
            worktree_path=worktree_path,
        )
        fetched = driver.storage.get_session(session.id)
        assert fetched is not None
        assert fetched.id == session.id

    def test_empty_command_raises(self, fake_tmux, tmp_path):
        # A profile with a truly empty command path should raise cleanly.
        empty_profile = HarnessProfile(
            name="empty", harness="x", command="",
            supports_slash_goal=False,
            goal_strategy=GoalStrategy.plain_prompt.value,
        )
        storage = HarnessStorage(str(tmp_path / "harness.db"))
        driver = HarnessDriver(storage=storage, tmux_driver=fake_tmux)
        with pytest.raises(HarnessDriverError, match="command is empty"):
            driver.start_session(
                task_id="task", agent_run_id="run",
                worktree_path="/tmp", profile=empty_profile,
            )


class TestCaptureOutputAndSendReply:
    def test_capture_returns_pushed_output(self, driver, fake_tmux, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        fake_tmux.push_output(session.tmux_session, "hello from harness\n")
        out = driver.capture_output(session)
        assert "hello from harness" in out
        fetched = driver.storage.get_session(session.id)
        assert fetched.last_output_at is not None
        assert fetched.last_output_at >= session.started_at

    def test_send_reply_includes_assistant_header(self, driver, fake_tmux, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        driver.send_reply(session, "Proceed with the safer option.")
        inputs = fake_tmux.inputs.get(session.tmux_session, [])  # type: ignore[attr-defined]
        joined = "\n".join(inputs)
        assert "ASSISTANT REPLY" in joined
        assert "Proceed with the safer option." in joined
        assert session.status == HarnessSessionStatus.running.value


class TestStopSession:
    def test_stop_terminates_session(self, driver, fake_tmux, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        assert fake_tmux.is_alive(driver._ref(session))
        driver.stop_session(session)
        assert not fake_tmux.is_alive(driver._ref(session))


# ---------------------------------------------------------------------------
# State transitions
# ---------------------------------------------------------------------------


class TestStateMarkers:
    def test_mark_waiting_for_reply_creates_interaction(self, driver, worktree_path):
        session = driver.start_session(
            task_id="task_w", agent_run_id="run_w",
            worktree_path=worktree_path,
        )
        interaction = driver.mark_waiting_for_reply(session, excerpt="please advise")
        assert interaction.type == ComposerInteractionType.needs_reply.value
        assert interaction.status == ComposerInteractionStatus.pending.value
        fetched = driver.storage.get_interaction(interaction.id)
        assert fetched is not None
        fetched_session = driver.storage.get_session(session.id)
        assert fetched_session.status == HarnessSessionStatus.waiting_for_reply.value

    def test_mark_completed_sets_ended_at(self, driver, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        driver.mark_completed(session)
        fetched = driver.storage.get_session(session.id)
        assert fetched.status == HarnessSessionStatus.completed.value
        assert fetched.ended_at is not None

    def test_mark_failed_records_reason(self, driver, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        driver.mark_failed(session, reason="classifier failure")
        fetched = driver.storage.get_session(session.id)
        assert fetched.status == HarnessSessionStatus.failed.value
        assert fetched.metadata.get("failure_reason") == "classifier failure"

    def test_mark_blocked_external_tracks_missing_env(self, driver, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        driver.mark_blocked_external(
            session, reason="missing_credentials",
            missing_env=["GITHUB_TOKEN"],
        )
        fetched = driver.storage.get_session(session.id)
        assert fetched.status == HarnessSessionStatus.blocked_external.value
        assert "GITHUB_TOKEN" in fetched.metadata.get("blocker", {}).get(
            "missing_env", [])

    def test_mark_verifying_transitions_session(self, driver, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        driver.mark_verifying(session)
        fetched = driver.storage.get_session(session.id)
        assert fetched.status == HarnessSessionStatus.verifying.value

    def test_mark_stalled_creates_interaction(self, driver, worktree_path):
        session = driver.start_session(
            task_id="t", agent_run_id="r", worktree_path=worktree_path,
        )
        interaction = driver.mark_stalled(session)
        assert interaction.type == ComposerInteractionType.ambiguous_harness_state.value
        fetched = driver.storage.get_session(session.id)
        assert fetched.status == HarnessSessionStatus.stalled.value


# ---------------------------------------------------------------------------
# Storage listing helpers (HTTP routes depend on these)
# ---------------------------------------------------------------------------


class TestStorageHelpers:
    def test_list_active_sessions_returns_running_only(self, storage, fake_tmux):
        for tid in ("a", "b"):
            session = HarnessSession.new(
                agent_run_id="run", task_id=tid,
                harness_profile="fake-test", harness="fake",
                tmux_session=f"s{tid}", working_directory="/tmp",
            )
            session.status = (HarnessSessionStatus.running.value
                              if tid == "a"
                              else HarnessSessionStatus.completed.value)
            storage.save_session(session)
        active = storage.list_active_sessions()
        assert len(active) == 1
        assert active[0].task_id == "a"

    def test_list_interactions_filters_by_status_and_task(self, storage):
        for i in range(4):
            inter = ComposerInteraction.new(
                agent_run_id="run", task_id=f"task_{i % 2}",
                session_id="sess",
                type_=ComposerInteractionType.needs_reply.value,
            )
            inter.status = (ComposerInteractionStatus.pending.value
                            if i % 2 == 0
                            else ComposerInteractionStatus.answered.value)
            storage.save_interaction(inter)
        pending = storage.list_interactions(status="pending")
        assert all(i.status == "pending" for i in pending)
        assert len(pending) >= 2

    def test_update_interaction_status_transitions_to_answered(self, storage):
        inter = ComposerInteraction.new(
            agent_run_id="run", task_id="task",
            session_id="sess", type_=ComposerInteractionType.needs_reply.value,
        )
        storage.save_interaction(inter)
        updated = storage.update_interaction_status(
            inter.id, ComposerInteractionStatus.answered.value,
            composer_reply="just go ahead",
        )
        assert updated.status == "answered"
        assert updated.composer_reply == "just go ahead"
        assert updated.resolved_at is not None

    def test_list_pending_interactions_is_a_helper(self, storage):
        for s in ("pending", "answered", "pending"):
            inter = ComposerInteraction.new(
                agent_run_id="run", task_id="task",
                session_id="s", type_="needs_reply",
            )
            inter.status = s
            storage.save_interaction(inter)
        pending = storage.list_pending_interactions()
        assert len(pending) == 2
        assert all(i.status == "pending" for i in pending)
