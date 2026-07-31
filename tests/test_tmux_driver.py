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
    ContainerTmuxDriver,
    FakeTmuxDriver,
    TmuxDriver,
    TmuxSessionRef,
    build_tmux_driver,
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


# ---------------------------------------------------------------------------
# ContainerTmuxDriver (mocked subprocess — no real Docker required)
# ---------------------------------------------------------------------------


class TestContainerTmuxDriverConstruction:
    def test_requires_docker_image(self):
        with pytest.raises(ValueError):
            ContainerTmuxDriver(docker_image="")

    def test_defaults(self):
        driver = ContainerTmuxDriver(docker_image="my-image:latest")
        assert driver.docker_bin == "docker"
        assert driver.memory == "2g"
        assert driver.cpus == "2.0"
        assert driver.pids_limit == 512
        assert driver.network is None


class TestContainerTmuxDriverSandboxFlags:
    def test_no_rm_flag(self):
        """Unlike DockerRuntime's short tasks, the session container
        must survive across many exec calls — no --rm."""
        driver = ContainerTmuxDriver(docker_image="img")
        assert "--rm" not in driver._sandbox_flags()

    def test_no_read_only_flag(self):
        """Harness CLIs write config/session state outside the
        worktree (e.g. ~/.claude) — root FS must stay writable."""
        driver = ContainerTmuxDriver(docker_image="img")
        assert "--read-only" not in driver._sandbox_flags()

    def test_cap_drop_and_no_new_privileges_present(self):
        driver = ContainerTmuxDriver(docker_image="img")
        flags = driver._sandbox_flags()
        assert "--cap-drop" in flags and "ALL" in flags
        assert "--security-opt" in flags and "no-new-privileges" in flags

    def test_network_none_by_default_is_not_forced(self):
        """Network is enabled by default (None = docker's default
        policy) since harness sessions need LLM API access."""
        driver = ContainerTmuxDriver(docker_image="img")
        assert "--network" not in driver._sandbox_flags()

    def test_network_explicit_none_disables(self):
        driver = ContainerTmuxDriver(docker_image="img", network="none")
        flags = driver._sandbox_flags()
        assert "--network" in flags
        assert flags[flags.index("--network") + 1] == "none"

    def test_custom_resource_limits_applied(self):
        driver = ContainerTmuxDriver(docker_image="img", memory="4g",
                                     cpus="1.5", pids_limit=256)
        flags = driver._sandbox_flags()
        assert flags[flags.index("--memory") + 1] == "4g"
        assert flags[flags.index("--cpus") + 1] == "1.5"
        assert flags[flags.index("--pids-limit") + 1] == "256"


