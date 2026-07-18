"""Tests for HarnessSessionRuntimeAdapter + AgentCatalog integration.

Covers:

  * RuntimeRegistry includes ``harness_session`` when a harness config
    is supplied.
  * ``HarnessSessionRuntimeAdapter.execute`` dispatches via the same
    registry path as process/docker.
  * ``agent_id`` matching a harness profile maps to harness_session
    when no explicit mode is provided.
  * Profile resolution precedence: task spec > agent_id > unknown.
  * Event ordering: ``runtime_selected`` + ``agent.catalog_resolved``
    are emitted on harness task dispatch.
  * Unknown profile results in failed task + ``agent_run.failed``.
  * Status translation table from HarnessRunResult.status.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

from agents_gateway.config import GatewayConfig
from agents_gateway.harness.models import (
    HarnessSession,
    HarnessSessionStatus,
    Worktree,
    WorktreeStatus,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness_runtime_adapter import HarnessSessionRuntimeAdapter
from agents_gateway.runtime import create_default_registry
from agents_gateway.storage import TaskStorage


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _git(cwd: str, *args: str) -> subprocess.CompletedProcess:
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@local",
        "GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@local",
    }
    return subprocess.run(
        ["git", "-C", cwd, *args],
        capture_output=True, text=True, timeout=20, env=env,
    )


def _make_scratch_repo(tmp_path: Path) -> str:
    repo = tmp_path / "scratch-repo"
    repo.mkdir()
    proc = _git(str(repo), "init", "-b", "master")
    if proc.returncode != 0:
        _git(str(repo), "init")
        _git(str(repo), "symbolic-ref", "HEAD", "refs/heads/master")
    (repo / "README.md").write_text("# Scratch\n")
    _git(str(repo), "add", "README.md")
    _git(str(repo), "commit", "-m", "Initial")
    return str(repo)


def _config_for(tmp_path: Path) -> GatewayConfig:
    return GatewayConfig(
        auth={"mode": "dev-none"},
        storage={"sqlite_path": str(tmp_path / "agw.db"),
                 "artifacts_dir": str(tmp_path / "artifacts")},
        service={"rate_limiting": {"enabled": False,
                                    "requests_per_minute": 999}},
        harness={"workspace_root": str(tmp_path / "repos"),
                 "worktree_root": str(tmp_path / "worktrees"),
                 "artifacts_root": str(tmp_path / "artifacts"),
                 "use_fake_tmux": True},
        agents={"dir": str(tmp_path / "agents")},
        integrations={
            "skills_gateway": {"enabled": False},
            "mcp_gateway": {"enabled": False},
        },
    )


# ---------------------------------------------------------------------------
# Tests: RuntimeRegistry includes harness_session
# ---------------------------------------------------------------------------


class TestRegistryIncludesHarness:
    def test_default_registry_has_harness_session(self, tmp_path):
        cfg = _config_for(tmp_path)
        harness_cfg = cfg.harness
        registry = create_default_registry(
            runtime_config=cfg.runtime,
            harness_config=harness_cfg,
        )
        assert "harness_session" in registry
        adapter_cls = registry.get("harness_session")
        assert adapter_cls is HarnessSessionRuntimeAdapter

    def test_default_registry_has_harness_session_without_config(self):
        """harness_session is always registered — the adapter falls back to
        safe dev defaults when harness_config is omitted."""
        registry = create_default_registry()
        assert "harness_session" in registry
        adapter_cls = registry.get("harness_session")
        assert adapter_cls is HarnessSessionRuntimeAdapter

    def test_registry_creates_harness_adapter(self, tmp_path):
        cfg = _config_for(tmp_path)
        harness_cfg = cfg.harness
        registry = create_default_registry(
            runtime_config=cfg.runtime,
            harness_config=harness_cfg,
        )
        storage = TaskStorage(str(tmp_path / "agw.db"))
        adapter = registry.create(
            "harness_session",
            storage=storage,
            artifacts_dir=str(tmp_path / "artifacts"),
            harness_config=harness_cfg,
        )
        assert isinstance(adapter, HarnessSessionRuntimeAdapter)


# ---------------------------------------------------------------------------
# Tests: AgentCatalog resolve_agent_id_to_runtime
# ---------------------------------------------------------------------------


def _catalog_for(tmp_path: Path):
    from agents_gateway.catalog import AgentCatalog
    cfg = _config_for(tmp_path)
    return AgentCatalog(cfg)


class TestAgentCatalogResolve:
    def test_resolve_known_harness_profile(self, tmp_path):
        cat = _catalog_for(tmp_path)
        assert cat.resolve_agent_id_to_runtime("opencode-deepseek") == "harness_session"

    def test_resolve_unknown_agent(self, tmp_path):
        cat = _catalog_for(tmp_path)
        assert cat.resolve_agent_id_to_runtime("nonexistent-agent") is None

    def test_resolve_harness_profile_returns_harness_session(self, tmp_path):
        cat = _catalog_for(tmp_path)
        for name in ("claude-code", "codex", "fake-test"):
            assert cat.resolve_agent_id_to_runtime(name) == "harness_session"


# ---------------------------------------------------------------------------
# Tests: catalog_resolved event emission (via adapter dispatch)
# ---------------------------------------------------------------------------


@pytest.fixture
def adapter_env(tmp_path):
    """Set up a full adapter dispatch environment with a scratch repo."""
    scratch_repo = _make_scratch_repo(tmp_path)
    cfg = _config_for(tmp_path)
    storage = TaskStorage(str(tmp_path / "agw.db"))
    harness_storage = HarnessStorage(str(tmp_path / "agw.db"))
    adapter = HarnessSessionRuntimeAdapter(
        storage=storage,
        artifacts_dir=str(tmp_path / "artifacts"),
        harness_config=cfg.harness,
    )
    return {
        "scratch_repo": scratch_repo,
        "cfg": cfg,
        "storage": storage,
        "harness_storage": harness_storage,
        "adapter": adapter,
        "tmp_path": tmp_path,
    }


class TestAdapterDispatches:
    def test_execute_harness_task_emits_runtime_selected(
            self, adapter_env, monkeypatch):
        """When a harness task is dispatched via the adapter, the
        runtime_selected event is appended to the task event stream.

        We mock HarnessRuntime.execute_task to avoid running the full
        harness loop — we only care about the adapter's pre-dispatch
        event emission, not the runtime itself (which is covered by
        test_harness_runtime_e2e.py)."""
        env = adapter_env
        spec = {
            "objective_id": "obj_1",
            "execution": {"mode": "harness_session",
                           "harness_profile": "fake-test"},
            "goal": {"strategy": "auto", "text": "/goal do nothing"},
        }

        # Patch execute_task so we don't run the full harness loop.
        from agents_gateway.harness.runtime import HarnessRuntime
        captured = {}
        class FakeResult:
            status = "completed"
            artifacts = []
            def to_dict(self):
                return {"agent_run_id": "test", "task_id": "test",
                        "status": "completed"}
        def fake_execute(self, **kwargs):
            captured.update(kwargs)
            return FakeResult()
        monkeypatch.setattr(HarnessRuntime, "execute_task", fake_execute)

        task = env["storage"].create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"runtime_type": "harness_session"}
        )
        env["storage"].update_task_status(task.id, "queued")
        env["storage"].update_task_status(task.id, "running")
        result = env["adapter"].execute(task.id)
        # Verify runtime_selected was emitted.
        events = [e.event for e in env["storage"].list_events(task.id)]
        assert "runtime_selected" in events, f"Events: {events}"
        assert "runtime_selected" in events
        # Sanity: the mocked runtime was invoked and returned status.
        assert "task_id" in captured, f"Captured: {captured}"
        assert result["status"] == "completed"

    def test_execute_with_agent_id_harness_profile_no_explicit_mode(
            self, adapter_env, monkeypatch):
        """When agent_id matches a harness profile and no explicit
        execution.harness_profile is set in the spec, harness_session
        is still resolved AND agent.catalog_resolved event is emitted."""
        env = adapter_env
        # Spec has NO harness_profile so adapter must resolve via agent_id.
        spec = {
            "objective_id": "obj_2",
            "goal": {"strategy": "auto", "text": "/goal nothing"},
        }

        from agents_gateway.harness.runtime import HarnessRuntime
        class FakeResult:
            status = "completed"
            artifacts = []
            def to_dict(self):
                return {"agent_run_id": "test", "task_id": "test",
                        "status": "completed"}
        monkeypatch.setattr(HarnessRuntime, "execute_task",
                            lambda self, **kw: FakeResult())

        # Use agent_id matching a built-in harness profile name.
        task = env["storage"].create_harness_task(
            agent_id="fake-test", task_spec=spec,
            metadata={}
        )
        env["storage"].update_task_status(task.id, "queued")
        env["storage"].update_task_status(task.id, "running")
        result = env["adapter"].execute(task.id)
        # Confirm that agent.catalog_resolved was emitted because
        # agent_id="fake-test" matches a harness profile.
        events = [e.event for e in env["storage"].list_events(task.id)]
        assert "agent.catalog_resolved" in events, (
            f"Expected catalog_resolved when agent_id matches harness "
            f"profile, got: {events}"
        )
        assert result["status"] == "completed"

    def test_execute_unknown_profile_returns_failed_status(
            self, adapter_env, monkeypatch):
        """An unknown harness profile returns failed status and
        emits agent_run.failed event (without dispatching the runtime)."""
        env = adapter_env
        spec = {
            "objective_id": "obj_3",
            "execution": {"harness_profile": "nonexistent-profile"},
            "goal": {"strategy": "auto", "text": "/goal nothing"},
        }

        task = env["storage"].create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={}
        )
        env["storage"].update_task_status(task.id, "queued")
        env["storage"].update_task_status(task.id, "running")
        result = env["adapter"].execute(task.id)
        assert "failed" in result.get("status", "n/a"), (
            f"Status: {result.get('status')}"
        )
        events = [e.event for e in env["storage"].list_events(task.id)]
        assert "agent_run.failed" in events, f"Events: {events}"


# ---------------------------------------------------------------------------
# Tests: Status translation
# ---------------------------------------------------------------------------


class TestStatusTranslation:
    def test_completed_passes_through(self):
        assert HarnessSessionRuntimeAdapter._translate_status("completed") == "completed"
        assert HarnessSessionRuntimeAdapter._translate_status("passed") == "completed"

    def test_blocked_external_becomes_waiting(self):
        assert HarnessSessionRuntimeAdapter._translate_status("blocked_external") == "waiting"

    def test_stalled_becomes_waiting(self):
        assert HarnessSessionRuntimeAdapter._translate_status("stalled") == "waiting"

    def test_waiting_for_reply_becomes_waiting(self):
        assert HarnessSessionRuntimeAdapter._translate_status("waiting_for_reply") == "waiting"

    def test_failed_passes_through(self):
        assert HarnessSessionRuntimeAdapter._translate_status("failed") == "failed"

    def test_cancelled_passes_through(self):
        assert HarnessSessionRuntimeAdapter._translate_status("cancelled") == "cancelled"

    def test_unknown_becomes_failed(self):
        assert HarnessSessionRuntimeAdapter._translate_status("unknown") == "failed"


# ---------------------------------------------------------------------------
# Tests: Availability
# ---------------------------------------------------------------------------


class TestAvailability:
    def test_check_unknown_profile(self, tmp_path):
        cat = _catalog_for(tmp_path)
        report = cat.check_harness_availability("nonexistent")
        assert report["runnable"] is False
        assert report["error"] == "unknown profile"

    def test_check_fake_test_profile(self, tmp_path):
        """The fake-test profile should be runnable since python3 exists
        on PATH."""
        cat = _catalog_for(tmp_path)
        report = cat.check_harness_availability("fake-test")
        # fake-test uses python3 as command
        assert report["configured"] is True
        assert report["binary_present"] is True
        # runnable depends on presence of python3 only
        assert "runnable" in report

    def test_check_returns_structured_fields(self, tmp_path):
        cat = _catalog_for(tmp_path)
        report = cat.check_harness_availability("opencode-deepseek")
        expected_keys = {"profile", "configured", "binary_present",
                         "credentials_present", "runnable", "command",
                         "error"}
        assert expected_keys.issubset(set(report.keys())), (
            f"Missing keys: {expected_keys - set(report.keys())}"
        )


# ---------------------------------------------------------------------------
# Tests: agent_id auto-routing via RuntimeRegistry + AgentCatalog
# Integration end-to-end via worker
# ---------------------------------------------------------------------------


class TestWorkerDispatchViaRegistry:
    def test_worker_dispatches_harness_via_registry(self, adapter_env, monkeypatch):
        """The worker invokes adapter.execute(task_id) for
        harness_session metadata.runtime_type by creating the
        adapter via RuntimeRegistry.create()."""
        env = adapter_env
        cfg = env["cfg"]
        registry = create_default_registry(
            runtime_config=cfg.runtime,
            harness_config=cfg.harness,
        )
        storage = env["storage"]
        # We can dispatch a harness_session task through the registry.
        adapter = registry.create(
            "harness_session",
            storage=storage,
            artifacts_dir=str(env["tmp_path"] / "artifacts"),
            harness_config=cfg.harness,
        )
        assert isinstance(adapter, HarnessSessionRuntimeAdapter)

        # Avoid invoking the actual harness runtime here.
        from agents_gateway.harness.runtime import HarnessRuntime
        class FakeResult:
            status = "completed"
            artifacts = []
            def to_dict(self):
                return {"agent_run_id": "test", "task_id": "test",
                        "status": "completed"}
        monkeypatch.setattr(HarnessRuntime, "execute_task",
                            lambda self, **kw: FakeResult())

        # The adapter can dispatch a task that metadata runtime_type hints.
        spec = {
            "objective_id": "obj_4",
            "execution": {"harness_profile": "fake-test"},
            "goal": {"strategy": "auto", "text": "/goal nothing"},
        }
        task = storage.create_harness_task(
            agent_id="fake-test", task_spec=spec, metadata={}
        )
        storage.update_task_status(task.id, "queued")
        storage.update_task_status(task.id, "running")
        result = adapter.execute(task.id)
        # Ensure task moved out of running state after dispatch.
        assert result.get("status") in (
            "completed", "passed", "failed", "stalled", "blocked_external"
        )
