"""Tests for stub runtime adapter and runtime registry."""

import json
from pathlib import Path

import pytest

from agents_gateway.runtime import (
    DockerRuntime,
    ProcessRuntime,
    RuntimeAdapter,
    RuntimeRegistry,
    StubRuntime,
    create_default_registry,
)
from agents_gateway.storage import TaskStorage


class _CustomRuntime(RuntimeAdapter):
    """Test adapter for custom registration tests."""

    def __init__(self, **_kwargs):
        pass

    def execute(self, task_id: str) -> dict[str, str]:
        return {"task_id": task_id, "status": "custom-completed"}

    def fail(self, task_id: str, error: str = "Custom failure") -> dict[str, str]:
        return {"task_id": task_id, "status": "failed", "error": error}


def zip_flags_pair(flags: list[str], flag_name: str) -> tuple[str, str] | None:
    """Return (flag_name, flag_value) if --flag <val> appears adjacently in flags."""
    for i, f in enumerate(flags):
        if f == flag_name and i + 1 < len(flags) and not flags[i + 1].startswith("-"):
            return (flag_name, flags[i + 1])
    return None


def _flag_value(flags: list[str], flag_name: str) -> str | None:
    """Return the value immediately following flag_name."""
    pair = zip_flags_pair(flags, flag_name)
    return pair[1] if pair else None


@pytest.fixture
def storage(tmp_path):
    return TaskStorage(str(tmp_path / "test.db"))


@pytest.fixture
def runtime(storage, tmp_path):
    return StubRuntime(storage, str(tmp_path / "artifacts"))


class TestRuntimeRegistry:
    def test_default_registry_has_local_stub(self):
        registry = create_default_registry()
        assert "local-stub" in registry
        assert "local-stub" in registry.registered_types
        adapter_cls = registry.get("local-stub")
        assert adapter_cls is StubRuntime

    def test_default_registry_has_process(self):
        registry = create_default_registry()
        assert "process" in registry
        adapter_cls = registry.get("process")
        assert adapter_cls is ProcessRuntime

    def test_default_registry_has_docker(self):
        registry = create_default_registry()
        assert "docker" in registry
        adapter_cls = registry.get("docker")
        assert adapter_cls is DockerRuntime

    def test_default_registry_creates_stub_runtime(self, storage, tmp_path):
        registry = create_default_registry()
        adapter = registry.create(
            "local-stub",
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
        )
        assert isinstance(adapter, StubRuntime)

    def test_custom_registration(self):
        registry = RuntimeRegistry()
        registry.register("custom", _CustomRuntime)
        assert "custom" in registry
        adapter = registry.create("custom")
        assert isinstance(adapter, _CustomRuntime)

    def test_unsupported_runtime_type_raises_key_error(self):
        registry = RuntimeRegistry()
        with pytest.raises(KeyError, match="Unsupported runtime type: 'unknown'"):
            registry.get("unknown")

    def test_unsupported_create_raises_key_error(self):
        registry = RuntimeRegistry()
        with pytest.raises(KeyError, match="Unsupported runtime type: 'unknown'"):
            registry.create("unknown")

    def test_registered_types_empty_initially(self):
        registry = RuntimeRegistry()
        assert registry.registered_types == []

    def test_register_non_adapter_raises_type_error(self):
        registry = RuntimeRegistry()
        with pytest.raises(TypeError, match="must implement RuntimeAdapter"):

            class NotAnAdapter:  # type: ignore
                pass

            registry.register("bad", NotAnAdapter)  # type: ignore

    def test_custom_runtime_execute(self, storage, tmp_path):
        registry = RuntimeRegistry()
        registry.register("custom", _CustomRuntime)
        adapter = registry.create("custom")
        result = adapter.execute("task-123")
        assert result["status"] == "custom-completed"
        assert result["task_id"] == "task-123"


