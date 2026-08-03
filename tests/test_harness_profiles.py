"""Tests for harness profiles (pi-coding-agent / opencode / claude / codex / fake-test)."""

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
    def test_builtin_table_has_five_profiles(self):
        assert set(BUILTIN_PROFILES) == {
            "pi-coding-agent", "opencode", "claude-code", "codex",
            "fake-test",
        }

    def test_pi_coding_agent_is_default(self):
        assert BUILTIN_PROFILES["pi-coding-agent"].default is True

    def test_list_profiles_returns_all_sorted(self):
        names = [p.name for p in list_profiles()]
        assert names == sorted(names)
        assert "pi-coding-agent" in names
        assert "opencode" in names

    @pytest.mark.parametrize("name,harness", [
        ("pi-coding-agent", "pi"),
        ("opencode", "opencode"),
        ("claude-code", "claude"),
        ("codex", "codex"),
        ("fake-test", "fake"),
    ])
    def test_profile_resolves_kind(self, name, harness):
        p = get_profile(name)
        assert p is not None
        assert p.harness == harness

    def test_opencode_passes_auto_flag(self):
        """Without --auto, opencode's permission TUI blocks forever on
        unattended sessions — nothing can ever answer the prompt."""
        p = get_profile("opencode")
        assert p is not None
        assert "--auto" in p.args

    def test_unknown_profile_returns_none(self):
        assert get_profile("nonexistent-xyz") is None

    def test_get_default_profile_returns_pi_when_unregistered(self):
        # No new profiles registered — should fall back to pi-coding-agent
        assert get_default_profile().name == "pi-coding-agent"

    def test_register_profile_overrides_builtin(self):
        custom = HarnessProfile(
            name="pi-coding-agent",
            harness="pi",
            command="/usr/local/bin/pi",
            args=("--no-network",),
            supports_slash_goal=False,
            goal_strategy=GoalStrategy.plain_prompt.value,
            description="Custom",
        )
        try:
            register_profile(custom)
            fetched = get_profile("pi-coding-agent")
            assert fetched.command == "/usr/local/bin/pi"
            assert "--no-network" in fetched.args
        finally:
            # Restore the default by re-registering the original
            register_profile(BUILTIN_PROFILES["pi-coding-agent"])

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
            # registry by removing the test-only entry.
            from agents_gateway.harness.profiles import _REGISTERED
            _REGISTERED.pop("opencode-gpt5", None)

    def test_unknown_profile_raises_in_validate(self):
        # The /harness-profiles/validate HTTP route returns 404 for unknown
        # profiles; here we exercise that get_profile returns None and
        # the caller's logic surfaces it correctly.
        assert get_profile("does-not-exist") is None


class TestProfileProperties:
    def test_opencode_does_not_use_slash_goal(self):
        """slash_goal was unreliable for opencode; plain_prompt
        registers cleanly (see goal.py resolve_strategy)."""
        assert BUILTIN_PROFILES["opencode"].supports_slash_goal is False

    def test_fake_test_supports_slash_goal(self):
        assert BUILTIN_PROFILES["fake-test"].supports_slash_goal is True

    def test_claude_does_not_support_slash_goal(self):
        assert BUILTIN_PROFILES["claude-code"].supports_slash_goal is False
        assert BUILTIN_PROFILES["codex"].supports_slash_goal is False
        assert BUILTIN_PROFILES["pi-coding-agent"].supports_slash_goal is False

    def test_profiles_use_tmux_stdin_input_mode(self):
        # opencode is the one deliberate exception: it runs in
        # process_json mode (see process_json.OpencodeJsonDriver) to
        # avoid the alt-screen scrollback loss that caused genuinely
        # successful tasks to be misclassified as stuck `running`.
        for p in BUILTIN_PROFILES.values():
            if p.name == "opencode":
                assert p.input_mode == "process_json"
            else:
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
                  "goal_strategy", "default", "description",
                  "model_arg_name", "default_model"):
            assert k in d


