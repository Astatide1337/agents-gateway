"""Harness-plane SQLite storage (workspaces, worktrees, sessions,
interactions, verification runs, enriched artifacts).

This module lives next to ``storage.TaskStorage`` but owns the tables
that were added as part of the harness worktree runtime milestone. We
use the SAME sqlite DB path so all task data co-exists; the new tables
are created with ``CREATE TABLE IF NOT EXISTS`` so existing databases
are upgraded in place without a migration.

All methods round-trip between Python dataclasses (in
``agents_gateway.harness.models``) and the SQLite JSON-style columns.
"""

from __future__ import annotations

import json
import sqlite3
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    ComposerInteraction,
    ComposerInteractionStatus,
    ComposerInteractionType,
    HarnessSession,
    HarnessSessionStatus,
    RepoWorkspace,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
    Worktree,
    WorktreeStatus,
)


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# SQLite-backed harness storage
# ---------------------------------------------------------------------------


class HarnessStorage:
    """Storage layer for harness sessions, worktrees, interactions, etc."""

    def __init__(self, db_path: str) -> None:
        self.db_path = db_path
        Path(db_path).parent.mkdir(parents=True, exist_ok=True)
        self._init_db()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        return conn

    def _init_db(self) -> None:
        conn = self._connect()
        conn.executescript("""
            CREATE TABLE IF NOT EXISTS repo_workspaces (
                id TEXT PRIMARY KEY,
                repo_url TEXT NOT NULL,
                owner TEXT NOT NULL,
                repo TEXT NOT NULL,
                default_branch TEXT NOT NULL,
                base_path TEXT NOT NULL,
                worktrees_path TEXT NOT NULL,
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL,
                last_fetched_at TEXT,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE TABLE IF NOT EXISTS worktrees (
                id TEXT PRIMARY KEY,
                task_id TEXT NOT NULL,
                agent_run_id TEXT NOT NULL,
                repo_workspace_id TEXT NOT NULL,
                branch TEXT NOT NULL,
                base_branch TEXT NOT NULL,
                path TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'created',
                created_at TEXT NOT NULL,
                deleted_at TEXT,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE TABLE IF NOT EXISTS harness_sessions (
                id TEXT PRIMARY KEY,
                agent_run_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                harness_profile TEXT NOT NULL,
                harness TEXT NOT NULL,
                runtime TEXT NOT NULL,
                tmux_session TEXT NOT NULL,
                tmux_window TEXT NOT NULL DEFAULT 'main',
                tmux_pane TEXT NOT NULL DEFAULT '0',
                working_directory TEXT NOT NULL DEFAULT '',
                status TEXT NOT NULL DEFAULT 'created',
                started_at TEXT NOT NULL,
                last_output_at TEXT,
                ended_at TEXT,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE TABLE IF NOT EXISTS composer_interactions (
                id TEXT PRIMARY KEY,
                agent_run_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                session_id TEXT NOT NULL,
                type TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'pending',
                prompt_excerpt TEXT DEFAULT '',
                full_context_ref TEXT DEFAULT '',
                created_at TEXT NOT NULL,
                resolved_at TEXT,
                composer_reply TEXT,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE TABLE IF NOT EXISTS verification_runs (
                id TEXT PRIMARY KEY,
                agent_run_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'created',
                commands_json TEXT DEFAULT '[]',
                started_at TEXT NOT NULL,
                completed_at TEXT,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE TABLE IF NOT EXISTS harness_artifacts (
                id TEXT PRIMARY KEY,
                agent_run_id TEXT NOT NULL,
                task_id TEXT NOT NULL,
                kind TEXT NOT NULL,
                name TEXT NOT NULL,
                path TEXT NOT NULL,
                mime_type TEXT DEFAULT 'application/octet-stream',
                size_bytes INTEGER DEFAULT 0,
                created_at TEXT NOT NULL,
                metadata_json TEXT DEFAULT '{}'
            );
            CREATE INDEX IF NOT EXISTS idx_worktrees_task_id ON worktrees(task_id);
            CREATE INDEX IF NOT EXISTS idx_worktrees_agent_run_id ON worktrees(agent_run_id);
            CREATE INDEX IF NOT EXISTS idx_harness_sessions_task_id ON harness_sessions(task_id);
            CREATE INDEX IF NOT EXISTS idx_harness_sessions_status ON harness_sessions(status);
            CREATE INDEX IF NOT EXISTS idx_composer_interactions_status ON composer_interactions(status);
            CREATE INDEX IF NOT EXISTS idx_composer_interactions_task_id ON composer_interactions(task_id);
            CREATE INDEX IF NOT EXISTS idx_verification_runs_agent_run_id ON verification_runs(agent_run_id);
            CREATE INDEX IF NOT EXISTS idx_harness_artifacts_agent_run_id ON harness_artifacts(agent_run_id);
        """)
        conn.commit()
        conn.close()

    # -------------------------------------------------------------------
    # Workspaces
    # -------------------------------------------------------------------

    def save_workspace(self, ws: RepoWorkspace) -> RepoWorkspace:
        conn = self._connect()
        conn.execute(
            """INSERT INTO repo_workspaces
               (id, repo_url, owner, repo, default_branch, base_path,
                worktrees_path, created_at, updated_at, last_fetched_at,
                metadata_json)
               VALUES (?,?,?,?,?,?,?,?,?,?,?)
               ON CONFLICT(id) DO UPDATE SET
                 repo_url=excluded.repo_url,
                 default_branch=excluded.default_branch,
                 base_path=excluded.base_path,
                 worktrees_path=excluded.worktrees_path,
                 updated_at=excluded.updated_at,
                 last_fetched_at=excluded.last_fetched_at,
                 metadata_json=excluded.metadata_json""",
            (ws.id, ws.repo_url, ws.owner, ws.repo, ws.default_branch,
             ws.base_path, ws.worktrees_path, ws.created_at, ws.updated_at,
             ws.last_fetched_at, json.dumps(ws.metadata)),
        )
        conn.commit()
        conn.close()
        return ws

    def get_workspace(self, ws_id: str) -> RepoWorkspace | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM repo_workspaces WHERE id=?", (ws_id,)
        ).fetchone()
        conn.close()
        return _row_to_workspace(row) if row else None

    def list_workspaces(self) -> list[RepoWorkspace]:
        conn = self._connect()
        rows = conn.execute(
            "SELECT * FROM repo_workspaces ORDER BY created_at"
        ).fetchall()
        conn.close()
        return [_row_to_workspace(r) for r in rows]

    def find_workspace(self, repo_url: str, owner: str, repo: str,
                       default_branch: str) -> RepoWorkspace | None:
        """Look up an existing workspace for a (owner/repo, branch) pair."""
        conn = self._connect()
        row = conn.execute(
            """SELECT * FROM repo_workspaces
               WHERE owner=? AND repo=? AND default_branch=?
               ORDER BY created_at DESC LIMIT 1""",
            (owner, repo, default_branch),
        ).fetchone()
        conn.close()
        return _row_to_workspace(row) if row else None

    def delete_workspace(self, ws_id: str) -> bool:
        conn = self._connect()
        cur = conn.execute(
            "DELETE FROM repo_workspaces WHERE id=?", (ws_id,)
        )
        conn.commit()
        conn.close()
        return cur.rowcount > 0

    # -------------------------------------------------------------------
    # Worktrees
    # -------------------------------------------------------------------

    def save_worktree(self, wt: Worktree) -> Worktree:
        conn = self._connect()
        conn.execute(
            """INSERT INTO worktrees
               (id, task_id, agent_run_id, repo_workspace_id, branch,
                base_branch, path, status, created_at, deleted_at,
                metadata_json)
               VALUES (?,?,?,?,?,?,?,?,?,?,?)
               ON CONFLICT(id) DO UPDATE SET
                 status=excluded.status,
                 deleted_at=excluded.deleted_at,
                 metadata_json=excluded.metadata_json""",
            (wt.id, wt.task_id, wt.agent_run_id, wt.repo_workspace_id,
             wt.branch, wt.base_branch, wt.path, wt.status,
             wt.created_at, wt.deleted_at, json.dumps(wt.metadata)),
        )
        conn.commit()
        conn.close()
        return wt

    def get_worktree(self, wt_id: str) -> Worktree | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM worktrees WHERE id=?", (wt_id,)
        ).fetchone()
        conn.close()
        return _row_to_worktree(row) if row else None

    def get_worktree_by_task(self, task_id: str) -> Worktree | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM worktrees WHERE task_id=? ORDER BY created_at DESC LIMIT 1",
            (task_id,),
        ).fetchone()
        conn.close()
        return _row_to_worktree(row) if row else None

    def get_worktree_by_run(self, agent_run_id: str) -> Worktree | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM worktrees WHERE agent_run_id=? "
            "ORDER BY created_at DESC LIMIT 1",
            (agent_run_id,),
        ).fetchone()
        conn.close()
        return _row_to_worktree(row) if row else None

    def list_worktrees(self) -> list[Worktree]:
        conn = self._connect()
        rows = conn.execute(
            "SELECT * FROM worktrees ORDER BY created_at DESC"
        ).fetchall()
        conn.close()
        return [_row_to_worktree(r) for r in rows]

    # -------------------------------------------------------------------
    # Sessions
    # -------------------------------------------------------------------

    def save_session(self, session: HarnessSession) -> HarnessSession:
        conn = self._connect()
        conn.execute(
            """INSERT INTO harness_sessions
               (id, agent_run_id, task_id, harness_profile, harness,
                runtime, tmux_session, tmux_window, tmux_pane,
                working_directory, status, started_at, last_output_at,
                ended_at, metadata_json)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
               ON CONFLICT(id) DO UPDATE SET
                 status=excluded.status,
                 tmux_session=excluded.tmux_session,
                 tmux_window=excluded.tmux_window,
                 tmux_pane=excluded.tmux_pane,
                 last_output_at=excluded.last_output_at,
                 ended_at=excluded.ended_at,
                 metadata_json=excluded.metadata_json""",
            (session.id, session.agent_run_id, session.task_id,
             session.harness_profile, session.harness, session.runtime,
             session.tmux_session, session.tmux_window, session.tmux_pane,
             session.working_directory, session.status, session.started_at,
             session.last_output_at, session.ended_at,
             json.dumps(session.metadata)),
        )
        conn.commit()
        conn.close()
        return session

    def get_session(self, session_id: str) -> HarnessSession | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM harness_sessions WHERE id=?", (session_id,)
        ).fetchone()
        conn.close()
        return _row_to_session(row) if row else None

    def get_session_by_task(self, task_id: str) -> HarnessSession | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM harness_sessions WHERE task_id=? "
            "ORDER BY started_at DESC LIMIT 1",
            (task_id,),
        ).fetchone()
        conn.close()
        return _row_to_session(row) if row else None

    def list_sessions(self, status: str | None = None,
                      task_id: str | None = None) -> list[HarnessSession]:
        clauses: list[str] = []
        params: list[Any] = []
        if status:
            clauses.append("status=?")
            params.append(status)
        if task_id:
            clauses.append("task_id=?")
            params.append(task_id)
        where = f" WHERE {' AND '.join(clauses)}" if clauses else ""
        conn = self._connect()
        rows = conn.execute(
            f"SELECT * FROM harness_sessions{where} ORDER BY started_at DESC",
            params,
        ).fetchall()
        conn.close()
        return [_row_to_session(r) for r in rows]

    def list_active_sessions(self) -> list[HarnessSession]:
        """Return sessions in non-terminal states (supervisor feed).

        ``verifying`` is intentionally excluded: while a session is in
        the ``verifying`` state the HarnessRuntime loop owns the
        verification drive; the supervisor would create unwanted race
        noise by re-classifying a session that's already transitively
        complete-pending-verification.
        """
        conn = self._connect()
        rows = conn.execute(
            """SELECT * FROM harness_sessions
               WHERE status IN (?, ?, ?, ?)
               ORDER BY started_at DESC""",
            (HarnessSessionStatus.created.value,
             HarnessSessionStatus.starting.value,
             HarnessSessionStatus.running.value,
             HarnessSessionStatus.waiting_for_reply.value),
        ).fetchall()
        conn.close()
        return [_row_to_session(r) for r in rows]

    def list_recoverable_sessions(self) -> list[HarnessSession]:
        """Sessions that should be re-examined after a restart.

        This is wider than ``list_active_sessions`` because it also
        includes ``verifying`` and ``stalled`` sessions — both of
        those should NOT be silently forgotten if the gateway process
        crashes and comes back. Only true terminal states
        (``completed``/``failed``/``blocked_external``/``cancelled``)
        are excluded.
        """
        conn = self._connect()
        rows = conn.execute(
            """SELECT * FROM harness_sessions
               WHERE status NOT IN (?, ?, ?, ?)
               ORDER BY started_at DESC""",
            (HarnessSessionStatus.completed.value,
             HarnessSessionStatus.failed.value,
             HarnessSessionStatus.blocked_external.value,
             HarnessSessionStatus.cancelled.value),
        ).fetchall()
        conn.close()
        return [_row_to_session(r) for r in rows]

    # -------------------------------------------------------------------
    # Composer interactions
    # -------------------------------------------------------------------

    def save_interaction(self, inter: ComposerInteraction) -> ComposerInteraction:
        conn = self._connect()
        conn.execute(
            """INSERT INTO composer_interactions
               (id, agent_run_id, task_id, session_id, type, status,
                prompt_excerpt, full_context_ref, created_at, resolved_at,
                composer_reply, metadata_json)
               VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
               ON CONFLICT(id) DO UPDATE SET
                 status=excluded.status,
                 resolved_at=excluded.resolved_at,
                 composer_reply=excluded.composer_reply,
                 metadata_json=excluded.metadata_json""",
            (inter.id, inter.agent_run_id, inter.task_id, inter.session_id,
             inter.type, inter.status, inter.prompt_excerpt,
             inter.full_context_ref, inter.created_at, inter.resolved_at,
             inter.composer_reply, json.dumps(inter.metadata)),
        )
        conn.commit()
        conn.close()
        return inter

    def get_interaction(self, interaction_id: str) -> ComposerInteraction | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM composer_interactions WHERE id=?",
            (interaction_id,),
        ).fetchone()
        conn.close()
        return _row_to_interaction(row) if row else None

    def list_interactions(self, status: str | None = None,
                          task_id: str | None = None,
                          agent_run_id: str | None = None) -> list[ComposerInteraction]:
        clauses: list[str] = []
        params: list[Any] = []
        if status:
            clauses.append("status=?")
            params.append(status)
        if task_id:
            clauses.append("task_id=?")
            params.append(task_id)
        if agent_run_id:
            clauses.append("agent_run_id=?")
            params.append(agent_run_id)
        where = f" WHERE {' AND '.join(clauses)}" if clauses else ""
        conn = self._connect()
        rows = conn.execute(
            f"SELECT * FROM composer_interactions{where} "
            "ORDER BY created_at DESC",
            params,
        ).fetchall()
        conn.close()
        return [_row_to_interaction(r) for r in rows]

    def list_pending_interactions(self) -> list[ComposerInteraction]:
        return self.list_interactions(status=ComposerInteractionStatus.pending.value)

    def update_interaction_status(self, interaction_id: str, status: str,
                                  composer_reply: str | None = None) -> ComposerInteraction | None:
        inter = self.get_interaction(interaction_id)
        if inter is None:
            return None
        inter.status = status
        if composer_reply is not None:
            inter.composer_reply = composer_reply
        if status in (ComposerInteractionStatus.answered.value,
                      ComposerInteractionStatus.cancelled.value,
                      ComposerInteractionStatus.expired.value):
            inter.resolved_at = _now()
        return self.save_interaction(inter)

    # -------------------------------------------------------------------
    # Verification runs
    # -------------------------------------------------------------------

    def save_verification_run(self, vr: VerificationRun) -> VerificationRun:
        conn = self._connect()
        conn.execute(
            """INSERT INTO verification_runs
               (id, agent_run_id, task_id, status, commands_json,
                started_at, completed_at, metadata_json)
               VALUES (?,?,?,?,?,?,?,?)
               ON CONFLICT(id) DO UPDATE SET
                 status=excluded.status,
                 commands_json=excluded.commands_json,
                 completed_at=excluded.completed_at,
                 metadata_json=excluded.metadata_json""",
            (vr.id, vr.agent_run_id, vr.task_id, vr.status,
             json.dumps([c.__dict__ for c in vr.commands]),
             vr.started_at, vr.completed_at, json.dumps(vr.metadata)),
        )
        conn.commit()
        conn.close()
        return vr

    def get_verification_run(self, vr_id: str) -> VerificationRun | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM verification_runs WHERE id=?", (vr_id,)
        ).fetchone()
        conn.close()
        return _row_to_verification_run(row) if row else None

    def get_verification_run_by_agent_run(self, agent_run_id: str) -> VerificationRun | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM verification_runs WHERE agent_run_id=? "
            "ORDER BY started_at DESC LIMIT 1",
            (agent_run_id,),
        ).fetchone()
        conn.close()
        return _row_to_verification_run(row) if row else None

    def list_verification_runs(self, task_id: str | None = None) -> list[VerificationRun]:
        sql = "SELECT * FROM verification_runs"
        params: list[Any] = []
        if task_id:
            sql += " WHERE task_id=?"
            params.append(task_id)
        sql += " ORDER BY started_at DESC"
        conn = self._connect()
        rows = conn.execute(sql, params).fetchall()
        conn.close()
        return [_row_to_verification_run(r) for r in rows]

    # -------------------------------------------------------------------
    # Harness artifacts (enriched; legacy `task_artifacts` untouched)
    # -------------------------------------------------------------------

    def add_harness_artifact(self, agent_run_id: str, task_id: str,
                            kind: str, name: str, path: str,
                            mime_type: str = "application/octet-stream",
                            size_bytes: int = 0,
                            metadata: dict[str, Any] | None = None) -> dict[str, Any]:
        now = _now()
        artifact_id = f"artifact_{uuid.uuid4().hex[:12]}"
        conn = self._connect()
        conn.execute(
            """INSERT INTO harness_artifacts
               (id, agent_run_id, task_id, kind, name, path, mime_type,
                size_bytes, created_at, metadata_json)
               VALUES (?,?,?,?,?,?,?,?,?,?)""",
            (artifact_id, agent_run_id, task_id, kind, name, path, mime_type,
             size_bytes, now, json.dumps(metadata or {})),
        )
        conn.commit()
        conn.close()
        return {
            "id": artifact_id,
            "agent_run_id": agent_run_id,
            "task_id": task_id,
            "kind": kind,
            "name": name,
            "path": path,
            "mime_type": mime_type,
            "size_bytes": size_bytes,
            "created_at": now,
            "metadata": dict(metadata or {}),
        }

    def get_harness_artifact(self, artifact_id: str) -> dict[str, Any] | None:
        conn = self._connect()
        row = conn.execute(
            "SELECT * FROM harness_artifacts WHERE id=?", (artifact_id,)
        ).fetchone()
        conn.close()
        if row is None:
            return None
        d = dict(row)
        try:
            d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
        except (json.JSONDecodeError, KeyError):
            d["metadata"] = {}
        return d

    def list_harness_artifacts(self, agent_run_id: str | None = None,
                               task_id: str | None = None) -> list[dict[str, Any]]:
        clauses: list[str] = []
        params: list[Any] = []
        if agent_run_id:
            clauses.append("agent_run_id=?")
            params.append(agent_run_id)
        if task_id:
            clauses.append("task_id=?")
            params.append(task_id)
        where = f" WHERE {' AND '.join(clauses)}" if clauses else ""
        conn = self._connect()
        rows = conn.execute(
            f"SELECT * FROM harness_artifacts{where} ORDER BY created_at",
            params,
        ).fetchall()
        conn.close()
        out: list[dict[str, Any]] = []
        for r in rows:
            d = dict(r)
            try:
                d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
            except (json.JSONDecodeError, KeyError):
                d["metadata"] = {}
            out.append(d)
        return out


