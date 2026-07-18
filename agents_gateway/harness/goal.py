"""Goal injection: how the task brief reaches the harness.

Strategies (see ``GoalStrategy`` for the enum):

  * auto         - choose slash_goal if profile supports it, else plain
  * slash_goal   - send ``/goal <text>`` (fails early if unsupported)
  * plain_prompt - send a normal instruction prompt
  * stdin_script - send a small multi-line script via tmux send-keys
  * file_based   - write .agent-task/{GOAL,SKILLS,TOOLS,VERIFICATION,
                   CONTEXT,RESULT_SCHEMA}.md (.json) files and send a
                   short "read .agent-task/GOAL.md" directive

The default for this milestone is ``file_based + plain_prompt`` for
harnesses without slash-goal support, and ``slash_goal`` when supported.
A combined strategy (file_based + slash_goal or plain_prompt) will write
the runtime files AND send a directive to read them — this is the most
robust universal path and is what `auto` resolves to.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from agents_gateway.harness.models import GoalStrategy
from agents_gateway.harness.profiles import HarnessProfile


AGENT_TASK_DIR = ".agent-task"

# Files written under the worktree's .agent-task/ directory.
GOAL_FILE = "GOAL.md"
SKILLS_FILE = "SKILLS.md"
TOOLS_FILE = "TOOLS.md"
VERIFICATION_FILE = "VERIFICATION.md"
CONTEXT_FILE = "CONTEXT.md"
RESULT_SCHEMA_FILE = "RESULT_SCHEMA.json"


@dataclass
class GoalInjectionResult:
    """The text to send through tmux + the files written to the worktree."""

    strategy: str
    sent_text: str
    files_written: list[str]
    write_dir: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "strategy": self.strategy,
            "sent_text": self.sent_text,
            "files_written": list(self.files_written),
            "write_dir": self.write_dir,
        }


@dataclass
class GoalContext:
    """Helper bundling the text written to .agent-task/* files.

    All fields are pre-rendered strings so the driver doesn't need to
    know about task shapes — the dispatcher composes them before
    calling ``write_runtime_files`` / ``inject_goal``.
    """

    title: str = ""
    brief: str = ""
    goal_text: str = ""
    skills_text: str = ""
    tools_text: str = ""
    verification_text: str = ""
    context_text: str = ""
    result_schema: dict[str, Any] | None = None


def resolve_strategy(profile: HarnessProfile, requested: str | None) -> str:
    """Resolve an effective strategy from profile + requested `goal.strategy`."""
    requested = requested or profile.goal_strategy or GoalStrategy.auto.value
    if requested == GoalStrategy.auto.value:
        if profile.supports_slash_goal:
            return GoalStrategy.slash_goal.value
        return GoalStrategy.plain_prompt.value
    if requested == GoalStrategy.slash_goal.value and not profile.supports_slash_goal:
        raise GoalInjectionError(
            f"Profile '{profile.name}' does not support slash_goal "
            f"(supports_slash_goal=false). Use plain_prompt or file_based."
        )
    return requested


def write_runtime_files(worktree_path: str | Path,
                        ctx: GoalContext) -> list[str]:
    """Write .agent-task/* files into the worktree. Returns file names.

    Always writes GOAL/SKILLS/TOOLS/VERIFICATION/CONTEXT/RESULT_SCHEMA.
    Missing fields are written with a minimal stub so the harness can
    unconditionally read every file.
    """
    base = Path(worktree_path) / AGENT_TASK_DIR
    base.mkdir(parents=True, exist_ok=True)
    written: list[str] = []
    schema = ctx.result_schema or _default_result_schema()

    _write_md(base / GOAL_FILE, _goal_md(ctx))
    written.append(GOAL_FILE)
    _write_md(base / SKILLS_FILE, ctx.skills_text or
              "No specific skills required for this task.")
    written.append(SKILLS_FILE)
    _write_md(base / TOOLS_FILE, ctx.tools_text or
              "MCP Gateway may be available for external tools when needed.")
    written.append(TOOLS_FILE)
    _write_md(base / VERIFICATION_FILE, ctx.verification_text or
              "No verification commands configured for this task.")
    written.append(VERIFICATION_FILE)
    _write_md(base / CONTEXT_FILE, ctx.context_text or "")
    written.append(CONTEXT_FILE)
    (base / RESULT_SCHEMA_FILE).write_text(json.dumps(schema, indent=2))
    written.append(RESULT_SCHEMA_FILE)
    return written


def build_directive_text(strategy: str, goal_text: str,
                        profile: HarnessProfile) -> str:
    """Compute the literal text the driver will send through tmux."""
    if strategy == GoalStrategy.slash_goal.value:
        slash = profile.goal_command or "/goal"
        return f"{slash} {goal_text}".rstrip()
    if strategy == GoalStrategy.plain_prompt.value:
        return _plain_prompt(goal_text)
    if strategy == GoalStrategy.stdin_script.value:
        return _stdin_script(goal_text)
    if strategy == GoalStrategy.file_based.value:
        return (
            "Read .agent-task/GOAL.md and complete the task. "
            "Follow .agent-task/VERIFICATION.md before declaring done. "
            "Skills are listed in .agent-task/SKILLS.md."
        )
    raise GoalInjectionError(f"Unknown goal strategy: {strategy}")


def inject_goal(worktree_path: str | Path, profile: HarnessProfile,
                ctx: GoalContext,
                requested_strategy: str | None = None) -> GoalInjectionResult:
    """Full goal injection: resolve strategy, write files, build text.

    Default behaviour: always write .agent-task/* files (so the harness
    has durable, structured context regardless of strategy), then
    build the directive text appropriate to the strategy.

    For the auto strategy we additionally combine a plain_prompt with
    file_based: this is the most robust universal path. For slash_goal
    we just send the slash command (the .agent-task/ files are still on
    disk for reference).
    """
    strategy = resolve_strategy(profile, requested_strategy)
    files = write_runtime_files(worktree_path, ctx)
    if strategy == GoalStrategy.auto.value:
        # Auto expands to "file_based + slash_goal" or
        # "file_based + plain_prompt" depending on profile support.
        if profile.supports_slash_goal:
            text = build_directive_text(GoalStrategy.slash_goal.value,
                                       ctx.goal_text, profile)
        else:
            text = build_directive_text(GoalStrategy.plain_prompt.value,
                                       ctx.goal_text, profile)
            # prefix with the file_based directive so the harness reads
            # the structured files first.
            file_directive = build_directive_text(
                GoalStrategy.file_based.value, ctx.goal_text, profile
            )
            text = f"{file_directive}\n\n{text}"
    else:
        text = build_directive_text(strategy, ctx.goal_text, profile)
        if strategy in (GoalStrategy.plain_prompt.value,
                        GoalStrategy.stdin_script.value):
            file_directive = build_directive_text(
                GoalStrategy.file_based.value, ctx.goal_text, profile
            )
            text = f"{file_directive}\n\n{text}"
    return GoalInjectionResult(
        strategy=strategy,
        sent_text=text,
        files_written=files,
        write_dir=str(Path(worktree_path) / AGENT_TASK_DIR),
    )


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


class GoalInjectionError(Exception):
    pass


def _write_md(path: Path, content: str) -> None:
    # For VERIFICATION.md we always prepend the spec-mandated warning
    # so callers can supply just the command list and still get a
    # spec-compliant file.
    if path.name == VERIFICATION_FILE and content:
        content = (
            "# Verification\n\n"
            "You may not mark this task complete until all required "
            "verification commands pass.\n\n" + content
        )
    path.write_text(content.strip() + ("\n" if content.strip() else ""))


def _goal_md(ctx: GoalContext) -> str:
    parts: list[str] = ["# Task Goal", ""]
    if ctx.title:
        parts.append(f"**Title:** {ctx.title}")
        parts.append("")
    if ctx.brief:
        parts.append("## Brief")
        parts.append(ctx.brief)
        parts.append("")
    if ctx.goal_text:
        parts.append("## Goal")
        parts.append(ctx.goal_text)
        parts.append("")
    parts.append(
        "_Work only in your assigned worktree. Do not mark this task "
        "complete until all required verification commands pass._"
    )
    return "\n".join(parts)


def _plain_prompt(goal_text: str) -> str:
    return (
        "You are working on this task:\n\n"
        f"{goal_text}\n\n"
        "Use the required skills listed in .agent-task/SKILLS.md. "
        "Work only in your assigned worktree. "
        "Do not mark this task complete until all required "
        "verification commands in .agent-task/VERIFICATION.md pass."
    )


def _stdin_script(goal_text: str) -> str:
    return (
        "cat <<'EOF' | head -n 200\n"
        f"{goal_text}\n"
        "EOF\n"
    )


def _default_result_schema() -> dict[str, Any]:
    return {
        "type": "object",
        "properties": {
            "summary": {"type": "string"},
            "status": {"type": "string",
                       "enum": ["completed", "blocked_external", "failed"]},
            "blockers": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "type": {"type": "string"},
                        "message": {"type": "string"},
                        "missing_env": {
                            "type": "array",
                            "items": {"type": "string"},
                        },
                    },
                },
            },
            "verification": {
                "type": "object",
                "properties": {
                    "status": {"type": "string"},
                    "commands": {"type": "array"},
                },
            },
        },
        "required": ["summary", "status", "verification"],
    }


__all__ = [
    "AGENT_TASK_DIR",
    "CONTEXT_FILE",
    "GOAL_FILE",
    "GoalContext",
    "GoalInjectionError",
    "GoalInjectionResult",
    "RESULT_SCHEMA_FILE",
    "SKILLS_FILE",
    "TOOLS_FILE",
    "VERIFICATION_FILE",
    "build_directive_text",
    "inject_goal",
    "resolve_strategy",
    "write_runtime_files",
]
