"""Verification runner: executes mandatory verification commands in
the task worktree, captures stdout/stderr as artifacts, and returns a
structured VerificationRun.

Key behaviours required by the spec:

  * A failed required command BLOCKS task completion but does NOT fail
    the agent_run — the failure summary is fed back into the harness
    session so the agent can continue fixing. The session stays
    ``running``. Only the harness's own end/exit (or a verified
    pass) ends the run.
  * Live E2E commands with missing required credentials are reported
    as blocked (``blocked_external`` + ``missing_credentials``). This
    is a valid blocker. The dispatcher surfaces a
    ComposerInteraction so a human can grant access later.
  * Each command's output goes into a per-run artifact directory under
    ``artifacts/<agent_run_id>/logs/verification-<name>.txt``.
"""

from __future__ import annotations

import os
import shlex
import subprocess
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
)
from agents_gateway.harness.storage import HarnessStorage


class VerificationError(Exception):
    pass


class VerificationRunner:
    """Run verification commands in a task worktree, capture artifacts.

    The runner is sandboxing-agnostic: it assumes the worktree path is
    a trusted location (set up by RepoWorkspaceManager). For
    containerised future work, the runner can be wrapped to invoke a
    container exec instead of subprocess directly.
    """

    def __init__(self, storage: HarnessStorage,
                 artifacts_root: str = "/var/lib/agents-gateway/artifacts",
                 command_timeout_seconds: int = 1800,
                 emit_event: Any | None = None) -> None:
        self.storage = storage
        self.artifacts_root = Path(artifacts_root)
        self.command_timeout = command_timeout_seconds
        # emit_event(session, name, data) optional callback.
        self.emit_event = emit_event or (lambda *a, **kw: None)

    # -------------------------------------------------------------------
    # Public API
    # -------------------------------------------------------------------

    def run(self, agent_run_id: str, task_id: str, worktree_path: str,
            commands: list[VerificationCommand],
            session: HarnessSession | None = None) -> VerificationRun:
        """Execute commands sequentially; blocking commands stop the run."""
        vr = VerificationRun.new(agent_run_id=agent_run_id, task_id=task_id)
        vr.status = VerificationRunStatus.running.value
        vr.started_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_verification_run(vr)
        if session is not None:
            self._emit(session, "verification.started",
                       {"verification_run_id": vr.id,
                        "command_count": len(commands)})

        run_artifact_root = self._run_artifact_root(agent_run_id)
        logs_dir = run_artifact_root / "logs"
        logs_dir.mkdir(parents=True, exist_ok=True)

        any_blocked = False
        any_required_failed = False

        for cmd in commands:
            self._emit(session, "verification.command_started",
                       {"name": cmd.name, "command": cmd.command})
            result = self._run_one(cmd, worktree_path, logs_dir, agent_run_id,
                                   task_id)
            vr.commands.append(result)
            self.storage.save_verification_run(vr)
            event_name = ("verification.command_passed"
                          if result.passed and not result.blocked
                          else "verification.command_failed"
                          if not result.blocked
                          else "verification.command_blocked")
            self._emit(session, event_name,
                       {"name": cmd.name, "passed": result.passed,
                        "blocked": result.blocked,
                        "exit_code": result.exit_code})

            if result.blocked:
                any_blocked = True
                # Stop running further commands if a required command is
                # blocked by external dependencies.
                if cmd.required:
                    break
            elif not result.passed and cmd.required:
                any_required_failed = True
                # Per spec: failed verification feeds back into the
                # harness. We DO NOT mark the agent_run as failed here.
                continue
            elif not result.passed and not cmd.required:
                # Optional command failed — record and continue.
                continue

        if any_blocked:
            vr.status = VerificationRunStatus.blocked.value
        elif any_required_failed:
            vr.status = VerificationRunStatus.failed.value
        else:
            vr.status = VerificationRunStatus.passed.value
        vr.completed_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_verification_run(vr)
        self._emit(session, "verification.completed",
                   {"status": vr.status})
        if vr.status == VerificationRunStatus.passed.value:
            self._emit(session, "verification.passed", {})
        elif vr.status == VerificationRunStatus.failed.value:
            self._emit(session, "verification.failed", {})
        else:
            self._emit(session, "verification.blocked", {})
        return vr

    def feed_failure_back(self, vr: VerificationRun,
                        session: HarnessSession,
                        driver: Any) -> None:
        """Push a verification failure summary back into the harness.

        The driver is supplied so we can call ``driver.send_reply``
        without circular imports. The harness is told which commands
        failed and told to continue fixing.
        """
        failed = [c for c in vr.commands
                  if not c.passed and not c.blocked and c.required]
        if not failed:
            return
        lines = [
            "VERIFICATION FEEDBACK (from Agents Gateway):",
            f"{len(failed)} required verification command(s) failed:",
            "",
        ]
        for cmd in failed:
            lines.append(f"- {cmd.name}: exit_code={cmd.exit_code}")
            lines.append(f"  command: {cmd.command}")
            if cmd.output_artifact:
                lines.append(f"  output_artifact: {cmd.output_artifact}")
            lines.append("")
        lines.extend([
            "Continue fixing until all required verification commands pass.",
            "Do not mark this task complete until they do.",
        ])
        self._emit(session, "verification.failed_feedback_sent",
                   {"failed_count": len(failed),
                    "names": [c.name for c in failed]})
        driver.send_reply(session, "\n".join(lines))

    def blocked_interactions(self, vr: VerificationRun,
                             session: HarnessSession) -> list[ComposerInteraction]:
        """Create Composer interactions for blocked commands (e.g. live E2E
        with missing credentials)."""
        interactions: list[ComposerInteraction] = []
        for c in vr.commands:
            if not c.blocked:
                continue
            inter = ComposerInteraction.new(
                agent_run_id=session.agent_run_id,
                task_id=session.task_id,
                session_id=session.id,
                type_=ComposerInteractionType.needs_credentials.value
                if "missing_credentials" in c.blocked_reason
                else ComposerInteractionType.external_blocker.value,
                prompt_excerpt=(
                    f"Verification command '{c.name}' blocked: "
                    f"{c.blocked_reason}"
                ),
                metadata={"command_name": c.name,
                           "command": c.command,
                           "blocked_reason": c.blocked_reason},
            )
            self.storage.save_interaction(inter)
            interactions.append(inter)
        return interactions

    # -------------------------------------------------------------------
    # Per-command runner
    # -------------------------------------------------------------------

    def _run_one(self, cmd: VerificationCommand, worktree_path: str,
                 logs_dir: Path, agent_run_id: str, task_id: str
                 ) -> VerificationCommandResult:
        # Live E2E credential gate: if any env_required var is missing,
        # mark command as blocked (do NOT execute).
        if cmd.live_e2e and cmd.env_required:
            missing = [v for v in cmd.env_required
                       if not os.environ.get(v)]
            if missing:
                return VerificationCommandResult(
                    name=cmd.name, command=cmd.command, required=cmd.required,
                    blocked=True,
                    blocked_reason=("missing_credentials: "
                                    + ", ".join(missing)),
                )

        # Parse command into argv. Use shlex so quoted strings survive.
        try:
            argv = shlex.split(cmd.command)
        except ValueError as e:
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                passed=False, exit_code=None,
                output_artifact="",
            )

        if not argv:
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                passed=False, exit_code=None,
            )

        # If the command contains shell metacharacters (use of shell
        # builtins like ``cd``, redirect operators like ``>``, logical
        # operators like ``&&``, or pipe ``|``), executing the argv
        # directly will fail — ``cd`` is a shell builtin and ``&&`` is
        # not a binary in PATH. Detect these cases and route through
        # ``/bin/bash -c`` instead so command chains work as intended.
        #
        # Also route ``uv run pytest`` (and ``uv sync``) through bash
        # when the worktree cwd contains ``:`` — uv parses the absolute
        # cwd looking for ``:`` path separators and fails with
        # ``error: path segment contains separator ':'`` when the cwd
        # has any. Worktrees under ``git@github.com:owner/repo/...``
        # always do. The bash wrapper avoids that path collision
        # because the colon only appears in PATH/argv strings passed
        # to uv, while the actual cwd lookup is via /bin/bash cwd.
        shell_tokens = {"&&", "||", ";", "|", ">", ">>", "<", "&"}
        argv0 = argv[0] if argv else ""
        cwd_has_colon = ":" in str(worktree_path)
        uv_invocation = argv0 == "uv" and len(argv) >= 2
        needs_shell = (
            any(tok in argv for tok in shell_tokens)
            or argv0 in ("cd", "source", "export", "exec", "pushd", "popd")
            or any("${" in a or "`" in a or "$(" in a for a in argv)
            or (cwd_has_colon and uv_invocation)
        )
        if needs_shell:
            argv = ["/bin/bash", "-c", cmd.command]

        # Execute inside the worktree with a clean environment. We do
        # NOT inherit from os.environ wholesale to avoid leaking gateway
        # secrets into verification subprocesses; we pass through only
        # an explicit allow-list.
        env = self._safe_env()
        cwd = Path(worktree_path)
        if not cwd.exists():
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                passed=False, exit_code=None,
                blocked_reason=f"worktree_path does not exist: {cwd}",
            )

        stdout_path = logs_dir / f"verification-{_safe_name(cmd.name)}.txt"
        stdout_path.parent.mkdir(parents=True, exist_ok=True)
        start = time.time()
        try:
            proc = subprocess.run(
                argv, cwd=str(cwd), env=env,
                capture_output=True, text=True,
                timeout=self.command_timeout,
            )
        except FileNotFoundError:
            stdout_path.write_text(
                f"Command not found: {argv[0]}\n")
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                exit_code=127, passed=False,
                output_artifact=str(stdout_path),
                duration_seconds=time.time() - start,
            )
        except subprocess.TimeoutExpired:
            stdout_path.write_text(
                f"Command timed out after {self.command_timeout}s\n")
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                exit_code=124, passed=False,
                output_artifact=str(stdout_path),
                duration_seconds=time.time() - start,
            )
        except Exception as e:
            stdout_path.write_text(f"Command crashed: {e}\n")
            return VerificationCommandResult(
                name=cmd.name, command=cmd.command, required=cmd.required,
                exit_code=1, passed=False,
                output_artifact=str(stdout_path),
                duration_seconds=time.time() - start,
            )

        # Record full output (stdout + stderr) into the artifact.
        full_output = (
            f"$ {cmd.command}\n"
            f"exit_code={proc.returncode}\n\n"
            f"--- stdout ---\n{proc.stdout}\n"
            f"--- stderr ---\n{proc.stderr}\n"
        )
        stdout_path.write_text(full_output)
        artifact = self.storage.add_harness_artifact(
            agent_run_id=agent_run_id, task_id=task_id,
            kind="test_output" if not cmd.live_e2e else "live_e2e_output",
            name=stdout_path.name, path=str(stdout_path),
            mime_type="text/plain", size_bytes=len(full_output.encode()),
            metadata={"command": cmd.command, "name": cmd.name,
                       "exit_code": proc.returncode,
                       "required": cmd.required},
        )
        return VerificationCommandResult(
            name=cmd.name, command=cmd.command, required=cmd.required,
            exit_code=proc.returncode,
            passed=(proc.returncode == 0),
            output_artifact=artifact["path"],
            duration_seconds=time.time() - start,
        )

    def _run_artifact_root(self, agent_run_id: str) -> Path:
        d = self.artifacts_root / agent_run_id
        d.mkdir(parents=True, exist_ok=True)
        return d

    def _safe_env(self) -> dict[str, str]:
        """Minimal env for verification subprocesses.

        We pass PATH, HOME (for tool discovery), LANG/LC_ALL (to avoid
        encoding crashes in pytest output), and explicit allow-listed
        variables. We do NOT pass gateway secrets/tokens to the
        verification subprocess — they're unnecessary for unit tests
        and would leak into captured artifacts.
        """
        env: dict[str, str] = {}
        for key in ("PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE",
                    "TERM", "SHELL", "USER", "USERNAME", "PYTEST_DISABLE_PLUGIN_AUTOLOAD"):
            v = os.environ.get(key)
            if v is not None:
                env[key] = v
        # Allow-listed overrides — explicit allow-list of variables
        # that the task spec exposes to verification. This is the right
        # place to add benign test-only envs.
        for key in ("UV_CACHE_DIR", "PYTHONPATH", "VIRTUAL_ENV"):
            v = os.environ.get(key)
            if v is not None:
                env[key] = v
        return env

    def _emit(self, session: HarnessSession | None, event: str,
              data: dict[str, Any]) -> None:
        if session is None:
            return
        try:
            self.emit_event(session, event, data)
        except Exception:
            pass


def _safe_name(name: str) -> str:
    safe = []
    for ch in name.strip():
        if ch.isalnum() or ch in ("-", "_"):
            safe.append(ch)
        else:
            safe.append("-")
    return "".join(safe)[:60] or "command"


__all__ = ["VerificationError", "VerificationRunner"]
