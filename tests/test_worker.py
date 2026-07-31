"""Tests for TaskWorker's execution pool.

Regression coverage for a real bug caught by live-running Conductor's
scripts/e2e-composer-live.sh: TaskWorker ran exactly one thread
server-wide, and execute_task blocks for a runtime adapter's entire
execute() call — so a single stuck/slow task blocked ALL other queued
tasks indefinitely, and Composer's own max_parallel_tasks setting had
no effect at the Agents Gateway execution layer at all. There was no
prior test coverage for TaskWorker at all.
"""

from __future__ import annotations

import threading
import time

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.runtime import RuntimeAdapter, RuntimeRegistry
from agents_gateway.storage import TaskStorage
from agents_gateway.worker import TaskWorker


class _SlowAdapter(RuntimeAdapter):
    """Sleeps briefly then reports completed — used to prove whether
    multiple tasks actually run concurrently or are serialized."""

    _sleep_seconds = 0.3

    def __init__(self, **kwargs) -> None:
        pass

    def execute(self, task_id: str) -> dict:
        time.sleep(self._sleep_seconds)
        return {"status": "completed"}

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict:
        return {"status": "failed", "error": error}


def _make_worker(tmp_path, pool_size: int) -> tuple[TaskWorker, TaskStorage]:
    storage = TaskStorage(str(tmp_path / "agw.db"))
    catalog = AgentCatalog(GatewayConfig())
    registry = RuntimeRegistry()
    registry.register("test-slow", _SlowAdapter)
    worker = TaskWorker(
        storage=storage, catalog=catalog, runtime_registry=registry,
        runtime_config=GatewayConfig().runtime, artifacts_dir=str(tmp_path / "artifacts"),
        poll_interval_seconds=0.05, pool_size=pool_size,
    )
    return worker, storage


def _enqueue_n_tasks(storage: TaskStorage, n: int) -> list[str]:
    ids = []
    for _ in range(n):
        t = storage.create_task(agent_id="whatever", metadata={"runtime_type": "test-slow"})
        storage.update_task_status(t.id, "queued")
        ids.append(t.id)
    return ids


class TestTaskWorkerPool:
    def test_default_pool_size_is_one(self, tmp_path):
        """Backward-compatible default for any direct caller that
        doesn't explicitly opt into a pool."""
        storage = TaskStorage(str(tmp_path / "agw.db"))
        catalog = AgentCatalog(GatewayConfig())
        registry = RuntimeRegistry()
        worker = TaskWorker(
            storage=storage, catalog=catalog, runtime_registry=registry,
            runtime_config=GatewayConfig().runtime, artifacts_dir=str(tmp_path / "artifacts"),
        )
        assert worker._pool_size == 1

    def test_start_creates_pool_size_threads(self, tmp_path):
        worker, _ = _make_worker(tmp_path, pool_size=3)
        try:
            worker.start()
            assert len(worker._threads) == 3
            assert all(t.is_alive() for t in worker._threads)
        finally:
            worker.stop()

    def test_pool_processes_tasks_concurrently_not_serially(self, tmp_path):
        """The actual regression proof: N slow tasks with pool_size=N
        must finish in roughly one task's duration, not N times that —
        otherwise the pool isn't providing real concurrency."""
        n = 3
        worker, storage = _make_worker(tmp_path, pool_size=n)
        ids = _enqueue_n_tasks(storage, n)
        start = time.monotonic()
        try:
            worker.start()
            deadline = time.monotonic() + 5.0
            while time.monotonic() < deadline:
                statuses = [storage.get_task(i).status for i in ids]
                if all(s == "completed" for s in statuses):
                    break
                time.sleep(0.05)
        finally:
            worker.stop()
        elapsed = time.monotonic() - start
        statuses = [storage.get_task(i).status for i in ids]
        assert all(s == "completed" for s in statuses), statuses
        # Serial execution would take >= n * _SlowAdapter._sleep_seconds
        # (0.9s for n=3); concurrent execution finishes well under that.
        assert elapsed < n * _SlowAdapter._sleep_seconds, (
            f"tasks took {elapsed:.2f}s — looks serialized, not concurrent"
        )

    def test_single_slow_task_does_not_block_other_queued_tasks(self, tmp_path):
        """The exact bug scenario: with pool_size >= 2, a permanently
        stuck task must not prevent a second, unrelated task from
        being claimed and completed."""
        class _StuckAdapter(RuntimeAdapter):
            def __init__(self, **kwargs) -> None:
                pass

            def execute(self, task_id: str) -> dict:
                time.sleep(30)  # never finishes within this test's window
                return {"status": "completed"}

            def fail(self, task_id: str, error: str = "x") -> dict:
                return {"status": "failed", "error": error}

        storage = TaskStorage(str(tmp_path / "agw.db"))
        catalog = AgentCatalog(GatewayConfig())
        registry = RuntimeRegistry()
        registry.register("test-stuck", _StuckAdapter)
        registry.register("test-slow", _SlowAdapter)
        worker = TaskWorker(
            storage=storage, catalog=catalog, runtime_registry=registry,
            runtime_config=GatewayConfig().runtime, artifacts_dir=str(tmp_path / "artifacts"),
            poll_interval_seconds=0.05, pool_size=2,
        )
        stuck = storage.create_task(agent_id="whatever", metadata={"runtime_type": "test-stuck"})
        storage.update_task_status(stuck.id, "queued")
        fine = storage.create_task(agent_id="whatever", metadata={"runtime_type": "test-slow"})
        storage.update_task_status(fine.id, "queued")

        try:
            worker.start()
            deadline = time.monotonic() + 3.0
            fine_completed = False
            while time.monotonic() < deadline:
                if storage.get_task(fine.id).status == "completed":
                    fine_completed = True
                    break
                time.sleep(0.05)
        finally:
            worker.stop(timeout_seconds=0.5)
        assert fine_completed, "second task never completed — stuck task blocked the pool"
