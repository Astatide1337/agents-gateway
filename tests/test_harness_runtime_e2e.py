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
                               profile, goal_context, goal_strategy=None,
                               model_override=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
                model_override=model_override,
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


class TestNoVerificationConfigured:
    def test_completes_without_crashing_when_no_verification_commands(
        self, tmp_path,
    ):
        """Regression test for a real bug caught by dispatching a raw
        task with no verification block (no test harness beyond
        Composer's own always-populated one had ever exercised this):
        _run_verification's "no verification configured" branch builds
        a fake stand-in VerificationRun via `type("VR", (), {...})()`
        missing the `metadata` attribute real VerificationRun always
        has. generate_review_report unconditionally reads
        `verification.metadata`, so ANY task with an empty verification
        list crashed at the finalize-success step with
        AttributeError: 'VR' object has no attribute 'metadata' —
        every single time, deterministically, with zero prior test
        coverage of this path at all."""
        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch, goal_text="/goal Say hello.",
            verification_commands=[],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        class NoVerificationRelay(BufferedRelay):
            def __init__(self):
                super().__init__()
                self.completed = False

            def on_submit(self, driver, session_name, text):
                if "/goal" not in text.lower():
                    return
                driver.push_output(session_name, "Hello!\n")
                driver.push_output(session_name, "DONE.\n")
                driver.mark_closed(session_name)
                self.completed = True

        relay_instance = NoVerificationRelay()
        result = runtime.execute_task(
            agent_run_id=task.id, task_id=task.id, task_spec=spec,
            relay_handler=relay_instance,
        )