# ---------------------------------------------------------------------------
# Row -> dataclass helpers
# ---------------------------------------------------------------------------


def _row_to_workspace(row: sqlite3.Row) -> RepoWorkspace:
    d = dict(row)
    try:
        d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
    except (json.JSONDecodeError, KeyError):
        d["metadata"] = {}
    return RepoWorkspace(**d)


def _row_to_worktree(row: sqlite3.Row) -> Worktree:
    d = dict(row)
    try:
        d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
    except (json.JSONDecodeError, KeyError):
        d["metadata"] = {}
    return Worktree(**d)


def _row_to_session(row: sqlite3.Row) -> HarnessSession:
    d = dict(row)
    try:
        d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
    except (json.JSONDecodeError, KeyError):
        d["metadata"] = {}
    return HarnessSession(**d)


def _row_to_interaction(row: sqlite3.Row) -> ComposerInteraction:
    d = dict(row)
    try:
        d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
    except (json.JSONDecodeError, KeyError):
        d["metadata"] = {}
    return ComposerInteraction(**d)


def _row_to_verification_run(row: sqlite3.Row) -> VerificationRun:
    d = dict(row)
    try:
        cmds_raw = json.loads(d.pop("commands_json", "[]"))
    except (json.JSONDecodeError, KeyError):
        cmds_raw = []
    try:
        d["metadata"] = json.loads(d.pop("metadata_json", "{}"))
    except (json.JSONDecodeError, KeyError):
        d["metadata"] = {}
    agents_gateway_vr = VerificationRun(
        id=d["id"], agent_run_id=d["agent_run_id"], task_id=d["task_id"],
        status=d["status"], started_at=d["started_at"],
        completed_at=d.get("completed_at"), metadata=d["metadata"],
    )
    for c in cmds_raw:
        agents_gateway_vr.commands.append(VerificationCommandResult(
            name=c["name"], command=c["command"], required=c["required"],
            exit_code=c.get("exit_code"), passed=c.get("passed", False),
            output_artifact=c.get("output_artifact", ""),
            blocked=c.get("blocked", False),
            blocked_reason=c.get("blocked_reason", ""),
            duration_seconds=c.get("duration_seconds", 0.0),
        ))
    return agents_gateway_vr


__all__ = ["HarnessStorage"]
