"""Harness profiles describe how to start a real coding harness.

A profile bundles the launch command, whether the harness supports
`/goal` slash commands, the preferred goal-injection strategy, and the
completion-strategy heuristic used by the classifier.

Built-in profiles:

  * opencode-deepseek  - default for opencode sessions
  * claude-code        - Anthropic Claude Code
  * codex              - OpenAI Codex CLI
  * fake-test          - in-tree fake harness for tests and the local
                          E2E script. Reads `/goal <text>` (or plain
                          text) from stdin, performs scripted behaviour,
                          prints "DONE" or asks a clarifying question.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from agents_gateway.harness.models import GoalStrategy


@dataclass(frozen=True)
class HarnessProfile:
    """How to launch one harness and feed it a goal."""

    name: str
    harness: str
    command: str
    args: tuple[str, ...] = ()
    supports_slash_goal: bool = False
    goal_command: str = "/goal"
    input_mode: str = "tmux_stdin"
    completion_strategy: str = "output_classifier"
    goal_strategy: str = GoalStrategy.auto.value
    default: bool = False
    env: tuple[tuple[str, str], ...] = ()
    description: str = ""

    def to_dict(self) -> dict[str, object]:
        return {
            "name": self.name,
            "harness": self.harness,
            "command": self.command,
            "args": list(self.args),
            "supports_slash_goal": self.supports_slash_goal,
            "goal_command": self.goal_command,
            "input_mode": self.input_mode,
            "completion_strategy": self.completion_strategy,
            "goal_strategy": self.goal_strategy,
            "default": self.default,
            "description": self.description,
        }


# Built-in profile table. The `fake-test` profile points at the
# shipped `agents/fake-test/run.py` fixture so the local E2E script
# can drive a deterministic harness without installing anything.
# We resolve the runner to an absolute path so the spawned tmux session
# (whose CWD is the per-task worktree) can find it regardless of the
# worktree path — the worktree may be a bare clone with no `agents/`
# directory inside it.
from pathlib import Path as _Path

_FAKE_TEST_RUNNER = str(
    (_Path(__file__).resolve().parent.parent.parent
     / "agents" / "fake-test" / "run.py")
)

BUILTIN_PROFILES: dict[str, HarnessProfile] = {
    "opencode-deepseek": HarnessProfile(
        name="opencode-deepseek",
        harness="opencode",
        command="opencode",
        args=(),
        supports_slash_goal=True,
        goal_command="/goal",
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.auto.value,
        default=True,
        description="opencode CLI with DeepSeek backend; supports /goal slash command.",
    ),
    "claude-code": HarnessProfile(
        name="claude-code",
        harness="claude",
        command="claude",
        args=(),
        supports_slash_goal=False,
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.plain_prompt.value,
        description="Anthropic Claude Code CLI; plain-text prompt input only.",
    ),
    "codex": HarnessProfile(
        name="codex",
        harness="codex",
        command="codex",
        args=(),
        supports_slash_goal=False,
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.plain_prompt.value,
        description="OpenAI Codex CLI; plain-text prompt input only.",
    ),
    "fake-test": HarnessProfile(
        name="fake-test",
        harness="fake",
        command="python3",
        args=(_FAKE_TEST_RUNNER,),
        supports_slash_goal=True,
        goal_command="/goal",
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.auto.value,
        description=(
            "Deterministic fake harness for tests and local E2E. "
            "Reads instructions from tmux stdin, performs scripted "
            "behaviour (write a file, ask a question, fail then fix)."
        ),
    ),
}


# User-registered profiles (extends the built-in table at runtime).
_REGISTERED: dict[str, HarnessProfile] = {}


def register_profile(profile: HarnessProfile) -> None:
    """Add or override a harness profile at runtime."""
    _REGISTERED[profile.name] = profile


def list_profiles() -> list[HarnessProfile]:
    """Return all known profiles (built-in + user-registered)."""
    merged = dict(BUILTIN_PROFILES)
    merged.update(_REGISTERED)
    out = list(merged.values())
    out.sort(key=lambda p: p.name)
    return out


def get_profile(name: str) -> HarnessProfile | None:
    merged = dict(BUILTIN_PROFILES)
    merged.update(_REGISTERED)
    return merged.get(name)


def get_default_profile() -> HarnessProfile:
    for p in list_profiles():
        if p.default:
            return p
    # Fall back to opencode-deepseek if no explicit default registered.
    return BUILTIN_PROFILES["opencode-deepseek"]


__all__ = [
    "BUILTIN_PROFILES",
    "HarnessProfile",
    "get_default_profile",
    "get_profile",
    "list_profiles",
    "register_profile",
]
