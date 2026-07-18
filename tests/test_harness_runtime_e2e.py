"""Full harness runtime E2E via FakeTmuxDriver + fake-test harness profile.

Three mandated flows:

  1. Easy-complete: harness writes a file, prints DONE, verification
     passes, completed.
  2. Asks-question + reply: harness asks a clarifying question,
     Composer replies via interaction, harness continues, passes.
  3. Fail-then-fix: harness claims done with broken file, verification
     fails, failure fed back, harness fixes, second verification passes,
     completed.

Each test fakes tmux via ``FakeTmuxDriver``. A "fake harness" is modeled
as a Python callable registered via ``register_session_handler``. The
FakeTmuxDriver invokes the handler on EVERY send_text and send_enter
call, with the text (and ``is_enter=False`` for text, True for Enter).
Most relays need both: the "Enter" event triggers action based on the
last received text. We use a small helper ``BufferedRelay`` to model
that pattern.
"""

from __future__ import annotations

import os
import subprocess
import threading
import time
from pathlib import Path

import pytest

from agents_gateway.harness.driver import HarnessDriver
from agents_gateway.harness.models import (
    ComposerInteractionStatus,
    HarnessSessionStatus,
    WorktreeStatus,
)
from agents_gateway.harness.profiles import get_profile
from agents_gateway.harness.runtime import (
    HarnessRuntime,
    HarnessRuntimeConfig,
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
        capture_output=True, text=True, timeout=30, env=env,
    )


def _make_scratch_repo(tmp_path: Path) -> str:
    repo = tmp_path / "scratch-repo"
    repo.mkdir()
    proc = _git(str(repo), "init", "-b", "master")
    if proc.returncode != 0:
        _git(str(repo), "init")
        _git(str(repo), "symbolic-ref", "HEAD", "refs/heads/master")
    (repo / "README.md").write_text("# Scratch repo\n")
    _git(str(repo), "add", "README.md")
    _git(str(repo), "commit", "-m", "Initial commit")
    return str(repo)


def _runtime(tmp_path, task_storage):
    fake_tmux = FakeTmuxDriver()
    hs = HarnessStorage(str(tmp_path / "harness.db"))
    hcfg = HarnessRuntimeConfig(
        workspace_root=str(tmp_path / "repos"),
        worktree_root=str(tmp_path / "worktrees"),
        artifacts_root=str(tmp_path / "artifacts"),
        session_poll_interval_seconds=0.05,
        session_stall_seconds=900,
        auto_commit=False,
        auto_push=False,
        auto_pr=False,
        use_fake_tmux=True,
        max_verify_iterations=10,
        command_timeout_seconds=20,
        completion_wait_seconds=0.02,
        relay_max_time_seconds=15.0,
    )
    runtime = HarnessRuntime(
        task_storage=task_storage,
        harness_storage=hs,
        task_storage_event_emitter=task_storage,
        config=hcfg,
        tmux_driver=fake_tmux,
    )
    return runtime, fake_tmux


def _make_task_spec(scratch_repo: str, goal_text: str,
                    verification_commands: list[dict]) -> dict:
    return {
        "title": "Test task",
        "brief": "Test",
        "repo": {"url": "file://" + scratch_repo, "owner": "o",
                 "name": "r", "base_branch": "master"},
        "execution": {"mode": "harness_session",
                      "harness_profile": "fake-test"},
        "goal": {"strategy": "auto", "text": goal_text},
        "verification": {"required": True,
                         "commands": verification_commands},
        "artifacts": {"html_report": True},
    }


class BufferedRelay:
    """Base class for fake-harness relays.

    Buffers text input until Enter is received, then calls
    ``on_submit(text_so_far)`` with the concatenated pending text.
    """

    def __init__(self) -> None:
        self.worktree_path: str | None = None
        self.calls = 0
        self._pending: list[str] = []

    def __call__(self, driver, session_name, text, is_enter):
        self.calls += 1
        if not is_enter:
            self._pending.append(text or "")
            return
        # Enter — flush pending + the literal "<Enter>" placeholder
        full_text = "\n".join(self._pending + [""])
        self._pending.clear()
        try:
            self.on_submit(driver, session_name, full_text)
        except Exception as e:
            print(f"RELAY ERROR in on_submit: {e}", flush=True)

    def on_submit(self, driver, session_name, text: str) -> None:
        raise NotImplementedError


# ---------------------------------------------------------------------------
# Flow 1: easy complete
# ---------------------------------------------------------------------------


class TestEasyComplete:
    def test_writes_file_and_passes_verification(self, tmp_path):
        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch,
            goal_text="/goal Write result.txt. AGENT_SCRATCH_FILE:result.txt",
            verification_commands=[
                {"name": "check file exists", "command": "ls result.txt",
                 "required": True},
            ],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        class EasyCompleteRelay(BufferedRelay):
            def __init__(self):
                super().__init__()
                self.completed = False

            def on_submit(self, driver, session_name, text):
                lower = text.lower()
                if "agent_scratch_file" not in lower and "/goal" not in lower:
                    return
                if self.worktree_path is None:
                    return
                scratch_file = "result.txt"
                for line in lower.splitlines():
                    if "agent_scratch_file:" in line:
                        scratch_file = line.split(
                            "agent_scratch_file:", 1)[1].strip()
                        break
                target = Path(self.worktree_path) / scratch_file
                target.write_text("harness output\n")
                driver.push_output(session_name, "Working on goal...\n")
                driver.push_output(session_name, "DONE.\n")
                driver.mark_closed(session_name)
                self.completed = True

        relay_instance = EasyCompleteRelay()
        orig_start = runtime.driver.start_session

        def start_session_wrap(*, task_id, agent_run_id, worktree_path,
                               profile, goal_context, goal_strategy=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
            )

        runtime.driver.start_session = start_session_wrap  # type: ignore

        result = runtime.execute_task(
            agent_run_id=task.id, task_id=task.id, task_spec=spec,
            relay_handler=relay_instance,
        )

        assert relay_instance.completed
        assert result.status == HarnessSessionStatus.completed.value
        artifacts = result.artifacts
        assert any(a["kind"] == "html_report" for a in artifacts)


