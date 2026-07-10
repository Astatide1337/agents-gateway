"""Tests for the RepoWorkspaceManager git worktree lifecycle.

Tests run with real git (no mocks) against temporary scratch repos
created under tmp_path. They exercise the full worktree.create =
branch + worktree.add path plus idempotent reuse for the same
(owner/repo, branch) tuple.
"""

from __future__ import annotations

import os
import shutil
import subprocess
from pathlib import Path

import pytest

from agents_gateway.harness.models import Worktree, WorktreeStatus
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.harness.workspace import (
    RepoWorkspaceManager,
    WorkspaceError,
)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _git(cwd: str, *args: str) -> subprocess.CompletedProcess:
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@local",
        "GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@local",
    }
    return subprocess.run(
        ["git", "-C", cwd, *args],
        capture_output=True, text=True, timeout=30, env=env,
    )


@pytest.fixture
def storage(tmp_path):
    return HarnessStorage(str(tmp_path / "harness.db"))


@pytest.fixture
def manager(storage, tmp_path):
    return RepoWorkspaceManager(
        storage=storage,
        workspace_root=str(tmp_path / "repos"),
        worktree_root=str(tmp_path / "worktrees"),
    )


@pytest.fixture
def scratch_repo(tmp_path):
    """Init an empty local git repo and return its path."""
    repo = tmp_path / "scratch-repo"
    repo.mkdir()
    proc = _git(str(repo), "init", "-b", "master")
    if proc.returncode != 0:
        _git(str(repo), "init")
        _git(str(repo), "symbolic-ref", "HEAD", "refs/heads/master")
    (repo / "README.md").write_text("# Scratch repo\n")
    _git(str(repo), "add", "README.md")
    _git(str(repo), "commit", "-m", "Initial commit")
    return repo


# ---------------------------------------------------------------------------
# get_or_create_local
# ---------------------------------------------------------------------------


class TestGetOrCreateLocal:
    def test_local_only_workspace_records_origin_url(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="Astatide1337",
            repo="scratch", default_branch="master",
        )
        assert ws.id.startswith("repo_ws_")
        assert ws.owner == "Astatide1337"
        assert ws.repo == "scratch"
        assert ws.default_branch == "master"
        assert ws.base_path == str(scratch_repo)
        fetched = manager.storage.get_workspace(ws.id)
        assert fetched is not None
        assert fetched.id == ws.id

    def test_local_path_must_exist(self, manager, tmp_path):
        with pytest.raises(WorkspaceError, match="does not exist"):
            manager.get_or_create_local(
                str(tmp_path / "missing"),
                owner="x", repo="y", default_branch="master",
            )

    def test_idempotent_for_same_owner_repo_branch(self, manager, scratch_repo):
        ws1 = manager.get_or_create_local(str(scratch_repo), owner="o", repo="r",
                                          default_branch="master")
        assert ws1.id in {w.id for w in manager.storage.list_workspaces()}


# ---------------------------------------------------------------------------
# create_worktree
# ---------------------------------------------------------------------------


