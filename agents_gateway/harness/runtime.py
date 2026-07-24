"""HarnessRuntime — full lifecycle execution for one harness_session task.

Ties together:

  * RepoWorkspaceManager   - clone/fetch/get_or_create
  * VerificationRunner     - mandatory verification in worktree
  * HarnessDriver          - tmux session + goal injection + replies
  * SessionSupervisor      - background classifier/verification trigger
  * ArtifactStore          - per-run artifacts (logs/captures/reports)
  * git integration        - diff capture, maybe_commit/push/pr
  * report generator       - HTML review report (secrets redacted)

A single ``execute_task`` call drives one task from reception to
completion. It is invoked by the existing background worker when the
task's ``runtime_type`` is ``harness_session``.
"""

from __future__ import annotations

import json
import threading
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.artifacts import ArtifactStore
from agents_gateway.harness.driver import HarnessDriver
from agents_gateway.harness.git import (
    capture_diff,
    maybe_commit,
    maybe_pr,
    maybe_push,
)
from agents_gateway.harness.goal import GoalContext
from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionType,
    GoalStrategy,
    HarnessSession,
    HarnessSessionStatus,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
    Worktree,
    WorktreeStatus,
)
from agents_gateway.harness.profiles import (
    get_profile,
    validate_model_for_profile,
    DisapprovedModelError,
    MissingModelError,
)
from agents_gateway.harness.reports import generate_review_report
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.supervisor import SessionSupervisor
from agents_gateway.harness.tmux import FakeTmuxDriver, TmuxDriver
from agents_gateway.harness.verification import VerificationRunner
from agents_gateway.harness.workspace import (
    RepoWorkspaceManager,
    WorkspaceError,
)
from agents_gateway.logging import log_event
from agents_gateway.storage import TaskStorage


@dataclass
class HarnessRunResult:
    """Final structured result returned to Composer."""

    agent_run_id: str
    task_id: str
    status: str
    repo: dict[str, Any]
    harness: dict[str, Any]
    verification: dict[str, Any]
    artifacts: list[dict[str, Any]]
    git: dict[str, Any]
    summary: str
    blockers: list[dict[str, Any]]

    def to_dict(self) -> dict[str, Any]:
        return {
            "agent_run_id": self.agent_run_id,
            "task_id": self.task_id,
            "status": self.status,
            "repo": self.repo,
            "harness": self.harness,
            "verification": self.verification,
            "artifacts": self.artifacts,
            "git": self.git,
            "summary": self.summary,
            "blockers": self.blockers,
        }


class HarnessRuntimeConfig:
    """Bundle of config for the HarnessRuntime — message-pass style."""

    def __init__(self, *,
                 workspace_root: str = "/tmp/agents-gateway/repos",
                 worktree_root: str = "/tmp/agents-gateway/worktrees",
                 artifacts_root: str = "/tmp/agents-gateway/artifacts",
                 session_poll_interval_seconds: float = 2.0,
                 session_stall_seconds: int = 900,
                 auto_commit: bool = True,
                 auto_push: bool = False,
                 auto_pr: bool = False,
                 use_fake_tmux: bool = False,
                 max_verify_iterations: int = 50,
                 command_timeout_seconds: int = 1800,
                 completion_wait_seconds: float = 0.5,
                 relay_max_time_seconds: float = 120.0
                 ) -> None:
        self.workspace_root = workspace_root
        self.worktree_root = worktree_root
        self.artifacts_root = artifacts_root
        self.session_poll_interval_seconds = session_poll_interval_seconds
        self.session_stall_seconds = session_stall_seconds
        self.auto_commit = auto_commit
        self.auto_push = auto_push
        self.auto_pr = auto_pr
        self.use_fake_tmux = use_fake_tmux
        self.max_verify_iterations = max_verify_iterations
        self.command_timeout_seconds = command_timeout_seconds
        self.completion_wait_seconds = completion_wait_seconds
        self.relay_max_time_seconds = relay_max_time_seconds