# ---------------------------------------------------------------------------
# Flow 2: ask-question + reply
# ---------------------------------------------------------------------------


class TestAskQuestionWithReply:
    def test_asks_then_replies_then_completes(self, tmp_path):
        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch,
            goal_text="/goal Write result.txt. AGENT_SCRATCH_FILE:result.txt "
                      "AGENT_ASK_QUESTION:true",
            verification_commands=[
                {"name": "check file exists", "command": "ls result.txt",
                 "required": True},
            ],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        class AskRelay(BufferedRelay):
            def __init__(self):
                super().__init__()
                self.phase = "ask"
                self.asked = False
                self.finished = False

            def on_submit(self, driver, session_name, text):
                lower = text.lower()
                if "/goal" in lower and not self.asked:
                    # Ask a question.
                    driver.push_output(
                        session_name,
                        "I need clarification: should the file be "
                        "uppercase or lowercase?\n",
                    )
                    self.asked = True
                    self.phase = "wait_reply"
                    return
                if "assistant reply" in lower:
                    # Composer replied — write file + complete.
                    if self.worktree_path is None:
                        return
                    target = Path(self.worktree_path) / "result.txt"
                    target.write_text("harness output\n")
                    driver.push_output(session_name, "Working on goal...\n")
                    driver.push_output(session_name, "DONE.\n")
                    driver.mark_closed(session_name)
                    self.phase = "done"
                    self.finished = True
                    return

        relay_instance = AskRelay()
        orig_start = runtime.driver.start_session

        def start_session_wrap(*, task_id, agent_run_id, worktree_path,
                               profile, goal_context, goal_strategy=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
            )

        runtime.driver.start_session = start_session_wrap  # type: ignore

        # Composer reply thread: poll pending interactions and answer.
        stop = threading.Event()

        def composer_replier():
            while not stop.is_set():
                hs = runtime.harness_storage
                pending = hs.list_pending_interactions()
                for interaction in pending:
                    hs.update_interaction_status(
                        interaction_id=interaction.id,
                        status=ComposerInteractionStatus.answered.value,
                        composer_reply="Use lowercase. Proceed per spec.",
                    )
                    sess = hs.get_session(interaction.session_id)
                    if sess is not None:
                        runtime.driver.send_reply(
                            sess, "Use lowercase. Proceed per spec.")
                time.sleep(0.05)

        thread = threading.Thread(target=composer_replier, daemon=True)
        thread.start()
        try:
            result = runtime.execute_task(
                agent_run_id=task.id, task_id=task.id, task_spec=spec,
                relay_handler=relay_instance,
            )
        finally:
            stop.set()
        thread.join(timeout=2.0)

        assert relay_instance.asked
        assert relay_instance.finished
        assert result.status == HarnessSessionStatus.completed.value
        interactions = runtime.harness_storage.list_interactions(
            status=ComposerInteractionStatus.answered.value)
        assert any(i.task_id == task.id for i in interactions)


# ---------------------------------------------------------------------------
# Flow 3: fail-then-fix via verification feedback
# ---------------------------------------------------------------------------


class TestFailThenFix:
    def test_fails_then_fixes(self, tmp_path):
        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch,
            goal_text="/goal Write result.txt with content '42'. "
                      "AGENT_SCRATCH_FILE:result.txt",
            verification_commands=[
                {"name": "check file contents",
                 "command": "grep -q '^42$' result.txt",
                 "required": True},
            ],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        attempts = {"n": 0}

        class FailThenFixRelay(BufferedRelay):
            def __init__(self):
                super().__init__()
                self.first_attempt = False
                self.fix_attempted = False

            def on_submit(self, driver, session_name, text):
                lower = text.lower()
                if "/goal" in lower and not self.first_attempt:
                    # First attempt: write wrong content
                    if self.worktree_path is None:
                        return
                    (Path(self.worktree_path) / "result.txt").write_text(
                        "wrong\n")
                    driver.push_output(session_name, "Wrote file.\n")
                    driver.push_output(session_name, "DONE.\n")
                    driver.mark_closed(session_name)
                    self.first_attempt = True
                    return
                if "verification feedback" in lower and not self.fix_attempted:
                    # Verification failed — fix the file
                    if self.worktree_path is None:
                        return
                    attempts["n"] += 1
                    (Path(self.worktree_path) / "result.txt").write_text(
                        "42\n")
                    driver.push_output(
                        session_name, "Fixed file contents.\n")
                    driver.push_output(session_name, "DONE.\n")
                    driver.mark_closed(session_name)
                    self.fix_attempted = True
                    return

        relay_instance = FailThenFixRelay()
        orig_start = runtime.driver.start_session

        def start_session_wrap(*, task_id, agent_run_id, worktree_path,
                               profile, goal_context, goal_strategy=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
            )

        runtime.driver.start_session = start_session_wrap  # type: ignore

        result = runtime.execute_task(
            agent_run_id=task.id, task_id=task.id, task_spec=spec,
            relay_handler=relay_instance,
        )

        assert relay_instance.first_attempt
        assert attempts["n"] >= 1
        assert result.status == HarnessSessionStatus.completed.value
        v = result.verification
        assert v["status"] == "passed"
