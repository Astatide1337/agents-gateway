"""Tests for verification runner.

Real subprocesses run against tmp_path worktrees. Live E2E is tested
with a fake credential gate via the env_required path.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from agents_gateway.harness.models import (
    ComposerInteraction,
    VerificationCommand,
    VerificationRun,
    VerificationRunStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.verification import VerificationRunner


@pytest.fixture
def storage(tmp_path):
    return HarnessStorage(str(tmp_path / "harness.db"))


@pytest.fixture
def runner(storage, tmp_path):
    return VerificationRunner(
        storage=storage,
        artifacts_root=str(tmp_path / "artifacts"),
        command_timeout_seconds=20,
    )


@pytest.fixture
def worktree_path(tmp_path):
    wt = tmp_path / "worktree"
    wt.mkdir()
    return str(wt)


# ---------------------------------------------------------------------------
# Basic command execution
# ---------------------------------------------------------------------------


class TestRunCommand:
    def test_passes_when_exit_zero(self, runner, worktree_path):
        cmds = [VerificationCommand(name="echo",
                                     command="echo hello",
                                     required=True)]
        vr = runner.run("run1", "task1", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.passed.value
        assert vr.commands[0].passed
        assert vr.commands[0].exit_code == 0
        assert vr.commands[0].output_artifact
        assert Path(vr.commands[0].output_artifact).exists()
        fetched = runner.storage.get_verification_run_by_agent_run("run1")
        assert fetched is not None

    def test_records_failure_when_exit_nonzero(self, runner, worktree_path):
        cmds = [VerificationCommand(name="false",
                                     command="false",
                                     required=True)]
        vr = runner.run("run2", "task2", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.failed.value
        assert vr.commands[0].passed is False
        assert vr.commands[0].exit_code == 1

    def test_failed_required_command_does_not_pass(self, runner, worktree_path):
        cmds = [
            VerificationCommand(name="ok", command="true", required=True),
            VerificationCommand(name="bad", command="false", required=True),
        ]
        vr = runner.run("run3", "task3", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.failed.value
        assert len(vr.commands) == 2

    def test_optional_failure_does_not_block(self, runner, worktree_path):
        cmds = [
            VerificationCommand(name="ok", command="true", required=True),
            VerificationCommand(name="opt", command="false", required=False),
        ]
        vr = runner.run("run4", "task4", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.passed.value

    def test_command_not_found_recorded(self, runner, worktree_path):
        cmds = [VerificationCommand(name="bad",
                                     command="/no/such/cmd-xyz",
                                     required=True)]
        vr = runner.run("run5", "task5", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.failed.value
        assert vr.commands[0].exit_code == 127
        assert vr.commands[0].passed is False

    def test_command_timeout_recorded(self, runner, worktree_path):
        cmds = [VerificationCommand(name="sleep",
                                     command="sleep 30",
                                     required=True)]
        runner.command_timeout = 1
        vr = runner.run("run6", "task6", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.failed.value
        assert vr.commands[0].exit_code == 124

    def test_shell_cd_chain_runs_via_bash(self, runner, worktree_path):
        """Commands using `cd X && cmd` (cd is a shell builtin) must
        be routed through /bin/bash -c rather than executed directly,
        otherwise argv[0]='cd' returns exit 127.
        """
        cmds = [VerificationCommand(
            name="cd_chain",
            command="cd . && echo ok",
            required=True,
        )]
        vr = runner.run("run_cd_chain", "task_cd", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.passed.value
        assert vr.commands[0].passed
        assert vr.commands[0].exit_code == 0

    def test_pipe_chain_runs_via_bash(self, runner, worktree_path):
        """Commands using shell pipes (|) must be routed through bash."""
        cmds = [VerificationCommand(
            name="pipe_chain",
            command="echo hello | grep hello",
            required=True,
        )]
        vr = runner.run("run_pipe", "task_pipe", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.passed.value
        assert vr.commands[0].exit_code == 0


# ---------------------------------------------------------------------------
# Live E2E credential gate
# ---------------------------------------------------------------------------


class TestLiveE2EBlocked:
    def test_missing_env_marks_blocked(self, runner, worktree_path, monkeypatch):
        for v in ("GITHUB_TOKEN", "CONDUCTOR_TOKEN"):
            monkeypatch.delenv(v, raising=False)
        cmds = [VerificationCommand(
            name="live_e2e", command="bash e2e-live.sh",
            required=True, live_e2e=True,
            env_required=["GITHUB_TOKEN", "CONDUCTOR_TOKEN"],
        )]
        vr = runner.run("run7", "task7", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.blocked.value
        assert vr.commands[0].blocked
        assert "missing_credentials" in vr.commands[0].blocked_reason
        assert "GITHUB_TOKEN" in vr.commands[0].blocked_reason
        assert "CONDUCTOR_TOKEN" in vr.commands[0].blocked_reason

    def test_present_env_runs_command(self, runner, worktree_path, monkeypatch):
        monkeypatch.setenv("GITHUB_TOKEN", "fake-token-for-test")
        cmds = [VerificationCommand(
            name="live_e2e", command="echo OK",
            required=True, live_e2e=True,
            env_required=["GITHUB_TOKEN"],
        )]
        vr = runner.run("run8", "task8", worktree_path, cmds)
        assert vr.status == VerificationRunStatus.passed.value
        assert not vr.commands[0].blocked

    def test_blocked_interactions_created(self, runner, worktree_path, monkeypatch):
        from agents_gateway.harness.models import HarnessSession
        for v in ("GITHUB_TOKEN",):
            monkeypatch.delenv(v, raising=False)
        session = HarnessSession.new(
            agent_run_id="run9", task_id="task9",
            harness_profile="fake-test", harness="fake",
            tmux_session="t9", working_directory=worktree_path,
        )
        runner.storage.save_session(session)
        cmds = [VerificationCommand(
            name="live_e2e", command="echo OK",
            required=True, live_e2e=True,
            env_required=["GITHUB_TOKEN"],
        )]
        vr = runner.run("run9", "task9", worktree_path, cmds, session=session)
        interactions = runner.blocked_interactions(vr, session)
        assert len(interactions) == 1
        assert interactions[0].type == "needs_credentials"
        assert "GITHUB_TOKEN" in interactions[0].metadata.get(
            "blocked_reason", "")


# ---------------------------------------------------------------------------
# Feed-failure-back
# ---------------------------------------------------------------------------


class TestFeedFailureBack:
    def test_feed_back_sends_message_via_driver(self, runner, worktree_path):
        from agents_gateway.harness.driver import HarnessDriver
        from agents_gateway.harness.tmux import FakeTmuxDriver
        from agents_gateway.harness.models import HarnessSession
        fake_tmux = FakeTmuxDriver()
        driver = HarnessDriver(storage=runner.storage, tmux_driver=fake_tmux)
        session = HarnessSession.new(
            agent_run_id="run10", task_id="task10",
            harness_profile="fake-test", harness="fake",
            tmux_session="t10", working_directory=worktree_path,
        )
        runner.storage.save_session(session)
        cmds = [VerificationCommand(name="false", command="false",
                                     required=True)]
        vr = runner.run("run10", "task10", worktree_path, cmds,
                       session=session)
        assert vr.status == VerificationRunStatus.failed.value
        runner.feed_failure_back(vr, session, driver)
        inputs = fake_tmux.inputs.get("t10", [])
        joined = "\n".join(inputs)
        assert "VERIFICATION FEEDBACK" in joined
        assert "false" in joined

    def test_feed_back_no_failed_skips(self, runner, worktree_path):
        from agents_gateway.harness.driver import HarnessDriver
        from agents_gateway.harness.tmux import FakeTmuxDriver
        from agents_gateway.harness.models import HarnessSession
        fake_tmux = FakeTmuxDriver()
        driver = HarnessDriver(storage=runner.storage, tmux_driver=fake_tmux)
        session = HarnessSession.new(
            agent_run_id="run11", task_id="task11",
            harness_profile="fake-test", harness="fake",
            tmux_session="t11", working_directory=worktree_path,
        )
        runner.storage.save_session(session)
        cmds = [VerificationCommand(name="true", command="true",
                                     required=True)]
        vr = runner.run("run11", "task11", worktree_path, cmds,
                       session=session)
        runner.feed_failure_back(vr, session, driver)
        assert fake_tmux.inputs.get("t11", []) == []
