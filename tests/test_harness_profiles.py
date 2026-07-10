"""Tests for harness profiles (opencode-deepseek / claude / codex / fake-test)."""

from __future__ import annotations

import pytest

from agents_gateway.harness.models import GoalStrategy
from agents_gateway.harness.profiles import (
    BUILTIN_PROFILES,
    HarnessProfile,
    get_default_profile,
    get_profile,
    list_profiles,
    register_profile,
)


class TestBuiltinProfiles:
    def test_builtin_table_has_four_profiles(self):
        assert set(BUILTIN_PROFILES) == {
            "opencode-deepseek", "claude-code", "codex", "fake-test",
        }

    def test_opencode_deeepseek_is_default(self):
        assert BUILTIN_PROFILES["opencode-deepseek"].default is True

    def test_list_profiles_returns_all_sorted(self):
        names = [p.name for p in list_profiles()]
        assert names == sorted(names)
        assert "opencode-deepseek" in names

    @pytest.mark.parametrize("name,harness", [
        ("opencode-deepseek", "opencode"),
        ("claude-code", "claude"),
        ("codex", "codex"),
        ("fake-test", "fake"),
    ])
    def test_profile_resolves_kind(self, name, harness):
        p = get_profile(name)
        assert p is not None
        assert p.harness == harness

    def test_unknown_profile_returns_none(self):
        assert get_profile("nonexistent-xyz") is None

    def test_get_default_profile_returns_opencode_when_unregistered(self):
        # No new profiles registered — should fall back to opencode-deepseek
        assert get_default_profile().name == "opencode-deepseek"

    def test_register_profile_overrides_builtin(self):
        custom = HarnessProfile(
            name="opencode-deepseek",
            harness="opencode",
            command="/usr/local/bin/opencode",
            args=("--no-network",),
            supports_slash_goal=True,
            goal_command="/goal",
            goal_strategy=GoalStrategy.slash_goal.value,
            description="Custom",
        )
        try:
            register_profile(custom)
            fetched = get_profile("opencode-deepseek")
            assert fetched.command == "/usr/local/bin/opencode"
            assert "--no-network" in fetched.args
        finally:
            # Restore the default by re-registering the original
            register_profile(BUILTIN_PROFILES["opencode-deepseek"])

    def test_register_profile_adds_new(self):
        custom = HarnessProfile(
            name="opencode-gpt5",
            harness="opencode",
            command="opencode",
            supports_slash_goal=True,
            description="GPT-5 variant",
        )
        try:
            register_profile(custom)
            assert get_profile("opencode-gpt5") is not None
            assert "opencode-gpt5" in [p.name for p in list_profiles()]
        finally:
            # Don't leak the profile to other tests — restore the
            # registry by re-registering the originals.
            from agents_gateway.harness.profiles import _REGISTERED
            _REGISTERED.pop("opencode-gpt5", None)

    def test_unknown_profile_raises_in_validate(self):
        # The /harness-profiles/validate HTTP route returns 404 for unknown
        # profiles; here we exercise that get_profile returns None and
        # the caller's logic surfaces it correctly.
        assert get_profile("does-not-exist") is None


class TestProfileProperties:
    def test_opencode_supports_slash_goal(self):
        assert BUILTIN_PROFILES["opencode-deepseek"].supports_slash_goal is True
        assert BUILTIN_PROFILES["fake-test"].supports_slash_goal is True

    def test_claude_does_not_support_slash_goal(self):
        assert BUILTIN_PROFILES["claude-code"].supports_slash_goal is False
        assert BUILTIN_PROFILES["codex"].supports_slash_goal is False

    def test_profiles_use_tmux_stdin_input_mode(self):
        for p in BUILTIN_PROFILES.values():
            assert p.input_mode == "tmux_stdin"

    def test_fake_test_points_at_run_py(self):
        p = get_profile("fake-test")
        assert p.command == "python3"
        # The bundled profile may use either the relative path (as in
        # initial registration) or an absolute path computed at module
        # import time. Both forms point at the same fixture.
        assert any(
            arg.endswith("agents/fake-test/run.py") for arg in p.args
        ), p.args

    def test_harness_profile_to_dict_contains_required_keys(self):
        d = get_profile("claude-code").to_dict()
        for k in ("name", "harness", "command", "args",
                  "supports_slash_goal", "goal_command",
                  "input_mode", "completion_strategy",
                  "goal_strategy", "default", "description"):
            assert k in d
