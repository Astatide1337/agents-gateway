"""Dataclasses + enums for the harness runtime plane.

These are deliberately framework-light (no Pydantic) so they work in
plain subprocess/threading contexts without model rebuild concerns.
They round-trip into the SQLite layer via the storage module's JSON
column.
"""

from __future__ import annotations

import enum
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _new_id(prefix: str) -> str:
    """Generate a typed identifier (e.g. wt_a3f2...) for storage rows."""
    return f"{prefix}_{uuid.uuid4().hex[:12]}"


def _slugify(text: str, max_len: int = 24) -> str:
    """Cheap slug for branch names (kept short to stay under git's limit)."""
    cleaned = []
    for ch in text.strip().lower():
        if ch.isalnum():
            cleaned.append(ch)
        elif ch in (" ", "-", "_", "."):
            cleaned.append("-")
    slug = "".join(cleaned).strip("-")
    while "--" in slug:
        slug = slug.replace("--", "-")
    return slug[:max_len].strip("-") or "task"


# ---------------------------------------------------------------------------
# Goal injection strategy enumeration
# ---------------------------------------------------------------------------


class GoalStrategy(str, enum.Enum):
    """How to deliver the goal text to a harness."""

    auto = "auto"
    slash_goal = "slash_goal"
    plain_prompt = "plain_prompt"
    stdin_script = "stdin_script"
    file_based = "file_based"


# ---------------------------------------------------------------------------
# Repo workspace + worktree
# ---------------------------------------------------------------------------


@dataclass
class RepoWorkspace:
    """A local clone/cache of a git repo used as the worktree base.

    Workspaces are referenced by id (e.g. repo_ws_<short-uuid>) and own a
    base_path (the cached clone) plus a worktrees_path (the directory
    under which task worktrees will be added).
    """

    id: str
    repo_url: str
    owner: str
    repo: str
    default_branch: str
    base_path: str
    worktrees_path: str
    created_at: str = field(default_factory=_now)
    updated_at: str = field(default_factory=_now)
    last_fetched_at: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def new(cls, repo_url: str, owner: str, repo: str,
            base_path: str, worktrees_path: str,
            default_branch: str = "master") -> RepoWorkspace:
        return cls(
            id=_new_id("repo_ws"),
            repo_url=repo_url,
            owner=owner,
            repo=repo,
            default_branch=default_branch,
            base_path=base_path,
            worktrees_path=worktrees_path,
        )


class WorktreeStatus(str, enum.Enum):
    created = "created"
    active = "active"
    dirty = "dirty"
    committed = "committed"
    failed = "failed"
    cleaned_up = "cleaned_up"


@dataclass
class Worktree:
    """An isolated git worktree for one task/agent_run."""

    id: str
    task_id: str
    agent_run_id: str
    repo_workspace_id: str
    branch: str
    base_branch: str
    path: str
    status: str = WorktreeStatus.created.value
    created_at: str = field(default_factory=_now)
    deleted_at: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def new(cls, task_id: str, agent_run_id: str,
            repo_workspace_id: str, branch: str, base_branch: str,
            path: str) -> Worktree:
        return cls(
            id=_new_id("wt"),
            task_id=task_id,
            agent_run_id=agent_run_id,
            repo_workspace_id=repo_workspace_id,
            branch=branch,
            base_branch=base_branch,
            path=path,
        )

    @classmethod
    def make_branch_name(cls, task_id: str, slug: str) -> str:
        """Convention: agent/<task_id_short>-<slug>."""
        return f"agent/{task_id[:18]}-{_slugify(slug)}"


# ---------------------------------------------------------------------------
# Harness session
# ---------------------------------------------------------------------------


class HarnessSessionStatus(str, enum.Enum):
    created = "created"
    starting = "starting"
    running = "running"
    waiting_for_reply = "waiting_for_reply"
    verifying = "verifying"
    completed = "completed"
    failed = "failed"
    blocked_external = "blocked_external"
    cancelled = "cancelled"
    stalled = "stalled"


