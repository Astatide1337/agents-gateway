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

import threading
from datetime import datetime, timezone
from typing import Any

from agents_gateway.harness.models import (
    HarnessSession,
    HarnessSessionStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.tmux import FakeTmuxDriver, TmuxDriver
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
        # Route to whichever driver actually owns this session — a
        # JSON-mode session was never a real tmux session, so checking
        # driver.tmux directly always reports it as missing/dead.
        try:
            owning_driver = driver._driver_for(fresh)
            try:
                alive = owning_driver.is_alive(driver._ref(fresh))
            except Exception:
                # OpencodeJsonDriver's session state is pure
                # in-process memory — a fresh instance (this AGW
                # process just (re)started) knows nothing about
                # sessions spawned before the restart. Reattach via
                # the PID persisted at spawn time (see
                # HarnessDriver._persist_json_pid) and retry once
                # before concluding the session is actually dead.
                pid = (fresh.metadata or {}).get("json_pid")
                reattach = getattr(owning_driver, "reattach", None)
                if pid is not None and reattach is not None:
                    reattach(fresh.tmux_session, fresh.working_directory, int(pid))
                    alive = owning_driver.is_alive(driver._ref(fresh))
                else:
                    raise
        except Exception:
            # If the underlying call itself crashed (no binary, etc.),
            # treat as missing — safer than silently assuming alive.
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


def resume_orphaned_harness_tasks(
    task_storage: Any,
    *,
    harness_config: Any = None,
    artifacts_dir: str = "",
) -> list[str]:
    """Resume harness_session tasks whose driving thread was lost.

    ``HarnessSessionRuntimeAdapter.execute()`` drives one task via a
    single long blocking call on a worker thread. An AGW process
    restart destroys that thread — and unlike a freshly queued task,
    the ``TaskWorker`` only ever claims ``status='queued'`` rows, so a
    task already ``status='running'`` when the process died is never
    picked up again on its own. It just sits frozen: reconcile_harness_
    sessions() (see above) keeps ``is_alive()`` answers accurate, but
    nothing re-drives the task toward verification/completion.

    Called once at boot, after reconcile_harness_sessions(). Each
    resume runs in its own background thread since HarnessRuntime.
    resume_task() blocks until the task reaches a terminal state.
    Returns the list of task_ids a resume thread was started for.

    Checks both "running" and "waiting" tasks — a task mid-interaction
    (waiting on a Composer reply) when the process died is exactly as
    orphaned as one in "running": reconcile_harness_sessions() above
    only refreshes is_alive() bookkeeping for it, nothing re-drives it.
    Confirmed live: a real integration task sat in "waiting" at restart
    and never resumed, even though the underlying harness session had
    already produced a genuine, verifiable completion.
    """
    from agents_gateway.harness_runtime_adapter import HarnessSessionRuntimeAdapter

    resumed: list[str] = []
    seen: set[str] = set()
    for status in ("running", "waiting"):
        for task in task_storage.list_tasks(limit=500, status=status):
            if task.id in seen:
                continue
            if (task.metadata or {}).get("runtime_type") != "harness_session":
                continue
            seen.add(task.id)
            adapter = HarnessSessionRuntimeAdapter(
                storage=task_storage, artifacts_dir=artifacts_dir,
                harness_config=harness_config,
            )
            t = threading.Thread(
                target=adapter.resume, args=(task.id,),
                name=f"harness-resume-{task.id[:8]}", daemon=True,
            )
            t.start()
            resumed.append(task.id)
    return resumed


__all__ = ["ReconcileResult", "reconcile_harness_sessions",
          "resume_orphaned_harness_tasks"]
