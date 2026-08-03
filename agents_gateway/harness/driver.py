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

import os
import shlex
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.anthropic_compat_proxy import ensure_proxy_running
from agents_gateway.harness.classifier import (
    ClassifierResult,
    HarnessState,
    classify_json_transcript,
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
    WireProtocol,
)
from agents_gateway.harness.profiles import (
    HarnessProfile,
    get_default_profile,
    get_profile,
)
from agents_gateway.harness.process_json import OpencodeJsonDriver, get_default_json_driver
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import (
    FakeTmuxDriver,
    TmuxDriver,
    TmuxSessionRef,
)

# session.runtime value for sessions running under OpencodeJsonDriver
# (see profiles.py's input_mode="process_json" and _driver_for below).
_JSON_RUNTIME = "process-json"


class HarnessDriverError(Exception):
    pass


_ANTHROPIC_COMPAT_PROXY_PORT_ENV = "AGW_ANTHROPIC_COMPAT_PROXY_PORT"
_DEFAULT_ANTHROPIC_COMPAT_PROXY_PORT = 8199


def _wire_protocol_env_prefix(profile: HarnessProfile, effective_model: str | None) -> list[str]:
    """Shell-quoted KEY=value tokens to prepend to argv so a harness CLI
    locked to one wire protocol can reach a provider outside its native
    ecosystem through the matching adapter. Empty when no adapter
    applies (bare launch, or the profile needs none)."""
    if profile.wire_protocol != WireProtocol.anthropic_messages or not effective_model:
        return []
    port = int(os.environ.get(_ANTHROPIC_COMPAT_PROXY_PORT_ENV, _DEFAULT_ANTHROPIC_COMPAT_PROXY_PORT))
    ensure_proxy_running(port)
    env = {"ANTHROPIC_BASE_URL": f"http://127.0.0.1:{port}", "ANTHROPIC_API_KEY": "proxy-managed"}
    return [f"{k}={shlex.quote(v)}" for k, v in env.items()]


def _distinctive_marker(sent_text: str, min_len: int = 20) -> str:
    """Pick a substring of injected goal text that can only appear in
    a tmux capture if the injection genuinely registered — used to
    verify goal injection landed (see start_session's retry logic).

    Prefers the ".agent-task/GOAL.md" file-based marker (present in
    every non-slash_goal strategy) since it's the most stable across
    line-wrapping; falls back to the first substantive line of the
    text itself (e.g. for slash_goal, which has no file marker).
    """
    if ".agent-task/GOAL.md" in sent_text:
        return ".agent-task/GOAL.md"
    for line in sent_text.splitlines():
        stripped = line.strip()
        if len(stripped) >= min_len:
            return stripped[:min_len]
    return sent_text.strip()[:min_len]


