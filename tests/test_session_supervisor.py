"""Tests for the SessionSupervisor — classification + state-machine
transitions driven by the output classifier + verification trigger
hook + interaction creation.

These tests exercise the supervisor in isolation: HarnessStorage is
real SQLite, the FakeTmuxDriver is used to drive captures, and the
HarnessDriver wraps both. The verification runner is stubbed.
"""

from __future__ import annotations

import threading
import time
from datetime import datetime, timezone

import pytest

from agents_gateway.harness.classifier import HarnessState
from agents_gateway.harness.driver import HarnessDriver
from agents_gateway.harness.models import (
    ComposerInteractionStatus,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.supervisor import SessionSupervisor
from agents_gateway.harness.tmux import FakeTmuxDriver


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


@pytest.fixture
def harness_stack(tmp_path):
    """Build a HarnessStorage + FakeTmuxDriver + HarnessDriver stack."""
    hs = HarnessStorage(str(tmp_path / "harness.db"))
    tmux = FakeTmuxDriver()
    driver = HarnessDriver(storage=hs, tmux_driver=tmux)
    return hs, tmux, driver


def _session_in_storage(hs: HarnessStorage, *, status: str = "running",
                        tmux_session: str = "agw_test_1",
                        last_output_at: str | None = None) -> HarnessSession:
    s = HarnessSession(
        id="session_test1", agent_run_id="run_test1",
        task_id="task_test1",
        harness_profile="fake-test", harness="fake",
        runtime="tmux-fake",
        tmux_session=tmux_session, tmux_window="main", tmux_pane="0",
        working_directory="/tmp/test",
        status=status,
        started_at="2026-01-01T00:00:00+00:00",
        last_output_at=last_output_at or _iso_now(),
        ended_at=None, metadata={},
    )
    hs.save_session(s)
    # Also pre-register the tmux session in the fake driver so captures
    # work.
    tmux_handler_holder = s.tmux_session
    return s


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _push(tmux: FakeTmuxDriver, session_name: str, text: str) -> None:
    tmux.push_output(session_name, text)


def _mark_alive(tmux: FakeTmuxDriver, session_name: str) -> None:
    # Calling create_session registers a fresh pane so is_alive is True.
    try:
        tmux.create_session(session_name, "/tmp", ["python3"])
    except Exception:
        pass


# ---------------------------------------------------------------------------
# tick_once — classify + dispatch
# ---------------------------------------------------------------------------


class TestTickOnce:
    def test_running_state_keeps_session_running(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session,
              "Some random harness output.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=900)
        n = sup.tick_once()
        assert n == 1
        fresh = hs.get_session(session.id)
        assert fresh.status == "running"

    def test_waiting_marker_creates_interaction_and_session_state(
            self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session,
              "I need clarification on whether to use X.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=900)
        sup.tick_once()
        fresh = hs.get_session(session.id)
        assert fresh.status == "waiting_for_reply"
        # An interaction should exist
        inter = hs.list_interactions(status=ComposerInteractionStatus
                                      .pending.value)
        assert any(i.session_id == session.id for i in inter)

    def test_completion_marker_triggers_verifying(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session,
              "Working.\nDONE.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=900)
        hooks = {"called": 0}

        def on_completed_claim(s):
            hooks["called"] += 1
        sup.on_completed_claim = on_completed_claim
        sup.tick_once()
        fresh = hs.get_session(session.id)
        assert fresh.status == "verifying"
        assert hooks["called"] == 1

    def test_failed_marker_marks_session_failed(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session,
              "Traceback (most recent call last)\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=900)
        sup.tick_once()
        fresh = hs.get_session(session.id)
        assert fresh.status == "failed"

    def test_stalled_after_silence_threshold(self, harness_stack):
        hs, tmux, driver = harness_stack
        # Session is "starting" so the first capture sets last_output_at.
        session = _session_in_storage(
            hs, status=HarnessSessionStatus.starting.value,
            last_output_at="2024-01-01T00:00:00+00:00")
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "Initializing...\n")
        # First tick — fresh output, classifier says running, capture
        # updates last_output_at.
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=1)
        sup.tick_once()
        cur = hs.get_session(session.id)
        assert cur.status in ("running", "starting")

        # Now produce NO new output, wait > stall_seconds, then tick.
        time.sleep(1.2)
        sup.tick_once()
        fresh = hs.get_session(session.id)
        assert fresh.status == "stalled"

    def test_busy_session_skipped_on_tick(self, harness_stack):
        """If a session is already in the _busy set (e.g. verification
        in flight), tick_once should skip it."""
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "DONE.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)
        # Mark session as busy before tick
        sup._busy.add(session.id)
        n = sup.tick_once()
        assert n == 0
        # Should NOT have transitioned
        fresh = hs.get_session(session.id)
        assert fresh.status == "running"


# ---------------------------------------------------------------------------
# Background thread lifecycle
# ---------------------------------------------------------------------------


class TestSupervisorThread:
    def test_start_stop(self, harness_stack):
        hs, tmux, driver = harness_stack
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01,
                                  stall_seconds=900)
        sup.start()
        time.sleep(0.05)
        assert sup.is_alive()
        sup.stop(timeout=2.0)
        assert not sup.is_alive()

    def test_thread_classifies_and_transitions(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "I need clarification.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.02,
                                  stall_seconds=900)
        sup.start()
        # Wait until the session transitions
        deadline = time.time() + 2.0
        while time.time() < deadline:
            fresh = hs.get_session(session.id)
            if fresh.status == "waiting_for_reply":
                break
            time.sleep(0.02)
        sup.stop(timeout=2.0)
        assert hs.get_session(session.id).status == "waiting_for_reply"


# ---------------------------------------------------------------------------
# Hook invocation
# ---------------------------------------------------------------------------


class TestCompletionHook:
    def test_hook_invoked_once_per_completed_claim(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "DONE.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)
        hooks = []
        sup.on_completed_claim = lambda s: hooks.append(s.id)
        sup.tick_once()
        sup.tick_once()  # Second tick should not call again (state != running)
        assert len(hooks) == 1
        assert hooks[0] == session.id

    def test_hook_exception_does_not_crash_supervisor(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "DONE.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)

        def raise_hook(s):
            raise RuntimeError("test hook boom")
        sup.on_completed_claim = raise_hook
        # Should not raise
        sup.tick_once()
        # Session should still be marked verifying regardless
        fresh = hs.get_session(session.id)
        assert fresh.status == "verifying"


# ---------------------------------------------------------------------------
# Skipping terminal / busy sessions
# ---------------------------------------------------------------------------


class TestSkipTerminalSessions:
    @pytest.mark.parametrize("status", [
        HarnessSessionStatus.completed.value,
        HarnessSessionStatus.failed.value,
        HarnessSessionStatus.cancelled.value,
        HarnessSessionStatus.blocked_external.value,
        HarnessSessionStatus.stalled.value,
        HarnessSessionStatus.verifying.value,
    ])
    def test_does_not_reclassify_terminal_status(self, harness_stack, status):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(hs, status=status)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "DONE.\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)
        n = sup.tick_once()
        assert n == 0
        # Session should not have changed
        assert hs.get_session(session.id).status == status


# ---------------------------------------------------------------------------
# Resume from waiting_for_reply when output resumes
# ---------------------------------------------------------------------------


class TestResumeRunning:
    def test_session_in_waiting_resumes_to_running_on_new_output(
            self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(
            hs, status=HarnessSessionStatus.waiting_for_reply.value)
        _mark_alive(tmux, session.tmux_session)
        # Push output WITHOUT a waiting marker — now running.
        _push(tmux, session.tmux_session, "Continuing work...\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)
        sup.tick_once()
        fresh = hs.get_session(session.id)
        assert fresh.status == "running"


# ---------------------------------------------------------------------------
# Start state gets classified (starting → running)
# ---------------------------------------------------------------------------


class TestStartingState:
    def test_starting_state_classified_normally(self, harness_stack):
        hs, tmux, driver = harness_stack
        session = _session_in_storage(
            hs, status=HarnessSessionStatus.starting.value)
        _mark_alive(tmux, session.tmux_session)
        _push(tmux, session.tmux_session, "Booting...\n")
        sup = SessionSupervisor(storage=hs, driver=driver,
                                  verification_runner=None,
                                  poll_interval_seconds=0.01)
        sup.tick_once()
        fresh = hs.get_session(session.id)
        # Should remain in a non-terminal state (running or starting->running)
        assert fresh.status in (
            HarnessSessionStatus.running.value,
            HarnessSessionStatus.starting.value,
        )