class TestModelOverride:
    """Per-task model override via HarnessProfile.effective_args."""

    def test_pi_uses_double_dash_model_flag(self):
        p = get_profile("pi-coding-agent")
        assert p.model_arg_name == "--model"

    def test_opencode_uses_dash_m_flag(self):
        p = get_profile("opencode")
        assert p.model_arg_name == "-m"

    def test_codex_fake_do_not_support_model_override(self):
        for name in ("codex", "fake-test"):
            p = get_profile(name)
            assert p.model_arg_name is None

    def test_claude_code_supports_optional_model_override(self):
        p = get_profile("claude-code")
        assert p.model_arg_name == "--model"
        assert p.model_optional is True

    def test_effective_args_injects_model_when_override_given(self):
        p = get_profile("pi-coding-agent")
        args = p.effective_args(
            model_override="nvidia/nemotron-3-ultra-550b-a55b:free"
        )
        assert "--model" in args
        assert args[args.index("--model") + 1] == \
            "nvidia/nemotron-3-ultra-550b-a55b:free"

    def test_effective_args_injects_default_model_when_no_override(self):
        p = HarnessProfile(
            name="test-pi",
            harness="pi",
            command="pi",
            args=("--thinking", "off"),
            model_arg_name="--model",
            default_model="nvidia/nemotron-3-ultra-550b-a55b:free",
        )
        args = p.effective_args()
        assert "--model" in args
        assert args[args.index("--model") + 1] == \
            "nvidia/nemotron-3-ultra-550b-a55b:free"

    def test_effective_args_skips_model_when_no_arg_name(self):
        p = get_profile("fake-test")
        args = p.effective_args(
            model_override="nvidia/nemotron-3-ultra-550b-a55b:free"
        )
        assert "--model" not in args
        assert "-m" not in args

    def test_effective_args_skips_model_when_override_and_default_both_empty(self):
        p = get_profile("pi-coding-agent")
        # pi-coding-agent has model_arg_name set but default_model=None,
        # so without a per-task override there should be no --model flag.
        args = p.effective_args()
        assert "--model" not in args

    def test_opencode_effective_args_uses_dash_m_flag(self):
        p = get_profile("opencode")
        args = p.effective_args(
            model_override="openrouter/nvidia/nemotron-3-ultra-550b-a55b:free"
        )
        assert "-m" in args
        assert args[args.index("-m") + 1] == \
            "openrouter/nvidia/nemotron-3-ultra-550b-a55b:free"


class TestModelPolicy:
    """Enforced free-model allowlist via validate_model_for_profile."""

    def test_missing_model_for_pi_raises_missing_model_error(self):
        from agents_gateway.harness.profiles import (
            MissingModelError,
            validate_model_for_profile,
        )
        p = get_profile("pi-coding-agent")
        with pytest.raises(MissingModelError):
            validate_model_for_profile("", p)
        with pytest.raises(MissingModelError):
            validate_model_for_profile(None, p)
        with pytest.raises(MissingModelError):
            validate_model_for_profile("   ", p)

    def test_disapproved_model_for_pi_raises_disapproved_model_error(self):
        from agents_gateway.harness.profiles import (
            DisapprovedModelError,
            validate_model_for_profile,
        )
        p = get_profile("pi-coding-agent")
        with pytest.raises(DisapprovedModelError) as exc:
            validate_model_for_profile("anthropic/claude-sonnet-4", p)
        assert "anthropic/claude-sonnet-4" in str(exc.value)
        assert "nvidia/nemotron-3-ultra-550b-a55b:free" in str(exc.value)

    def test_approved_model_for_pi_passes(self):
        from agents_gateway.harness.profiles import validate_model_for_profile
        p = get_profile("pi-coding-agent")
        # Default allowlist contains nvidia/nemotron-3-ultra-550b-a55b:free
        validated = validate_model_for_profile(
            "nvidia/nemotron-3-ultra-550b-a55b:free", p
        )
        assert validated == "nvidia/nemotron-3-ultra-550b-a55b:free"

    def test_opencode_approved_model_passes(self):
        from agents_gateway.harness.profiles import validate_model_for_profile
        p = get_profile("opencode")
        validated = validate_model_for_profile(
            "nvidia/nemotron-3-ultra-550b-a55b:free", p
        )
        assert validated == "nvidia/nemotron-3-ultra-550b-a55b:free"

    def test_profiles_without_model_arg_name_skip_validation(self):
        """codex, fake-test ignore model override entirely."""
        from agents_gateway.harness.profiles import validate_model_for_profile
        for name in ("codex", "fake-test"):
            p = get_profile(name)
            # Should return the model (or empty string) without validation
            assert validate_model_for_profile("any-model-id", p) == "any-model-id"
            assert validate_model_for_profile("", p) == ""
            assert validate_model_for_profile(None, p) == ""

    def test_claude_code_bare_launch_needs_no_model(self):
        from agents_gateway.harness.profiles import validate_model_for_profile
        p = get_profile("claude-code")
        assert validate_model_for_profile("", p) == ""
        assert validate_model_for_profile(None, p) == ""

    def test_claude_code_override_still_validated_against_allowlist(self):
        from agents_gateway.harness.profiles import (
            DisapprovedModelError,
            validate_model_for_profile,
        )
        p = get_profile("claude-code")
        with pytest.raises(DisapprovedModelError):
            validate_model_for_profile("some-random-model", p)

    def test_custom_allowlist_via_env(self, monkeypatch):
        from agents_gateway.harness.profiles import (
            reload_allowlist,
            validate_model_for_profile,
        )
        monkeypatch.setenv("AGW_APPROVED_FREE_MODELS", "custom/model-a,custom/model-b")
        reload_allowlist()
        p = get_profile("pi-coding-agent")
        # custom/model-a should now be valid
        assert validate_model_for_profile("custom/model-a", p) == "custom/model-a"
        assert validate_model_for_profile("custom/model-b", p) == "custom/model-b"
        # nvidia/nemotron should now be invalid
        with pytest.raises(Exception):
            validate_model_for_profile("nvidia/nemotron-3-ultra-550b-a55b:free", p)


