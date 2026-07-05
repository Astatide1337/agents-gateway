"""Tests for task storage and state machine."""

import tempfile
from pathlib import Path

import pytest

from agents_gateway.storage import (
    ALL_STATES,
    TaskStorage,
    TransitionError,
    validate_transition,
)


@pytest.fixture
def storage(tmp_path):
    db_path = str(tmp_path / "test.db")
    return TaskStorage(db_path)


class TestValidateTransition:
    @pytest.mark.parametrize("current,target", [
        ("created", "queued"),
        ("created", "cancelled"),
        ("queued", "running"),
        ("queued", "cancelled"),
        ("running", "waiting"),
        ("running", "completed"),
        ("running", "failed"),
        ("running", "cancelled"),
        ("waiting", "running"),
        ("waiting", "cancelled"),
    ])
    def test_valid_transitions(self, current, target):
        assert validate_transition(current, target) is True

    @pytest.mark.parametrize("current,target", [
        ("completed", "running"),
        ("failed", "queued"),
        ("cancelled", "running"),
        ("created", "running"),
        ("queued", "completed"),
        ("running", "created"),
    ])
    def test_invalid_transitions(self, current, target):
        assert validate_transition(current, target) is False


class TestTaskStorage:
    def test_create_task(self, storage):
        task = storage.create_task("test-agent", "test input")
        assert task.id
        assert task.agent_id == "test-agent"
        assert task.status == "created"

    def test_get_task(self, storage):
        task = storage.create_task("test-agent")
        fetched = storage.get_task(task.id)
        assert fetched is not None
        assert fetched.id == task.id

    def test_get_task_not_found(self, storage):
        assert storage.get_task("nonexistent") is None

    def test_list_tasks(self, storage):
        storage.create_task("agent-1")
        storage.create_task("agent-2")
        tasks = storage.list_tasks()
        assert len(tasks) == 2

    def test_list_tasks_filters_by_status(self, storage):
        created = storage.create_task("agent-1")
        queued = storage.create_task("agent-2")
        storage.update_task_status(queued.id, "queued")

        tasks = storage.list_tasks(status="queued")

        assert [t.id for t in tasks] == [queued.id]
        assert created.id not in [t.id for t in tasks]

    def test_list_tasks_filters_by_agent_id(self, storage):
        agent_1_task = storage.create_task("agent-1")
        storage.create_task("agent-2")

        tasks = storage.list_tasks(agent_id="agent-1")

        assert [t.id for t in tasks] == [agent_1_task.id]

    def test_list_tasks_rejects_invalid_status(self, storage):
        with pytest.raises(ValueError, match="Invalid task status"):
            storage.list_tasks(status="not-a-status")

    def test_update_task_status(self, storage):
        task = storage.create_task("test-agent")
        updated = storage.update_task_status(task.id, "queued")
        assert updated.status == "queued"
        updated = storage.update_task_status(task.id, "running")
        assert updated.status == "running"

    def test_invalid_transition_raises(self, storage):
        task = storage.create_task("test-agent")
        with pytest.raises(TransitionError):
            storage.update_task_status(task.id, "running")

    def test_cancel_from_created(self, storage):
        task = storage.create_task("test-agent")
        cancelled = storage.cancel_task(task.id)
        assert cancelled.status == "cancelled"

    def test_cancel_from_queued(self, storage):
        task = storage.create_task("test-agent")
        storage.update_task_status(task.id, "queued")
        cancelled = storage.cancel_task(task.id)
        assert cancelled.status == "cancelled"

    def test_cancel_from_running(self, storage):
        task = storage.create_task("test-agent")
        storage.update_task_status(task.id, "queued")
        storage.update_task_status(task.id, "running")
        cancelled = storage.cancel_task(task.id)
        assert cancelled.status == "cancelled"

    def test_cancel_completed_raises(self, storage):
        task = storage.create_task("test-agent")
        storage.update_task_status(task.id, "queued")
        storage.update_task_status(task.id, "running")
        storage.update_task_status(task.id, "completed")
        with pytest.raises(TransitionError):
            storage.cancel_task(task.id)

    def test_full_lifecycle(self, storage):
        task = storage.create_task("test-agent", "input")
        storage.update_task_status(task.id, "queued")
        storage.update_task_status(task.id, "running")
        storage.update_task_status(task.id, "completed")
        final = storage.get_task(task.id)
        assert final.status == "completed"


class TestEvents:
    def test_append_event(self, storage):
        task = storage.create_task("test-agent")
        event = storage.append_event(task.id, "custom_event", {"key": "value"})
        assert event.event == "custom_event"
        assert event.data == {"key": "value"}

    def test_list_events(self, storage):
        task = storage.create_task("test-agent")
        storage.append_event(task.id, "event_1", {"step": 1})
        storage.append_event(task.id, "event_2", {"step": 2})
        events = storage.list_events(task.id)
        assert len(events) >= 3  # task_created + 2 custom

    def test_events_append_only(self, storage):
        task = storage.create_task("test-agent")
        events_before = storage.list_events(task.id)
        storage.append_event(task.id, "extra", {})
        events_after = storage.list_events(task.id)
        assert len(events_after) > len(events_before)


class TestRuns:
    def test_create_run(self, storage):
        task = storage.create_task("test-agent")
        run = storage.create_run(task.id)
        assert run.task_id == task.id
        assert run.status == "started"


class TestArtifacts:
    def test_add_artifact(self, storage):
        task = storage.create_task("test-agent")
        artifact = storage.add_artifact(task.id, "result.json", "/data/result.json", 1024)
        assert artifact.name == "result.json"
        assert artifact.size_bytes == 1024

    def test_list_artifacts(self, storage):
        task = storage.create_task("test-agent")
        storage.add_artifact(task.id, "a.json", "/a", 100)
        storage.add_artifact(task.id, "b.json", "/b", 200)
        artifacts = storage.list_artifacts(task.id)
        assert len(artifacts) == 2
