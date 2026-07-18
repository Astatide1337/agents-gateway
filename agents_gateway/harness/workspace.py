"""Repo workspace + worktree manager.

A ``RepoWorkspaceManager`` owns the local clone and the worktrees_path
for one (owner/repo, branch) tuple. It uses real git via
``subprocess.run([...])`` with command arrays (never shell interpolation)

Topology:

  repo workspace (cached clone)
    |
    +-- base_path       <- bare-ish clone (or full clone) of the repo
    +-- worktrees_path  <- directory under which task worktrees live
          |
          +-- task_<task_id_short>/  <- one isolated git worktree per task
                |
                +-- .agent-task/  <- runtime files written by goal.py

For tests we accept a "local-only" mode where the workspace base_path
points at an existing local directory (no clone happens) — useful for
the bundled fake-test harness running against an in-tree scratch repo.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import (
    RepoWorkspace,
    Worktree,
    WorktreeStatus,
    _slugify,
)
from agents_gateway.harness.storage import HarnessStorage


class WorkspaceError(Exception):
    pass


class RepoWorkspaceManager:
    """Manages the lifecycle of one repo workspace + its worktrees.

    One manager instance per (owner/repo, branch); the dispatcher
    creates or reuses one per task. Idempotent for the same (owner/repo,
    branch) tuple — repeated calls return the same workspace.
    """

    def __init__(self, storage: HarnessStorage,
                 workspace_root: str = "/var/lib/agents-gateway/repos",
                 worktree_root: str = "/var/lib/agents-gateway/worktrees",
                 git_bin: str = "git",
                 allow_local_path: bool = True) -> None:
        self.storage = storage
        self.workspace_root = Path(workspace_root)
        self.worktree_root = Path(worktree_root)
        self.git_bin = git_bin
        self.allow_local_path = allow_local_path

    # -------------------------------------------------------------------
    # Workspace bootstrap
    # -------------------------------------------------------------------

    def get_or_create(self, repo_url: str, owner: str, repo: str,
                      default_branch: str = "master",
                      force_clone: bool = False) -> RepoWorkspace:
        """Get existing or create a fresh workspace cached on disk."""
        existing = self.storage.find_workspace(repo_url, owner, repo, default_branch)
        if existing and not force_clone:
            return existing
        ws_id = f"repo_ws_{uuid.uuid4().hex[:12]}"
        base_path = str(self.workspace_root / owner / repo / ws_id)
        worktrees_path = str(self.worktree_root / owner / repo / ws_id)
        Path(base_path).mkdir(parents=True, exist_ok=True)
        Path(worktrees_path).mkdir(parents=True, exist_ok=True)
        ws = RepoWorkspace(
            id=ws_id, repo_url=repo_url, owner=owner, repo=repo,
            default_branch=default_branch, base_path=base_path,
            worktrees_path=worktrees_path,
        )
        self._bootstrap_clone(ws)
        self.storage.save_workspace(ws)
        return ws

    def get_or_create_local(self, local_path: str, owner: str,
                            repo: str,
                            default_branch: str = "master") -> RepoWorkspace:
        """Use an existing local repo dir as the workspace base (no clone).

        Required for the bundled fake-test harness and the local E2E
        script where we point at an in-tree scratch repo. The local
        path's origin (if any) is treated as the canonical remote.
        """
        if not self.allow_local_path:
            raise WorkspaceError(
                "allow_local_path=False; refusing to use local path "
                f"{local_path} as workspace base"
            )
        local = Path(local_path).resolve()
        if not local.is_dir():
            raise WorkspaceError(f"Local path does not exist: {local}")
        ws_id = f"repo_ws_{uuid.uuid4().hex[:12]}"
        worktrees_path = str(self.worktree_root / owner / repo / ws_id)
        Path(worktrees_path).mkdir(parents=True, exist_ok=True)
        origin_url = self._safe_origin(local)
        ws = RepoWorkspace(
            id=ws_id, repo_url=origin_url or str(local),
            owner=owner, repo=repo, default_branch=default_branch,
            base_path=str(local), worktrees_path=worktrees_path,
        )
        self.storage.save_workspace(ws)
        return ws

    def fetch(self, workspace: RepoWorkspace) -> RepoWorkspace:
        """Run `git fetch <remote> <branch>` in the base clone.

        For local-only workspaces (no `origin` remote) we treat the
        base_path as authoritative: we update the last_fetched_at
        timestamp without invoking fetch.
        """
        Path(workspace.base_path).mkdir(parents=True, exist_ok=True)
        if not (Path(workspace.base_path) / ".git").exists() and \
                self.allow_local_path:
            workspace.last_fetched_at = datetime.now(timezone.utc).isoformat()
            workspace.updated_at = workspace.last_fetched_at
            self.storage.save_workspace(workspace)
            return workspace
        # Even if /.git exists, the clone may not have a real `origin`
        # configured (e.g. an init'd scratch repo). Check for it
        # before invoking fetch so we never crash on local-only workspaces.
        remote_check = subprocess.run(
            [self.git_bin, "-C", workspace.base_path, "remote"],
            capture_output=True, text=True, timeout=10,
        )
        if remote_check.returncode != 0 or not remote_check.stdout.strip():
            # No remotes — treat as authoritative local-only.
            workspace.last_fetched_at = datetime.now(timezone.utc).isoformat()
            workspace.updated_at = workspace.last_fetched_at
            self.storage.save_workspace(workspace)
            return workspace
        argv = [
            self.git_bin, "-C", workspace.base_path,
            "fetch", "origin", workspace.default_branch,
        ]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=120)
        if proc.returncode != 0:
            raise WorkspaceError(f"git fetch failed: {proc.stderr.strip()}")
        workspace.last_fetched_at = datetime.now(timezone.utc).isoformat()
        workspace.updated_at = workspace.last_fetched_at
        self.storage.save_workspace(workspace)
        return workspace

    # -------------------------------------------------------------------
    # Worktree lifecycle
    # -------------------------------------------------------------------

    def create_worktree(self, workspace: RepoWorkspace,
                        task_id: str, agent_run_id: str,
                        slug: str, base_branch: str | None = None) -> Worktree:
        """Create an isolated worktree for one task on a new branch."""
        target_branch = base_branch or workspace.default_branch
        branch_name = Worktree.make_branch_name(task_id, slug)
        wt_path = Path(workspace.worktrees_path) / f"task_{task_id[:18]}"
        if wt_path.exists():
            # Defensive — never reuse a worktree for a different task.
            raise WorkspaceError(
                f"Worktree path already exists for task {task_id}: {wt_path}"
            )
        wt_path.parent.mkdir(parents=True, exist_ok=True)
        base_dir = Path(workspace.base_path)
        # If base is not a git repo (local-only), init one and use it as
        # the worktree parent. This is the fake-test / local E2E path.
        if not (base_dir / ".git").exists() and self.allow_local_path:
            self._init_local_repo(base_dir, target_branch)
        # Create the new branch starting from origin/<base_branch> if
        # available, otherwise from the local base_branch ref.
        argv_branch = [
            self.git_bin, "-C", str(base_dir),
            "branch", branch_name,
            f"origin/{target_branch}",
        ]
        proc = subprocess.run(argv_branch, capture_output=True, text=True, timeout=30)
        if proc.returncode != 0:
            # Fallback: branch from local base branch
            argv_fallback = [
                self.git_bin, "-C", str(base_dir),
                "branch", branch_name, target_branch,
            ]
            proc2 = subprocess.run(argv_fallback, capture_output=True,
                                    text=True, timeout=30)
            if proc2.returncode != 0:
                # If the local ref also doesn't exist, try HEAD
                argv_head = [
                    self.git_bin, "-C", str(base_dir),
                    "branch", branch_name, "HEAD",
                ]
                proc3 = subprocess.run(argv_head, capture_output=True,
                                        text=True, timeout=30)
                if proc3.returncode != 0:
                    raise WorkspaceError(
                        f"git branch {branch_name} failed: "
                        f"{proc.stderr.strip()} / {proc2.stderr.strip()} / "
                        f"{proc3.stderr.strip()}"
                    )
        argv_worktree = [
            self.git_bin, "-C", str(base_dir),
            "worktree", "add", "--checkout",
            str(wt_path), branch_name,
        ]
        proc = subprocess.run(argv_worktree, capture_output=True,
                               text=True, timeout=60)
        if proc.returncode != 0:
            raise WorkspaceError(
                f"git worktree add failed: {proc.stderr.strip()}"
            )
        wt = Worktree(
            id=f"wt_{uuid.uuid4().hex[:12]}",
            task_id=task_id,
            agent_run_id=agent_run_id,
            repo_workspace_id=workspace.id,
            branch=branch_name,
            base_branch=target_branch,
            path=str(wt_path),
            status=WorktreeStatus.created.value,
        )
        self.storage.save_worktree(wt)
        return wt

    def remove_worktree(self, worktree: Worktree,
                        force: bool = False) -> Worktree:
        """Safely remove a worktree and its branch.

        Idempotent: if the on-disk worktree was already removed we just
        update DB state. Never raises on missing worktree.
        """
        wt_path = Path(worktree.path)
        base_dir = self._base_dir_for_worktree(worktree)
        if wt_path.exists():
            argv = [
                self.git_bin, "-C", str(base_dir),
                "worktree", "remove",
                "--force" if force else "",
                str(wt_path),
            ]
            argv = [a for a in argv if a]
            try:
                subprocess.run(argv, capture_output=True, text=True, timeout=30)
            except Exception:
                if not force:
                    shutil.rmtree(wt_path, ignore_errors=True)
        # Branch removal is best-effort: prune it but don't fail the
        # whole cleanup if the branch has unmerged commits.
        if worktree.branch and base_dir and (Path(base_dir) / ".git").exists():
            try:
                subprocess.run(
                    [self.git_bin, "-C", str(base_dir),
                     "branch", "-D" if force else "-d",
                     worktree.branch],
                    capture_output=True, text=True, timeout=15,
                )
            except Exception:
                pass
        worktree.status = WorktreeStatus.cleaned_up.value
        worktree.deleted_at = datetime.now(timezone.utc).isoformat()
        self.storage.save_worktree(worktree)
        return worktree

    # -------------------------------------------------------------------
    # helpers
    # -------------------------------------------------------------------

    def _base_dir_for_worktree(self, wt: Worktree) -> str:
        ws = self.storage.get_workspace(wt.repo_workspace_id)
        return ws.base_path if ws else ""

    def _bootstrap_clone(self, ws: RepoWorkspace) -> None:
        """Clone the canonical repo into ws.base_path."""
        base_dir = Path(ws.base_path)
        # If base path already exists and contains a clone, skip.
        if (base_dir / ".git").exists():
            return
        if not ws.repo_url:
            if self.allow_local_path:
                # Local-only workspace with no remote — init an empty repo
                self._init_local_repo(base_dir, ws.default_branch)
                return
            raise WorkspaceError(
                f"Cannot clone workspace {ws.id}: no repo_url configured"
            )
        argv = [
            self.git_bin, "clone", "--branch", ws.default_branch,
            "--no-single-branch", ws.repo_url, str(base_dir),
        ]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=300)
        if proc.returncode != 0:
            # Surface a clear error to the dispatcher; the dispatcher
            # converts this into a `blocked_external` agent_run status.
            raise WorkspaceError(
                f"git clone failed for {ws.repo_url}: {proc.stderr.strip()}"
            )

    def _init_local_repo(self, base_dir: Path, default_branch: str) -> None:
        """Init an empty git repo suitable for worktree scratch use."""
        if not base_dir.exists():
            base_dir.mkdir(parents=True, exist_ok=True)
        if (base_dir / ".git").exists():
            return
        argv = [self.git_bin, "init", "-b", default_branch, str(base_dir)]
        proc = subprocess.run(argv, capture_output=True, text=True, timeout=30)
        if proc.returncode != 0 and "unknown switch" in (proc.stderr or "").lower():
            # Older git without -b: init then set default branch.
            subprocess.run(
                [self.git_bin, "init", str(base_dir)],
                capture_output=True, text=True, timeout=30,
            )
            subprocess.run(
                [self.git_bin, "-C", str(base_dir), "symbolic-ref",
                 "HEAD", f"refs/heads/{default_branch}"],
                capture_output=True, text=True, timeout=15,
            )
        # Make initial commit so HEAD exists (needed for worktree add).
        if not (base_dir / ".git").exists():
            return
        # Only commit if there's something to commit (existing files) —
        # otherwise leave the repo empty so callers can stage real
        # files into it.
        argv_status = [self.git_bin, "-C", str(base_dir), "status", "--porcelain"]
        status = subprocess.run(argv_status, capture_output=True,
                                text=True, timeout=15).stdout
        if status.strip():
            # Configure a benign identity for the initial commit if
            # none is set; never overrides an explicit identity.
            env = {
                **os.environ,
                "GIT_AUTHOR_NAME": os.environ.get("GIT_AUTHOR_NAME", "agents-gateway"),
                "GIT_AUTHOR_EMAIL": os.environ.get("GIT_AUTHOR_EMAIL", "agw@local"),
                "GIT_COMMITTER_NAME": os.environ.get("GIT_COMMITTER_NAME", "agents-gateway"),
                "GIT_COMMITTER_EMAIL": os.environ.get("GIT_COMMITTER_EMAIL", "agw@local"),
            }
            subprocess.run(
                [self.git_bin, "-C", str(base_dir), "add", "-A"],
                capture_output=True, text=True, timeout=15, env=env,
            )
            subprocess.run(
                [self.git_bin, "-C", str(base_dir), "commit", "-m",
                 "Initial scratch commit (agents-gateway local workspace)"],
                capture_output=True, text=True, timeout=30, env=env,
            )

    def _safe_origin(self, local: Path) -> str:
        """Return `git config --get remote.origin.url` or empty string."""
        try:
            proc = subprocess.run(
                [self.git_bin, "-C", str(local), "config", "--get",
                 "remote.origin.url"],
                capture_output=True, text=True, timeout=10,
            )
            return proc.stdout.strip() if proc.returncode == 0 else ""
        except Exception:
            return ""


__all__ = ["RepoWorkspaceManager", "WorkspaceError"]
