"""HarnessDriver: orchestrates a real coding harness inside one worktree.

The driver takes a task brief and a worktree, picks the right harness
profile, starts a tmux session, injects the goal, captures output,
supervises state, and exposes reply/stop surfaces for Composer.

It is intentionally synchronous-on-start / async-on-supervision:

  * ``start_session``   - synchronous; spawns tmux session, injects goal
  * ``capture_output``  - synchronous; returns the recent tmux capture
  * ``send_reply``      - synchronous; sends Composer's reply text
  * ``stop_session``    - synchronous; kills the tmux session
  * ``classify_state``  - synchronous; uses the classifier module

The supervisor (separate module) calls these on a poll interval.
"""

from __future__ import annotations

import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.classifier import (
    ClassifierResult,
    HarnessState,
    classify_state,
)
from agents_gateway.harness.goal import (
    GoalContext,
    GoalInjectionResult,
    inject_goal,
)
from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
)
from agents_gateway.harness.profiles import (
    HarnessProfile,
    get_default_profile,
    get_profile,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import (
    FakeTmuxDriver,
    TmuxDriver,
    TmuxSessionRef,
)


class HarnessDriverError(Exception):
    pass


class HarnessDriver:
    """Driver layer between Composer dispatch and the tmux subprocess plane."""

    def __init__(self, storage: HarnessStorage,
                 tmux_driver: TmuxDriver | FakeTmuxDriver | None = None,
                 session_prefix: str = "agw_",
                 capture_lines: int = 2000,
                 emit_event: Any | None = None) -> None:
        self.storage = storage
        self.tmux = tmux_driver or TmuxDriver()
        self.session_prefix = session_prefix
        self.capture_lines = capture_lines
        # Track the last captured output signature per session to
        # preserve stall-detection semantics (see capture_output).
        self._last_capture: dict[str, str] = {}
        # emit_event is optional; if provided it's called as
        # emit_event(session, event_name, data) by the driver. This lets
        # the dispatcher wire it to TaskStorage.append_event without a
        # hard dependency here.
        self.emit_event = emit_event or (lambda *a, **kw: None)

    # -------------------------------------------------------------------
    # Session lifecycle
    # -------------------------------------------------------------------

    def start_session(self, task_id: str, agent_run_id: str,
                      worktree_path: str,
                      profile: HarnessProfile | str | None = None,
                      goal_context: GoalContext | None = None,
                      goal_strategy: str | None = None
                      ) -> HarnessSession:
        """Bootstrap a harness session for one task + worktree."""
        if isinstance(profile, str):
            profile = get_profile(profile) or get_default_profile()
        elif profile is None:
            profile = get_default_profile()

        # Compose the spawn command: profile.command + profile.args
        # + a marker argv so the harness can identify itself.
        cmd_parts = [profile.command] + list(profile.args)
        # Sanitize against empty argv (would break tmux).
        cmd_parts = [p for p in cmd_parts if p]
        if not cmd_parts:
            raise HarnessDriverError(
                f"Profile '{profile.name}' command is empty"
            )

        # Construct an idempotent-ish tmux session name based on task
        # id (truncated). This is safe because task ids are random UUIDs.
        tmux_session = self._tmux_session_name(task_id)
        ref = self.tmux.create_session(
            session_name=tmux_session, cwd=worktree_path, command=cmd_parts,
        )

        session = HarnessSession(
            id=self._new_session_id(),
            agent_run_id=agent_run_id, task_id=task_id,
            harness_profile=profile.name, harness=profile.harness,
            runtime="tmux" if not isinstance(self.tmux, FakeTmuxDriver) else "tmux-fake",
            tmux_session=ref.session, tmux_window=ref.window, tmux_pane=ref.pane,
            working_directory=worktree_path,
            status=HarnessSessionStatus.starting.value,
        )
        self.storage.save_session(session)
        self._emit(session, "session.created", {"profile": profile.name})

        # Wait for the harness process to be ready before injecting the
        # goal. Full-screen TUI harnesses (opencode, claude-code) need
        # time to render their UI after spawning; if we send text too
        # early the keystrokes are lost. We poll the tmux pane until it
        # has content (indicating the TUI has rendered) or a short
        # timeout expires. The FakeTmuxDriver doesn't need this.
        if not isinstance(self.tmux, FakeTmuxDriver):
            import time as _time
            _ready_deadadline = _time.time() + 15.0
            while _time.time() < _ready_deadadline:
                try:
                    _early = self.tmux.capture(self._ref(session), lines=50)
                except Exception:
                    _early = ""
                if _early and _early.strip():
                    break
                _time.sleep(1.0)

        # Inject goal if provided.
        if goal_context is not None:
            try:
                self.inject_goal(session, goal_context,
                                requested_strategy=goal_strategy)
            except Exception as e:
                import traceback as _tb
                self._emit(session, "session.goal_injection_failed",
                           {"error": str(e), "trace": _tb.format_exc()})
                session.status = HarnessSessionStatus.failed.value
                session.ended_at = datetime.now(timezone.utc).isoformat()
                session.metadata = dict(session.metadata or {})
                session.metadata["goal_injection_error"] = str(e)
                session.metadata["goal_injection_trace"] = _tb.format_exc()
                self.storage.save_session(session)
                return session

        # Mark the session running even before the harness has spoken —
        # the supervisor will adjust state via classify_state.
        session.status = HarnessSessionStatus.running.value
        session.last_output_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_session(session)
        self._emit(session, "session.started", {})
        return session

    def inject_goal(self, session: HarnessSession,
                    ctx: GoalContext,
                    requested_strategy: str | None = None) -> GoalInjectionResult:
        """Write .agent-task/* files and send the directive into tmux."""
        ref = self._ref(session)
        result = inject_goal(
            worktree_path=session.working_directory,
            profile=self._profile_for(session),
            ctx=ctx, requested_strategy=requested_strategy,
        )
        # Send the directive text in two parts: the file-based directive
        # + the actual goal text (so harnesses that don't read files
        # still get the goal). We send it as two chunks rather than one
        # long block to keep tmux send-keys line lengths reasonable.
        self.tmux.send_text(ref, result.sent_text)
        self.tmux.send_enter(ref)
        self._emit(session, "goal.injected",
                   {"strategy": result.strategy,
                    "files_written": result.files_written})
        return result

    def capture_output(self, session: HarnessSession,
                       lines: int | None = None) -> str:
        """Return recent tmux capture; update last_output_at.

        The session's ``last_output_at`` field is only updated when the
        captured output is non-empty AND differs from the last captured
        blob for this session. This preserves stall-detection semantics:
        if nothing has changed since the previous capture, the timestamp
        reflects the time of the last *new* output, not merely the
        time of our latest poll.
        """
        ref = self._ref(session)
        capture = self.tmux.capture(ref, lines=lines or self.capture_lines)
        if not capture:
            return capture
        # Track previous capture per session to detect real churn.
        prev = self._last_capture.get(session.id, "")
        if capture != prev:
            self._last_capture[session.id] = capture
            session.last_output_at = datetime.now(timezone.utc).isoformat()
            self.storage.save_session(session)
            self._emit(session, "agent.output_captured",
                       {"bytes": len(capture)})
        return capture

    def classify_state(self, session: HarnessSession,
                       stall_seconds: int = 900,
                       now_override: str | None = None) -> ClassifierResult:
        """Helper wrapper around the classifier using session storage."""
        output = self.capture_output(session)
        alive = self.tmux.is_alive(self._ref(session))
        return classify_state(
            output=output,
            last_output_at=session.last_output_at,
            now=now_override, stall_seconds=stall_seconds,
            process_alive=alive,
        )

    def send_reply(self, session: HarnessSession, reply_text: str) -> None:
        """Send a Composer reply into the session.

        Composer replies are wrapped in a clear "ASSISTANT REPLY:" header
        so the agent can distinguish them from its own echoed input.
        """
        ref = self._ref(session)
        header = "ASSISTANT REPLY (from Composer):"
        for line in (header + "\n" + reply_text).splitlines() or [""]:
            self.tmux.send_text(ref, line)
            self.tmux.send_enter(ref)
        # Set status back to running; the supervisor will adjust.
        session.status = HarnessSessionStatus.running.value
        session.last_output_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_session(session)
        self._emit(session, "agent.resumed", {"reply_chars": len(reply_text)})

    def mark_waiting_for_reply(self, session: HarnessSession,
                               excerpt: str = "") -> ComposerInteraction:
        """Create a pending Composer interaction for the session."""
        session.status = HarnessSessionStatus.waiting_for_reply.value
        self.storage.save_session(session)
        # Capture full context so Composer can read it.
        capture = self.capture_output(session)
        interaction = ComposerInteraction.new(
            agent_run_id=session.agent_run_id,
            task_id=session.task_id,
            session_id=session.id,
            type_=ComposerInteractionType.needs_reply.value,
            prompt_excerpt=excerpt or capture[-400:],
            full_context_ref=f"capture://session/{session.id}",
            metadata={"capture_length": len(capture)},
        )
        self.storage.save_interaction(interaction)
        self._emit(session, "agent.waiting_for_reply",
                   {"interaction_id": interaction.id})
        self._emit(session, "composer.interaction.created",
                   {"interaction_id": interaction.id})
        return interaction

    def mark_verifying(self, session: HarnessSession) -> None:
        """Harness claimed completion: transition to verifying."""
        session.status = HarnessSessionStatus.verifying.value
        self.storage.save_session(session)
        self._emit(session, "agent.claimed_complete", {})

    def mark_stalled(self, session: HarnessSession,
                    interaction_type: str =
                    ComposerInteractionType.ambiguous_harness_state.value
                    ) -> ComposerInteraction:
        """Stall detected: surface as ambiguous_harness_state interaction."""
        session.status = HarnessSessionStatus.stalled.value
        self.storage.save_session(session)
        capture = self.capture_output(session)
        interaction = ComposerInteraction.new(
            agent_run_id=session.agent_run_id,
            task_id=session.task_id, session_id=session.id,
            type_=interaction_type,
            prompt_excerpt="No measurable progress from harness for "
                          "configured stall duration.",
            full_context_ref=f"capture://session/{session.id}",
            metadata={"capture_length": len(capture)},
        )
        self.storage.save_interaction(interaction)
        self._emit(session, "composer.interaction.created",
                   {"interaction_id": interaction.id, "kind": interaction_type})
        return interaction

    def mark_completed(self, session: HarnessSession) -> None:
        session.status = HarnessSessionStatus.completed.value
        session.ended_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_session(session)
        self._emit(session, "agent_run.completed", {})

    def mark_failed(self, session: HarnessSession, reason: str = "") -> None:
        session.status = HarnessSessionStatus.failed.value
        session.ended_at = datetime.now(timezone.utc).isoformat()
        if reason:
            session.metadata = dict(session.metadata)
            session.metadata["failure_reason"] = reason
        self.storage.save_session(session)
        self._emit(session, "agent_run.failed", {"reason": reason})

    def mark_blocked_external(self, session: HarnessSession,
                              reason: str, missing_env: list[str] | None = None
                              ) -> None:
        session.status = HarnessSessionStatus.blocked_external.value
        session.ended_at = datetime.now(timezone.utc).isoformat()
        session.metadata = dict(session.metadata)
        session.metadata["blocker"] = {
            "type": reason,
            "missing_env": list(missing_env or []),
        }
        self.storage.save_session(session)
        self._emit(session, "agent_run.blocked_external",
                   {"reason": reason, "missing_env": list(missing_env or [])})

    def stop_session(self, session: HarnessSession) -> None:
        """Forcibly stop the harness session."""
        ref = self._ref(session)
        try:
            self.tmux.terminate(ref)
        except Exception:
            pass
        if session.status not in (HarnessSessionStatus.completed.value,
                                   HarnessSessionStatus.failed.value):
            session.status = HarnessSessionStatus.cancelled.value
        session.ended_at = session.ended_at or datetime.now(timezone.utc).isoformat()
        self.storage.save_session(session)
        self._emit(session, "session.stopped", {"final_status": session.status})

    # -------------------------------------------------------------------
    # helpers
    # -------------------------------------------------------------------

    def _ref(self, session: HarnessSession) -> TmuxSessionRef:
        return TmuxSessionRef(session=session.tmux_session,
                              window=session.tmux_window,
                              pane=session.tmux_pane)

    def _profile_for(self, session: HarnessSession) -> HarnessProfile:
        return get_profile(session.harness_profile) or get_default_profile()

    def _tmux_session_name(self, task_id: str) -> str:
        return f"{self.session_prefix}{task_id[:18]}"

    def _new_session_id(self) -> str:
        import uuid as _u
        return f"session_{_u.uuid4().hex[:12]}"

    def _emit(self, session: HarnessSession, event: str,
              data: dict[str, Any]) -> None:
        try:
            self.emit_event(session, event, data)
        except Exception:
            pass


__all__ = ["HarnessDriver", "HarnessDriverError"]