class TestBillingMode:
    """Metered (API-key) vs subscription (flat-rate CLI login) profiles."""

    def test_pi_and_opencode_are_metered(self):
        assert get_profile("pi-coding-agent").billing_mode == "metered"
        assert get_profile("opencode").billing_mode == "metered"

    def test_claude_and_codex_are_subscription(self):
        assert get_profile("claude-code").billing_mode == "subscription"
        assert get_profile("codex").billing_mode == "subscription"

    def test_to_dict_includes_billing_mode(self):
        d = get_profile("claude-code").to_dict()
        assert d["billing_mode"] == "subscription"

    def test_billing_mode_defaults_to_metered(self):
        p = HarnessProfile(name="custom", harness="x", command="x")
        assert p.billing_mode == "metered"


class TestTaskTypeRouting:
    """resolve_route() — declarative task-kind -> ordered-provider table."""

    def test_default_route_is_free_tier_only(self):
        from agents_gateway.harness.profiles import resolve_route
        route = resolve_route(None)
        assert len(route) >= 1
        assert route[0].name == "pi-coding-agent"

    def test_unknown_task_kind_falls_back_to_default(self):
        from agents_gateway.harness.profiles import resolve_route
        route = resolve_route("some-unlisted-kind")
        assert route == resolve_route(None)

    def test_code_review_prefers_subscription_tiers_first(self):
        from agents_gateway.harness.profiles import resolve_route
        route = resolve_route("code-review")
        names = [p.name for p in route]
        assert names == ["claude-code", "codex", "pi-coding-agent"]
        # subscription tiers come before the metered free-tier fallback
        assert route[0].billing_mode == "subscription"
        assert route[-1].billing_mode == "metered"

    def test_long_horizon_fix_prefers_codex_first(self):
        from agents_gateway.harness.profiles import resolve_route
        route = resolve_route("long-horizon-fix")
        assert [p.name for p in route][0] == "codex"

    def test_register_route_adds_new_route(self):
        from agents_gateway.harness.profiles import (
            TASK_TYPE_ROUTES,
            register_route,
            resolve_route,
        )
        try:
            register_route("my-custom-kind", ("codex", "pi-coding-agent"))
            route = resolve_route("my-custom-kind")
            assert [p.name for p in route] == ["codex", "pi-coding-agent"]
        finally:
            TASK_TYPE_ROUTES.pop("my-custom-kind", None)

    def test_route_with_unresolvable_name_skips_it(self):
        from agents_gateway.harness.profiles import (
            TASK_TYPE_ROUTES,
            register_route,
            resolve_route,
        )
        try:
            register_route("stale-route", ("nonexistent-profile-xyz", "pi-coding-agent"))
            route = resolve_route("stale-route")
            assert [p.name for p in route] == ["pi-coding-agent"]
        finally:
            TASK_TYPE_ROUTES.pop("stale-route", None)

    def test_route_with_all_unresolvable_falls_back_to_default_profile(self):
        from agents_gateway.harness.profiles import (
            TASK_TYPE_ROUTES,
            register_route,
            resolve_route,
        )
        try:
            register_route("empty-route", ("nonexistent-a", "nonexistent-b"))
            route = resolve_route("empty-route")
            assert len(route) == 1
            assert route[0].name == "pi-coding-agent"
        finally:
            TASK_TYPE_ROUTES.pop("empty-route", None)
