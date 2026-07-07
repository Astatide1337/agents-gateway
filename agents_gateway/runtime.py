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

    # Set by create_default_registry() so adapters can read sandbox config.
    runtime_config: Any = None

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


def create_default_registry(runtime_config: Any = None) -> RuntimeRegistry:
    """Create a RuntimeRegistry pre-populated with the built-in adapters."""
    registry = RuntimeRegistry()
    registry.register("local-stub", StubRuntime)
    registry.register("process", ProcessRuntime)
    registry.register("docker", DockerRuntime)
    # Stash the runtime config on the registry so adapters can pull it.
    registry.runtime_config = runtime_config
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
        # Worker owns queued->running->terminal. StubRuntime only emits events.
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
        from agents_gateway.storage import TransitionError
        try:
            self.storage.update_task_status(task_id, "completed")
        except TransitionError:
            pass
        return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        """Simulate a failure path for testing."""
        self.storage.append_event(task_id, "runtime_error", {"error": error})
        from agents_gateway.storage import TransitionError
        try:
            self.storage.update_task_status(task_id, "failed")
        except TransitionError:
            pass
        return {"agent_id": "", "task_id": task_id, "status": "failed", "error": error}


class DockerRuntime(RuntimeAdapter):
    """Runtime that executes an agent task in a hardened Docker container.

    Sandbox flags (mandatory, non-negotiable):
      --rm                          remove container on exit
      -i                            interactive (stdin)
      --network none                no network access by default
      --read-only                   root filesystem read-only
      --cap-drop ALL                drop all Linux capabilities
      --security-opt no-new-privileges  forbid privilege escalation
      --user <non-root>             run as non-root uid
      --memory <limit>              memory ceiling (default 512m)
      --cpus <limit>                CPU quota (default 1.0)
      --pids-limit <n>              PID cap (default 128)
      --tmpfs /tmp:rw,noexec,nosuid,size=<n>   scratch space with noexec

    Network can be enabled ONLY when agent runtime config has
    network=true at the manifest level AND the gateway-level
    RuntimeConfig allows it. Default is no network.

    NEVER mounts:
      /var/run/docker.sock
      host home directory
      repo root
      secret directories
      .env files
      Cloudflare credentials
      SSH keys
    """

    def __init__(self, storage: TaskStorage, artifacts_dir: str,
                 docker_image: str = "", command: str = "/bin/cat",
                 runtime_config: Any = None, **kwargs: Any) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.docker_image = docker_image or "alpine:latest"
        self.command = command
        self.runtime_config = runtime_config

    def _sandbox_flags(self) -> list[str]:
        cfg = self.runtime_config
        memory = getattr(cfg, "docker_memory", "512m") if cfg else "512m"
        cpus = getattr(cfg, "docker_cpus", 1.0) if cfg else 1.0
        pids = getattr(cfg, "docker_pids_limit", 128) if cfg else 128
        tmpfs_size = getattr(cfg, "docker_tmpfs_size", "64m") if cfg else "64m"
        return [
            "--rm",
            "-i",
            "--network", "none",
            "--read-only",
            "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges",
            "--user", "65534:65534",
            "--memory", memory,
            "--cpus", str(cpus),
            "--pids-limit", str(pids),
            "--tmpfs", f"/tmp:rw,noexec,nosuid,size={tmpfs_size}",
        ]

    def execute(self, task_id: str) -> dict[str, Any]:
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")
        # Worker has already moved task to running; we ONLY own running ->
        # terminal. We do not queue/transition here.
        self.storage.append_event(task_id, "runtime_started",
                                  {"runtime": "docker",
                                   "image": self.docker_image,
                                   "sandbox": "hardened"})

        container_name = f"agw-task-{task_id[:12]}"
        timeout = getattr(self.runtime_config, "task_timeout_seconds", 300) if self.runtime_config else 300
        measured_timeout = max(1, int(timeout))
        try:
            docker_cmd = (
                ["docker", "run"]
                + self._sandbox_flags()
                + ["--name", container_name, self.docker_image]
                + shlex.split(self.command)
            )
            result = subprocess.run(
                docker_cmd,
                input=task.input,
                capture_output=True,
                text=True,
                timeout=measured_timeout,
            )
        except FileNotFoundError:
            return self._complete_with_error(
                task_id,
                "Docker CLI not found. Install Docker to use docker runtime.",
            )
        except subprocess.TimeoutExpired:
            self._cleanup_container(container_name)
            return self._complete_with_error(task_id, f"Container timed out after {measured_timeout}s")
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
            "sandbox": "hardened",
        }
        artifact_json = json.dumps(artifact_data, indent=2)
        artifact_path = self.artifacts_dir / task_id / "result.json"
        artifact_path.parent.mkdir(parents=True, exist_ok=True)
        artifact_path.write_text(artifact_json)

        size = len(artifact_json.encode())
        self.storage.add_artifact(task_id, "result.json", str(artifact_path), size)
        self.storage.append_event(task_id, "artifact_created",
                                  {"name": "result.json", "size": size})

        self._storage_finalize(task_id, result.returncode)
        return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        return self._complete_with_error(task_id, error)

    def _complete_with_error(self, task_id: str, error: str) -> dict[str, Any]:
        self.storage.append_event(task_id, "runtime_error", {"error": error})
        self._storage_finalize(task_id, returncode=1)
        return {"agent_id": "", "task_id": task_id, "status": "failed", "error": error}

    def _storage_finalize(self, task_id: str, returncode: int) -> None:
        """Worker owns queued->running; here we ONLY attempt running->terminal.
        Worker checks task status afterwards and transitions to the real
        terminal state if needed. This keeps a single authority (the worker)
        for state transitions.
        """
        from agents_gateway.storage import TransitionError
        try:
            self.storage.update_task_status(
                task_id, "completed" if returncode == 0 else "failed"
            )
        except TransitionError:
            # Worker thread always re-checks terminal state after execute().
            pass

    @staticmethod
    def _cleanup_container(name: str) -> None:
        try:
            subprocess.run(["docker", "rm", "-f", name],
                           capture_output=True, timeout=10)
        except Exception:
            pass


