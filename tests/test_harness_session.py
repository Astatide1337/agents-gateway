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

    def test_retries_goal_injection_once_if_pane_looks_unchanged(
        self, storage, worktree_path,
    ):
        """The ready-wait loop is best-effort and can inject the goal
        blind before the TUI is ready — must retry, not drop silently."""
        from unittest.mock import MagicMock, patch
        from agents_gateway.harness.tmux import TmuxSessionRef

        real_tmux = MagicMock()
        real_tmux.create_session.return_value = TmuxSessionRef(session="agw_test")
        # Ready-wait loop's first capture() call sees content immediately
        # (real TUI has rendered); post-injection capture never contains
        # the injected marker text -> triggers retry.
        real_tmux.capture.side_effect = ["welcome screen"] * 10
        emitted: list[tuple[str, dict]] = []
        driver = HarnessDriver(
            storage=storage, tmux_driver=real_tmux,
            emit_event=lambda session, event, data: emitted.append((event, data)),
        )

        with patch("time.sleep"):
            session = driver.start_session(
                task_id="task_retry", agent_run_id="run_retry",
                worktree_path=worktree_path,
                profile=get_profile("fake-test"),
                goal_context=GoalContext(goal_text="hello world"),
            )

        assert session.status == HarnessSessionStatus.running.value
        # First attempt via send_text (paste-buffer); retry uses a
        # genuinely different mechanism (send_text_literal), not a
        # repeat of the one that already failed.
        assert real_tmux.send_text.call_count == 1
        assert real_tmux.send_text_literal.call_count == 1
        assert any(e == "goal.injection_unconfirmed_retrying" for e, _ in emitted)

    def test_does_not_retry_when_marker_present_even_if_pane_also_changed(
        self, storage, worktree_path,
    ):
        """A byte-diff pre/post check false-positives on unrelated
        re-renders; a content marker check must not retry once the
        real marker text is present, even if the screen also changed."""
        from unittest.mock import MagicMock, patch
        from agents_gateway.harness.tmux import TmuxSessionRef

        real_tmux = MagicMock()
        real_tmux.create_session.return_value = TmuxSessionRef(session="agw_test")
        real_tmux.capture.side_effect = [
            "welcome screen",  # ready-wait
            "welcome screen\n/goal hello world\n(thinking...)",  # marker present
        ]
        emitted: list[tuple[str, dict]] = []
        driver = HarnessDriver(
            storage=storage, tmux_driver=real_tmux,
            emit_event=lambda session, event, data: emitted.append((event, data)),
        )

        with patch("time.sleep"):
            session = driver.start_session(
                task_id="task_no_retry", agent_run_id="run_no_retry",
                worktree_path=worktree_path,
                profile=get_profile("fake-test"),
                goal_context=GoalContext(goal_text="hello world"),
            )

        assert session.status == HarnessSessionStatus.running.value
        # Only the initial send_text — no retry, since the marker was found.
        assert real_tmux.send_text.call_count == 1
        assert not any(e == "goal.injection_unconfirmed_retrying" for e, _ in emitted)

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

    def test_wire_protocol_adapter_env_injected_for_claude_code_override(
        self, driver, fake_tmux, worktree_path, monkeypatch,
    ):
        calls = []
        monkeypatch.setattr(
            "agents_gateway.harness.driver.ensure_proxy_running",
            lambda port: calls.append(port),
        )
        session = driver.start_session(
            task_id="task_cc", agent_run_id="run_cc",
            worktree_path=worktree_path,
            profile=get_profile("claude-code"),
            model_override="nvidia-nim/z-ai/glm-5.2",
        )
        cmd = fake_tmux.spawn_commands[session.tmux_session]
        assert any(c.startswith("ANTHROPIC_BASE_URL=") for c in cmd)
        assert any(c.startswith("ANTHROPIC_API_KEY=") for c in cmd)
        assert calls == [8199]

    def test_wire_protocol_adapter_not_injected_for_bare_claude_code(
        self, driver, fake_tmux, worktree_path, monkeypatch,
    ):
        calls = []
        monkeypatch.setattr(
            "agents_gateway.harness.driver.ensure_proxy_running",
            lambda port: calls.append(port),
        )
        session = driver.start_session(
            task_id="task_cc2", agent_run_id="run_cc2",
            worktree_path=worktree_path,
            profile=get_profile("claude-code"),
        )
        cmd = fake_tmux.spawn_commands[session.tmux_session]
        assert not any(c.startswith("ANTHROPIC_BASE_URL=") for c in cmd)
        assert calls == []

    def test_wire_protocol_adapter_not_injected_for_profile_without_it(
        self, driver, fake_tmux, worktree_path, monkeypatch,
    ):
        calls = []
        monkeypatch.setattr(
            "agents_gateway.harness.driver.ensure_proxy_running",
            lambda port: calls.append(port),
        )
        session = driver.start_session(
            task_id="task_pi", agent_run_id="run_pi",
            worktree_path=worktree_path,
            profile=get_profile("pi-coding-agent"),
            model_override="nvidia/nemotron-3-ultra-550b-a55b:free",
        )
        cmd = fake_tmux.spawn_commands[session.tmux_session]
        assert not any(c.startswith("ANTHROPIC_BASE_URL=") for c in cmd)
        assert calls == []

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


