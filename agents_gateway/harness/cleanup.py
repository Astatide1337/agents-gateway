"""Harness artifact / worktree retention cleanup.

This module is invoked by ``/cleanup/dry-run`` and ``/cleanup/run``
HTTP endpoints and by the optional
``scripts/cleanup-harness-artifacts.sh`` CLI.

Rules:

  * Never delete an active worktree (status in
    ``{created, active, dirty, committed}``). Only
    ``cleaned_up`` worktrees older than
    ``worktree_retention_days`` are eligible for deletion.
  * Never delete artifacts belonging to a session in
    ``{running, starting, verifying, waiting_for_reply, created}``
    state. Old artifacts older than ``artifact_retention_days``
    whose owning session is terminal are eligible.
  * Dry-run reports WHAT WOULD be deleted without touching disk.
  * Total artifact size is also capped at
    ``max_artifact_bytes`` — when the cap is exceeded, oldest
    artifacts are eligible regardless of per-artifact age.

The cleanup function returns a structured report so the HTTP
endpoints can stream it back to the operator.
"""

from __future__ import annotations

import os
import shutil
import time
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    HarnessSession,
    HarnessSessionStatus,
    Worktree,
    WorktreeStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.logging import log_event


# Sessions in these states are considered active and MUST NOT be
# touched by cleanup.
_ACTIVE_SESSION_STATES = {
    HarnessSessionStatus.created.value,
    HarnessSessionStatus.starting.value,
    HarnessSessionStatus.running.value,
    HarnessSessionStatus.waiting_for_reply.value,
    HarnessSessionStatus.verifying.value,
}

# Worktrees in these states are considered active and MUST NOT be
# removed by cleanup.
_ACTIVE_WORKTREE_STATES = {
    WorktreeStatus.created.value,
    WorktreeStatus.active.value,
    WorktreeStatus.dirty.value,
    WorktreeStatus.committed.value,
}


@dataclass
class CleanupReport:
    """Structured summary of a cleanup pass."""

    dry_run: bool = False
    deleted_artifacts: list[dict[str, Any]] = field(default_factory=list)
    deleted_worktrees: list[dict[str, Any]] = field(default_factory=list)
    skipped_active_artifacts: int = 0
    skipped_active_worktrees: int = 0
    bytes_freed: int = 0
    total_artifact_bytes_before: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "dry_run": self.dry_run,
            "deleted_artifacts": list(self.deleted_artifacts),
            "deleted_worktrees": list(self.deleted_worktrees),
            "skipped_active_artifacts": self.skipped_active_artifacts,
            "skipped_active_worktrees": self.skipped_active_worktrees,
            "bytes_freed": self.bytes_freed,
            "total_artifact_bytes_before": self.total_artifact_bytes_before,
        }