class ProcessRuntime(RuntimeAdapter):
    """Runtime that executes an agent script as a subprocess.

    WARNING: ProcessRuntime is NOT a sandbox. It runs on the host/container
    process environment and inherits user, filesystem, network, environment,
    and secrets visible to the gateway process. Use it only for trusted
    local workflows and NEVER in production unless explicitly allowed via
    ``AGW_RUNTIME__ALLOW_PROCESS=true`` and only with vetted agent manifests.

    ProcessRuntime is gated on production by the gateway startup. If
    `auth.environment == 'production'` AND
    `runtime.allow_process == False` then attempts to use a
    `runtime.type: process` manifest will fail at the registry.create step
    with a KeyError explaining the policy.
    """

    def __init__(self, storage: TaskStorage, artifacts_dir: str,
                 command: str = "", runtime_config: Any = None,
                 **kwargs: Any) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir)
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        self.command = command
        self.runtime_config = runtime_config

    def _check_allowed(self) -> None:
        cfg = self.runtime_config
        if cfg is None:
            return
        env = getattr(cfg, "_environment", "dev")
        allow = getattr(cfg, "allow_process", False)
        if env == "production" and not allow:
            raise KeyError(
                "ProcessRuntime is disabled in production. Set "
                "AGW_RUNTIME__ALLOW_PROCESS=true on the gateway to permit "
                "trusted-local process execution."
            )

    def execute(self, task_id: str) -> dict[str, Any]:
        self._check_allowed()
        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")
        # Worker owns transitions; we only emit events and finalize.
        self.storage.append_event(task_id, "runtime_started",
                                  {"runtime": "process",
                                   "command": self.command,
                                   "sandbox": "none-process-runtime-is-trusted-only"})
        if not self.command:
            return self._complete_with_error(task_id, "No command configured in agent manifest")
        timeout = getattr(self.runtime_config, "task_timeout_seconds", 300) \
            if self.runtime_config else 300
        try:
            parsed = shlex.split(self.command)
            result = subprocess.run(
                parsed,
                input=task.input,
                capture_output=True,
                text=True,
                timeout=timeout,
            )
        except FileNotFoundError:
            return self._complete_with_error(task_id, f"Command not found: {parsed[0] if parsed else self.command}")
        except subprocess.TimeoutExpired:
            return self._complete_with_error(task_id, f"Command timed out after {timeout}s")
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
        self._storage_finalize(task_id, result.returncode)
        return artifact_data

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        return self._complete_with_error(task_id, error)

    def _complete_with_error(self, task_id: str, error: str) -> dict[str, Any]:
        self.storage.append_event(task_id, "runtime_error",
                                  {"error": error})
        self._storage_finalize(task_id, returncode=1)
        return {"agent_id": "", "task_id": task_id, "status": "failed", "error": error}

    def _storage_finalize(self, task_id: str, returncode: int) -> None:
        from agents_gateway.storage import TransitionError
        try:
            self.storage.update_task_status(
                task_id, "completed" if returncode == 0 else "failed"
            )
        except TransitionError:
            pass
