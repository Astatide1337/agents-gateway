"""Harness worktree runtime for Composer-driven agent execution.

This subpackage implements the runtime plane that lets a Composer
(Conductor) hand tasks off to real coding harnesses (Claude Code /
opencode / Codex / future harnesses) running inside isolated git
worktrees under tmux sessions.

Public surface:

  * models      - dataclasses + enums for workspace/worktree/session/
                  interaction/verification/artifact
  * profiles    - HarnessProfile catalog (opencode, claude, codex,
                  fake-test) with goal-injection metadata
  * tmux        - TmuxDriver + FakeTmuxDriver for managing sessions
  * goal        - Goal injection strategies (auto/slash_goal/plain/
                  file_based) writing .agent-task/* runtime files
  * classifier  - Heuristic session-state classifier (running/waiting/
                  completed_claimed/failed_claimed/stalled/unknown)
  * workspace   - RepoWorkspaceManager (clone/fetch/worktree lifecycle)
  * verification- VerificationRunner (commands in worktree -> artifacts)
  * artifacts   - ArtifactStore layout per agent_run
  * reports     - HTML review report generator (secret-free)
  * driver      - HarnessDriver orchestrating the full session lifecycle
  * interactions- InteractionStore for Composer pending/answered queue
  * supervisor  - SessionSupervisor background poller
  * client_skills / client_mcp - downstream gateway clients
"""

from agents_gateway.harness.models import (
    ArtifactKind,
    ComposerInteraction,
    ComposerInteractionStatus,
    ComposerInteractionType,
    GoalStrategy,
    HarnessSession,
    HarnessSessionStatus,
    RepoWorkspace,
    VerificationCommand,
    VerificationCommandResult,
    VerificationRun,
    VerificationRunStatus,
    Worktree,
    WorktreeStatus,
)
from agents_gateway.harness.profiles import (
    BUILTIN_PROFILES,
    HarnessProfile,
    get_profile,
    list_profiles,
    register_profile,
)
from agents_gateway.harness.tmux import (
    FakeTmuxDriver,
    TmuxDriver,
    TmuxSessionRef,
)

__all__ = [
    "ArtifactKind",
    "BUILTIN_PROFILES",
    "ComposerInteraction",
    "ComposerInteractionStatus",
    "ComposerInteractionType",
    "FakeTmuxDriver",
    "GoalStrategy",
    "HarnessProfile",
    "HarnessSession",
    "HarnessSessionStatus",
    "RepoWorkspace",
    "TmuxDriver",
    "TmuxSessionRef",
    "VerificationCommand",
    "VerificationCommandResult",
    "VerificationRun",
    "VerificationRunStatus",
    "Worktree",
    "WorktreeStatus",
    "get_profile",
    "list_profiles",
    "register_profile",
]
