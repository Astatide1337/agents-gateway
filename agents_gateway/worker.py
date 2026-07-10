"""Background task worker for Agents Gateway.

The worker claims queued tasks atomically (UPDATE ... WHERE status='queued'
returns the row only if this worker wins the race) and drives the runtime
adapter on a worker thread. Routes don't execute runtimes.
"""

from __future__ import annotations

import json
import sqlite3
import threading
import time
import uuid
from pathlib import Path
from typing import Any

from agents_gateway.catalog import AgentCatalog
from agents_gateway.logging import log_event
from agents_gateway.runtime import RuntimeAdapter, RuntimeRegistry
from agents_gateway.storage import TaskStorage, TransitionError


class TaskWorker:
    def __init__(
        self,
        storage: TaskStorage,
        catalog: AgentCatalog,
        runtime_registry: RuntimeRegistry,
        runtime_config: Any,
        artifacts_dir: str,
        poll_interval_seconds: float = 0.5,
        harness_config: Any = None,
    ) -> None:
        self._storage = storage
        self._catalog = catalog
        self._runtime_registry = runtime_registry
        self._runtime_config = runtime_config
        self._artifacts_dir = artifacts_dir
        self._poll_interval = poll_interval_seconds
        self._harness_config = harness_config
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        if self._thread and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._run_loop, name="agw-task-worker", daemon=True
        )
        self._thread.start()
        log_event("worker_started", "Task worker started")

    def stop(self, timeout_seconds: float = 5.0) -> None:
        self._stop.set()
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=timeout_seconds)
        log_event("worker_stopped", "Task worker stopped")

    def is_alive(self) -> bool:
        return self._thread is not None and self._thread.is_alive()

    def _run_loop(self) -> None:
        while not self._stop.is_set():
            claimed = self._claim_next_queued_task()
            if claimed is None:
                time.sleep(self._poll_interval)
                continue
            try:
                self._execute_task(claimed)
            except Exception as e:
                log_event("worker_task_crash",
                          f"Task {claimed} crashed: {e}",
                          task_id=claimed, level="ERROR")
                try:
                    self._storage.append_event(claimed, "worker_crash",
                                              {"error": str(e)})
                    self._storage.update_task_status(claimed, "failed")
                except Exception:
                    pass

    def _claim_next_queued_task(self) -> str | None:
        """Atomically claim a queued task by transitioning it to running.

        Uses an atomic UPDATE that only affects rows with status='queued'.
        SQLite's row-locking on UPDATE guarantees only one writer can match
        a given row at a time, so concurrent workers cannot both claim the
        same task.
        """
        conn = sqlite3.connect(self._storage.db_path)
        conn.row_factory = sqlite3.Row
        try:
            conn.execute("BEGIN IMMEDIATE")
            row = conn.execute(
                "SELECT id, agent_id FROM tasks WHERE status='queued' "
                "ORDER BY created_at ASC LIMIT 1"
            ).fetchone()
            if row is None:
                conn.rollback()
                return None
            now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            cur = conn.execute(
                "UPDATE tasks SET status='running', updated_at=? WHERE id=? AND status='queued'",
                (now, row["id"]),
            )
            if cur.rowcount != 1:
                # Someone else got it.
                conn.rollback()
                return None
            task_id = row["id"]
            agent_id = row["agent_id"]
            run_id = str(uuid.uuid4())
            conn.execute(
                "INSERT INTO task_runs (id, task_id, status, started_at) VALUES (?,?,?,?)",
                (run_id, task_id, "started", now),
            )
            conn.execute(
                "INSERT INTO task_events (id, task_id, event, data_json, created_at) VALUES (?,?,?,?,?)",
                (str(uuid.uuid4()), task_id, "task_running",
                 json.dumps({"from": "queued", "to": "running", "claimed_by": "worker"}), now),
            )
            conn.commit()
            return task_id
        except Exception:
            conn.rollback()
            return None
        finally:
            conn.close()

    def _execute_task(self, task_id: str) -> None:
        task = self._storage.get_task(task_id)
        if task is None:
            return

        # ═══════════════════════════════════════════════════════════
        # Harness-worktree-runtime routing.
        # When `task.metadata.runtime_type == 'harness_session'` the task
        # is composer-controlled: it bypasses the agent catalog and
        # dispatches to the HarnessRuntime which executes the full
        # lifecycle (workspace -> worktree -> tmux session ->
        # verification -> report -> result).
        # ═══════════════════════════════════════════════════════════
        meta = getattr(task, "metadata", {}) or {}
        runtime_type = meta.get("runtime_type")
        if runtime_type == "harness_session":
            self._execute_harness_task(task_id, task)
            return

        agent = self._catalog.get_agent(task.agent_id)
        if agent is None:
            self._storage.append_event(task_id, "runtime_error",
                                      {"error": f"agent '{task.agent_id}' not found"})
            self._storage.update_task_status(task_id, "failed")
            return

        log_event("worker_task_start",
                  f"Executing task {task_id} via {agent.runtime.type}",
                  task_id=task_id, agent_id=task.agent_id,
                  runtime_type=agent.runtime.type)
        self._storage.append_event(task_id, "runtime_started",
                                  {"runtime": agent.runtime.type,
                                   "task_id": task_id})

        try:
            adapter = self._runtime_registry.create(
                agent.runtime.type,
                storage=self._storage,
                artifacts_dir=self._artifacts_dir,
                command=agent.runtime.command,
                docker_image=getattr(agent.runtime, "docker_image", "") or "",
                runtime_config=self._runtime_config,
            )
        except KeyError as e:
            self._storage.append_event(task_id, "runtime_error",
                                      {"error": str(e)})
            self._storage.update_task_status(task_id, "failed")
            return

        try:
            result = adapter.execute(task_id)
            # Convert the adapter's terminal signal into a final state.
            # The adapter may have already moved the task to completed/failed;
            # if not, drive it from result["status"].
            current = self._storage.get_task(task_id)
            if current and current.status == "running":
                final = "completed" if result.get("status") == "completed" else "failed"
                try:
                    self._storage.update_task_status(task_id, final)
                except TransitionError:
                    pass
        except Exception as e:
            self._storage.append_event(task_id, "runtime_error",
                                      {"error": str(e)})
            current = self._storage.get_task(task_id)
            if current and current.status == "running":
                try:
                    self._storage.update_task_status(task_id, "failed")
                except TransitionError:
                    pass

    # -------------------------------------------------------------------
    # Harness-runtime execution path
    # -------------------------------------------------------------------

    def _execute_harness_task(self, task_id: str, task) -> None:
        """Drive a harness_session task through the full lifecycle."""
        # Lazily import so unit tests that don't exercise harness runtime
        # don't pay the import cost.
        from agents_gateway.harness.runtime import (
            HarnessRuntime,
            HarnessRuntimeConfig,
        )
        from agents_gateway.harness.storage import HarnessStorage

        log_event("worker_harness_task_start",
                  f"Executing harness_session task {task_id}",
                  task_id=task_id, agent_id=task.agent_id,
                  runtime_type="harness_session")
        self._storage.append_event(task_id, "runtime_started",
                                  {"runtime": "harness_session",
                                   "task_id": task_id})

        # Build the runtime config from the gateway config if provided;
        # fall back to defaults if caller is invoking the worker directly.
        hcfg = getattr(self, "_harness_config", None)
        if hcfg is None:
            hcfg = HarnessRuntimeConfig(
                use_fake_tmux=True,  # safe default for tests/CLI dev
                auto_commit=False,  # don't auto-commit scratch work
                workspace_root="/tmp/agents-gateway/repos",
                worktree_root="/tmp/agents-gateway/worktrees",
                artifacts_root="/tmp/agents-gateway/artifacts",
            )

        # Build a new HarnessStorage for the same DB path.
        hstorage = HarnessStorage(self._storage.db_path)

        # Pull the rich task spec out of the task input JSON.
        import json as _json
        try:
            spec = _json.loads(task.input) if task.input else {}
        except (ValueError, TypeError):
            spec = {}

        runtime = HarnessRuntime(
            task_storage=self._storage,
            harness_storage=hstorage,
            config=hcfg,
        )
        try:
            result = runtime.execute_task(
                agent_run_id=task_id,  # we reuse task_id as run id for now
                task_id=task_id,
                task_spec=spec,
            )
            # Translate the harness status into the legacy task state
            # machine: completed / failed / waiting.
            final_status = result.status
            if final_status in ("completed", "passed"):
                final_status = "completed"
            elif final_status in ("blocked_external", "stalled",
                                  "waiting_for_reply"):
                # Map non-terminal-but-not-running harness states to
                # the legacy "waiting" task state so users see the task
                # awaiting Composer input. Composer interactions are
                # already created by the runtime/supervisor.
                final_status = "waiting"
            elif final_status == "failed":
                final_status = "failed"
            else:
                final_status = "failed"  # conservative default
            try:
                self._storage.update_task_status(task_id, final_status)
            except TransitionError:
                # If a transition fails (e.g. someone cancelled in the
                # meantime), record and move on.
                self._storage.append_event(task_id, "transition_skipped",
                                          {"target": final_status,
                                           "current": task.status})
        except Exception as e:
            log_event("worker_harness_task_crash",
                      f"Harness task {task_id} crashed: {e}",
                      task_id=task_id, level="ERROR")
            self._storage.append_event(task_id, "runtime_error",
                                      {"error": str(e), "kind": "harness"})
            current = self._storage.get_task(task_id)
            if current and current.status == "running":
                try:
                    self._storage.update_task_status(task_id, "failed")
                except TransitionError:
                    pass
