"""RuntimeAdapter implementation for the harness_session runtime type.

This adapter wraps ``agents_gateway.harness.runtime.HarnessRuntime`` so
``harness_session`` tasks flow through the same ``RuntimeRegistry``
dispatch path as ``process`` / ``docker`` tasks. The worker no longer
special-cases harness tasks; it constructs the adapter via the
registry and calls ``adapter.execute(task_id)`` like every other
runtime.

The adapter:

  * Pulls the harness task spec out of ``task.input`` (the rich
    JSON body written by ``TaskStorage.create_harness_task``).
  * Builds a ``HarnessRuntime`` instance reusing the gateway's
    ``HarnessRuntimeConfig`` (passed via the registry).
  * Drives the full lifecycle (workspace -> worktree -> session ->
    verification -> report).
  * Translates the resulting ``HarnessRunResult.status`` into the
    legacy task state machine (completed / failed / waiting).
  * Records the harness runtime result + report artifact paths into
    the task's ``task_artifacts`` table so the normal
    ``/tasks/{id}/artifacts`` endpoint surfaces them.

This module is imported lazily by ``runtime.py`` so the import cost
is only paid when ``harness_session`` tasks are dispatched.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from agents_gateway.logging import log_event
from agents_gateway.runtime import RuntimeAdapter
from agents_gateway.storage import TaskStorage, TransitionError


class HarnessSessionRuntimeAdapter(RuntimeAdapter):
    """RuntimeAdapter for ``runtime_type == 'harness_session'`` tasks.

    The adapter is constructed by ``RuntimeRegistry.create`` and
    receives the same kwargs as every other adapter (``storage``,
    ``artifacts_dir``, ``runtime_config``). The optional
    ``harness_config`` kwarg is threaded through from the registry's
    ``harness_config`` attribute (set by ``create_default_registry``).
    """

    def __init__(self,
                 storage: TaskStorage,
                 artifacts_dir: str = "",
                 runtime_config: Any = None,
                 harness_config: Any = None,
                 **_kwargs: Any) -> None:
        self.storage = storage
        self.artifacts_dir = Path(artifacts_dir or "/tmp/agents-gateway/artifacts")
        self.runtime_config = runtime_config
        self.harness_config = harness_config

    # ------------------------------------------------------------------
    # RuntimeAdapter interface
    # ------------------------------------------------------------------

    def execute(self, task_id: str) -> dict[str, Any]:
        # Lazy imports keep server.py startup cost low when no harness
        # tasks are ever dispatched.
        from agents_gateway.harness.runtime import (
            HarnessRuntime,
            HarnessRuntimeConfig,
        )
        from agents_gateway.harness.storage import HarnessStorage
        from agents_gateway.harness.profiles import get_profile

        task = self.storage.get_task(task_id)
        if task is None:
            raise ValueError(f"Task not found: {task_id}")

        # Pull the rich harness task spec out of the `input` column.
        try:
            spec = json.loads(task.input) if task.input else {}
        except (ValueError, TypeError):
            spec = {}

        # Resolve the harness profile: precedence is
        #   1. explicit task spec execution.harness_profile
        #   2. agent_id mapped through the harness-profile catalog
        #      (covers a task created with agent_id="opencode-deepseek"
        #      and no explicit harness_profile)
        profile_name = (spec.get("execution", {}).get("harness_profile")
                        or spec.get("harness_profile")
                        or "")
        if not profile_name:
            # Map agent_id -> harness profile when agent_id matches a
            # known harness profile name.
            if task.agent_id and get_profile(task.agent_id) is not None:
                profile_name = task.agent_id
                spec.setdefault("execution", {})["harness_profile"] = profile_name
                self.storage.append_event(
                    task_id, "agent.catalog_resolved",
                    {"agent_id": task.agent_id,
                     "resolved_to": "harness_session",
                     "harness_profile": profile_name})
        if profile_name and get_profile(profile_name) is None:
            return self._fail(task_id, f"unknown harness profile: {profile_name}")

        # Build the runtime config. Prefer the gateway's harness config
        # (which already has worktrees/artifacts roots etc set); fall
        # back to safe dev defaults if invoked without one (e.g. tests).
        hcfg = self.harness_config
        if hcfg is None:
            hcfg = HarnessRuntimeConfig(
                use_fake_tmux=True,
                auto_commit=False,
                workspace_root=str(self.artifacts_dir.parent / "repos"),
                worktree_root=str(self.artifacts_dir.parent / "worktrees"),
                artifacts_root=str(self.artifacts_dir),
            )

        hstorage = HarnessStorage(self.storage.db_path)
        runtime = HarnessRuntime(
            task_storage=self.storage,
            harness_storage=hstorage,
            task_storage_event_emitter=self.storage,
            config=hcfg,
        )

        log_event("worker_harness_task_start",
                  f"Executing harness_session task {task_id}",
                  task_id=task_id, agent_id=task.agent_id,
                  runtime_type="harness_session")
        self.storage.append_event(task_id, "runtime_selected",
                                  {"runtime": "harness_session",
                                   "harness_profile": profile_name or ""})

        try:
            result = runtime.execute_task(
                agent_run_id=task_id,
                task_id=task_id,
                task_spec=spec,
            )
        except Exception as e:
            log_event("worker_harness_task_crash",
                      f"Harness task {task_id} crashed: {e}",
                      task_id=task_id, level="ERROR")
            self.storage.append_event(task_id, "runtime_error",
                                      {"error": str(e), "kind": "harness"})
            self._finalize(task_id, "failed")
            self._sync_session_failed(task_id)
            return {"agent_run_id": task_id, "task_id": task_id,
                    "status": "failed", "error": str(e)}

        # Translate harness runtime status -> legacy task state machine.
        final = self._translate_status(result.status)
        self._finalize(task_id, final)

        # Record harness artifacts into the legacy task_artifacts table
        # too so /tasks/{id}/artifacts surfaces them uniformly.
        try:
            for a in result.artifacts:
                self.storage.add_artifact(
                    task_id, a.get("name", "artifact"),
                    a.get("path", ""), a.get("size_bytes", 0) or 0,
                )
        except Exception:
            pass

        return result.to_dict()

    def resume(self, task_id: str) -> dict[str, Any] | None:
        """Resume a harness_session task whose driving thread was lost
        (AGW process restart mid-execution). The TaskWorker only ever
        claims ``status='queued'`` tasks, so a task already
        ``status='running'`` when the process died is never picked up
        again on its own — this is the explicit recovery path, called
        from startup reconciliation for exactly those orphaned tasks.

        Returns None when there was nothing to resume (no harness
        session ever existed for this task — safe to leave to normal
        dispatch/retry handling instead).
        """
        from agents_gateway.harness.runtime import (
            HarnessRuntime,
            HarnessRuntimeConfig,
        )
        from agents_gateway.harness.storage import HarnessStorage

        task = self.storage.get_task(task_id)
        if task is None:
            return None
        try:
            spec = json.loads(task.input) if task.input else {}
        except (ValueError, TypeError):
            spec = {}

        hcfg = self.harness_config
        if hcfg is None:
            hcfg = HarnessRuntimeConfig(
                use_fake_tmux=True,
                auto_commit=False,
                workspace_root=str(self.artifacts_dir.parent / "repos"),
                worktree_root=str(self.artifacts_dir.parent / "worktrees"),
                artifacts_root=str(self.artifacts_dir),
            )

        hstorage = HarnessStorage(self.storage.db_path)
        runtime = HarnessRuntime(
            task_storage=self.storage,
            harness_storage=hstorage,
            task_storage_event_emitter=self.storage,
            config=hcfg,
        )

        log_event("worker_harness_task_resume",
                  f"Resuming orphaned harness_session task {task_id}",
                  task_id=task_id, agent_id=task.agent_id,
                  runtime_type="harness_session")

        try:
            result = runtime.resume_task(task_id=task_id, task_spec=spec)
        except Exception as e:
            log_event("worker_harness_task_crash",
                      f"Harness task {task_id} crashed on resume: {e}",
                      task_id=task_id, level="ERROR")
            self.storage.append_event(task_id, "runtime_error",
                                      {"error": str(e), "kind": "harness_resume"})
            self._finalize(task_id, "failed")
            self._sync_session_failed(task_id)
            return {"agent_run_id": task_id, "task_id": task_id,
                    "status": "failed", "error": str(e)}

        if result is None:
            return None

        final = self._translate_status(result.status)
        self._finalize(task_id, final)
        try:
            for a in result.artifacts:
                self.storage.add_artifact(
                    task_id, a.get("name", "artifact"),
                    a.get("path", ""), a.get("size_bytes", 0) or 0,
                )
        except Exception:
            pass
        return result.to_dict()

    def fail(self, task_id: str, error: str = "Simulated failure") -> dict[str, Any]:
        log_event("harness_runtime_failed",
                  f"task {task_id} failed: {error}",
                  task_id=task_id, level="ERROR")
        self.storage.append_event(task_id, "agent_run.failed",
                                  {"reason": error})
        self._finalize(task_id, "failed")
        return {"agent_run_id": task_id, "task_id": task_id,
                "status": "failed", "error": error}

    # ------------------------------------------------------------------
    # helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _translate_status(harness_status: str) -> str:
        """Map a HarnessRunResult.status to a legacy task state."""
        if harness_status in ("completed", "passed"):
            return "completed"
        if harness_status in ("blocked_external", "stalled",
                              "waiting_for_reply"):
            # Non-terminal-but-not-running: surface to Composer as
            # `waiting` so the existing task UI shows it as needing
            # Composer input. Composer interactions already exist.
            return "waiting"
        if harness_status == "failed":
            return "failed"
        if harness_status == "cancelled":
            return "cancelled"
        # Conservative default for any unknown status.
        return "failed"

    def _finalize(self, task_id: str, final_status: str) -> None:
        cur = self.storage.get_task(task_id)
        if cur is None:
            return
        if cur.status == final_status:
            return
        # If the task somehow slipped out of `running` (e.g. someone
        # cancelled it), record the skip rather than fighting the
        # state machine.
        try:
            self.storage.update_task_status(task_id, final_status)
        except TransitionError:
            self.storage.append_event(
                task_id, "transition_skipped",
                {"target": final_status, "current": cur.status},
            )

    def _sync_session_failed(self, task_id: str) -> None:
        """Best-effort: mark this task's harness session terminal so
        `runtime_status` stops shadowing the legacy `status=failed`."""
        from agents_gateway.harness.models import HarnessSessionStatus
        from agents_gateway.harness.storage import HarnessStorage
        try:
            hstorage = HarnessStorage(self.storage.db_path)
            session = hstorage.get_session_by_task(task_id)
            if session is None:
                return
            if session.status in (HarnessSessionStatus.completed.value,
                                  HarnessSessionStatus.failed.value,
                                  HarnessSessionStatus.cancelled.value):
                return
            session.status = HarnessSessionStatus.failed.value
            from datetime import datetime, timezone
            session.ended_at = session.ended_at or datetime.now(timezone.utc).isoformat()
            hstorage.save_session(session)
        except Exception:
            pass

    def _fail(self, task_id: str, reason: str) -> dict[str, Any]:
        self.storage.append_event(task_id, "agent_run.failed", {"reason": reason})
        self._finalize(task_id, "failed")
        return {"agent_run_id": task_id, "task_id": task_id,
                "status": "failed", "error": reason}


__all__ = ["HarnessSessionRuntimeAdapter"]