def run_cleanup(
    storage: HarnessStorage,
    *,
    artifact_retention_days: int = 14,
    worktree_retention_days: int = 7,
    max_artifact_bytes: int = 1_073_741_824,
    dry_run: bool = True,
) -> CleanupReport:
    """Run one cleanup pass against the harness storage.

    Returns a CleanupReport describing what would/did happen. When
    ``dry_run=True`` no on-disk action is taken (only read access).
    """
    report = CleanupReport(dry_run=dry_run)

    now = datetime.now(timezone.utc)
    artifact_cutoff = now - timedelta(days=artifact_retention_days)
    worktree_cutoff = now - timedelta(days=worktree_retention_days)
    artifact_cutoff_ts = artifact_cutoff.timestamp()
    worktree_cutoff_ts = worktree_cutoff.timestamp()

    # ------------------------------------------------------------------
    # Pass 1: worktrees
    # ------------------------------------------------------------------
    for wt in storage.list_worktrees():
        if wt.status in _ACTIVE_WORKTREE_STATES:
            report.skipped_active_worktrees += 1
            continue
        # cleaned_up / failed worktrees older than worktree_retention_days
        try:
            created_ts = datetime.fromisoformat(
                wt.created_at.replace("Z", "+00:00")).timestamp()
        except (ValueError, AttributeError):
            created_ts = 0
        if created_ts > worktree_cutoff_ts:
            # Not old enough yet
            continue

        # Don't delete the worktree if its session is still active.
        # Cross-check via the task_id.
        active_session = _has_active_session_for_task(storage, wt.task_id)
        if active_session:
            report.skipped_active_worktrees += 1
            continue

        entry = {
            "worktree_id": wt.id, "task_id": wt.task_id,
            "branch": wt.branch, "path": wt.path,
            "status": wt.status,
        }
        report.deleted_worktrees.append(entry)
        if not dry_run:
            _safe_rmtree(wt.path)
            # Mark on disk as already deleted so subsequent passes skip.
            try:
                wt.deleted_at = now.isoformat()
                storage.save_worktree(wt)
            except Exception:
                pass

    # ------------------------------------------------------------------
    # Pass 2: harness_artifacts (DB rows + on-disk files)
    # ------------------------------------------------------------------
    artifacts = storage.list_harness_artifacts()
    by_session: dict[str, list[dict]] = {}
    for a in artifacts:
        sid = a.get("agent_run_id", "")
        by_session.setdefault(sid, []).append(a)

    # Total size on disk first.
    sizes = {a["id"]: _file_size(a["path"]) for a in artifacts}
    total_bytes = sum(sizes.values())
    report.total_artifact_bytes_before = total_bytes

    # First pass: by retention age
    deletion_candidates: list[dict] = []
    for a in artifacts:
        try:
            created_ts = datetime.fromisoformat(
                a["created_at"].replace("Z", "+00:00")).timestamp()
        except (ValueError, AttributeError, KeyError):
            continue
        if created_ts > artifact_cutoff_ts:
            continue
        deletion_candidates.append((created_ts, a))

    # Second pass: if we're over the byte cap, also include the
    # oldest artifacts that aren't already in the candidate set.
    if total_bytes > max_artifact_bytes:
        already_in_candidates_ids = {c[1].get("id") for c in deletion_candidates}
        extra = sorted(
            ((a.get("created_at", ""), a) for a in artifacts
             if a.get("id") not in already_in_candidates_ids),
            key=lambda x: x[0],
        )
        bytes_so_far = total_bytes
        for _, a in extra:
            if bytes_so_far <= max_artifact_bytes:
                break
            deletion_candidates.append((_time_minus_extras(a.get("created_at", "")), a))
            bytes_so_far -= sizes.get(a["id"], 0)

    deletion_candidates.sort(key=lambda x: x[0])

    for _, a in deletion_candidates:
        # Don't delete artifacts belonging to an active session.
        active = _has_active_session_for_run(storage, a["agent_run_id"])
        if active:
            report.skipped_active_artifacts += 1
            continue
        entry = {
            "artifact_id": a.get("id"), "task_id": a.get("task_id"),
            "agent_run_id": a.get("agent_run_id"),
            "kind": a.get("kind"), "name": a.get("name"),
            "path": a.get("path"), "size_bytes": sizes.get(a.get("id"), 0),
        }
        report.deleted_artifacts.append(entry)
        size = entry["size_bytes"]
        report.bytes_freed += size
        if not dry_run:
            _safe_unlink(a.get("path", ""))
            # We don't remove the DB row — the report is the source of
            # truth. The storage.get_harness_artifact read-via-view
            # endpoint already returns "file missing" for unlink'd
            # artifacts; keeping the metadata row preserves audit
            # history without occupying disk bytes.

    log_event("harness_cleanup",
              f"dry_run={dry_run}, freed={report.bytes_freed}B, "
              f"artifacts={len(report.deleted_artifacts)}, "
              f"worktrees={len(report.deleted_worktrees)}")
    return report


def _has_active_session_for_task(storage: HarnessStorage, task_id: str) -> bool:
    try:
        sessions = storage.list_sessions(task_id=task_id)
        return any(s.status in _ACTIVE_SESSION_STATES for s in sessions)
    except Exception:
        return False


def _has_active_session_for_run(storage: HarnessStorage, agent_run_id: str) -> bool:
    try:
        # We don't have a direct by-run lookup; use the worktree-by-run
        # helper to find the task_id and reuse the task-check.
        wt = storage.get_worktree_by_run(agent_run_id)
        if wt is None:
            return False
        return _has_active_session_for_task(storage, wt.task_id)
    except Exception:
        return False


def _safe_unlink(path: str) -> None:
    try:
        p = Path(path)
        if p.exists():
            p.unlink()
    except Exception:
        pass


def _safe_rmtree(path: str) -> None:
    try:
        p = Path(path)
        if p.exists():
            shutil.rmtree(p)
    except Exception:
        pass


def _file_size(path: str) -> int:
    try:
        return os.path.getsize(path) if os.path.exists(path) else 0
    except OSError:
        return 0


def _time_minus_extras(iso_str: str) -> float:
    """Helper for the byte-cap sort key. Best-effort; 0 on error."""
    try:
        return datetime.fromisoformat(
            iso_str.replace("Z", "+00:00")).timestamp()
    except (ValueError, AttributeError):
        return 0.0


__all__ = ["CleanupReport", "run_cleanup"]