class TestContainerTmuxDriverCommandConstruction:
    def test_create_session_runs_container_then_execs_tmux(self):
        driver = ContainerTmuxDriver(docker_image="my-image")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            ref = driver.create_session("sess", "/work/dir", ["python3", "run.py"])
        assert ref.session == "sess"
        assert mock.call_count == 2
        run_argv = mock.call_args_list[0][0][0]
        assert run_argv[:2] == ["docker", "run"]
        assert "--name" in run_argv and "sess" in run_argv
        assert "my-image" in run_argv
        assert "-v" in run_argv
        assert "/work/dir:/work/dir" in run_argv
        exec_argv = mock.call_args_list[1][0][0]
        assert exec_argv[:3] == ["docker", "exec", "sess"]
        assert "tmux" in exec_argv and "new-session" in exec_argv

    def test_create_session_empty_command_raises(self):
        driver = ContainerTmuxDriver(docker_image="my-image")
        with pytest.raises(ValueError):
            driver.create_session("sess", "/work", [])

    def test_create_session_cleans_up_container_on_tmux_failure(self):
        driver = ContainerTmuxDriver(docker_image="my-image")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.side_effect = [
                subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
                subprocess.CompletedProcess(args=[], returncode=1, stdout="", stderr="tmux boom"),
                subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
            ]
            with pytest.raises(RuntimeError):
                driver.create_session("sess", "/work", ["cmd"])
        rm_argv = mock.call_args_list[2][0][0]
        assert rm_argv == ["docker", "rm", "-f", "sess"]

    def test_send_text_execs_into_container(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            ref = TmuxSessionRef(session="sess")
            driver.send_text(ref, "hello")
        argv = mock.call_args_list[0][0][0]
        assert argv[:3] == ["docker", "exec", "sess"]
        assert "tmux" in argv and "send-keys" in argv

    def test_capture_execs_into_container_and_returns_stdout(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="captured output", stderr="",
            )
            out = driver.capture(TmuxSessionRef(session="sess"))
        assert out == "captured output"
        argv = mock.call_args[0][0]
        assert argv[:3] == ["docker", "exec", "sess"]

    def test_is_alive_false_when_container_not_running(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="false\n", stderr="",
            )
            alive = driver.is_alive(TmuxSessionRef(session="sess"))
        assert alive is False
        # Only the inspect call is made; tmux has-session is skipped.
        assert mock.call_count == 1

    def test_is_alive_checks_tmux_when_container_running(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.side_effect = [
                subprocess.CompletedProcess(args=[], returncode=0, stdout="true\n", stderr=""),
                subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr=""),
            ]
            alive = driver.is_alive(TmuxSessionRef(session="sess"))
        assert alive is True
        assert mock.call_count == 2

    def test_is_alive_true_on_transient_inspect_timeout(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.side_effect = subprocess.TimeoutExpired(cmd=["docker"], timeout=10)
            alive = driver.is_alive(TmuxSessionRef(session="sess"))
        assert alive is True

    def test_is_alive_true_on_transient_has_session_timeout(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.side_effect = [
                subprocess.CompletedProcess(args=[], returncode=0, stdout="true\n", stderr=""),
                subprocess.TimeoutExpired(cmd=["tmux"], timeout=10),
            ]
            alive = driver.is_alive(TmuxSessionRef(session="sess"))
        assert alive is True

    def test_terminate_kills_tmux_then_removes_container(self):
        driver = ContainerTmuxDriver(docker_image="img")
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=0, stdout="", stderr="",
            )
            driver.terminate(TmuxSessionRef(session="sess"))
        assert mock.call_count == 2
        kill_argv = mock.call_args_list[0][0][0]
        assert kill_argv[:3] == ["docker", "exec", "sess"]
        assert "kill-session" in kill_argv
        rm_argv = mock.call_args_list[1][0][0]
        assert rm_argv == ["docker", "rm", "-f", "sess"]


# ---------------------------------------------------------------------------
# build_tmux_driver — backend selection
# ---------------------------------------------------------------------------


class _Cfg:
    def __init__(self, **kw):
        self.use_fake_tmux = kw.get("use_fake_tmux", False)
        self.backend = kw.get("backend", "host-tmux")
        self.docker_image = kw.get("docker_image", "")
        self.docker_memory = kw.get("docker_memory", "2g")
        self.docker_cpus = kw.get("docker_cpus", "2.0")
        self.docker_pids_limit = kw.get("docker_pids_limit", 512)
        self.docker_network = kw.get("docker_network", None)


class TestBuildTmuxDriver:
    def test_fake_tmux_wins_regardless_of_backend(self):
        cfg = _Cfg(use_fake_tmux=True, backend="docker", docker_image="img")
        assert isinstance(build_tmux_driver(cfg), FakeTmuxDriver)

    def test_host_tmux_backend_default(self):
        cfg = _Cfg()
        assert isinstance(build_tmux_driver(cfg), TmuxDriver)

    def test_docker_backend_returns_container_driver(self):
        cfg = _Cfg(backend="docker", docker_image="my-image")
        driver = build_tmux_driver(cfg)
        assert isinstance(driver, ContainerTmuxDriver)
        assert driver.docker_image == "my-image"

    def test_docker_backend_passes_resource_limits(self):
        cfg = _Cfg(backend="docker", docker_image="img", docker_memory="4g",
                   docker_cpus="1.0", docker_pids_limit=100, docker_network="none")
        driver = build_tmux_driver(cfg)
        assert driver.memory == "4g"
        assert driver.cpus == "1.0"
        assert driver.pids_limit == 100
        assert driver.network == "none"

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

    def test_is_alive_true_on_transient_timeout(self):
        """Regression test for a real bug caught by live-running
        scripts/e2e-composer-live.sh: a transient `tmux has-session`
        timeout (host under load — concurrent docker/pytest/tmux
        activity) used to propagate as an uncaught
        subprocess.TimeoutExpired through classify_state's polling
        loop, permanently crashing the whole harness task
        ("worker_harness_task_crash"). A slow has-session check proves
        nothing about whether the session actually died — assume
        alive so a monitoring hiccup is never worse than an unknown."""
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.side_effect = subprocess.TimeoutExpired(cmd=["tmux"], timeout=5)
            ref = TmuxSessionRef(session="s")
            assert driver.is_alive(ref) is True

    def test_create_session_raises_if_tmux_fails(self):
        driver = TmuxDriver()
        with patch("agents_gateway.harness.tmux.subprocess.run") as mock:
            mock.return_value = subprocess.CompletedProcess(
                args=[], returncode=1, stdout="", stderr="tmux: bad option",
            )
            with pytest.raises(RuntimeError, match="tmux create_session failed"):
                driver.create_session("s", "/w", ["cmd"])