@dataclass
class HarnessSession:
    """A running harness process owned by a tmux session."""

    id: str
    agent_run_id: str
    task_id: str
    harness_profile: str
    harness: str
    runtime: str
    tmux_session: str
    tmux_window: str = "main"
    tmux_pane: str = "0"
    working_directory: str = ""
    status: str = HarnessSessionStatus.created.value
    started_at: str = field(default_factory=_now)
    last_output_at: str = field(default_factory=_now)
    ended_at: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def new(cls, agent_run_id: str, task_id: str, harness_profile: str,
            harness: str, tmux_session: str, working_directory: str,
            runtime: str = "tmux") -> HarnessSession:
        return cls(
            id=_new_id("session"),
            agent_run_id=agent_run_id,
            task_id=task_id,
            harness_profile=harness_profile,
            harness=harness,
            runtime=runtime,
            tmux_session=tmux_session,
            working_directory=working_directory,
        )


# ---------------------------------------------------------------------------
# Composer interactions
# ---------------------------------------------------------------------------


class ComposerInteractionType(str, enum.Enum):
    needs_reply = "needs_reply"
    needs_credentials = "needs_credentials"
    external_blocker = "external_blocker"
    verification_failure_context = "verification_failure_context"
    ambiguous_harness_state = "ambiguous_harness_state"


class ComposerInteractionStatus(str, enum.Enum):
    pending = "pending"
    answered = "answered"
    cancelled = "cancelled"
    expired = "expired"


@dataclass
class ComposerInteraction:
    """A pending question/blocker that Composer must answer for the agent."""

    id: str
    agent_run_id: str
    task_id: str
    session_id: str
    type: str
    status: str = ComposerInteractionStatus.pending.value
    prompt_excerpt: str = ""
    full_context_ref: str = ""
    created_at: str = field(default_factory=_now)
    resolved_at: str | None = None
    composer_reply: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def new(cls, agent_run_id: str, task_id: str, session_id: str,
            type_: str, prompt_excerpt: str = "",
            full_context_ref: str = "",
            metadata: dict[str, Any] | None = None) -> ComposerInteraction:
        return cls(
            id=_new_id("interaction"),
            agent_run_id=agent_run_id,
            task_id=task_id,
            session_id=session_id,
            type=type_,
            prompt_excerpt=prompt_excerpt[:1000],
            full_context_ref=full_context_ref,
            metadata=dict(metadata or {}),
        )


# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------


class VerificationRunStatus(str, enum.Enum):
    created = "created"
    running = "running"
    passed = "passed"
    failed = "failed"
    blocked = "blocked"


@dataclass
class VerificationCommand:
    name: str
    command: str
    required: bool = True
    live_e2e: bool = False
    env_required: list[str] = field(default_factory=list)


@dataclass
class VerificationCommandResult:
    name: str
    command: str
    required: bool
    exit_code: int | None = None
    passed: bool = False
    output_artifact: str = ""
    blocked: bool = False
    blocked_reason: str = ""
    duration_seconds: float = 0.0


@dataclass
class VerificationRun:
    id: str
    agent_run_id: str
    task_id: str
    status: str = VerificationRunStatus.created.value
    commands: list[VerificationCommandResult] = field(default_factory=list)
    started_at: str = field(default_factory=_now)
    completed_at: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def new(cls, agent_run_id: str, task_id: str) -> VerificationRun:
        return cls(id=_new_id("verif"), agent_run_id=agent_run_id, task_id=task_id)

    @property
    def all_required_passed(self) -> bool:
        return all(
            c.passed for c in self.commands
            if c.required and not c.blocked
        )

    @property
    def any_blocked(self) -> bool:
        return any(c.blocked for c in self.commands)


# ---------------------------------------------------------------------------
# Proof artifacts
# ---------------------------------------------------------------------------


class ArtifactKind(str, enum.Enum):
    log = "log"
    test_output = "test_output"
    e2e_output = "e2e_output"
    live_e2e_output = "live_e2e_output"
    screenshot = "screenshot"
    video = "video"
    html_report = "html_report"
    diff = "diff"
    patch = "patch"
    coverage = "coverage"
    api_capture = "api_capture"
    terminal_capture = "terminal_capture"
    metadata = "metadata"


__all__ = [
    "ArtifactKind",
    "ComposerInteraction",
    "ComposerInteractionStatus",
    "ComposerInteractionType",
    "GoalStrategy",
    "HarnessSession",
    "HarnessSessionStatus",
    "RepoWorkspace",
    "VerificationCommand",
    "VerificationCommandResult",
    "VerificationRun",
    "VerificationRunStatus",
    "Worktree",
    "WorktreeStatus",
    "_new_id",
    "_slugify",
]
