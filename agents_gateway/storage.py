"""SQLite task storage with state machine validation."""

from __future__ import annotations

import json
import sqlite3
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from pydantic import BaseModel


VALID_TRANSITIONS: dict[str, set[str]] = {
    "created": {"queued", "cancelled"},
    "queued": {"running", "cancelled"},
    "running": {"waiting", "completed", "failed", "cancelled"},
    "waiting": {"running", "cancelled"},
    "completed": set(),
    "failed": set(),
    "cancelled": set(),
}

ALL_STATES = {"created", "queued", "running", "waiting", "completed", "failed", "cancelled"}


class TaskRecord(BaseModel):
    id: str
    agent_id: str
    status: str
    input: str = ""
    output: str = ""
    error: str = ""
    created_at: str
    updated_at: str


class TaskEvent(BaseModel):
    id: str
    task_id: str
    event: str
    data: dict[str, Any] = {}
    created_at: str


class TaskRun(BaseModel):
    id: str
    task_id: str
    status: str = "started"
    started_at: str
    completed_at: str | None = None


class TaskArtifact(BaseModel):
    id: str
    task_id: str
    name: str
    path: str
    size_bytes: int = 0
    created_at: str


class TransitionError(Exception):
    pass


def validate_transition(current: str, target: str) -> bool:
    if current not in VALID_TRANSITIONS:
        return False
    return target in VALID_TRANSITIONS[current]


