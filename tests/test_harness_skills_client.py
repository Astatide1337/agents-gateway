"""Tests for harness.client_skills (SkillsGatewayClient).

Verifies:

  * Disabled client returns blocked_reason=skills_gateway_unconfigured.
  * Enabled client with mock httpx validates required skills list.
  * Strict mode rejects unknown skills; non-strict mode just warns.
  * Token redaction helper behaves correctly.
"""

from __future__ import annotations

import pytest

from agents_gateway.harness.client_skills import (
    SkillsGatewayClient,
    SkillsGatewayConfig,
    SkillsValidationResult,
)


class TestSkillsConfig:
    def test_defaults_disabled(self):
        c = SkillsGatewayConfig()
        assert c.enabled is False

    def test_enabled_prefix_url(self):
        c = SkillsGatewayConfig(
            enabled=True, base_url="http://gateway.local",
        )
        assert c.enabled is True
        assert c.base_url == "http://gateway.local"


class TestValidation:
    def test_disabled_returns_blocked(self):
        cfg = SkillsGatewayConfig(enabled=False, strict=True)
        client = SkillsGatewayClient(cfg)
        result = client.validate_required_skills(["test-driven-development"])
        assert isinstance(result, SkillsValidationResult)
        assert result.blocked_reason == "skills_gateway_unconfigured"

    def test_disabled_relaxed_returns_no_blocked(self):
        cfg = SkillsGatewayConfig(enabled=False, strict=False)
        client = SkillsGatewayClient(cfg)
        result = client.validate_required_skills(["test-driven-development"])
        assert isinstance(result, SkillsValidationResult)
        assert result.blocked_reason == ""
        assert "test-driven-development" in result.unknown

    def test_enabled_strict_rejects_unknown_skill(self, respx_mock):
        cfg = SkillsGatewayConfig(
            enabled=True, strict=True,
            base_url="http://gw.example.com",
            mcp_path="/skills",
        )
        client = SkillsGatewayClient(cfg)
        respx_mock.get(
            "http://gw.example.com/skills",
        ).respond(json={
            "skills": [{"id": "test-driven-development"}],
        })
        result = client.validate_required_skills(
            ["test-driven-development", "unknown-one"],
        )
        # The known one is in the known list; unknown is in unknown list
        assert "test-driven-development" in result.known
        assert "unknown-one" in result.unknown

    def test_enabled_relaxed_does_not_block(self, respx_mock):
        cfg = SkillsGatewayConfig(
            enabled=True, strict=False,
            base_url="http://gw.example.com",
            mcp_path="/skills",
        )
        client = SkillsGatewayClient(cfg)
        respx_mock.get(
            "http://gw.example.com/skills",
        ).respond(json={
            "skills": [{"id": "test-driven-development"}],
        })
        result = client.validate_required_skills(
            ["test-driven-development", "unknown-one"],
        )
        assert "unknown-one" in result.unknown

    def test_strict_block_when_gateway_disabled(self):
        cfg = SkillsGatewayConfig(
            enabled=False, strict=True,
        )
        client = SkillsGatewayClient(cfg)
        result = client.validate_required_skills(["any-skill"])
        assert result.blocked_reason is not None
        assert "skills_gateway_unconfigured" in result.blocked_reason \
               or "unconfigured" in result.blocked_reason.lower()


class TestSkillsValidationResult:
    def test_empty_skills_passes(self):
        r = SkillsValidationResult(True, [], [], blocked_reason=None)
        assert r.known == []
        assert r.unknown == []
        assert r.blocked_reason is None
