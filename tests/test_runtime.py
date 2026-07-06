"""Tests for stub runtime adapter and runtime registry."""

import json
from pathlib import Path

import pytest

from agents_gateway.runtime import (
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
        task = storage.create_task("test-agent")
        runtime.execute(task.id)
        events = storage.list_events(task.id)
        event_names = [e.event for e in events]
        assert "task_created" in event_names
        assert "task_queued" in event_names
        assert "task_running" in event_names
        assert "task_completed" in event_names
        assert "artifact_created" in event_names

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
