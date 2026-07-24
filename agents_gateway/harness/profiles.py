"""Harness profiles describe how to start a real coding harness.

A profile bundles the launch command, whether the harness supports
``/goal`` slash commands, the preferred goal-injection strategy, and the
completion-strategy heuristic used by the classifier.

Built-in profiles:

  * pi-coding-agent  - PI Coding Agent CLI (default; model configurable)
  * opencode         - opencode CLI (model configurable)
  * claude-code      - Anthropic Claude Code
  * codex            - OpenAI Codex CLI
  * fake-test        - in-tree fake harness for tests and the local E2E
                        script. Reads ``/goal <text>`` (or plain
                        text) from stdin, performs scripted behaviour,
                        prints "DONE" or asks a clarifying question.

The ``pi-coding-agent`` and ``opencode`` profiles do NOT hardcode a
model — the dispatcher sets ``task_spec.execution.model`` and the driver
injects the right CLI flag (``--model`` for PI, ``-m`` for opencode)
from the profile's ``model_arg_name``.

Model validation is enforced by ``validate_model_for_profile`` (see
below). If the profile declares ``model_arg_name`` (i.e. it accepts a
model override), the dispatcher MUST supply a model that is on the
approved free-model allowlist (configured via ``AGW_APPROVED_FREE_MODELS``,
default ``nvidia/nemotron-3-ultra-550b-a55b:free``). If no model is
supplied, or if the supplied model is not on the allowlist, the dispatch
fails with a clear error. Profiles without ``model_arg_name``
(claude-code, codex, fake-test) ignore the override and launch with
their own runtime defaults.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

from agents_gateway.harness.models import GoalStrategy


@dataclass(frozen=True)
class HarnessProfile:
    """How to launch one harness and feed it a goal.

    The model is **not** baked into ``args``. Instead each profile that
    supports a model override declares a CLI flag name via
    ``model_arg_name`` (e.g. ``--model`` for PI, ``-m`` for opencode).
    At session spawn time the driver injects
    ``[model_arg_name, task_spec.execution.model]`` if the dispatcher
    supplied a model override; otherwise a ``default_model`` (if set
    on the profile) is used. Profiles without ``model_arg_name``
    (claude-code, codex, fake-test) silently ignore model overrides
    and always launch with their own runtime defaults.
    """

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
    # CLI flag the driver uses to inject ``task_spec.execution.model``
    # (or ``default_model`` if the dispatcher did not set one).
    # ``None`` disables model override for this profile.
    model_arg_name: str | None = None
    default_model: str | None = None

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
            "model_arg_name": self.model_arg_name,
            "default_model": self.default_model,
        }

    def effective_args(self, model_override: str | None = None
                       ) -> tuple[str, ...]:
        """Return the full argv to pass after ``command``.

        If this profile declares ``model_arg_name`` and a model is
        available (either the override from the dispatcher or the
        profile's ``default_model``), the (flag, value) pair is
        appended. Otherwise plain ``args`` are returned untouched.
        """
        out = list(self.args)
        if self.model_arg_name:
            model = model_override or self.default_model
            if model:
                out.extend([self.model_arg_name, model])
        return tuple(out)


# Model allowlist enforcement

class MissingModelError(ValueError):
    """Raised when a profile that requires a model receives none."""
    pass


class DisapprovedModelError(ValueError):
    """Raised when a model is not on the approved allowlist."""

    def __init__(self, model: str, allowlist: list[str]) -> None:
        self.model = model
        self.allowlist = allowlist
        super().__init__(
            f"Model '{model}' is not on the approved free-model allowlist. "
            f"Allowed: {', '.join(allowlist)}"
        )


def _load_allowlist() -> list[str]:
    env_val = os.environ.get("AGW_APPROVED_FREE_MODELS", "")
    if not env_val:
        return ["nvidia/nemotron-3-ultra-550b-a55b:free"]
    return [m.strip() for m in env_val.split(",") if m.strip()]


_ALLOWLIST: list[str] | None = None


def _get_allowlist() -> list[str]:
    global _ALLOWLIST
    if _ALLOWLIST is None:
        _ALLOWLIST = _load_allowlist()
    return _ALLOWLIST


def reload_allowlist() -> list[str]:
    """Force reload of the allowlist from environment (for tests)."""
    global _ALLOWLIST
    _ALLOWLIST = _load_allowlist()
    return _ALLOWLIST


def validate_model_for_profile(model: str | None, profile: "HarnessProfile") -> str:
    """Validate that the model is present and approved for the profile.

    Args:
        model: The model ID to validate (e.g. from task_spec.execution.model).
        profile: The HarnessProfile that will receive the model.

    Returns:
        The validated model string (same as input if valid).

    Raises:
        MissingModelError: If profile requires a model but none is supplied.
        DisapprovedModelError: If model is not on the approved allowlist.
    """
    if profile.model_arg_name is None:
        # Profile doesn't support model override (claude-code, codex, fake-test)
        return model or ""

    if not model or not model.strip():
        raise MissingModelError(
            f"Profile '{profile.name}' requires a model override "
            f"(flag: {profile.model_arg_name}) but task_spec.execution.model "
            "was empty. Set CONDUCTOR_COMPOSER_LLM_MODEL in the environment "
            "or provide a per-task model in the plan."
        )

    model = model.strip()
    allowlist = _get_allowlist()
    if model not in allowlist:
        raise DisapprovedModelError(model, allowlist)

    return model


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
    "pi-coding-agent": HarnessProfile(
        name="pi-coding-agent",
        harness="pi",
        command="pi",
        args=(
            "--thinking", "off",
        ),
        supports_slash_goal=False,
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.plain_prompt.value,
        default=True,
        description=(
            "PI Coding Agent CLI; plain-text prompt input. The model "
            "is intentionally NOT hardcoded here — the dispatcher passes "
            "it via task_spec.execution.model (or the profile's "
            "default_model fallback). Uses --model <id>."
        ),
        model_arg_name="--model",
    ),
    "opencode": HarnessProfile(
        name="opencode",
        harness="opencode",
        command="opencode",
        args=(),
        supports_slash_goal=True,
        goal_command="/goal",
        input_mode="tmux_stdin",
        completion_strategy="output_classifier",
        goal_strategy=GoalStrategy.auto.value,
        description=(
            "opencode CLI; supports /goal slash command. The model is "
            "configurable via task_spec.execution.model (or "
            "default_model fallback). Uses -m <provider/model>."
        ),
        model_arg_name="-m",
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
    # Fall back to pi-coding-agent if no explicit default registered.
    return BUILTIN_PROFILES["pi-coding-agent"]


__all__ = [
    "BUILTIN_PROFILES",
    "HarnessProfile",
    "get_default_profile",
    "get_profile",
    "list_profiles",
    "register_profile",
    "MissingModelError",
    "DisapprovedModelError",
    "validate_model_for_profile",
    "reload_allowlist",
]