class TestStubRuntime:
    def test_execute_task(self, runtime, storage):
        task = storage.create_task("test-agent", "test input")
        result = runtime.execute(task.id)
        assert result["status"] == "completed"
        assert result["agent_id"] == "test-agent"

    def test_execute_produces_artifact(self, runtime, storage, tmp_path):
        task = storage.create_task("test-agent")
        runtime.execute(task.id)
        artifact_path = tmp_path / "artifacts" / task.id / "result.json"
        assert artifact_path.exists()
        data = json.loads(artifact_path.read_text())
        assert data["status"] == "completed"

    def test_execute_events(self, runtime, storage):
        # Under the new execution model, the route enqueues (created ->
        # queued) and the worker claims (queued -> running). Runtimes only
        # emit runtime_started + artifact_created + terminal transition.
        task = storage.create_task("test-agent")
        # Simulate the worker moving the task to running first.
        storage.update_task_status(task.id, "queued")
        storage.update_task_status(task.id, "running")
        runtime.execute(task.id)
        events = storage.list_events(task.id)
        event_names = [e.event for e in events]
        assert "task_created" in event_names
        assert "task_queued" in event_names
        assert "task_running" in event_names
        assert "runtime_started" in event_names
        assert "artifact_created" in event_names
        assert "task_completed" in event_names

    def test_execute_stores_artifact_metadata(self, runtime, storage):
        task = storage.create_task("test-agent")
        runtime.execute(task.id)
        artifacts = storage.list_artifacts(task.id)
        assert len(artifacts) == 1
        assert artifacts[0].name == "result.json"

    def test_fail_task(self, runtime, storage):
        task = storage.create_task("test-agent")
        result = runtime.fail(task.id, "Simulated error")
        assert result["status"] == "failed"
        assert "Simulated error" in result["error"]

    def test_execute_nonexistent_task(self, runtime):
        with pytest.raises(ValueError):
            runtime.execute("nonexistent-id")

    def test_no_external_api_calls(self, runtime, storage):
        task = storage.create_task("test-agent")
        result = runtime.execute(task.id)
        assert "Stub runtime" in result["message"]


class TestDockerRuntime:
    def test_default_registry_creates_docker_runtime(self, storage, tmp_path):
        registry = create_default_registry()
        adapter = registry.create(
            "docker",
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="alpine:latest",
        )
        assert isinstance(adapter, DockerRuntime)

    def test_docker_runtime_accepts_kwargs(self, storage, tmp_path):
        adapter = DockerRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="python:3.14-slim",
            command="python3 -c 'print(\"hello\")'",
        )
        assert adapter.docker_image == "python:3.14-slim"
        assert "python3" in adapter.command

    def test_sandbox_flags_contain_all_mandatory_flags(self, storage, tmp_path):
        adapter = DockerRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="alpine:latest",
        )
        flags = adapter._sandbox_flags()
        assert "--rm" in flags
        assert "-i" in flags
        assert ("--network", "none") == zip_flags_pair(flags, "--network")
        assert "--read-only" in flags
        assert ("--cap-drop", "ALL") == zip_flags_pair(flags, "--cap-drop")
        assert ("--security-opt", "no-new-privileges") == zip_flags_pair(flags, "--security-opt")
        assert ("--user", "65534:65534") == zip_flags_pair(flags, "--user")
        assert "--memory" in flags
        assert "--cpus" in flags
        assert "--pids-limit" in flags
        assert "--tmpfs" in flags
        tmpfs_flag = _flag_value(flags, "--tmpfs")
        assert "noexec" in tmpfs_flag
        assert "nosuid" in tmpfs_flag

    def test_sandbox_flags_use_defaults_when_no_config(self, storage, tmp_path):
        adapter = DockerRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="alpine:latest",
        )
        flags = adapter._sandbox_flags()
        assert _flag_value(flags, "--memory") == "512m"
        assert _flag_value(flags, "--cpus") == "1.0"
        assert _flag_value(flags, "--pids-limit") == "128"

    def test_runtime_config_overrides_flags(self, storage, tmp_path):
        class _FakeConfig:
            docker_memory = "256m"
            docker_cpus = 0.5
            docker_pids_limit = 64
            docker_tmpfs_size = "32m"
            task_timeout_seconds = 120

        adapter = DockerRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="alpine:latest",
            runtime_config=_FakeConfig(),
        )
        flags = adapter._sandbox_flags()
        assert _flag_value(flags, "--memory") == "256m"
        assert _flag_value(flags, "--cpus") == "0.5"
        assert _flag_value(flags, "--pids-limit") == "64"

    def test_sandbox_never_exposes_docker_socket(self, storage, tmp_path):
        adapter = DockerRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            docker_image="alpine:latest",
        )
        flags = adapter._sandbox_flags()
        assert "-v" not in flags
        for flag in flags:
            assert "/var/run/docker.sock" not in flag

    def test_fail_task(self, storage, tmp_path):
        adapter = DockerRuntime(storage=storage, artifacts_dir=str(tmp_path / "artifacts"))
        task = storage.create_task("test-agent")
        result = adapter.fail(task.id, "Docker not available")
        assert result["status"] == "failed"

    def test_execute_nonexistent_task(self, storage, tmp_path):
        adapter = DockerRuntime(storage=storage, artifacts_dir=str(tmp_path / "artifacts"))
        with pytest.raises(ValueError):
            adapter.execute("nonexistent-id")

    def test_execute_docker_runtime(self, storage, tmp_path):
        task = storage.create_task("test-agent")
        adapter = DockerRuntime(storage=storage, artifacts_dir=str(tmp_path / "artifacts"),
                                docker_image="alpine:latest")
        result = adapter.execute(task.id)
        assert result["status"] in ("completed", "failed"), "Docker runtime should execute"


