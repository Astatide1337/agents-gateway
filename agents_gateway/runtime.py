"""Runtime adapter interface, registry, and local-stub adapter."""

from __future__ import annotations

import json
from abc import ABC, abstractmethod
from pathlib import Path
from typing import Any

from agents_gateway.storage import TaskStorage


class RuntimeAdapter(ABC):
    """Abstract interface for runtime adapters."""

    @abstractmethod
    def execute(self, task_id: str) -> dict[str, Any]:
        ...

    @abstractmethod
    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        ...


class RuntimeRegistry:
    """Registry for runtime adapters keyed by runtime type.

    Maps runtime type strings (e.g. "local-stub") to adapter classes
    and provides factory-style creation.
    """

    def __init__(self) -> None:
        self._adapters: dict[str, type[RuntimeAdapter]] = {}

    def register(self, runtime_type: str, adapter_cls: type[RuntimeAdapter]) -> None:
        if not issubclass(adapter_cls, RuntimeAdapter):
            raise TypeError(
                f"{adapter_cls.__name__} must implement RuntimeAdapter"
            )
        self._adapters[runtime_type] = adapter_cls

    def get(self, runtime_type: str) -> type[RuntimeAdapter]:
        adapter = self._adapters.get(runtime_type)
        if adapter is None:
            raise KeyError(
                f"Unsupported runtime type: '{runtime_type}'. "
                f"Supported types: {', '.join(sorted(self._adapters))}"
            )
        return adapter

    def create(self, runtime_type: str, **kwargs: Any) -> RuntimeAdapter:
        adapter_cls = self.get(runtime_type)
        return adapter_cls(**kwargs)

    @property
    def registered_types(self) -> list[str]:
        return list(self._adapters)

    def __contains__(self, runtime_type: str) -> bool:
        return runtime_type in self._adapters


def create_default_registry() -> RuntimeRegistry:
    """Create a RuntimeRegistry pre-populated with the built-in adapters."""
    registry = RuntimeRegistry()
    registry.register("local-stub", StubRuntime)
    return registry


class StubRuntime(RuntimeAdapter):
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