class HarnessDriver:
    """Driver layer between Composer dispatch and the tmux subprocess plane."""

    def __init__(self, storage: HarnessStorage,
                 tmux_driver: TmuxDriver | FakeTmuxDriver | None = None,
                 json_driver: OpencodeJsonDriver | None = None,
                 session_prefix: str = "agw_",
                 capture_lines: int = 2000,
                 emit_event: Any | None = None) -> None:
        self.storage = storage
        self.tmux = tmux_driver or TmuxDriver()
        # Profiles with input_mode="process_json" use this instead of
        # self.tmux (see _driver_for). Defaults to the process-wide
        # singleton, not a fresh instance — see get_default_json_driver.
        self.json_driver = json_driver or get_default_json_driver()
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
                      goal_strategy: str | None = None,
                      model_override: str | None = None,
                      ) -> HarnessSession:
        """Bootstrap a harness session for one task + worktree.

        ``model_override`` is an optional model id (e.g.
        ``nvidia/nemotron-3-ultra-550b-a55b:free``). It is injected
        via the profile's ``model_arg_name`` flag (e.g. ``--model``);
        profiles without ``model_arg_name`` ignore it.
        """
        if isinstance(profile, str):
            profile = get_profile(profile) or get_default_profile()
        elif profile is None:
            profile = get_default_profile()

        use_json = profile.input_mode == "process_json"
        driver = self.json_driver if use_json else self.tmux

        # Compose the spawn command: profile.command + effective_args
        # (effective_args injects the model override flag via the
        # profile's model_arg_name if the override or a default_model
        # is present) + a marker argv so the harness can identify itself.
        cmd_parts = [profile.command] + list(
            profile.effective_args(model_override=model_override)
        )
        # A wire-protocol adapter (see _wire_protocol_env_prefix) is
        # env-var driven and only meaningful for real-shell tmux
        # launches, not the JSON driver's direct subprocess.Popen argv.
        if not use_json:
            effective_model = model_override or profile.default_model
            cmd_parts = _wire_protocol_env_prefix(profile, effective_model) + cmd_parts
        # Sanitize against empty argv (would break tmux).
        cmd_parts = [p for p in cmd_parts if p]
        if not cmd_parts:
            raise HarnessDriverError(
                f"Profile '{profile.name}' command is empty"
            )

        # Construct an idempotent-ish tmux session name based on task
        # id (truncated). This is safe because task ids are random UUIDs.
        tmux_session = self._tmux_session_name(task_id)
        ref = driver.create_session(
            session_name=tmux_session, cwd=worktree_path, command=cmd_parts,
        )

        if use_json:
            runtime = _JSON_RUNTIME
        elif not isinstance(self.tmux, FakeTmuxDriver):
            runtime = "tmux"
        else:
            runtime = "tmux-fake"
        session = HarnessSession(
            id=self._new_session_id(),
            agent_run_id=agent_run_id, task_id=task_id,
            harness_profile=profile.name, harness=profile.harness,
            runtime=runtime,
            tmux_session=ref.session, tmux_window=ref.window, tmux_pane=ref.pane,
            working_directory=worktree_path,
            status=HarnessSessionStatus.starting.value,
        )
        # Record which model actually got injected so downstream
        # events / reports can surface it without parsing argv later.
        if profile.model_arg_name:
            effective_model = (
                model_override or profile.default_model
            )
            if effective_model:
                session.metadata = dict(session.metadata or {})
                session.metadata["harness_model"] = effective_model
        self.storage.save_session(session)
        self._emit(session, "session.created", {"profile": profile.name})

        # Wait for the TUI to render before injecting the goal, or
        # early keystrokes are lost. Not needed for FakeTmuxDriver or
        # JSON-mode sessions (goal is a process argv, not typed input).
        if not isinstance(self.tmux, FakeTmuxDriver) and not use_json:
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
                # The ready-wait above is best-effort and can proceed
                # blind under host load, silently dropping the goal.
                # Confirm by checking for a distinctive substring of
                # what was actually sent (not a pre/post byte-compare,
                # which false-positives on unrelated TUI re-renders).
                result = self.inject_goal(session, goal_context,
                                         requested_strategy=goal_strategy)
                # JSON-mode: goal is a process argv, delivered synchronously.
                if not isinstance(self.tmux, FakeTmuxDriver) and not use_json:
                    import time as _time2
                    marker = _distinctive_marker(result.sent_text)
                    _time2.sleep(2.0)
                    try:
                        post_capture = self.tmux.capture(self._ref(session), lines=200)
                    except Exception:
                        post_capture = ""
                    if not (marker and marker in post_capture):
                        self._emit(session, "goal.injection_unconfirmed_retrying",
                                   {"marker": marker})
                        # Use a different delivery mechanism, not a
                        # repeat of the one that already failed.
                        ref = self._ref(session)
                        send_literal = getattr(self.tmux, "send_text_literal", None)
                        if send_literal is not None:
                            send_literal(ref, result.sent_text)
                            self.tmux.send_enter(ref)
                        else:
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

        if use_json:
            self._persist_json_pid(session)

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
        """Write .agent-task/* files and send the directive to the driver."""
        ref = self._ref(session)
        driver = self._driver_for(session)
        result = inject_goal(
            worktree_path=session.working_directory,
            profile=self._profile_for(session),
            ctx=ctx, requested_strategy=requested_strategy,
        )
        # For JSON-mode sessions this send_text+send_enter pair is what
        # spawns `opencode run <sent_text> ...`.
        driver.send_text(ref, result.sent_text)
        driver.send_enter(ref)
        self._emit(session, "goal.injected",
                   {"strategy": result.strategy,
                    "files_written": result.files_written})
        return result

    def capture_output(self, session: HarnessSession,
                       lines: int | None = None) -> str:
        """Return recent driver capture; update last_output_at.

        The session's ``last_output_at`` field is only updated when the
        captured output is non-empty AND differs from the last captured
        blob for this session. This preserves stall-detection semantics:
        if nothing has changed since the previous capture, the timestamp
        reflects the time of the last *new* output, not merely the
        time of our latest poll.
        """
        ref = self._ref(session)
        capture = self._driver_for(session).capture(ref, lines=lines or self.capture_lines)
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
        ref = self._ref(session)
        if session.runtime == _JSON_RUNTIME:
            alive = self.json_driver.is_alive(ref)
            return classify_json_transcript(
                text=output, process_alive=alive,
                exit_code=self.json_driver.exit_code(ref),
                harness_profile=session.harness_profile,
            )
        alive = self.tmux.is_alive(ref)
        return classify_state(
            output=output,
            last_output_at=session.last_output_at,
            now=now_override, stall_seconds=stall_seconds,
            process_alive=alive,
            harness_profile=session.harness_profile,
        )

    def send_reply(self, session: HarnessSession, reply_text: str) -> None:
        """Send a Composer reply into the session.

        Composer replies are wrapped in a clear "ASSISTANT REPLY:" header
        so the agent can distinguish them from its own echoed input.
        """
        ref = self._ref(session)
        driver = self._driver_for(session)
        header = "ASSISTANT REPLY (from Composer):"
        full_text = header + "\n" + reply_text
        if session.runtime == _JSON_RUNTIME:
            # One-shot process model: send_enter spawns a new process,
            # so the reply goes as ONE message, not one per line.
            driver.send_text(ref, full_text)
            driver.send_enter(ref)
            self._persist_json_pid(session)
        else:
            for line in full_text.splitlines() or [""]:
                driver.send_text(ref, line)
                driver.send_enter(ref)
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

    def mark_usage_limited(self, session: HarnessSession, evidence: str = "") -> None:
        """Subscription-tier provider hit its own usage cap.

        Distinct from mark_stalled/mark_failed: this is not silence or
        a crash, it's the harness itself reporting a rate/quota limit
        (see classifier.HarnessState.usage_limited). Sets session
        status to "usage_limited" so /tasks/{id}'s runtime_status
        surfaces it to Conductor, which maps it to a
        restart-with-fallback (see conductor/composer/service.py's
        reconcile loop + scheduler._resolve_provider_fallback) rather
        than a terminal failure.
        """
        session.status = HarnessSessionStatus.usage_limited.value
        session.metadata = dict(session.metadata)
        session.metadata["usage_limit_evidence"] = evidence
        self.storage.save_session(session)
        self._emit(session, "agent.usage_limited",
                   {"harness_profile": session.harness_profile, "evidence": evidence})

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
            self._driver_for(session).terminate(ref)
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

    def _driver_for(self, session: HarnessSession):
        """Route a session to the driver that created it. Sessions are
        pinned to whichever driver started them (see start_session's
        ``use_json``) — a profile's input_mode only matters at spawn
        time, not on every subsequent call."""
        if session.runtime == _JSON_RUNTIME:
            return self.json_driver
        return self.tmux

    def _profile_for(self, session: HarnessSession) -> HarnessProfile:
        return get_profile(session.harness_profile) or get_default_profile()

    def _persist_json_pid(self, session: HarnessSession) -> None:
        """Record the just-spawned process's PID on the session so a
        later HarnessDriver instance (e.g. after an AGW restart, which
        loses all in-memory OpencodeJsonDriver state) can reattach
        instead of wrongly concluding the session is dead. Called
        after every JSON-mode spawn, not just the first — replies spawn
        a brand new process each time."""
        if session.runtime != _JSON_RUNTIME:
            return
        try:
            pid = self.json_driver.get_pid(self._ref(session))
        except Exception:
            return
        if pid is None:
            return
        session.metadata = dict(session.metadata or {})
        session.metadata["json_pid"] = pid
        self.storage.save_session(session)

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
