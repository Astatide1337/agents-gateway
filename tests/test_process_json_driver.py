"""OpencodeJsonDriver + classify_json_transcript.

Uses a scripted fake binary (tests/fixtures/fake_opencode_run.py) so
these run offline and deterministically.
"""
from __future__ import annotations

import os
import time

import pytest

from agents_gateway.harness.classifier import HarnessState, classify_json_transcript
from agents_gateway.harness.process_json import OpencodeJsonDriver, OpencodeJsonDriverError
from agents_gateway.harness.tmux import TmuxSessionRef

FIXTURE = os.path.join(os.path.dirname(__file__), "fixtures", "fake_opencode_run.py")


@pytest.fixture
def driver(tmp_path):
    return OpencodeJsonDriver(binary=FIXTURE, log_dir=str(tmp_path))


def _wait_exit(driver, ref, timeout=10):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not driver.is_alive(ref):
            return
        time.sleep(0.05)
    raise AssertionError("fake process never exited")


class TestSessionLifecycle:
    def test_create_session_does_not_spawn(self, driver, tmp_path):
        ref = driver.create_session("s1", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        assert isinstance(ref, TmuxSessionRef)
        assert not driver.is_alive(ref)
        assert driver.exit_code(ref) is None

    def test_send_text_then_enter_spawns_and_completes(self, driver, tmp_path):
        ref = driver.create_session("s2", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "Implement divide(a,b).")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        assert driver.exit_code(ref) == 0
        capture = driver.capture(ref)
        assert "[sent] Implement divide(a,b)." in capture
        assert "1 passed (required)" in capture
        assert "exit=0" in capture

    def test_send_enter_with_no_pending_text_is_a_noop(self, driver, tmp_path):
        ref = driver.create_session("s3", cwd=str(tmp_path), command=["opencode"])
        driver.send_enter(ref)
        assert not driver.is_alive(ref)
        assert driver.exit_code(ref) is None

    def test_capture_on_unknown_session_raises(self, driver):
        with pytest.raises(OpencodeJsonDriverError):
            driver.capture(TmuxSessionRef(session="nope"))

    def test_terminate_removes_log_and_state(self, driver, tmp_path):
        ref = driver.create_session("s4", cwd=str(tmp_path), command=["opencode"])
        driver.send_text(ref, "hello")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        log_path = os.path.join(str(tmp_path), "s4.ndjson")
        assert os.path.exists(log_path)
        driver.terminate(ref)
        assert not os.path.exists(log_path)
        with pytest.raises(OpencodeJsonDriverError):
            driver.is_alive(ref)


class TestSpawnArgv:
    """subprocess.Popen(cwd=...) alone isn't enough to scope opencode's
    bash tool to the right directory for a git worktree — --dir is required."""

    def test_spawn_includes_explicit_dir_flag(self, driver, tmp_path, monkeypatch):
        captured_argv = {}

        class _FakeProc:
            pid = 99999

            def poll(self):
                return 0

        def fake_popen(argv, cwd=None, stdout=None, stderr=None):
            captured_argv["argv"] = argv
            captured_argv["cwd"] = cwd
            return _FakeProc()

        monkeypatch.setattr("subprocess.Popen", fake_popen)
        ref = driver.create_session("s_argv", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "do the thing")
        driver.send_enter(ref)

        argv = captured_argv["argv"]
        assert "--dir" in argv
        assert argv[argv.index("--dir") + 1] == str(tmp_path)
        assert captured_argv["cwd"] == str(tmp_path)


class TestSessionResumption:
    def test_reply_resumes_with_session_and_continue_flags(self, driver, tmp_path):
        ref = driver.create_session("s5", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "Implement divide(a,b).")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        first_capture = driver.capture(ref)
        assert "Resumed." not in first_capture

        driver.send_text(ref, "Use float division.")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        second_capture = driver.capture(ref)
        assert "Resumed." in second_capture


class TestReattachAcrossRestart:
    """OpencodeJsonDriver's session tracking is pure in-process memory
    — a fresh instance (e.g. after an AGW restart) knows nothing about
    sessions an earlier instance spawned. reattach() reconstructs
    enough state from a persisted PID to resume polling instead of
    wrongly concluding the process is dead."""

    def test_fresh_driver_instance_reattaches_via_pid(self, tmp_path):
        first = OpencodeJsonDriver(binary=FIXTURE, log_dir=str(tmp_path / "logs"))
        ref = first.create_session("s_restart", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        first.send_text(ref, "Implement divide(a,b).")
        first.send_enter(ref)
        pid = first.get_pid(ref)
        assert pid is not None

        # Simulate an AGW restart: a completely separate driver
        # instance, no shared in-memory state with `first` at all.
        second = OpencodeJsonDriver(binary=FIXTURE, log_dir=str(tmp_path / "logs"))
        with pytest.raises(OpencodeJsonDriverError):
            second.is_alive(ref)  # unknown to this instance — not yet reattached

        second.reattach("s_restart", str(tmp_path), pid)
        # is_alive is best-effort (raw PID liveness, no Popen handle)
        # — the fake process is fast and may have already exited by
        # the time we get here; either answer is valid, it must just
        # not raise.
        second.is_alive(ref)
        # capture() works fully regardless — it's a pure log-file read.
        _wait_exit(first, ref)
        capture = second.capture(ref)
        assert "1 passed (required)" in capture

    def test_reattach_to_dead_pid_reports_not_alive(self, tmp_path):
        driver = OpencodeJsonDriver(binary=FIXTURE, log_dir=str(tmp_path / "logs"))
        ref = driver.reattach("s_dead", str(tmp_path), pid=999999999)  # not a real PID
        assert driver.is_alive(ref) is False

    def test_spawn_after_reattach_raises_if_still_alive(self, tmp_path):
        """A reply must never spawn a second overlapping process for a
        session the driver only knows about via reattachment."""
        driver = OpencodeJsonDriver(binary=FIXTURE, log_dir=str(tmp_path / "logs"))
        ref = driver.reattach("s_overlap", str(tmp_path), pid=os.getpid())  # this test process itself is "alive"
        driver.send_text(ref, "another message")
        with pytest.raises(OpencodeJsonDriverError):
            driver.send_enter(ref)


class TestClassifyJsonTranscriptDirectly:
    """classify_json_transcript's own logic, independent of the driver."""

    def test_running_while_alive(self):
        r = classify_json_transcript(text="", process_alive=True)
        assert r.state == HarnessState.running

    def test_clean_exit_no_markers_defaults_completed(self):
        r = classify_json_transcript(text="did some stuff, all good", process_alive=False, exit_code=0)
        assert r.state == HarnessState.completed_claimed

    def test_nonzero_exit_is_failed(self):
        r = classify_json_transcript(text="crashed somehow", process_alive=False, exit_code=1)
        assert r.state == HarnessState.failed_claimed

    def test_failure_marker_beats_clean_exit(self):
        r = classify_json_transcript(text="Traceback (most recent call last): ...",
                                     process_alive=False, exit_code=0)
        assert r.state == HarnessState.failed_claimed

    def test_waiting_marker_detected(self):
        r = classify_json_transcript(text="Should I use float or int? Please confirm.",
                                     process_alive=False, exit_code=0)
        assert r.state == HarnessState.waiting_for_reply

    def test_usage_limit_marker_for_subscription_profile(self):
        r = classify_json_transcript(text="You've hit your usage limit for today.",
                                     process_alive=False, exit_code=0,
                                     harness_profile="claude-code")
        assert r.state == HarnessState.usage_limited

    def test_rate_limit_marker_for_metered_profile(self):
        """Metered profiles (opencode) hit API rate limits, not
        subscription usage caps — caught by the generic fallback markers."""
        r = classify_json_transcript(
            text="[error] Rate limit exceeded. Please try again later.",
            process_alive=False, exit_code=1, harness_profile="opencode",
        )
        assert r.state == HarnessState.usage_limited

    def test_usage_limit_marker_takes_priority_over_nonzero_exit(self):
        """A rate-limited call still exits non-zero — usage_limited must
        win over the generic failed_claimed nonzero-exit default."""
        r = classify_json_transcript(
            text="too many requests, please slow down",
            process_alive=False, exit_code=1,
        )
        assert r.state == HarnessState.usage_limited

    def test_usage_limit_marker_detected_while_process_still_alive(self):
        r = classify_json_transcript(
            text="[error] Rate limit exceeded. Please try again later.",
            process_alive=True, harness_profile="opencode",
        )
        assert r.state == HarnessState.usage_limited


class TestInternalLogErrorSurfacing:
    def test_capture_includes_error_lines_written_after_spawn(self, tmp_path):
        internal_log = tmp_path / "opencode.log"
        internal_log.write_text(
            "timestamp=2026-01-01T00:00:00.000Z level=INFO message=stale, before spawn\n")

        driver = OpencodeJsonDriver(
            binary=FIXTURE, log_dir=str(tmp_path / "logs"),
            internal_log_path=str(internal_log),
        )
        ref = driver.create_session("s10", cwd=str(tmp_path),
                                    command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "trigger_hang_ratelimit please")
        driver.send_enter(ref)

        assert driver.is_alive(ref), "fixture should still be sleeping"

        with open(internal_log, "a") as f:
            f.write("timestamp=2026-01-01T00:00:01.000Z level=ERROR message=\"stream error\" "
                    "error.error=\"AI_APICallError: Rate limit exceeded: free-models-per-day\"\n")

        capture = driver.capture(ref)
        assert "rate limit exceeded" in capture.lower()
        assert "stale, before spawn" not in capture, \
            "must not surface log lines written before this run's spawn"

        _wait_exit(driver, ref)

    def test_pre_spawn_log_lines_excluded_via_offset(self, tmp_path):
        internal_log = tmp_path / "opencode.log"
        internal_log.write_text(
            "timestamp=2026-01-01T00:00:00.000Z level=ERROR message=\"unrelated prior run\"\n")

        driver = OpencodeJsonDriver(
            binary=FIXTURE, log_dir=str(tmp_path / "logs"),
            internal_log_path=str(internal_log),
        )
        ref = driver.create_session("s11", cwd=str(tmp_path),
                                    command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "Implement divide(a,b).")
        driver.send_enter(ref)
        _wait_exit(driver, ref)

        capture = driver.capture(ref)
        assert "unrelated prior run" not in capture


class TestDriverAndClassifierEndToEnd:
    def test_completed_task_classifies_as_completed_claimed(self, driver, tmp_path):
        ref = driver.create_session("s6", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "Implement divide(a,b).")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        result = classify_json_transcript(
            text=driver.capture(ref), process_alive=driver.is_alive(ref),
            exit_code=driver.exit_code(ref),
        )
        assert result.state == HarnessState.completed_claimed

    def test_failing_task_classifies_as_failed_claimed(self, driver, tmp_path):
        ref = driver.create_session("s7", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "trigger_fail please")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        result = classify_json_transcript(
            text=driver.capture(ref), process_alive=driver.is_alive(ref),
            exit_code=driver.exit_code(ref),
        )
        assert result.state == HarnessState.failed_claimed

    def test_clarifying_question_classifies_as_waiting_for_reply(self, driver, tmp_path):
        ref = driver.create_session("s8", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "trigger_waiting please")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        result = classify_json_transcript(
            text=driver.capture(ref), process_alive=driver.is_alive(ref),
            exit_code=driver.exit_code(ref),
        )
        assert result.state == HarnessState.waiting_for_reply

    def test_rate_limited_task_classifies_as_usage_limited_not_failed(self, driver, tmp_path):
        """_render() must not drop "error"-type NDJSON events — an
        OpenRouter 429 is surfaced that way, not as plain text."""
        ref = driver.create_session("s9", cwd=str(tmp_path), command=["opencode", "--auto", "-m", "x/model"])
        driver.send_text(ref, "trigger_ratelimit please")
        driver.send_enter(ref)
        _wait_exit(driver, ref)
        capture = driver.capture(ref)
        assert "rate limit" in capture.lower(), "error event text must reach the rendered transcript"
        result = classify_json_transcript(
            text=capture, process_alive=driver.is_alive(ref),
            exit_code=driver.exit_code(ref),
        )
        assert result.state == HarnessState.usage_limited