class TestResumeTask:
    """resume_task() covers the case execute_task() never had to
    handle: a task whose original execute_task() call was abandoned
    mid-flight (the process that ran it died) rather than one this
    process is driving start-to-finish. Live incident this regresses:
    an AGW restart orphaned two in-flight harness_session tasks —
    TaskWorker only claims status='queued' rows, so neither was ever
    picked up again; one had actually finished (needed only
    finalization) and the other was still mid-turn (needed continued
    driving without re-creating its already-existing worktree, which
    execute_task() would fail on)."""

    def test_returns_none_when_no_session_ever_existed(self, tmp_path):
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        result = runtime.resume_task(task_id="never-started", task_spec={})
        assert result is None

    def test_short_circuits_for_already_terminal_session(self, tmp_path):
        """Mirrors the live backend task: the harness had already
        declared completion (and, in the real incident, verification
        would still need to run — here we drive a real task to a
        terminal state via execute_task() first) before anything
        called resume. resume_task() must report the existing final
        state without attempting to drive the session further."""
        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch, goal_text="/goal Say hello.",
            verification_commands=[],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        class NoVerificationRelay(BufferedRelay):
            def on_submit(self, driver, session_name, text):
                if "/goal" not in text.lower():
                    return
                driver.push_output(session_name, "Hello!\n")
                driver.push_output(session_name, "DONE.\n")
                driver.mark_closed(session_name)

        first_result = runtime.execute_task(
            agent_run_id=task.id, task_id=task.id, task_spec=spec,
            relay_handler=NoVerificationRelay(),
        )
        assert first_result.status == HarnessSessionStatus.completed.value

        drive_calls = []
        runtime._drive_session = lambda **kw: drive_calls.append(kw) or None

        resumed = runtime.resume_task(task_id=task.id, task_spec=spec)
        assert resumed is not None
        assert resumed.status == HarnessSessionStatus.completed.value
        assert not drive_calls, "already-terminal session must not be re-driven"

    def test_does_not_recreate_worktree_for_in_progress_session(self, tmp_path):
        """The core regression: resume_task() must reuse the existing
        worktree, never call create_worktree() again — that call
        raises defensively when the path already exists (see
        RepoWorkspaceManager.create_worktree's "never reuse a worktree
        for a different task" guard), which is the exact
        worktree_creation_failed failure mode a naive re-dispatch hit
        in production."""
        from agents_gateway.harness.models import (
            HarnessSession,
        )

        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, fake_tmux = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch, goal_text="/goal Say hello.",
            verification_commands=[],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        # Build real workspace + worktree records the normal way
        # (proves resume_task reuses genuine on-disk state), but never
        # start a session — simulates the process dying between
        # worktree creation and session start.
        workspace = runtime._prepare_workspace(task.id, task.id, spec)
        worktree = runtime._create_worktree(workspace, task.id, task.id, spec)

        session = HarnessSession.new(
            agent_run_id=task.id, task_id=task.id, harness_profile="fake-test",
            harness="fake-test", tmux_session=f"agw_{task.id[:18]}",
            working_directory=worktree.path, runtime="tmux",
        )
        session.status = HarnessSessionStatus.running.value
        runtime.harness_storage.save_session(session)

        def create_worktree_should_not_be_called(*a, **kw):
            raise AssertionError(
                "resume_task must not call create_worktree for an "
                "already-existing worktree"
            )
        runtime.workspace_mgr.create_worktree = create_worktree_should_not_be_called

        drive_calls = []
        def fake_drive_session(**kw):
            drive_calls.append(kw)
            class FakeResult:
                status = "completed"
                artifacts = []
            return FakeResult()
        runtime._drive_session = fake_drive_session

        result = runtime.resume_task(task_id=task.id, task_spec=spec)

        assert result is not None
        assert result.status == "completed"
        assert len(drive_calls) == 1
        assert drive_calls[0]["worktree"].id == worktree.id
        assert drive_calls[0]["workspace"].id == workspace.id
        assert drive_calls[0]["session"].id == session.id

    def test_reattaches_json_mode_session_before_driving(self, tmp_path):
        """Real incident this regresses: resume_task() drove a
        genuinely-finished JSON-mode (opencode) session straight into
        the supervisor loop without reattaching it first. OpencodeJson
        Driver keeps ALL session state in the spawning process's
        memory (see its own module docstring) — this HarnessRuntime's
        driver never spawned the session, so classify_state()'s
        is_alive() call raised "unknown session" on every tick,
        silently swallowed by the supervisor's per-session try/except,
        and the drive loop spun with no progress until
        relay_max_time_seconds gave up and marked a session that had
        actually completed as `stalled` instead."""
        from agents_gateway.harness.models import HarnessSession

        scratch = _make_scratch_repo(tmp_path)
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        spec = _make_task_spec(
            scratch, goal_text="/goal Say hello.", verification_commands=[],
        )
        task = task_storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"},
        )
        task_storage.update_task_status(task.id, "queued")
        task_storage.update_task_status(task.id, "running")

        workspace = runtime._prepare_workspace(task.id, task.id, spec)
        worktree = runtime._create_worktree(workspace, task.id, task.id, spec)

        session = HarnessSession.new(
            agent_run_id=task.id, task_id=task.id, harness_profile="fake-test",
            harness="fake-test", tmux_session=f"agw_{task.id[:18]}",
            working_directory=worktree.path, runtime="process-json",
        )
        session.status = HarnessSessionStatus.running.value
        session.metadata = {"json_pid": 999999999}  # not a real PID
        runtime.harness_storage.save_session(session)

        reattach_calls = []
        orig_reattach = runtime.driver.json_driver.reattach
        def spy_reattach(session_name, cwd, pid, **kw):
            reattach_calls.append((session_name, cwd, pid))
            return orig_reattach(session_name, cwd, pid, **kw)
        runtime.driver.json_driver.reattach = spy_reattach

        runtime._drive_session = lambda **kw: None  # isolate the reattach step

        runtime.resume_task(task_id=task.id, task_spec=spec)

        assert reattach_calls == [(session.tmux_session, worktree.path, 999999999)]


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
                               profile, goal_context, goal_strategy=None,
                               model_override=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
                model_override=model_override,
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
                               profile, goal_context, goal_strategy=None,
                               model_override=None):
            relay_instance.worktree_path = worktree_path
            return orig_start(
                task_id=task_id, agent_run_id=agent_run_id,
                worktree_path=worktree_path, profile=profile,
                goal_context=goal_context, goal_strategy=goal_strategy,
                model_override=model_override,
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


class TestFrontendDesignQualityInjection:
    """A dispatched agent never actually calls out to the Skills Gateway
    in practice (confirmed live: zero such calls across a real multi-hour
    build) — so a skill name in required_skills alone does nothing.
    Goal-text injection is the one channel proven to reach the agent;
    the anti-slop design-quality standard must ride along on it whenever
    a task touches UI the user will actually look at."""

    def test_frontend_skill_triggers_design_quality_text(self, tmp_path):
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        text = runtime._compose_skills_text(["javascript", "html", "css"])
        assert "Frontend design quality" in text
        assert "shadcn/ui" in text
        assert "Never use emoji as UI chrome" in text

    def test_backend_only_skills_do_not_trigger_it(self, tmp_path):
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        text = runtime._compose_skills_text(["python", "fastapi", "sqlite"])
        assert "Frontend design quality" not in text
        assert "shadcn/ui" not in text

    def test_no_skills_returns_empty(self, tmp_path):
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        assert runtime._compose_skills_text([]) == ""

    def test_marker_matching_is_case_insensitive(self, tmp_path):
        ts_db = str(tmp_path / "task-storage.db")
        task_storage = TaskStorage(ts_db)
        runtime, _ = _runtime(tmp_path, task_storage)
        text = runtime._compose_skills_text(["JavaScript", "HTML5-Audio"])
        assert "Frontend design quality" in text
