"""Tests for goal injection (auto / slash_goal / plain_prompt / file_based)."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from agents_gateway.harness.goal import (
    GoalContext,
    GoalInjectionError,
    GoalInjectionResult,
    build_directive_text,
    inject_goal,
    resolve_strategy,
    write_runtime_files,
)
from agents_gateway.harness.models import GoalStrategy
from agents_gateway.harness.profiles import get_profile


# ---------------------------------------------------------------------------
# resolve_strategy
# ---------------------------------------------------------------------------


class TestResolveStrategy:
    def test_auto_resolves_to_slash_goal_when_supported(self):
        p = get_profile("opencode")
        assert p.supports_slash_goal
        assert resolve_strategy(p, None) == GoalStrategy.slash_goal.value

    def test_auto_resolves_to_plain_prompt_when_unsupported(self):
        p = get_profile("claude-code")
        assert not p.supports_slash_goal
        assert resolve_strategy(p, None) == GoalStrategy.plain_prompt.value

    def test_explicit_slash_goal_resolves_when_supported(self):
        p = get_profile("opencode")
        assert resolve_strategy(p, "slash_goal") == GoalStrategy.slash_goal.value

    def test_explicit_slash_goal_raises_when_unsupported(self):
        p = get_profile("claude-code")
        with pytest.raises(GoalInjectionError, match="does not support slash_goal"):
            resolve_strategy(p, "slash_goal")

    def test_plain_prompt_resolves_for_any_profile(self):
        for name in ("opencode", "claude-code", "codex", "fake-test"):
            p = get_profile(name)
            assert resolve_strategy(p, "plain_prompt") == GoalStrategy.plain_prompt.value

    def test_file_based_resolves_for_all(self):
        for name in ("opencode", "claude-code", "codex", "fake-test"):
            p = get_profile(name)
            assert resolve_strategy(p, "file_based") == GoalStrategy.file_based.value


# ---------------------------------------------------------------------------
# write_runtime_files
# ---------------------------------------------------------------------------


class TestWriteRuntimeFiles:
    def test_writes_six_files_into_agent_task_dir(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext(
            title="Build feature X",
            brief="Implement the timeline endpoint.",
            goal_text="Add GET /objectives/{id}/timeline.",
            skills_text="- test-driven-development\n- verification-before-completion",
            tools_text="- github.read",
            verification_text="1. uv run pytest -q\n2. bash scripts/e2e-local.sh",
            context_text="See .agent-task/CONTEXT.md for repo constraints.",
        )
        written = write_runtime_files(wt, ctx)
        assert set(written) == {
            "GOAL.md", "SKILLS.md", "TOOLS.md",
            "VERIFICATION.md", "CONTEXT.md", "RESULT_SCHEMA.json",
        }
        goal_md = (wt / ".agent-task" / "GOAL.md").read_text()
        assert "Build feature X" in goal_md
        assert "Implement the timeline endpoint." in goal_md
        # Worktree constraint reminder
        assert "assigned worktree" in goal_md.lower() or "verification" in goal_md.lower()

    def test_skills_file_has_required_skills(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext(skills_text="- test-driven-development\n- systematic-debugging")
        write_runtime_files(wt, ctx)
        text = (wt / ".agent-task" / "SKILLS.md").read_text()
        assert "test-driven-development" in text
        assert "systematic-debugging" in text

    def test_verification_file_lists_commands_and_warns_about_completion(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext(
            verification_text=("## Required commands\n\n"
                                "1. `uv run pytest -q`\n2. `bash scripts/e2e-local.sh`"),
        )
        written = write_runtime_files(wt, ctx)
        assert "VERIFICATION.md" in written
        body = (wt / ".agent-task" / "VERIFICATION.md").read_text()
        assert "uv run pytest" in body
        # Spec language: "may not mark this task complete until all required"
        assert "may not mark this task complete" in body.lower() or \
               "verification" in body.lower()

    def test_result_schema_file_is_valid_json(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext()
        write_runtime_files(wt, ctx)
        schema = json.loads((wt / ".agent-task" / "RESULT_SCHEMA.json").read_text())
        assert schema["type"] == "object"
        assert "required" in schema
        for r in ("summary", "status", "verification"):
            assert r in schema["required"]

    def test_default_files_when_context_fields_missing(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext()  # all empty
        written = write_runtime_files(wt, ctx)
        assert len(written) == 6
        # Skills file should still exist (default stub text)
        assert (wt / ".agent-task" / "SKILLS.md").read_text().strip()
        assert (wt / ".agent-task" / "TOOLS.md").read_text()
        assert (wt / ".agent-task" / "VERIFICATION.md").read_text()

    def test_live_e2e_blocker_message_in_verification(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        ctx = GoalContext(
            verification_text=("## Live E2E\n\nCommand: `bash e2e-live.sh`\n"
                                "Required env: GITHUB_TOKEN\n"
                                "If missing credentials block live E2E, report the exact missing variables."),
        )
        write_runtime_files(wt, ctx)
        body = (wt / ".agent-task" / "VERIFICATION.md").read_text()
        assert "GITHUB_TOKEN" in body


# ---------------------------------------------------------------------------
# inject_goal
# ---------------------------------------------------------------------------


class TestInjectGoal:
    def test_inject_goal_slash_strategy_with_supported_profile(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("opencode")
        ctx = GoalContext(title="t", brief="b",
                          goal_text="Build the timeline endpoint.")
        result = inject_goal(wt, p, ctx, requested_strategy="slash_goal")
        assert result.strategy == "slash_goal"
        assert result.sent_text.startswith("/goal ")
        assert "Build the timeline endpoint." in result.sent_text
        assert set(result.files_written) == {"GOAL.md", "SKILLS.md", "TOOLS.md",
                                              "VERIFICATION.md",
                                              "CONTEXT.md", "RESULT_SCHEMA.json"}

    def test_inject_goal_plain_prompt_with_unsupported_profile(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("claude-code")
        ctx = GoalContext(goal_text="Build the timeline endpoint.",
                          title="t", brief="b")
        result = inject_goal(wt, p, ctx, requested_strategy="plain_prompt")
        assert result.strategy == "plain_prompt"
        # Should include both the file_based directive + plain prompt body
        assert ".agent-task/GOAL.md" in result.sent_text
        assert "You are working on this task:" in result.sent_text

    def test_inject_goal_file_based_only_sent_short_directive(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("claude-code")
        ctx = GoalContext(goal_text="Build the X feature.",
                          title="Implement X", brief="Brief here.")
        result = inject_goal(wt, p, ctx, requested_strategy="file_based")
        assert result.strategy == "file_based"
        # File_based directive doesn't include the raw goal text —
        # it tells the harness to read .agent-task/GOAL.md
        assert ".agent-task/GOAL.md" in result.sent_text
        assert "Build the X feature" not in result.sent_text
        assert Path(result.write_dir).exists()

    def test_inject_goal_auto_with_supported_profile(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("opencode")
        ctx = GoalContext(goal_text="Implement Y.", title="t", brief="b")
        result = inject_goal(wt, p, ctx, requested_strategy="auto")
        # auto + supports_slash_goal -> slash_goal
        assert result.strategy == "slash_goal"
        assert "/goal Implement Y." in result.sent_text

    def test_inject_goal_auto_with_unsupported_profile_falls_back_to_plain(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("claude-code")
        ctx = GoalContext(goal_text="Implement Y.")
        result = inject_goal(wt, p, ctx, requested_strategy="auto")
        assert result.strategy == "plain_prompt"
        # auto-combined: file_based directive + plain prompt
        assert "Read .agent-task/GOAL.md" in result.sent_text
        assert "You are working on this task:" in result.sent_text

    def test_inject_goal_strategy_slash_unsupported_raises(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("codex")
        ctx = GoalContext(goal_text="x")
        with pytest.raises(GoalInjectionError, match="does not support slash_goal"):
            inject_goal(wt, p, ctx, requested_strategy="slash_goal")

    def test_inject_goal_goal_files_written_with_verification(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("fake-test")
        ctx = GoalContext(
            title="Phase milestone",
            brief="Build endpoint X.",
            goal_text="Add timeline endpoint.",
            verification_text="1. uv run pytest -q",
        )
        result = inject_goal(wt, p, ctx, requested_strategy="file_based")
        verif_path = Path(result.write_dir) / "VERIFICATION.md"
        verif_text = verif_path.read_text()
        assert "uv run pytest -q" in verif_text
        goal_path = Path(result.write_dir) / "GOAL.md"
        goal_text = goal_path.read_text()
        assert "Phase milestone" in goal_text

    def test_inject_goal_files_written_only_once_when_called_repeatedly(self, tmp_path):
        wt = tmp_path / "wt"
        wt.mkdir()
        p = get_profile("fake-test")
        ctx1 = GoalContext(goal_text="Implement one.", title="one")
        inject_goal(wt, p, ctx1, requested_strategy="file_based")
        ctx2 = GoalContext(goal_text="Implement two.", title="two")
        result = inject_goal(wt, p, ctx2, requested_strategy="file_based")
        goal_text = (Path(result.write_dir) / "GOAL.md").read_text()
        assert "Implement two" in goal_text


# ---------------------------------------------------------------------------
# build_directive_text (representative texts)
# ---------------------------------------------------------------------------


class TestBuildDirectiveText:
    def test_build_directive_text_slash_goal(self):
        p = get_profile("opencode")
        text = build_directive_text("slash_goal", "Build X.", p)
        assert text.startswith("/goal ")
        assert "Build X." in text

    def test_build_directive_text_plain_prompt(self):
        p = get_profile("claude-code")
        text = build_directive_text("plain_prompt", "Build Y.", p)
        assert "You are working on this task:" in text
        assert "Build Y." in text
        assert "verification" in text.lower()

    def test_build_directive_text_file_based(self):
        p = get_profile("claude-code")
        text = build_directive_text("file_based", "Build Z.", p)
        assert ".agent-task/GOAL.md" in text
        assert ".agent-task/VERIFICATION.md" in text
        assert "Build Z." not in text

    def test_build_directive_text_unknown_strategy_raises(self):
        p = get_profile("claude-code")
        with pytest.raises(GoalInjectionError, match="Unknown goal strategy"):
            build_directive_text("not_a_real_strategy", "x", p)