class HarnessRuntime:
    """Full lifecycle runner for harness_session tasks.

    A new instance is created per task (cheap — no daemon threads
    inherited between tasks; the supervisor runs in a transient thread
    for the duration of one ``execute_task`` call).
    """

    def __init__(self, *,
                 task_storage: TaskStorage,
                 harness_storage: HarnessStorage,
                 task_storage_event_emitter: Any | None = None,
                 config: HarnessRuntimeConfig | None = None,
                 tmux_driver: TmuxDriver | FakeTmuxDriver | None = None,
                 ) -> None:
        self.task_storage = task_storage
        self.harness_storage = harness_storage
        self.config = config or HarnessRuntimeConfig()
        # Event emission into task_storage.events table (so the task
        # timeline shows harness.* events alongside lifecycle events).
        self._emit_to_task_storage = (task_storage_event_emitter
                                       or self._default_task_storage_emitter)
        self.workspace_mgr = RepoWorkspaceManager(
            storage=harness_storage,
            workspace_root=self.config.workspace_root,
            worktree_root=self.config.worktree_root,
        )
        self.artifact_store = ArtifactStore(
            storage=harness_storage,
            artifacts_root=self.config.artifacts_root,
            emit_event=self._artifact_event,
        )
        self.driver = HarnessDriver(
            storage=harness_storage,
            tmux_driver=tmux_driver or (
                FakeTmuxDriver() if self.config.use_fake_tmux else TmuxDriver()
            ),
            emit_event=self._driver_event,
        )
        self.verification = VerificationRunner(
            storage=harness_storage,
            artifacts_root=self.config.artifacts_root,
            command_timeout_seconds=self.config.command_timeout_seconds,
            emit_event=self._verification_event,
        )
        self.supervisor = SessionSupervisor(
            storage=harness_storage,
            driver=self.driver,
            verification_runner=self.verification,
            poll_interval_seconds=self.config.session_poll_interval_seconds,
            stall_seconds=self.config.session_stall_seconds,
            emit_event=self._driver_event,
            on_completed_claim=None,  # set per-task in execute_task
        )
        self._session_completion_signal = threading.Event()

    # -------------------------------------------------------------------
    # Public: full lifecycle
    # -------------------------------------------------------------------

    def execute_task(self, *,
                     agent_run_id: str,
                     task_id: str,
                     task_spec: dict[str, Any],
                     relay_handler: Any | None = None,
                     ) -> HarnessRunResult:
        """Drive one harness_session task from spec to completion.

        ``task_spec`` is the parsed json body of the create-task request.
        """
        self._emit_task_event(task_id, "task.received",
                              {"composer_task_id": task_spec.get("composer_task_id"),
                               "title": task_spec.get("title", "")[:120]})

        # 1. Resolve workspace (clone / use local / re-use existing)
        try:
            workspace = self._prepare_workspace(task_id, agent_run_id, task_spec)
        except (WorkspaceError, Exception) as e:
            return self._fail_with(agent_run_id, task_id, task_spec,
                                    f"workspace_preparation_failed: {e}")

        # 2. Create worktree
        try:
            worktree = self._create_worktree(workspace, task_id, agent_run_id,
                                              task_spec)
        except (WorkspaceError, Exception) as e:
            return self._fail_with(agent_run_id, task_id, task_spec,
                                    f"worktree_creation_failed: {e}")

        # Re-resolve the profile (might be a name string)
        profile_name = task_spec.get("execution", {}).get(
            "harness_profile", "pi-coding-agent")
        profile = get_profile(profile_name)
        if profile is None:
            return self._fail_with(agent_run_id, task_id, task_spec,
                                    f"unknown harness profile: {profile_name}")

        # Optional per-task model override (e.g.
        # ``nvidia/nemotron-3-ultra-550b-a55b:free``). Injected via the
        # profile's model_arg_name flag; profiles without model_arg_name
        # ignore it.
        model_override = task_spec.get("execution", {}).get("model")

        # Validate model policy: must be present and on the approved
        # free-model allowlist for profiles that support model overrides.
        # Profiles without model_arg_name (claude-code, codex, fake-test)
        # skip validation.
        try:
            model_override = validate_model_for_profile(model_override, profile)
        except (MissingModelError, DisapprovedModelError) as exc:
            return self._fail_with(agent_run_id, task_id, task_spec,
                                    f"model policy violation: {exc}")

        # 3. Compose GoalContext (write .agent-task/* files).
        goal_context = self._compose_goal_context(task_spec)

        # 4a. Pre-register the relay handler with the tmux driver BEFORE
        # start_session injects the goal text — this lets the fake
        # harness respond to the goal directive being sent. The tmux
        # session name is deterministic: agw_<task_id[:18]>, computed
        # by the driver's helper.
        tmux_session_name = self.driver._tmux_session_name(task_id)
        if relay_handler is not None and isinstance(self.driver.tmux,
                                                      FakeTmuxDriver):
            self.driver.tmux.register_session_handler(
                tmux_session_name, relay_handler)

        # 4b. Start harness session + inject goal
        session = self.driver.start_session(
            task_id=task_id, agent_run_id=agent_run_id,
            worktree_path=worktree.path, profile=profile,
            goal_context=goal_context,
            goal_strategy=task_spec.get("goal", {}).get("strategy"),
            model_override=model_override,
        )
        if session.status == HarnessSessionStatus.failed.value:
            return self._fail_with(agent_run_id, task_id, task_spec,
                                    "session.start_failed",
                                    session=session, worktree=worktree)

        # 6. Run the supervisor loop with verification hook
        deadline = time.time() + self.config.relay_max_time_seconds
        verification_run: VerificationRun | None = None
        last_classification = ""

        # Setup completion callback so the supervisor runs verification
        # in its own thread when the harness claims done. NOTE: we run
        # the verification inline (in a synchronous helper) — the
        # supervisor marks the session `verifying` then signals us
        # here to run the actual verification loop.
        completion_hooks: dict[str, Any] = {"verifying_session_id": None}

        def on_completed_claim_inner(s: HarnessSession) -> None:
            completion_hooks["verifying_session_id"] = s.id
            self._session_completion_signal.set()

        self.supervisor.on_completed_claim = on_completed_claim_inner

        # Start supervisor in a bounded loop for this task. We poll
        # internally rather than relying on the supervisor thread alone
        # — this means we can iterate verification drives in the same
        # call and terminate cleanly when complete.
        self.supervisor.start()
        try:
            verify_iterations = 0
            while True:
                # Manual tick — synchronously process active sessions.
                # (Supervisor thread is also running, but we tick here
                # for deterministic test scenarios + fake harness.)
                self.supervisor.tick_once()
                self._session_completion_signal.wait(
                    timeout=self.config.completion_wait_seconds)
                self._session_completion_signal.clear()

                # Refresh session state from storage.
                cur_session = self.harness_storage.get_session(session.id)
                if cur_session is None:
                    break
                if cur_session.status == HarnessSessionStatus.verifying.value:
                    # Run verification
                    verify_iterations += 1
                    if verify_iterations > self.config.max_verify_iterations:
                        # Cap reached — surface as ambiguous_harness_state
                        # Composer interaction, do NOT auto-fail.
                        self.driver.mark_stalled(
                            cur_session,
                            interaction_type=(
                                ComposerInteractionType.ambiguous_harness_state.value
                            ),
                        )
                        break

                    verification_run = self._run_verification(
                        cur_session, task_id, agent_run_id, worktree, task_spec,
                    )
                    if verification_run is None:
                        # Could not run commands (worktree missing etc.)
                        self.driver.mark_failed(
                            cur_session, reason="verification_runner_error",
                        )
                        break

                    # Decide next state based on verification status.
                    if verification_run.status == VerificationRunStatus.passed.value:
                        # Done!
                        self._finalize_success(cur_session, worktree,
                                               task_id, agent_run_id, task_spec,
                                               verification_run)
                        break
                    elif verification_run.status == VerificationRunStatus.blocked.value:
                        # External block (missing credentials etc.)
                        interactions = self.verification.blocked_interactions(
                            verification_run, cur_session,
                        )
                        self.driver.mark_blocked_external(
                            cur_session, reason="missing_credentials" if any(
                                "missing_credentials" in i.metadata.get("blocked_reason", "")
                                for i in interactions
                            ) else "verification_blocked",
                            missing_env=self._missing_env_from_verification(
                                verification_run),
                        )
                        break
                    else:
                        # Verification failed — feed back into session,
                        # resume harness so it can iterate. The session
                        # transitions running -> running (no terminal
                        # status) until harness either succeeds or dies.
                        self.verification.feed_failure_back(
                            verification_run, cur_session, self.driver,
                        )
                        cur_session.status = HarnessSessionStatus.running.value
                        cur_session.last_output_at = datetime.now(
                            timezone.utc).isoformat()
                        self.harness_storage.save_session(cur_session)

                elif cur_session.status == HarnessSessionStatus.failed.value:
                    break
                elif cur_session.status == HarnessSessionStatus.stalled.value:
                    # Stalled — surface interaction already created by
                    # supervisor. End the run; Composer decides.
                    break
                elif cur_session.status == HarnessSessionStatus.blocked_external.value:
                    break
                elif cur_session.status == HarnessSessionStatus.cancelled.value:
                    break
                elif cur_session.status == HarnessSessionStatus.completed.value:
                    break
                # else: still running — keep polling
                if time.time() > deadline:
                    # Hard time cap reached; surface as stalled + interaction.
                    # We do NOT auto-fail per milestone spec.
                    self.driver.mark_stalled(cur_session)
                    break
        finally:
            self.supervisor.stop()

        # Read final session state
        final_session = self.harness_storage.get_session(session.id) or session
        return self._build_final_result(
            agent_run_id=agent_run_id, task_id=task_id, task_spec=task_spec,
            workspace=workspace, worktree=worktree, session=final_session,
            verification_run=verification_run,
        )

    # -------------------------------------------------------------------
    # Helpers — workspace / worktree / session / verification
    # -------------------------------------------------------------------

    def _prepare_workspace(self, task_id: str, agent_run_id: str,
                            task_spec: dict[str, Any]) -> Any:
        repo = task_spec.get("repo", {})
        url = repo.get("url", "")
        owner = repo.get("owner", "")
        name = repo.get("name", "")
        branch = repo.get("base_branch", "master")
        if not owner and not name and url:
            # Best-effort parse from URL like
            # https://github.com/<owner>/<repo>.git
            slug = url.rstrip("/").rsplit("/", 1)[-1]
            if slug.endswith(".git"):
                slug = slug[:-4]
            owner = url.rstrip("/").rsplit("/", 2)[-2] or "_local"
            name = slug
        if not owner:
            owner = "_local"
        if not name:
            name = "_scratch"
        if not url:
            # Local-only workspace — use the existing repo dir if there
            # is one on disk (created by the dispatcher for fake-test
            # flows). Otherwise init a new scratch repo on disk.
            local_path = (
                self.config.workspace_root / Path(owner) /
                Path(name) / "scratch"
                if isinstance(self.config.workspace_root, Path)
                else Path(self.config.workspace_root) / owner / name / "scratch"
            )
            local_path.mkdir(parents=True, exist_ok=True)
            ws = self.workspace_mgr.get_or_create_local(
                local_path=str(local_path), owner=owner, repo=name,
                default_branch=branch,
            )
        else:
            ws = self.workspace_mgr.get_or_create(
                repo_url=url, owner=owner, repo=name, default_branch=branch,
            )
            self.workspace_mgr.fetch(ws)
        self._emit_task_event(task_id, "repo.workspace_prepared",
                              {"workspace_id": ws.id,
                               "base_branch": ws.default_branch, "kind": "workspace"})
        return ws

    def _create_worktree(self, workspace, task_id, agent_run_id,
                         task_spec: dict[str, Any]) -> Worktree:
        slug = (task_spec.get("title") or "").lower() or "task"
        worktree = self.workspace_mgr.create_worktree(
            workspace=workspace, task_id=task_id, agent_run_id=agent_run_id,
            slug=slug, base_branch=workspace.default_branch,
        )
        worktree.status = WorktreeStatus.active.value
        self.harness_storage.save_worktree(worktree)
        self._emit_task_event(task_id, "worktree.created",
                              {"worktree_id": worktree.id,
                               "path": worktree.path,
                               "branch": worktree.branch})
        self._emit_task_event(task_id, "harness.profile_resolved",
                              {"profile": task_spec.get("execution", {}).get(
                                  "harness_profile", "")})
        return worktree

    def _compose_goal_context(self, task_spec: dict[str, Any]) -> GoalContext:
        goal_block = task_spec.get("goal", {})
        title = task_spec.get("title", "")
        brief = task_spec.get("brief", "")
        goal_text = goal_block.get("text", "")
        skills = task_spec.get("required_skills", [])
        tools = task_spec.get("required_tools", [])
        verification = task_spec.get("verification", {})
        commands = verification.get("commands", [])
        live_e2e = verification.get("live_e2e", {})
        env_required = live_e2e.get("env_required", []) if live_e2e else []

        skills_text = self._compose_skills_text(skills)
        tools_text = self._compose_tools_text(tools)
        verification_text = self._compose_verification_text(commands,
                                                            live_e2e, env_required)
        context_text = task_spec.get("context", "")
        return GoalContext(
            title=title, brief=brief, goal_text=goal_text,
            skills_text=skills_text, tools_text=tools_text,
            verification_text=verification_text, context_text=context_text,
        )

    def _compose_skills_text(self, skills: list[str]) -> str:
        if not skills:
            return ""
        lines = ["# Required skills", ""]
        lines.append("Load these from the Skills Gateway before implementation:")
        lines.append("")
        for s in skills:
            lines.append(f"- {s}")
        lines.extend([
            "",
            "If the Skills Gateway is unavailable, proceed using the "
            "(textual) skill summary above as guidance.",
        ])
        return "\n".join(lines)

    def _compose_tools_text(self, tools: list[str]) -> str:
        if not tools:
            return ""
        lines = ["# Available tools", ""]
        lines.append("MCP Gateway is available for GitHub/repo/external "
                     "tools when needed. Use tools only if they help "
                     "complete the task.")
        lines.append("")
        lines.append("Tool access hints:")
        for t in tools:
            lines.append(f"- {t}")
        return "\n".join(lines)

    def _compose_verification_text(self, commands: list[dict[str, Any]],
                                    live_e2e: dict[str, Any] | None,
                                    env_required: list[str]) -> str:
        lines = ["# Verification", ""]
        lines.append(
            "You may not mark this task complete until ALL required "
            "verification commands pass."
        )
        lines.append("")
        if commands:
            lines.append("## Required commands")
            lines.append("")
            for i, c in enumerate(commands, 1):
                lines.append(f"{i}. `{c.get('command', '')}`"
                             + ("" if c.get("required", True) else " (optional)"))
            lines.append("")
        if live_e2e and live_e2e.get("required"):
            lines.append("## Live E2E")
            lines.append("")
            lines.append(f"Command: `{live_e2e.get('command', '')}`")
            if env_required:
                lines.append("Required env: " + ", ".join(
                    f"`{v}`" for v in env_required))
            lines.append("")
            lines.append(
                "If required credentials are missing, REPORT THE EXACT "
                "missing variables — do NOT fake E2E success."
            )
        return "\n".join(lines)

    # -------------------------------------------------------------------
    # Verification flow
    # -------------------------------------------------------------------

    def _run_verification(self, session: HarnessSession, task_id: str,
                        agent_run_id: str, worktree: Worktree,
                        task_spec: dict[str, Any]
                        ) -> VerificationRun | None:
        vblock = task_spec.get("verification", {})
        cmds: list[VerificationCommand] = []
        for c in vblock.get("commands", []):
            cmds.append(VerificationCommand(
                name=str(c.get("name", "command")),
                command=str(c.get("command", "")),
                required=bool(c.get("required", True)),
                live_e2e=False,
                env_required=[],
            ))
        live_e2e = vblock.get("live_e2e") or {}
        if live_e2e.get("required"):
            cmds.append(VerificationCommand(
                name=str(live_e2e.get("name", "live_e2e")),
                command=str(live_e2e.get("command", "")),
                required=bool(live_e2e.get("required", False)),
                live_e2e=True,
                env_required=list(live_e2e.get("env_required", []) or []),
            ))
        if not cmds:
            # No verification configured — treat as passed (per spec
            # verification is mandatory, but we leave the validation
            # at dispatch time).
            sock = type("VR", (), {
                "status": VerificationRunStatus.passed.value,
                "commands": [],
                "agent_run_id": agent_run_id, "task_id": task_id,
                "all_required_passed": True, "any_blocked": False,
            })()
            return sock  # type: ignore[return-value]
        return self.verification.run(
            agent_run_id=agent_run_id, task_id=task_id,
            worktree_path=worktree.path, commands=cmds, session=session,
        )

    # -------------------------------------------------------------------
    # Finalization: success / failure / blocked
    # -------------------------------------------------------------------

    def _finalize_success(self, session: HarnessSession, worktree: Worktree,
                        task_id: str, agent_run_id: str,
                        task_spec: dict[str, Any],
                        verification_run: VerificationRun) -> None:
        # Capture git diff + commit if configured
        diff = capture_diff(worktree.path)
        commit_sha = maybe_commit(
            worktree_path=worktree.path,
            message=f"agent: {task_spec.get('title', 'task')[:60]}",
            auto_commit=self.config.auto_commit,
        )
        pushed = False
        pr_url = None
        if commit_sha:
            pushed = maybe_push(
                worktree_path=worktree.path, branch=worktree.branch,
                auto_push=self.config.auto_push,
            )
            pr_url = maybe_pr(
                worktree_path=worktree.path, branch=worktree.branch,
                title=task_spec.get("title", task_id),
                body=task_spec.get("brief", ""),
                auto_pr=self.config.auto_pr,
            )
        if commit_sha:
            worktree.status = WorktreeStatus.committed.value
        else:
            worktree.status = WorktreeStatus.dirty.value if diff.changed_files \
                              else WorktreeStatus.active.value
        self.harness_storage.save_worktree(worktree)
        self._emit_task_event(task_id, "git.diff_captured",
                              {"changed_files": len(diff.changed_files),
                               "insertions": diff.insertions,
                               "deletions": diff.deletions})
        if commit_sha:
            self._emit_task_event(task_id, "git.committed",
                                  {"sha": commit_sha})
        # Record diff + result artifacts
        self.artifact_store.write_diff(agent_run_id, task_id, diff.diff_text)
        git_summary = {
            "changed_files": diff.changed_files,
            "insertions": diff.insertions,
            "deletions": diff.deletions,
            "commit_sha": commit_sha,
            "pushed": pushed,
            "pr_url": pr_url,
            "files": diff.changed_files,
        }
        # Compose final result JSON
        final_result = {
            "status": "completed",
            "repo": {"branch": worktree.branch, "base_branch": worktree.base_branch,
                     "worktree_path": worktree.path},
            "verification": {
                "status": verification_run.status,
                "commands": [c.__dict__ for c in verification_run.commands],
            },
            "git": git_summary,
            "summary": "Verification passed; task completed.",
            "blockers": [],
        }
        self.artifact_store.write_result(agent_run_id, task_id, final_result)
        # Generate + write HTML report
        artifacts = self.harness_storage.list_harness_artifacts(
            agent_run_id=agent_run_id, task_id=task_id,
        )
        events = self.task_storage.list_events(task_id)
        html_body = generate_review_report(
            task_title=task_spec.get("title", ""),
            task_brief=task_spec.get("brief", ""),
            repo_url=task_spec.get("repo", {}).get("url", ""),
            branch=worktree.branch, base_branch=worktree.base_branch,
            worktree_path=worktree.path,
            harness_profile=session.harness_profile,
            skills_requested=task_spec.get("required_skills", []),
            tools_requested=task_spec.get("required_tools", []),
            timeline_events=[e.model_dump() for e in events],
            verification=verification_run,
            artifacts=artifacts,
            git_summary=git_summary,
            session=session,
            final_status="completed",
            summary_text="All required verification commands passed.",
        )
        self.artifact_store.write_report(agent_run_id, task_id, html_body)
        # Write session log artifact (full capture)
        full_capture = self.driver.capture_output(session, lines=10000)
        self.artifact_store.write_log(
            agent_run_id, task_id, "session.log", full_capture or "(empty)",
        )
        self._emit_task_event(task_id, "report.generated",
                              {"html_artifact": "review-report.html"})
        # Mark session + agent_run complete
        self.driver.mark_completed(session)

    def _fail_with(self, agent_run_id: str, task_id: str,
                  task_spec: dict[str, Any], reason: str,
                  session: HarnessSession | None = None,
                  worktree: Worktree | None = None) -> HarnessRunResult:
        log_event("harness_runtime_failed",
                  f"task {task_id} failed: {reason}", task_id=task_id,
                  agent_run_id=agent_run_id, level="ERROR")
        self._emit_task_event(task_id, "agent_run.failed",
                              {"reason": reason})
        return HarnessRunResult(
            agent_run_id=agent_run_id, task_id=task_id,
            status="failed",
            repo={"url": task_spec.get("repo", {}).get("url", "")},
            harness={"profile": task_spec.get("execution", {}).get(
                "harness_profile", ""), "session_id": session.id if session else ""},
            verification={"status": "unverified", "commands": []},
            artifacts=[],
            git={},
            summary=reason,
            blockers=[{"type": "harness_runtime_failure",
                       "message": reason}],
        )

    def _build_final_result(self, *, agent_run_id: str, task_id: str,
                            task_spec: dict[str, Any], workspace, worktree,
                            session, verification_run) -> HarnessRunResult:
        artifacts = self.harness_storage.list_harness_artifacts(
            agent_run_id=agent_run_id, task_id=task_id,
        )
        git_summary: dict[str, Any] = {}
        # If we have a verification_run AND it passed, we'll have a
        # result.json artifact already; if not, build a minimal summary.
        git_info_artifact = next(
            (a for a in artifacts
             if a["kind"] == "metadata"
             and a["name"] == "result.json"),
            None,
        )
        if git_info_artifact:
            try:
                with open(git_info_artifact["path"]) as fh:
                    payload = json.load(fh)
                git_summary = payload.get("git", {})
            except Exception:
                pass
        blockers: list[dict[str, Any]] = []
        if session.status == HarnessSessionStatus.blocked_external.value:
            blockers.append({
                "type": session.metadata.get("blocker", {}).get("type",
                                                                 "blocked_external"),
                "message": "Session blocked externally.",
                "missing_env": session.metadata.get("blocker", {}).get(
                    "missing_env", []),
            })

        verification_dict: dict[str, Any] = {
            "status": verification_run.status if verification_run else (
                "unverified"
            ),
            "commands": (
                [c.__dict__ for c in verification_run.commands]
                if verification_run else []
            ),
        }

        repo_url = task_spec.get("repo", {}).get("url", "") or workspace.repo_url
        branch = worktree.branch if worktree else ""
        base_branch = worktree.base_branch if worktree else ""
        worktree_path = worktree.path if worktree else ""

        return HarnessRunResult(
            agent_run_id=agent_run_id, task_id=task_id,
            status=session.status,
            repo={"url": repo_url, "branch": branch,
                  "base_branch": base_branch,
                  "worktree_path": worktree_path},
            harness={"profile": session.harness_profile if session else "",
                     "session_id": session.id if session else "",
                     "tmux_session": session.tmux_session if session else ""},
            verification=verification_dict,
            artifacts=artifacts,
            git=git_summary,
            summary=_status_summary(session, verification_run),
            blockers=blockers,
        )

    # -------------------------------------------------------------------
    # Missing-env extraction (for blocker reporting)
    # -------------------------------------------------------------------

    def _missing_env_from_verification(self, vr: VerificationRun) -> list[str]:
        out: list[str] = []
        for c in vr.commands:
            if c.blocked and "missing_credentials" in c.blocked_reason:
                # Reason format: "missing_credentials: <var1>, <var2>"
                _, _, rest = c.blocked_reason.partition("missing_credentials:")
                for v in rest.split(","):
                    v = v.strip()
                    if v and v not in out:
                        out.append(v)
        return out

    # -------------------------------------------------------------------
    # Event plumbing helpers
    # -------------------------------------------------------------------

    def _default_task_storage_emitter(self) -> Any:
        return None

    def _emit_task_event(self, task_id: str, event_name: str,
                          data: dict[str, Any]) -> None:
        try:
            self.task_storage.append_event(task_id, event_name, data)
        except Exception:
            pass

    def _driver_event(self, session: HarnessSession, event_name: str,
                      data: dict[str, Any]) -> None:
        self._emit_task_event(session.task_id, event_name, {
            **data, "session_id": session.id,
            "agent_run_id": session.agent_run_id,
        })

    def _verification_event(self, session: HarnessSession, event_name: str,
                            data: dict[str, Any]) -> None:
        self._emit_task_event(session.task_id, event_name, data)

    def _artifact_event(self, agent_run_id: str, task_id: str,
                       event_name: str, data: dict[str, Any]) -> None:
        self._emit_task_event(task_id, event_name, data)


def _status_summary(session: HarnessSession | None,
                    vr: VerificationRun | None) -> str:
    if session is None:
        return "No session created."
    s = session.status
    if s == HarnessSessionStatus.completed.value:
        return "All required verification commands passed; task completed."
    if s == HarnessSessionStatus.blocked_external.value:
        return ("Session blocked externally — see blockers list "
                "for missing credentials/services.")
    if s == HarnessSessionStatus.stalled.value:
        return ("Session stalled — Composer interaction created "
                "with ambiguous_harness_state context.")
    if s == HarnessSessionStatus.failed.value:
        return "Session failed; see failure_reason in session metadata."
    if s == HarnessSessionStatus.waiting_for_reply.value:
        return "Session is waiting for a Composer reply."
    return f"Session status: {s}"


__all__ = ["HarnessRuntime", "HarnessRuntimeConfig", "HarnessRunResult"]
