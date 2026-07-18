"""Git integration for the harness worktree runtime.

Provides three operations, all executed inside the task worktree:

  * capture_diff(worktree_path)    - returns a structured diff summary
                                     (changed files, insertions,
                                     deletions, diff text)
  * maybe_commit(worktree_path)   - stage + commit any changes if
                                     auto_commit is enabled; returns the
                                     new commit sha or None
  * maybe_push / maybe_pr          - placeholders that always return
                                     None/false unless credentials exist
                                     (PR creation is explicitly deferred
                                     per the milestone spec)

All shell calls use ``subprocess.run([...])`` with argv arrays.
"""

from __future__ import annotations

import os
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any


class GitError(Exception):
    pass


@dataclass
class DiffSummary:
    changed_files: list[str]
    insertions: int
    deletions: int
    numstat: str
    diff_text: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "changed_files": list(self.changed_files),
            "insertions": self.insertions,
            "deletions": self.deletions,
            "numstat": self.numstat,
            "diff_size_bytes": len(self.diff_text.encode()),
        }


def capture_diff(worktree_path: str | Path,
                 base_ref: str | None = None) -> DiffSummary:
    """Capture a git diff against ``base_ref`` (default: HEAD).

    Always uses --no-pager, --no-color, and an explicit PATH so we
    don't accidentally invoke a pager that blocks the subprocess.
    """
    cwd = str(worktree_path)
    # Build the diff target ref: if base_ref is None, diff against the
    # parent HEAD; if provided (e.g. origin/master), diff against that.
    try:
        diff_args = [
            "git", "-C", cwd, "diff", "--no-color", "--no-pager",
            "--numstat",
        ]
        if base_ref:
            # We want working tree vs base ref, so use `git diff <ref>`
            # plus an `--unified=0` (faster than full Unified context).
            diff_args.extend([base_ref])
        else:
            # Diff working tree against HEAD
            diff_args.extend([base_ref or "HEAD"])
        proc = subprocess.run(
            diff_args, capture_output=True, text=True, timeout=120,
        )
        numstat = proc.stdout or ""
        changed, ins, dele = _parse_numstat(numstat)
    except FileNotFoundError:
        return DiffSummary([], 0, 0, "", "")
    except subprocess.TimeoutExpired:
        return DiffSummary([], 0, 0, "", "")

    # Capture full diff text separately so we can store it as artifact.
    try:
        diff_full = subprocess.run(
            ["git", "-C", cwd, "diff", "--no-color", "--no-pager",
             base_ref or "HEAD"],
            capture_output=True, text=True, timeout=120,
        ).stdout or ""
    except Exception:
        diff_full = ""

    # Also include unstaged changes that aren't in the staged numstat
    # (e.g. new untracked files added by the agent are not shown by
    # `git diff` until `git add` — supervisor explicitly asks the agent
    # to commit, so this is OK for now).
    if not changed:
        # Try 'git status --porcelain' for untracked changes summary.
        try:
            status = subprocess.run(
                ["git", "-C", cwd, "status", "--porcelain"],
                capture_output=True, text=True, timeout=20,
            ).stdout or ""
            for line in status.splitlines():
                if len(line) >= 3:
                    changed.append(line[3:].strip())
        except Exception:
            pass
    return DiffSummary(changed, ins, dele, numstat, diff_full)


def maybe_commit(worktree_path: str | Path,
                 message: str = "auto: harness worktree commit",
                 author_name: str = "agents-gateway",
                 author_email: str = "agw@local",
                 auto_commit: bool = True) -> str | None:
    """Stage + commit changes if any. Returns commit SHA or None.

    We configure a benign identity for the commit and require explicit
    staging (no -a). Returns the SHA of the new commit, or None if no
    changes were committed.
    """
    if not auto_commit:
        return None
    cwd = str(worktree_path)
    # Check whether there are any changes to commit
    status_proc = subprocess.run(
        ["git", "-C", cwd, "status", "--porcelain"],
        capture_output=True, text=True, timeout=20,
    )
    if not status_proc.stdout.strip():
        return None
    env = {
        **os.environ,
        "GIT_AUTHOR_NAME": os.environ.get("GIT_AUTHOR_NAME", author_name),
        "GIT_AUTHOR_EMAIL": os.environ.get("GIT_AUTHOR_EMAIL", author_email),
        "GIT_COMMITTER_NAME": os.environ.get("GIT_COMMITTER_NAME", author_name),
        "GIT_COMMITTER_EMAIL": os.environ.get("GIT_COMMITTER_EMAIL", author_email),
    }
    # Stage everything (the agent owns the worktree)
    subprocess.run(
        ["git", "-C", cwd, "add", "-A"],
        capture_output=True, text=True, timeout=30, env=env,
    )
    # Commit; allow empty fail (no error if no changes after add)
    commit_proc = subprocess.run(
        ["git", "-C", cwd, "commit", "-m", message],
        capture_output=True, text=True, timeout=60, env=env,
    )
    if commit_proc.returncode != 0:
        return None
    sha_proc = subprocess.run(
        ["git", "-C", cwd, "rev-parse", "HEAD"],
        capture_output=True, text=True, timeout=15,
    )
    return sha_proc.stdout.strip() or None


def maybe_push(worktree_path: str | Path, branch: str,
               remote: str = "origin", auto_push: bool = False) -> bool:
    """Placeholder push: only attempts if auto_push=True and a remote exists.

    PR creation (``maybe_pr``) is explicitly deferred per milestone spec.
    """
    if not auto_push:
        return False
    cwd = str(worktree_path)
    # Check origin exists
    remote_proc = subprocess.run(
        ["git", "-C", cwd, "remote", "get-url", remote],
        capture_output=True, text=True, timeout=10,
    )
    if remote_proc.returncode != 0:
        return False
    try:
        proc = subprocess.run(
            ["git", "-C", cwd, "push", "-u", remote, branch],
            capture_output=True, text=True, timeout=120,
        )
        return proc.returncode == 0
    except Exception:
        return False


def maybe_pr(worktree_path: str | Path, branch: str, title: str,
             body: str = "", auto_pr: bool = False,
             gh_bin: str = "gh") -> str | None:
    """Placeholder PR creation: returns PR URL only if auto_pr=True and
    ``gh`` CLI is available and configured. None otherwise.
    """
    if not auto_pr:
        return None
    cwd = str(worktree_path)
    try:
        proc = subprocess.run(
            [gh_bin, "pr", "create", "--title", title, "--body", body,
             "--head", branch],
            cwd=cwd, capture_output=True, text=True, timeout=120,
        )
        if proc.returncode != 0:
            return None
        # `gh pr create` returns the PR URL on the last line.
        for line in (proc.stdout or "").splitlines():
            line = line.strip()
            if line.startswith("http"):
                return line
        return None
    except FileNotFoundError:
        return None
    except Exception:
        return None


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _parse_numstat(numstat: str) -> tuple[list[str], int, int]:
    files: list[str] = []
    insertions = 0
    deletions = 0
    for line in numstat.splitlines():
        parts = line.split("\t")
        if len(parts) < 3:
            continue
        ins_str, dele_str, filename = parts[0], parts[1], parts[2]
        try:
            if ins_str != "-":
                insertions += int(ins_str)
            if dele_str != "-":
                deletions += int(dele_str)
        except ValueError:
            pass
        files.append(filename)
    return files, insertions, deletions


__all__ = [
    "DiffSummary",
    "GitError",
    "capture_diff",
    "maybe_commit",
    "maybe_pr",
    "maybe_push",
]
