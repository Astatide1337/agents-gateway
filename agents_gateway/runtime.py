"""Runtime adapter interface, registry, and built-in adapters."""

from __future__ import annotations

import json
import os
import shlex
import subprocess
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
    registry.register("process", ProcessRuntime)
    registry.register("docker", DockerRuntime)
    return registry


class StubRuntime(RuntimeAdapter):
    """Safe local stub runtime that completes tasks without external calls."""

    def __init__(self, storage: TaskStorage, artifacts_dir: str, **kwargs: Any) -> None:
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


class DockerRuntime(RuntimeAdapter):
    """Runtime that executes an agent task in a Docker container.

    The agent manifest must specify ``docker_image`` in the runtime config,
    e.g. ``python:3.14-slim``.  The task input is passed on stdin and the
    container exit code / stdout is captured as structured artifacts.
    """

    def __init__(self, storage: TaskStorage, artifacts_dir: str, docker_image: str = "",
                 command: str = "/bin/cat", **kwargs: Any) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.docker_image = docker_image or "alpine:latest"
        self.command = command

    def execute(self, task_id: str) -> dict[str, Any]:
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")

        if task.status == "created":
            self.storage.update_task_status(task_id, "queued")
        task = self.storage.get_task(task_id)
        if task and task.status == "queued":
            self.storage.update_task_status(task_id, "running")

        self.storage.append_event(task_id, "runtime_started",
                                  {"runtime": "docker", "image": self.docker_image})

        container_name = f"agw-task-{task_id[:12]}"
        try:
            docker_cmd = [
                "docker", "run", "--rm",
                "--name", container_name,
                "-i",  # interactive (stdin)
                self.docker_image,
            ] + shlex.split(self.command)

            result = subprocess.run(
                docker_cmd,
                input=task.input,
                capture_output=True,
                text=True,
                timeout=300,
            )
        except FileNotFoundError:
            return self._complete_with_error(
                task_id, "Docker CLI not found. Install Docker to use docker runtime.")
        except subprocess.TimeoutExpired:
            self._cleanup_container(container_name)
            return self._complete_with_error(task_id, "Container timed out after 300s")
        except Exception as e:
            return self._complete_with_error(task_id, f"Docker execution failed: {e}")

        artifact_data = {
            "agent_id": task.agent_id,
            "task_id": task_id,
            "status": "completed" if result.returncode == 0 else "failed",
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "image": self.docker_image,
        }
        artifact_json = json.dumps(artifact_data, indent=2)
        artifact_path = self.artifacts_dir / task_id / "result.json"
        artifact_path.parent.mkdir(parents=True, exist_ok=True)
        artifact_path.write_text(artifact_json)

        size = len(artifact_json.encode())
        self.storage.add_artifact(task_id, "result.json", str(artifact_path), size)
        self.storage.append_event(task_id, "artifact_created",
                                  {"name": "result.json", "size": size})

        if result.returncode == 0:
            self.storage.update_task_status(task_id, "completed")
            return artifact_data
        else:
            self.storage.update_task_status(task_id, "failed")
            return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        return self._complete_with_error(task_id, error)

    def _complete_with_error(self, task_id: str, error: str) -> dict[str, Any]:
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

    @staticmethod
    def _cleanup_container(name: str) -> None:
        try:
            subprocess.run(["docker", "rm", "-f", name], capture_output=True, timeout=10)
        except Exception:
            pass


class ProcessRuntime(RuntimeAdapter):
    """Runtime that executes an agent script as a subprocess.

    The agent manifest must specify ``command`` in the runtime config,
    e.g. ``python3 agents/my-agent/run.py``.  The command is run with the
    task input passed on stdin and the exit code / stdout captured as
    structured artifacts.
    """

    def __init__(self, storage: TaskStorage, artifacts_dir: str, command: str = "",
                 **kwargs: Any) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.command = command

    def execute(self, task_id: str) -> dict[str, Any]:
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")

        if task.status == "created":
            self.storage.update_task_status(task_id, "queued")
        task = self.storage.get_task(task_id)
        if task and task.status == "queued":
            self.storage.update_task_status(task_id, "running")

        self.storage.append_event(task_id, "runtime_started",
                                  {"runtime": "process", "command": self.command})

        if not self.command:
            return self._complete_with_error(task_id, "No command configured in agent manifest")

        try:
            parsed = shlex.split(self.command)
            result = subprocess.run(
                parsed,
                input=task.input,
                capture_output=True,
                text=True,
                timeout=300,
            )
        except FileNotFoundError:
            return self._complete_with_error(
                task_id, f"Command not found: {parsed[0] if parsed else self.command}")
        except subprocess.TimeoutExpired:
            return self._complete_with_error(task_id, "Command timed out after 300s")
        except Exception as e:
            return self._complete_with_error(task_id, f"Command failed: {e}")

        artifact_data = {
            "agent_id": task.agent_id,
            "task_id": task_id,
            "status": "completed" if result.returncode == 0 else "failed",
            "returncode": result.returncode,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "command": self.command,
        }
        artifact_json = json.dumps(artifact_data, indent=2)
        artifact_path = self.artifacts_dir / task_id / "result.json"
        artifact_path.parent.mkdir(parents=True, exist_ok=True)
        artifact_path.write_text(artifact_json)

        size = len(artifact_json.encode())
        self.storage.add_artifact(task_id, "result.json", str(artifact_path), size)
        self.storage.append_event(task_id, "artifact_created",
                                  {"name": "result.json", "size": size})

        if result.returncode == 0:
            self.storage.update_task_status(task_id, "completed")
            return artifact_data
        else:
            self.storage.update_task_status(task_id, "failed")
            return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        return self._complete_with_error(task_id, error)

    def _complete_with_error(self, task_id: str, error: str) -> dict[str, Any]:
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