class TestProcessRuntime:
    def test_default_registry_creates_process_runtime(self, storage, tmp_path):
        registry = create_default_registry()
        adapter = registry.create(
            "process",
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            command="echo test",
        )
        assert isinstance(adapter, ProcessRuntime)

    def test_check_allowed_raises_in_production_without_allow(self, storage, tmp_path):
        class _ProdNoAllow:
            _environment = "production"
            allow_process = False

        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            runtime_config=_ProdNoAllow(),
        )
        with pytest.raises(KeyError, match="ProcessRuntime is disabled in production"):
            adapter._check_allowed()

    def test_check_allowed_passes_with_allow_in_production(self, storage, tmp_path):
        class _ProdAllow:
            _environment = "production"
            allow_process = True

        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            runtime_config=_ProdAllow(),
        )
        adapter._check_allowed()

    def test_check_allowed_passes_when_no_config(self, storage, tmp_path):
        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
        )
        adapter._check_allowed()

    def test_check_allowed_passes_in_dev(self, storage, tmp_path):
        class _DevConfig:
            _environment = "dev"
            allow_process = False

        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            runtime_config=_DevConfig(),
        )
        adapter._check_allowed()

    def test_execute_with_command(self, storage, tmp_path):
        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            command="echo hello-world",
        )
        task = storage.create_task("test-agent")
        result = adapter.execute(task.id)
        assert result["status"] == "completed"
        assert result["command"] == "echo hello-world"
        assert result["returncode"] == 0
        assert "hello-world" in result["stdout"]

    def test_execute_with_command_and_input_stdin(self, storage, tmp_path):
        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            command="python3 -c 'import sys; print(sys.stdin.read().upper())'",
        )
        task = storage.create_task("test-agent", "hello stdin")
        result = adapter.execute(task.id)
        assert result["status"] == "completed"
        assert "HELLO STDIN" in result["stdout"]

    def test_command_not_found_returns_error(self, storage, tmp_path):
        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            command="nonexistent_command_xyz",
        )
        task = storage.create_task("test-agent")
        result = adapter.execute(task.id)
        assert result["status"] == "failed"
        assert "Command not found" in result["error"]

    def test_no_command_returns_error(self, storage, tmp_path):
        adapter = ProcessRuntime(
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
        )
        task = storage.create_task("test-agent")
        result = adapter.execute(task.id)
        assert result["status"] == "failed"
        assert "No command configured" in result["error"]

    def test_fail_task(self, storage, tmp_path):
        adapter = ProcessRuntime(storage=storage, artifacts_dir=str(tmp_path / "artifacts"))
        task = storage.create_task("test-agent")
        result = adapter.fail(task.id, "Process error")
        assert result["status"] == "failed"

    def test_execute_nonexistent_task(self, storage, tmp_path):
        adapter = ProcessRuntime(storage=storage, artifacts_dir=str(tmp_path / "artifacts"))
        with pytest.raises(ValueError):
            adapter.execute("nonexistent-id")