# ---------------------------------------------------------------------------
# opencode's process_json input_mode — end-to-end through HarnessDriver,
# not just OpencodeJsonDriver in isolation (see test_process_json_driver.py).
# ---------------------------------------------------------------------------

import os as _os
import time as _time

from agents_gateway.harness.classifier import HarnessState
from agents_gateway.harness.process_json import OpencodeJsonDriver

_FAKE_OPENCODE = _os.path.join(_os.path.dirname(__file__), "fixtures", "fake_opencode_run.py")


@pytest.fixture
def json_driver(tmp_path):
    return HarnessDriver(
        storage=HarnessStorage(str(tmp_path / "harness.db")),
        json_driver=OpencodeJsonDriver(binary=_FAKE_OPENCODE, log_dir=str(tmp_path / "logs")),
    )


def _wait_state(driver, session, expected, timeout=10):
    deadline = _time.time() + timeout
    result = None
    while _time.time() < deadline:
        result = driver.classify_state(session)
        if result.state == expected:
            return result
        _time.sleep(0.05)
    raise AssertionError(f"expected {expected}, last saw {result.state if result else None}")


class TestOpencodeProcessJsonEndToEnd:
    def test_start_session_uses_json_driver_and_skips_tui_waits(self, json_driver, worktree_path):
        session = json_driver.start_session(
            task_id="task_json", agent_run_id="run_json",
            worktree_path=worktree_path,
            profile=get_profile("opencode"),
            goal_context=GoalContext(goal_text="Implement divide(a,b)."),
            model_override="x/model",
        )
        assert session.runtime == "process-json"
        assert session.status == HarnessSessionStatus.running.value

    def test_classify_state_reaches_completed_claimed(self, json_driver, worktree_path):
        session = json_driver.start_session(
            task_id="task_json2", agent_run_id="run_json2",
            worktree_path=worktree_path,
            profile=get_profile("opencode"),
            goal_context=GoalContext(goal_text="Implement divide(a,b)."),
            model_override="x/model",
        )
        result = _wait_state(json_driver, session, HarnessState.completed_claimed)
        assert "1 passed (required)" in json_driver.capture_output(session)

    def test_send_reply_resumes_the_opencode_session(self, json_driver, worktree_path):
        session = json_driver.start_session(
            task_id="task_json3", agent_run_id="run_json3",
            worktree_path=worktree_path,
            profile=get_profile("opencode"),
            goal_context=GoalContext(goal_text="Implement divide(a,b)."),
            model_override="x/model",
        )
        _wait_state(json_driver, session, HarnessState.completed_claimed)

        json_driver.send_reply(session, "Use float division.")
        _wait_state(json_driver, session, HarnessState.completed_claimed)
        assert "Resumed." in json_driver.capture_output(session)

    def test_stop_session_terminates_json_driver_process(self, json_driver, worktree_path):
        session = json_driver.start_session(
            task_id="task_json4", agent_run_id="run_json4",
            worktree_path=worktree_path,
            profile=get_profile("opencode"),
            goal_context=GoalContext(goal_text="Implement divide(a,b)."),
            model_override="x/model",
        )
        json_driver.stop_session(session)
        assert session.status in (
            HarnessSessionStatus.cancelled.value,
            HarnessSessionStatus.completed.value,
        )

    def test_separately_constructed_driver_can_still_stop_the_session(
        self, tmp_path, worktree_path, monkeypatch,
    ):
        """server.py's cancel_task route constructs a fresh HarnessDriver
        per request with no explicit json_driver — without a shared
        default, that instance can't find the running process, and
        stop_session silently reports success without killing it.
        Isolates the module-level singleton via monkeypatch to avoid
        leaking session state into other tests."""
        import agents_gateway.harness.process_json as process_json_module

        monkeypatch.setattr(process_json_module, "_default_driver", None)

        def _fake_default_driver():
            if process_json_module._default_driver is None:
                process_json_module._default_driver = OpencodeJsonDriver(
                    binary=_FAKE_OPENCODE, log_dir=str(tmp_path / "logs"),
                )
            return process_json_module._default_driver

        monkeypatch.setattr(process_json_module, "get_default_json_driver", _fake_default_driver)
        monkeypatch.setattr("agents_gateway.harness.driver.get_default_json_driver", _fake_default_driver)

        storage_a = HarnessStorage(str(tmp_path / "harness.db"))

        # Neither driver is given an explicit json_driver — exactly how
        # server.py's routes construct HarnessDriver today.
        driver_a = HarnessDriver(storage=storage_a)
        session = driver_a.start_session(
            task_id="task_cancel_x", agent_run_id="run_cancel_x",
            worktree_path=worktree_path,
            profile=get_profile("opencode"),
            goal_context=GoalContext(goal_text="Implement divide(a,b)."),
            model_override="x/model",
        )

        driver_b = HarnessDriver(storage=storage_a)
        ref = driver_b._ref(session)
        # Must resolve to the SAME underlying process, not raise
        # "unknown session".
        assert driver_b.json_driver.is_alive(ref) or driver_b.json_driver.exit_code(ref) is not None
        driver_b.stop_session(session)

        # terminate() fully removes session state (not just marks it
        # dead) — confirm the shared singleton (driver_a's view too)
        # reflects the real termination, not a stale "still alive".
        with pytest.raises(Exception):
            driver_a.json_driver.is_alive(ref)
