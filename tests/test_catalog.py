"""Tests for agent catalog and profiles."""

import pytest

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig


@pytest.fixture
def agents_dir(tmp_path):
    d = tmp_path / "agents"
    d.mkdir()

    (d / "agent-a").mkdir()
    (d / "agent-a" / "agent.yaml").write_text(
        "id: agent-a\nname: Agent A\ndescription: First agent\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
    )

    (d / "agent-b").mkdir()
    (d / "agent-b" / "agent.yaml").write_text(
        "id: agent-b\nname: Agent B\ndescription: Second agent\nversion: 0.2.0\nruntime:\n  type: local-stub\nrisk_level: medium\n"
    )

    (d / "broken").mkdir()
    (d / "broken" / "agent.yaml").write_text("invalid: yes")

    return d


@pytest.fixture
def config(agents_dir, tmp_path):
    return GatewayConfig(agents={"dir": str(agents_dir)})


class TestAgentCatalog:
    def test_list_agents(self, config):
        catalog = AgentCatalog(config)
        agents = catalog.list_agents()
        ids = [a.id for a in agents]
        assert "agent-a" in ids
        assert "agent-b" in ids

    def test_invalid_agent_excluded(self, config):
        catalog = AgentCatalog(config)
        agents = catalog.list_agents()
        ids = [a.id for a in agents]
        assert "broken" not in ids

    def test_invalid_count(self, config):
        catalog = AgentCatalog(config)
        assert catalog.invalid_count == 1

    def test_total_count(self, config):
        catalog = AgentCatalog(config)
        assert catalog.total_count == 2

    def test_get_agent(self, config):
        catalog = AgentCatalog(config)
        agent = catalog.get_agent("agent-a")
        assert agent is not None
        assert agent.name == "Agent A"

    def test_get_agent_not_found(self, config):
        catalog = AgentCatalog(config)
        assert catalog.get_agent("nonexistent") is None

    def test_catalog_entries(self, config):
        catalog = AgentCatalog(config)
        entries = catalog.catalog_entries()
        assert len(entries) == 2
        assert entries[0].id in ("agent-a", "agent-b")

    def test_search_agents(self, config):
        catalog = AgentCatalog(config)
        results = catalog.search_agents("first")
        assert len(results) == 1
        assert results[0].id == "agent-a"

    def test_validate_all(self, config):
        catalog = AgentCatalog(config)
        results = catalog.validate_all()
        assert len(results) >= 1


class TestProfiles:
    def test_profile_filters_agents(self, agents_dir):
        cfg = GatewayConfig(
            agents={"dir": str(agents_dir)},
            profiles={"dev": {"agents": ["agent-a"]}},
            profile="dev",
        )
        catalog = AgentCatalog(cfg)
        agents = catalog.list_agents()
        assert len(agents) == 1
        assert agents[0].id == "agent-a"

    def test_unknown_profile_fails(self, agents_dir):
        cfg = GatewayConfig(
            agents={"dir": str(agents_dir)},
            profile="nonexistent",
        )
        catalog = AgentCatalog(cfg)
        with pytest.raises(ValueError, match="Unknown profile"):
            catalog.list_agents()

    def test_no_profile_shows_all(self, config):
        catalog = AgentCatalog(config)
        agents = catalog.list_agents()
        assert len(agents) == 2
