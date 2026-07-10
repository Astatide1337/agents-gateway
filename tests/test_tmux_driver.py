"""Tests for the TmuxDriver + FakeTmuxDriver surface.

The real TmuxDriver is NOT exercised in unit tests (it would create
state in the host tmux daemon). We test the FakeTmuxDriver fully and
verify the TmuxDriver class builds command arrays correctly (mocked).
"""

from __future__ import annotations

import subprocess
from unittest.mock import patch

import pytest

from agents_gateway.harness.tmux import (
    FakeTmuxDriver,
    TmuxDriver,
    TmuxSessionRef,
)


# ---------------------------------------------------------------------------
# FakeTmuxDriver
# ---------------------------------------------------------------------------


class TestFakeTmuxDriver:
    def test_create_session_spawns_virtual_pane(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("test-sess", "/tmp/work", ["echo", "hello"])
        assert ref.session == "test-sess"
        assert ref.window == "main"
        assert ref.pane == "0"
        # spawn_commands + inputs recorded for assertion
        assert driver.spawn_commands["test-sess"] == ["echo", "hello"]
        assert driver.is_alive(ref) is True

    def test_send_text_records_input(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./harness"])
        driver.send_text(ref, "hello world")
        driver.send_enter(ref)
        assert driver.inputs["s"] == ["hello world", "<Enter>"]

    def test_capture_returns_pushed_output(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])
        driver.push_output("s", "line one\nline two\n")
        out = driver.capture(ref)
        assert "line one" in out
        assert "line two" in out

    def test_capture_limits_lines(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])
        for i in range(100):
            driver.push_output("s", f"line-{i}\n")
        out = driver.capture(ref, lines=10)
        assert "line-99" in out  # last 10 contains most recent
        assert "line-50" not in out

    def test_capture_on_unknown_session_empty(self):
        driver = FakeTmuxDriver()
        ref = TmuxSessionRef(session="ghost")
        assert driver.capture(ref) == ""

    def test_terminate_closes_session(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])
        driver.terminate(ref)
        assert driver.is_alive(ref) is False

    def test_register_session_handler_invoked_on_send(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])
        captured_texts: list[str] = []

        def handler(drv, session, text, is_enter):
            captured_texts.append((text, is_enter))
            drv.push_output(session, f"[handled: {text}]\n")

        driver.register_session_handler("s", handler)
        driver.send_text(ref, "/goal do something")
        driver.send_enter(ref)
        assert captured_texts == [("/goal do something", False),
                                  ("<Enter>", True)]
        out = driver.capture(ref)
        assert "/goal do something" in out

    def test_mark_closed_makes_is_alive_false(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])
        driver.mark_closed("s")
        assert driver.is_alive(ref) is False

    def test_create_session_records_empty_command_as_proof_of_call(self):
        # FakeTmuxDriver accepts empty command (it doesn't spawn anything)
        # for use in tests where the handler drives output. Real TmuxDriver
        # rejects empty argv; FakeTmuxDriver just records it.
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", [])
        assert driver.spawn_commands["s"] == []

    def test_handler_can_push_then_close(self):
        driver = FakeTmuxDriver()
        ref = driver.create_session("s", "/tmp", ["./h"])

        def handler(drv, session, text, is_enter):
            drv.push_output(session, "DONE.\n")
            drv.mark_closed(session)

        driver.register_session_handler("s", handler)
        driver.send_text(ref, "/goal do")
        driver.send_enter(ref)
        assert driver.is_alive(ref) is False


# ---------------------------------------------------------------------------
# TmuxDriver (real — only via subprocess mocked)
# ---------------------------------------------------------------------------


class TestTmuxDriverCommandConstruction:
    def test_create_session_invokes_tmux_with_quoted_argv(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            driver.create_session("sess", "/work/dir",
                                  ["python3", "agents/fake-test/run.py"])
        # Verify the subprocess call was made with an argv list (not shell str)
        cast = mock.call_args[0]
        argv = cast[0]
        assert argv[0] == "tmux"
        assert "new-session" in argv
        assert "-s" in argv
        assert "sess" in argv
        assert "-c" in argv
        assert "/work/dir" in argv
        # The command string is shell-quoted and merged into one arg.
        assert any("python3" in (a or "") for a in argv)

    def test_send_text_uses_send_keys_literal(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            ref = TmuxSessionRef(session="s", window="main", pane="0")
            driver.send_text(ref, "some text with spaces")
        argv = mock.call_args[0][0]
        assert argv[0] == "tmux"
        assert argv[1] == "send-keys"
        assert "-l" in argv  # literal mode

    def test_send_enter_sends_the_seq_Enter(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            ref = TmuxSessionRef(session="s")
            driver.send_enter(ref)
        argv = mock.call_args[0][0]
        assert "Enter" in argv

    def test_capture_returns_stdout_when_rc_zero(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="capture\nlines", stderr="",
            )
            ref = TmuxSessionRef(session="s")
            out = driver.capture(ref)
        assert out == "capture\nlines"

    def test_is_alive_true_on_rc_zero(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            ref = TmuxSessionRef(session="s")
            assert driver.is_alive(ref) is True

    def test_is_alive_false_on_nonzero(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=1, stdout="", stderr="no session",
            )
            ref = TmuxSessionRef(session="s")
            assert driver.is_alive(ref) is False

    def test_create_session_raises_if_tmux_fails(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=1, stdout="", stderr="tmux: bad option",
            )
            with pytest.raises(RuntimeError, match="tmux create_session failed"):
                driver.create_session("s", "/w", ["cmd"])