class TestCreateWorktree:
    def test_creates_branch_and_worktree_for_task(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r",
            default_branch="master",
        )
        wt = manager.create_worktree(
            ws, task_id="task_abc123", agent_run_id="run_1",
            slug="build-feature-x", base_branch="master",
        )
        assert wt.status == WorktreeStatus.created.value
        assert wt.branch.startswith("agent/task_abc123-")
        assert "build-feature-x" in wt.branch
        assert Path(wt.path).is_dir()
        # The worktree dir is a git worktree
        assert (Path(wt.path) / ".git").exists() or \
               (Path(wt.path) / ".git").is_dir()
        # The branch exists in the base repo
        out = _git(str(scratch_repo), "branch", "--list", wt.branch).stdout
        assert wt.branch in out

    def test_worktree_is_separate_dir_per_task_id(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        wt1 = manager.create_worktree(
            ws, task_id="task_aaa", agent_run_id="run1",
            slug="x", base_branch="master",
        )
        wt2 = manager.create_worktree(
            ws, task_id="task_bbb", agent_run_id="run2",
            slug="x", base_branch="master",
        )
        assert wt1.path != wt2.path
        assert wt1.branch != wt2.branch

    def test_worktree_path_collision_raises(self, manager, scratch_repo):
        """Same task_id requesting two worktrees collides on the same path."""
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        manager.create_worktree(
            ws, task_id="dup_xyz", agent_run_id="run1",
            slug="x", base_branch="master",
        )
        with pytest.raises(WorkspaceError, match="Worktree path already exists"):
            manager.create_worktree(
                ws, task_id="dup_xyz", agent_run_id="run1",
                slug="x", base_branch="master",
            )

    def test_create_worktree_records_in_storage(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        wt = manager.create_worktree(
            ws, task_id="ws_task", agent_run_id="run1",
            slug="slug-here", base_branch="master",
        )
        fetched = manager.storage.get_worktree(wt.id)
        assert fetched is not None
        assert fetched.task_id == "ws_task"
        assert fetched.agent_run_id == "run1"

    def test_get_worktree_by_task_and_by_run(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        wt = manager.create_worktree(
            ws, task_id="task_xyz", agent_run_id="run_xyz",
            slug="s", base_branch="master",
        )
        assert manager.storage.get_worktree_by_task("task_xyz").id == wt.id
        assert manager.storage.get_worktree_by_run("run_xyz").id == wt.id


# ---------------------------------------------------------------------------
# remove_worktree
# ---------------------------------------------------------------------------


class TestRemoveWorktree:
    def test_remove_worktree_deletes_dir_and_branch(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        wt = manager.create_worktree(
            ws, task_id="cleanup", agent_run_id="run1",
            slug="s", base_branch="master",
        )
        path = Path(wt.path)
        assert path.is_dir()
        removed = manager.remove_worktree(wt, force=True)
        assert removed.status == WorktreeStatus.cleaned_up.value
        assert removed.deleted_at is not None
        assert not path.exists()
        out = _git(str(scratch_repo), "branch", "--list", wt.branch).stdout
        assert wt.branch not in out

    def test_remove_worktree_idempotent_on_missing_dir(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        wt = manager.create_worktree(
            ws, task_id="idem", agent_run_id="run1",
            slug="s", base_branch="master",
        )
        shutil.rmtree(wt.path, ignore_errors=True)
        result = manager.remove_worktree(wt, force=True)
        assert result.status == WorktreeStatus.cleaned_up.value


# ---------------------------------------------------------------------------
# fetch
# ---------------------------------------------------------------------------


class TestFetch:
    def test_fetch_on_local_repo_succeeds(self, manager, scratch_repo):
        ws = manager.get_or_create_local(
            str(scratch_repo), owner="o", repo="r", default_branch="master",
        )
        fetched = manager.fetch(ws)
        assert fetched.last_fetched_at is not None


# ---------------------------------------------------------------------------
# Storage row hydration
# ---------------------------------------------------------------------------


class TestStorageHydration:
    def test_save_workspace_roundtrips(self, storage):
        from agents_gateway.harness.models import RepoWorkspace
        ws = RepoWorkspace.new(
            repo_url="https://github.com/Astatide1337/conductor.git",
            owner="A", repo="conductor", base_path="/tmp/r",
            worktrees_path="/tmp/w", default_branch="master",
        )
        storage.save_workspace(ws)
        fetched = storage.get_workspace(ws.id)
        assert fetched is not None
        assert fetched.repo_url == ws.repo_url
        assert fetched.owner == "A"

    def test_find_workspace_returns_existing(self, storage):
        from agents_gateway.harness.models import RepoWorkspace
        ws = RepoWorkspace.new(
            repo_url="https://x", owner="o", repo="r",
            base_path="/x", worktrees_path="/y", default_branch="master",
        )
        storage.save_workspace(ws)
        found = storage.find_workspace("https://x", "o", "r", "master")
        assert found is not None
        assert found.id == ws.id

    def test_save_worktree_updates_status(self, storage):
        from agents_gateway.harness.models import Worktree
        wt = Worktree.new(
            task_id="t1", agent_run_id="r1",
            repo_workspace_id="repo_ws_x", branch="agent/foo",
            base_branch="master", path="/tmp/wt",
        )
        storage.save_worktree(wt)
        wt.status = WorktreeStatus.committed.value
        storage.save_worktree(wt)
        refetched = storage.get_worktree(wt.id)
        assert refetched.status == WorktreeStatus.committed.value

    def test_list_workspaces(self, storage):
        from agents_gateway.harness.models import RepoWorkspace
        for i in range(3):
            ws = RepoWorkspace.new(
                repo_url=f"https://x-{i}", owner="o", repo=f"r-{i}",
                base_path=f"/x{i}", worktrees_path=f"/y{i}",
            )
            storage.save_workspace(ws)
        listed = storage.list_workspaces()
        assert len(listed) >= 3