class TaskStorage:
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
            CREATE TABLE IF NOT EXISTS tasks (
                id TEXT PRIMARY KEY,
                agent_id TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'created',
                input TEXT DEFAULT '',
                output TEXT DEFAULT '',
                error TEXT DEFAULT '',
                created_at TEXT NOT NULL,
                updated_at TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS task_events (
                id TEXT PRIMARY KEY,
                task_id TEXT NOT NULL,
                event TEXT NOT NULL,
                data_json TEXT DEFAULT '{}',
                created_at TEXT NOT NULL,
                FOREIGN KEY (task_id) REFERENCES tasks(id)
            );
            CREATE TABLE IF NOT EXISTS task_runs (
                id TEXT PRIMARY KEY,
                task_id TEXT NOT NULL,
                status TEXT NOT NULL DEFAULT 'started',
                started_at TEXT NOT NULL,
                completed_at TEXT,
                FOREIGN KEY (task_id) REFERENCES tasks(id)
            );
            CREATE TABLE IF NOT EXISTS task_artifacts (
                id TEXT PRIMARY KEY,
                task_id TEXT NOT NULL,
                name TEXT NOT NULL,
                path TEXT NOT NULL,
                size_bytes INTEGER DEFAULT 0,
                created_at TEXT NOT NULL,
                FOREIGN KEY (task_id) REFERENCES tasks(id)
            );
        """)
        conn.commit()
        conn.close()

    def create_task(self, agent_id: str, input_data: Any = "") -> TaskRecord:
        if not isinstance(input_data, str):
            input_data = json.dumps(input_data)
        now = datetime.now(timezone.utc).isoformat()
        task_id = str(uuid.uuid4())
        conn = self._connect()
        conn.execute(
            "INSERT INTO tasks (id, agent_id, status, input, output, error, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)",
            (task_id, agent_id, "created", input_data, "", "", now, now),
        )
        conn.execute(
            "INSERT INTO task_events (id, task_id, event, data_json, created_at) VALUES (?,?,?,?,?)",
            (str(uuid.uuid4()), task_id, "task_created", json.dumps({"agent_id": agent_id}), now),
        )
        conn.commit()
        conn.close()
        return TaskRecord(id=task_id, agent_id=agent_id, status="created", input=input_data, created_at=now, updated_at=now)

    def get_task(self, task_id: str) -> TaskRecord | None:
        conn = self._connect()
        row = conn.execute("SELECT * FROM tasks WHERE id=?", (task_id,)).fetchone()
        conn.close()
        if row is None:
            return None
        return TaskRecord(**dict(row))

    def list_tasks(self, limit: int = 50, offset: int = 0) -> list[TaskRecord]:
        conn = self._connect()
        rows = conn.execute("SELECT * FROM tasks ORDER BY created_at DESC LIMIT ? OFFSET ?", (limit, offset)).fetchall()
        conn.close()
        return [TaskRecord(**dict(r)) for r in rows]

    def update_task_status(self, task_id: str, new_status: str) -> TaskRecord:
        task = self.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")
        if not validate_transition(task.status, new_status):
            raise TransitionError(f"Invalid transition: {task.status} -> {new_status}")
        now = datetime.now(timezone.utc).isoformat()
        conn = self._connect()
        conn.execute("UPDATE tasks SET status=?, updated_at=? WHERE id=?", (new_status, now, task_id))
        event_name = f"task_{new_status}"
        conn.execute(
            "INSERT INTO task_events (id, task_id, event, data_json, created_at) VALUES (?,?,?,?,?)",
            (str(uuid.uuid4()), task_id, event_name, json.dumps({"from": task.status, "to": new_status}), now),
        )
        conn.commit()
        conn.close()
        task.status = new_status
        task.updated_at = now
        return task

    def cancel_task(self, task_id: str) -> TaskRecord:
        return self.update_task_status(task_id, "cancelled")

    def append_event(self, task_id: str, event: str, data: dict[str, Any] | None = None) -> TaskEvent:
        now = datetime.now(timezone.utc).isoformat()
        event_id = str(uuid.uuid4())
        conn = self._connect()
        conn.execute(
            "INSERT INTO task_events (id, task_id, event, data_json, created_at) VALUES (?,?,?,?,?)",
            (event_id, task_id, event, json.dumps(data or {}), now),
        )
        conn.commit()
        conn.close()
        return TaskEvent(id=event_id, task_id=task_id, event=event, data=data or {}, created_at=now)

    def list_events(self, task_id: str) -> list[TaskEvent]:
        conn = self._connect()
        rows = conn.execute("SELECT * FROM task_events WHERE task_id=? ORDER BY created_at", (task_id,)).fetchall()
        conn.close()
        events = []
        for r in rows:
            d = dict(r)
            try:
                d["data"] = json.loads(d.pop("data_json", "{}"))
            except (json.JSONDecodeError, KeyError):
                d["data"] = {}
            events.append(TaskEvent(**d))
        return events

    def create_run(self, task_id: str) -> TaskRun:
        now = datetime.now(timezone.utc).isoformat()
        run_id = str(uuid.uuid4())
        conn = self._connect()
        conn.execute(
            "INSERT INTO task_runs (id, task_id, status, started_at) VALUES (?,?,?,?)",
            (run_id, task_id, "started", now),
        )
        conn.commit()
        conn.close()
        return TaskRun(id=run_id, task_id=task_id, status="started", started_at=now)

    def add_artifact(self, task_id: str, name: str, path: str, size_bytes: int = 0) -> TaskArtifact:
        now = datetime.now(timezone.utc).isoformat()
        artifact_id = str(uuid.uuid4())
        conn = self._connect()
        conn.execute(
            "INSERT INTO task_artifacts (id, task_id, name, path, size_bytes, created_at) VALUES (?,?,?,?,?,?)",
            (artifact_id, task_id, name, path, size_bytes, now),
        )
        conn.commit()
        conn.close()
        return TaskArtifact(id=artifact_id, task_id=task_id, name=name, path=path, size_bytes=size_bytes, created_at=now)

    def list_artifacts(self, task_id: str) -> list[TaskArtifact]:
        conn = self._connect()
        rows = conn.execute("SELECT * FROM task_artifacts WHERE task_id=? ORDER BY created_at", (task_id,)).fetchall()
        conn.close()
        return [TaskArtifact(**dict(r)) for r in rows]
