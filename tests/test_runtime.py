"""Tests for stub runtime adapter."""

import json
from pathlib import Path

import pytest

from agents_gateway.runtime import StubRuntime
from agents_gateway.storage import TaskStorage


@pytest.fixture
def storage(tmp_path):
    return TaskStorage(str(tmp_path / "test.db"))


@pytest.fixture
def runtime(storage, tmp_path):
    return StubRuntime(storage, str(tmp_path / "artifacts"))


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
