"""Session supervisor + harness run orchestrator.

The SessionSupervisor is a background thread that periodically inspects
active sessions, classifies their state, creates Composer interactions
for waiting/stalled sessions, and triggers verification when a harness
claims completion. It does NOT autonomously mark tasks complete — that
is the HarnessRuntime's responsibility based on verification results.

The HarnessRuntime ties together all the pieces for one task:

  workspace clone
    → worktree
    → session start + goal injection
    → supervisor watches for waiting_for_reply / completed_claimed
    → on completion claim: run verification
    → on pass: capture diff, commit if configured, generate report,
      mark agent_run completed, mark session completed
    → on failure: feed failure summary back into session (continue)
                  OR mark failed if session died
    → on blocked external: create composer interaction + stalled state
"""

from __future__ import annotations

import threading
import time
from datetime import datetime, timezone
from typing import Any

from agents_gateway.harness.classifier import (
    ClassifierResult,
    HarnessState,
    classify_state,
)
from agents_gateway.harness.driver import HarnessDriver
from agents_gateway.harness.models import (
    ComposerInteraction,
    HarnessSession,
    HarnessSessionStatus,
    VerificationCommand,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.verification import VerificationRunner


class SessionSupervisor:
    """Background thread that periodically inspects active sessions."""

    def __init__(self, storage: HarnessStorage,
                 driver: HarnessDriver,
                 verification_runner: VerificationRunner,
                 poll_interval_seconds: float = 10.0,
                 stall_seconds: int = 900,
                 emit_event: Any | None = None,
                 on_completed_claim: Any | None = None) -> None:
        self.storage = storage
        self.driver = driver
        self.verification = verification_runner
        self.poll_interval = poll_interval_seconds
        self.stall_seconds = stall_seconds
        self.emit_event = emit_event or (lambda *a, **kw: None)
        # Optional callback invoked when a session claims completion.
        # on_completed_claim(session) -> None. The runtime registers
        # its handler here so verification runs in the supervisor
        # thread, off the request path.
        self.on_completed_claim = on_completed_claim
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()
        # sessions that should NOT be reclassified on a given tick
        # (e.g. a verification is already in flight for them)
        self._busy: set[str] = set()

    def start(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._loop, name="agw-session-supervisor", daemon=True,
        )
        self._thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=timeout)
        self._thread = None

    def is_alive(self) -> bool:
        return self._thread is not None and self._thread.is_alive()

    # -------------------------------------------------------------------

    def tick_once(self) -> int:
        """Process all active sessions once. Returns number classified."""
        sessions = self.storage.list_active_sessions()
        processed = 0
        for session in sessions:
            with self._lock:
                if session.id in self._busy:
                    continue
                self._busy.add(session.id)
            try:
                self._process_session(session)
                processed += 1
            finally:
                with self._lock:
                    self._busy.discard(session.id)
        return processed

    def _loop(self) -> None:
        while not self._stop.is_set():
            try:
                self.tick_once()
            except Exception:
                pass
            time.sleep(self.poll_interval)

    def _process_session(self, session: HarnessSession) -> None:
        # Re-fetch from storage to get the freshest status (race-safe).
        fresh = self.storage.get_session(session.id)
        if fresh is None:
            return
        if fresh.status not in (
            HarnessSessionStatus.running.value,
            HarnessSessionStatus.waiting_for_reply.value,
            HarnessSessionStatus.starting.value,
        ):
            return
        result = self.driver.classify_state(
            fresh, stall_seconds=self.stall_seconds,
        )
        if result.state == HarnessState.waiting_for_reply:
            if fresh.status != HarnessSessionStatus.waiting_for_reply.value:
                self.driver.mark_waiting_for_reply(
                    fresh, excerpt=result.excerpt,
                )
        elif result.state == HarnessState.completed_claimed:
            # IMPORTANT: never mark session completed from classifier
            # alone — verify first. Transition to verifying + invoke
            # the runtime's handler if registered.
            if fresh.status != HarnessSessionStatus.verifying.value:
                self.driver.mark_verifying(fresh)
                if self.on_completed_claim is not None:
                    try:
                        self.on_completed_claim(fresh)
                    except Exception:
                        pass
        elif result.state == HarnessState.failed_claimed:
            # A dead harness or hard error: mark the session failed.
            # The runtime will surface final state.
            self.driver.mark_failed(
                fresh, reason=f"classifier detected {result.state}: "
                              f"{result.evidence}",
            )
        elif result.state == HarnessState.stalled:
            if fresh.status != HarnessSessionStatus.stalled.value:
                self.driver.mark_stalled(fresh)
        elif result.state == HarnessState.running:
            # If a session was waiting_for_reply or stalled earlier but
            # now the harness has spoken again, resume to running.
            if fresh.status == HarnessSessionStatus.waiting_for_reply.value:
                fresh.status = HarnessSessionStatus.running.value
                fresh.last_output_at = datetime.now(timezone.utc).isoformat()
                self.storage.save_session(fresh)
                self.emit_event(fresh, "agent.resumed",
                                {"reason": "output resumed"})


__all__ = ["SessionSupervisor"]
