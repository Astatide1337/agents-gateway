"""Local stub runtime adapter."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from agents_gateway.storage import TaskStorage


class StubRuntime:
    """Safe local stub runtime that completes tasks without external calls."""

    def __init__(self, storage: TaskStorage, artifacts_dir: str) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)

    def execute(self, task_id: str) -> dict[str, Any]:
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")

        if task.status == "created":
            self.storage.update_task_status(task_id, "queued")
        task = self.storage.get_task(task_id)
        if task and task.status == "queued":
            self.storage.update_task_status(task_id, "running")
        self.storage.append_event(task_id, "runtime_started", {"runtime": "local-stub"})

        artifact_data = {
            "agent_id": task.agent_id,
            "task_id": task_id,
            "status": "completed",
            "message": "Stub runtime completed task. Real runtime adapter not configured.",
        }
        artifact_json = json.dumps(artifact_data, indent=2)
        artifact_path = self.artifacts_dir / task_id / "result.json"
        artifact_path.parent.mkdir(parents=True, exist_ok=True)
        artifact_path.write_text(artifact_json)

        size = len(artifact_json.encode())
        self.storage.add_artifact(task_id, "result.json", str(artifact_path), size)
        self.storage.append_event(task_id, "artifact_created", {"name": "result.json", "size": size})
        self.storage.update_task_status(task_id, "completed")

        return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        """Simulate a failure path for testing."""
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")

        if task.status == "created":
            self.storage.update_task_status(task_id, "queued")
        task = self.storage.get_task(task_id)
        if task and task.status == "queued":
            self.storage.update_task_status(task_id, "running")
        self.storage.append_event(task_id, "runtime_error", {"error": error})
        self.storage.update_task_status(task_id, "failed")

        return {"agent_id": task.agent_id, "task_id": task_id, "status": "failed", "error": error}
