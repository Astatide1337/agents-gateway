"""Tests for Skills Gateway validation (AGW-008)."""

from __future__ import annotations

from unittest.mock import create_autospec, patch

import httpx
import pytest

from agents_gateway.config import GatewayConfig, SkillsGatewayIntegrationConfig
from agents_gateway.manifest import AgentManifest, ValidationResult
from agents_gateway.skills_validation import (
    SkillDefinition,
    SkillsGatewayClient,
    validate_agent_skills,
)


@pytest.fixture
def skills_config_disabled() -> SkillsGatewayIntegrationConfig:
    return SkillsGatewayIntegrationConfig(enabled=False)


@pytest.fixture
def skills_config_enabled_relaxed() -> SkillsGatewayIntegrationConfig:
    return SkillsGatewayIntegrationConfig(enabled=True, strict=False)


@pytest.fixture
def skills_config_enabled_strict() -> SkillsGatewayIntegrationConfig:
    return SkillsGatewayIntegrationConfig(enabled=True, strict=True)


class TestSkillsGatewayClient:
    def test_fetch_skills_list_format(self, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(
            json=[{"id": "skill-a", "name": "Skill A"}, {"id": "skill-b", "name": "Skill B"}],
        )
        config = SkillsGatewayIntegrationConfig(enabled=True, base_url="http://localhost:8091")
        client = SkillsGatewayClient(config)
        skills = client.fetch_skills()
        assert len(skills) == 2
        assert skills[0].id == "skill-a"
        assert skills[1].id == "skill-b"

    def test_fetch_skills_object_format(self, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(
            json={"skills": [{"id": "skill-x"}, {"id": "skill-y"}]},
        )
        config = SkillsGatewayIntegrationConfig(enabled=True, base_url="http://localhost:8091")
        client = SkillsGatewayClient(config)
        skills = client.fetch_skills()
        assert len(skills) == 2
        assert skills[0].id == "skill-x"
        assert skills[1].id == "skill-y"

    def test_fetch_skills_http_error(self, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(status_code=503)
        config = SkillsGatewayIntegrationConfig(enabled=True, base_url="http://localhost:8091")
        client = SkillsGatewayClient(config)
        with pytest.raises(httpx.HTTPStatusError):
            client.fetch_skills()

    def test_fetch_skills_timeout(self, respx_mock):
        respx_mock.get("http://localhost:8091/skills").side_effect = httpx.ConnectError("Connection refused")
        config = SkillsGatewayIntegrationConfig(enabled=True, base_url="http://localhost:8091")
        client = SkillsGatewayClient(config)
        with pytest.raises(httpx.ConnectError):
            client.fetch_skills()


class TestValidateAgentSkills:
    def test_disabled_returns_empty(self, skills_config_disabled):
        results = validate_agent_skills("agent-a", ["skill-1"], skills_config_disabled)
        assert results == []

    def test_no_skills_returns_empty(self, skills_config_enabled_relaxed):
        results = validate_agent_skills("agent-a", [], skills_config_enabled_relaxed)
        assert results == []

    def test_missing_skill_warning_when_not_strict(self, skills_config_enabled_relaxed, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(
            json=[{"id": "skill-a"}],
        )
        results = validate_agent_skills("agent-a", ["skill-a", "skill-b"], skills_config_enabled_relaxed)
        assert len(results) == 1
        assert results[0].severity == "warning"
        assert "skill-b" in results[0].message

    def test_missing_skill_error_when_strict(self, skills_config_enabled_strict, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(
            json=[{"id": "skill-a"}],
        )
        results = validate_agent_skills("agent-a", ["skill-a", "skill-b"], skills_config_enabled_strict)
        assert len(results) == 1
        assert results[0].severity == "error"
        assert "skill-b" in results[0].message

    def test_all_skills_found(self, skills_config_enabled_strict, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(
            json=[{"id": "skill-a"}, {"id": "skill-b"}, {"id": "skill-c"}],
        )
        results = validate_agent_skills("agent-a", ["skill-a", "skill-b"], skills_config_enabled_strict)
        assert results == []

    def test_fetch_failure_is_warning_when_not_strict(self, skills_config_enabled_relaxed, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(status_code=503)
        results = validate_agent_skills("agent-a", ["skill-a"], skills_config_enabled_relaxed)
        assert len(results) == 1
        assert results[0].severity == "warning"
        assert "fetch failed" in results[0].message.lower()

    def test_fetch_failure_is_error_when_strict(self, skills_config_enabled_strict, respx_mock):
        respx_mock.get("http://localhost:8091/skills").respond(status_code=503)
        results = validate_agent_skills("agent-a", ["skill-a"], skills_config_enabled_strict)
        assert len(results) == 1
        assert results[0].severity == "error"
        assert "fetch failed" in results[0].message.lower()


class TestCatalogIntegration:
    def test_validate_all_includes_skills_when_enabled(self, tmp_path, respx_mock):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()

        (agents_dir / "agent-a").mkdir()
        (agents_dir / "agent-a" / "agent.yaml").write_text(
            "id: agent-a\nname: Agent A\ndescription: Test\nversion: 0.1.0\n"
            "runtime:\n  type: local-stub\nskills:\n  - skill-a\n  - skill-unknown\n"
        )

        respx_mock.get("http://localhost:8091/skills").respond(
            json=[{"id": "skill-a"}, {"id": "skill-b"}],
        )

        cfg = GatewayConfig(
            agents={"dir": str(agents_dir)},
            integrations={"skills_gateway": {"enabled": True, "strict": True, "base_url": "http://localhost:8091"}},
        )
        from agents_gateway.catalog import AgentCatalog
        catalog = AgentCatalog(cfg)
        results = catalog.validate_all()
        skills_errors = [r for r in results if "skill-unknown" in r.message]
        assert len(skills_errors) == 1
        assert skills_errors[0].severity == "error"

    def test_validate_all_skips_skills_when_disabled(self, tmp_path):
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()

        (agents_dir / "agent-a").mkdir()
        (agents_dir / "agent-a" / "agent.yaml").write_text(
            "id: agent-a\nname: Agent A\ndescription: Test\nversion: 0.1.0\n"
            "runtime:\n  type: local-stub\nskills:\n  - skill-unknown\n"
        )

        cfg = GatewayConfig(
            agents={"dir": str(agents_dir)},
            integrations={"skills_gateway": {"enabled": False}},
        )
        from agents_gateway.catalog import AgentCatalog
        catalog = AgentCatalog(cfg)
        results = catalog.validate_all()
        skills_results = [r for r in results if "skill" in r.message.lower()]
        assert len(skills_results) == 0
