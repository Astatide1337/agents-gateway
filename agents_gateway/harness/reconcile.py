"""Startup reconciliation for harness tmux sessions.

When the Agents Gateway process restarts while tmux harness sessions
are still alive on the host, the supervisor needs to:

  * for every session whose tmux session is still alive: mark it
    ``recovered_after_restart`` so the supervisor resumes
    supervision, and emit ``session.recovered_after_restart``.
  * for every session whose tmux session has died: mark it
    ``stalled`` (or ``failed`` for purely-local crashes) and emit
    ``session.missing_after_restart`` so Composer knows the run is
    not silently forgotten.

This module is called once from ``server.create_app`` after the
worker has started. It is safe to call idempotently — a second call
discovers sessions that are now in a terminal state and skips them.

The reconciliation is cheap: one ``tmux has-session`` per recoverable
session, and per-session state mutations are written back to the
HarnessStorage. We do NOT restart any harness process on the host —
the harness session state recovery is best-effort; if a session is
dead we surface it as a Composer interaction rather than auto-restart.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from agents_gateway.harness.models import (
    HarnessSession,
    HarnessSessionStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import FakeTmuxDriver, TmuxDriver, TmuxSessionRef
from agents_gateway.harness.driver import HarnessDriver


class ReconcileResult:
    """Summary of one reconciliation pass."""
    def __init__(self) -> None:
        self.recovered: list[str] = []
        self.missing: list[str] = []
        self.skipped: list[str] = []

    def __repr__(self) -> str:
        return (f"ReconcileResult(recovered={len(self.recovered)}, "
                f"missing={len(self.missing)}, "
                f"skipped={len(self.skipped)})")


def reconcile_harness_sessions(
    harness_storage: HarnessStorage,
    *,
    driver: HarnessDriver | None = None,
    emit_event: Any | None = None,
) -> ReconcileResult:
    """Re-examine registered harness sessions after a restart.

    Returns a ReconcileResult describing how many sessions were
    recovered vs missing vs skipped.
    """
    result = ReconcileResult()
    emit = emit_event or _default_emitter(harness_storage)

    if driver is None:
        driver = HarnessDriver(storage=harness_storage)

    for session in harness_storage.list_recoverable_sessions():
        # Re-check freshness from storage — another worker may have
        # already moved the session while we build this list.
        fresh = harness_storage.get_session(session.id)
        if fresh is None:
            result.skipped.append(session.id)
            continue
        if fresh.status in (HarnessSessionStatus.completed.value,
                            HarnessSessionStatus.failed.value,
                            HarnessSessionStatus.blocked_external.value,
                            HarnessSessionStatus.cancelled.value):
            result.skipped.append(fresh.id)
            continue
        # Use the underlying tmux driver's is_alive to check liveness.
        try:
            alive = driver.tmux.is_alive(
                TmuxSessionRef(session=fresh.tmux_session,
                                window=fresh.tmux_window,
                                pane=fresh.tmux_pane))
        except Exception:
            # If tmux call itself crashed (no binary, etc.), treat
            # as missing — safer than silently assuming alive.
            alive = False
        if alive:
            _mark_recovered(harness_storage, fresh, emit)
            result.recovered.append(fresh.id)
        else:
            _mark_missing(harness_storage, fresh, emit)
            result.missing.append(fresh.id)
    return result


def _mark_recovered(harness_storage: HarnessStorage, session: HarnessSession,
                    emit: Any) -> None:
    """Mark a session recovered after restart."""
    session.status = HarnessSessionStatus.running.value
    session.last_output_at = datetime.now(timezone.utc).isoformat()
    session.metadata = dict(session.metadata)
    session.metadata["recovered_after_restart"] = True
    harness_storage.save_session(session)
    try:
        emit(session, "session.recovered_after_restart", {})
    except Exception:
        pass
    try:
        emit(session, "supervisor.resumed", {})
    except Exception:
        pass


def _mark_missing(harness_storage: HarnessStorage, session: HarnessSession,
                  emit: Any) -> None:
    """Mark a session missing after restart.

    Use ``stalled`` (not ``failed``) so Composer can still pick it up
    as an ambiguous_harness_state interaction rather than a hard
    failure — matches the supervisor's own stall-handling convention.
    """
    prev_status = session.status
    session.status = HarnessSessionStatus.stalled.value
    session.ended_at = session.ended_at or datetime.now(
        timezone.utc).isoformat()
    session.metadata = dict(session.metadata)
    session.metadata["missing_after_restart"] = True
    session.metadata["pre_restart_status"] = prev_status
    harness_storage.save_session(session)
    try:
        emit(session, "session.missing_after_restart",
             {"previous_status": prev_status})
    except Exception:
        pass


def _default_emitter(harness_storage: HarnessStorage):
    """Build a default task-storage-event emitter.

    The emitter posts events back into the task_storage events table
    so the session's task timeline records the recovery / missing
    markers. We construct a thin shim that writes into the same DB
    via TaskStorage.
    """
    from agents_gateway.storage import TaskStorage
    task_storage = TaskStorage(harness_storage.db_path)

    def emit(session: HarnessSession, event_name: str, data: dict) -> None:
        try:
            task_storage.append_event(session.task_id, event_name, data)
        except Exception:
            pass
    return emit


__all__ = ["ReconcileResult", "reconcile_harness_sessions"]
