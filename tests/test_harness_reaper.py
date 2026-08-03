"""Tests for reap_orphaned_processes — startup cleanup of harness
processes left running behind a terminal-status session (see
reaper.py's docstring for the live incident this closes)."""
from __future__ import annotations

import subprocess
import time

import pytest

from agents_gateway.harness.models import HarnessSession, HarnessSessionStatus
from agents_gateway.harness.reaper import reap_orphaned_processes
from agents_gateway.harness.storage import HarnessStorage


@pytest.fixture
def storage(tmp_path):
    return HarnessStorage(str(tmp_path / "harness.db"))


def _session(status: str, pid=None, runtime="process-json") -> HarnessSession:
    s = HarnessSession.new(
        agent_run_id="run_1", task_id="task_1", harness_profile="opencode",
        harness="opencode", tmux_session="agw_task_1", working_directory="/tmp",
        runtime=runtime,
    )
    s.status = status
    if pid is not None:
        s.metadata = {"json_pid": pid}
    return s


def _spawn_sleeper() -> subprocess.Popen:
    return subprocess.Popen(["sleep", "30"])


class TestReapOrphanedProcesses:
    def test_kills_a_live_process_behind_a_terminal_session(self, storage):
        proc = _spawn_sleeper()
        try:
            session = _session(HarnessSessionStatus.cancelled.value, pid=proc.pid)
            storage.save_session(session)

            killed = reap_orphaned_processes(storage, grace_seconds=0.2)

            assert len(killed) == 1
            assert killed[0]["pid"] == proc.pid
            assert killed[0]["session_id"] == session.id
            proc.wait(timeout=5)
            assert proc.poll() is not None
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)

    def test_covers_every_terminal_status_not_just_cancelled(self, storage):
        procs = []
        try:
            for status in (
                HarnessSessionStatus.completed.value,
                HarnessSessionStatus.failed.value,
                HarnessSessionStatus.blocked_external.value,
            ):
                proc = _spawn_sleeper()
                procs.append(proc)
                storage.save_session(_session(status, pid=proc.pid))

            killed = reap_orphaned_processes(storage, grace_seconds=0.2)

            assert len(killed) == 3
            for proc in procs:
                proc.wait(timeout=5)
                assert proc.poll() is not None
        finally:
            for proc in procs:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=5)

    def test_leaves_a_running_sessions_process_alone(self, storage):
        proc = _spawn_sleeper()
        try:
            storage.save_session(_session(HarnessSessionStatus.running.value, pid=proc.pid))

            killed = reap_orphaned_processes(storage, grace_seconds=0.2)

            assert killed == []
            assert proc.poll() is None
        finally:
            proc.kill()
            proc.wait(timeout=5)

    def test_leaves_a_waiting_for_reply_sessions_process_alone(self, storage):
        """waiting_for_reply is a legitimate paused-but-alive state, not
        terminal — must never be reaped."""
        proc = _spawn_sleeper()
        try:
            storage.save_session(
                _session(HarnessSessionStatus.waiting_for_reply.value, pid=proc.pid)
            )

            killed = reap_orphaned_processes(storage, grace_seconds=0.2)

            assert killed == []
            assert proc.poll() is None
        finally:
            proc.kill()
            proc.wait(timeout=5)

    def test_terminal_session_with_no_pid_is_skipped_not_an_error(self, storage):
        storage.save_session(_session(HarnessSessionStatus.completed.value, pid=None))
        killed = reap_orphaned_processes(storage, grace_seconds=0.0)
        assert killed == []

    def test_terminal_session_with_an_already_dead_pid_is_a_noop(self, storage):
        proc = _spawn_sleeper()
        dead_pid = proc.pid
        proc.kill()
        proc.wait(timeout=5)

        storage.save_session(_session(HarnessSessionStatus.failed.value, pid=dead_pid))
        killed = reap_orphaned_processes(storage, grace_seconds=0.0)
        assert killed == []

    def test_a_non_tmux_runtime_session_is_still_checked_by_pid(self, storage):
        """json_pid is only ever set for process-json runtime sessions in
        practice, but the reaper should key off the metadata field
        itself, not assume runtime — defensive against future runtimes
        that might also persist a pid."""
        proc = _spawn_sleeper()
        try:
            storage.save_session(
                _session(HarnessSessionStatus.cancelled.value, pid=proc.pid, runtime="tmux")
            )
            killed = reap_orphaned_processes(storage, grace_seconds=0.2)
            assert len(killed) == 1
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)

    def test_no_sessions_at_all_is_a_clean_noop(self, storage):
        assert reap_orphaned_processes(storage, grace_seconds=0.0) == []

    def test_sigkill_escalation_for_a_process_ignoring_sigterm(self, storage):
        """A process that ignores SIGTERM must still be forced down via
        SIGKILL after the grace period, not left running."""
        proc = subprocess.Popen(
            ["python3", "-c", "import signal,time; "
             "signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(30)"]
        )
        try:
            time.sleep(0.3)  # let the signal handler install
            storage.save_session(_session(HarnessSessionStatus.cancelled.value, pid=proc.pid))

            killed = reap_orphaned_processes(storage, grace_seconds=1.0)

            assert len(killed) == 1
            proc.wait(timeout=5)
            assert proc.poll() is not None
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait(timeout=5)
