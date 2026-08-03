"""Startup reconciliation: kill harness processes left running behind a
terminal-status session.

Live incident: a harness session's underlying ``opencode`` process is
only ever killed via ``HarnessDriver.stop_session()`` calling
``driver.terminate(ref)`` — but ``OpencodeJsonDriver.terminate()`` looks
the session up in its own in-memory ``_sessions`` dict first and is a
silent no-op if that entry is missing (see process_json.py). That dict
is wiped on every AGW restart. So: session A gets marked terminal
(completed/failed/cancelled/blocked_external) in storage, its process
either never receives a terminate call or the call finds no in-memory
state to act on, and the process — still holding real memory — runs
forever. Eight of these accumulated over one night and pushed the host
into full swap exhaustion.

``reap_orphaned_processes`` closes this gap independently of whether a
graceful terminate ever happened: on every AGW boot, look at every
terminal-status session's persisted ``json_pid`` (see
``HarnessDriver._persist_json_pid``) and kill it if it's still alive.
A session's own status is the source of truth for whether a process
*should* exist — a terminal session with a live PID is a contradiction
by definition, not just a symptom.
"""

from __future__ import annotations

import logging
import os
import signal
import time

from agents_gateway.harness.models import HarnessSessionStatus
from agents_gateway.harness.storage import HarnessStorage

logger = logging.getLogger(__name__)

__all__ = ["reap_orphaned_processes"]

_TERMINAL_STATUSES = (
    HarnessSessionStatus.completed.value,
    HarnessSessionStatus.failed.value,
    HarnessSessionStatus.cancelled.value,
    HarnessSessionStatus.blocked_external.value,
)


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False
    except OSError:
        return False


def reap_orphaned_processes(
    storage: HarnessStorage, grace_seconds: float = 2.0,
) -> list[dict]:
    """Kill any process referenced by a terminal-status session's
    ``json_pid``. Best-effort: a lookup or kill failure for one session
    never stops the pass over the rest. Returns a list of
    ``{"session_id", "task_id", "pid"}`` dicts for every process
    actually killed, for the caller to log."""
    killed: list[dict] = []
    for status in _TERMINAL_STATUSES:
        try:
            sessions = storage.list_sessions(status=status)
        except Exception:
            logger.exception("reap_orphaned_processes: failed listing status=%s", status)
            continue
        for session in sessions:
            pid = (session.metadata or {}).get("json_pid")
            if not isinstance(pid, int):
                continue
            try:
                if not _pid_alive(pid):
                    continue
                os.kill(pid, signal.SIGTERM)
            except Exception:
                logger.exception(
                    "reap_orphaned_processes: SIGTERM failed for pid=%s session=%s",
                    pid, session.id,
                )
                continue
            killed.append({
                "session_id": session.id, "task_id": session.task_id, "pid": pid,
            })

    if killed and grace_seconds > 0:
        time.sleep(grace_seconds)
        for entry in killed:
            pid = entry["pid"]
            if _pid_alive(pid):
                try:
                    os.kill(pid, signal.SIGKILL)
                except Exception:
                    logger.exception(
                        "reap_orphaned_processes: SIGKILL failed for pid=%s", pid,
                    )

    return killed
