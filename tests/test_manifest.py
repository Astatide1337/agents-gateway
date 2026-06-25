"""Tests for agent manifest schema and validation."""

from pathlib import Path

import pytest

from agents_gateway.manifest import (
    AgentManifest,
    RiskLevel,
    ValidationResult,
    load_manifest,
)


class TestAgentManifest:
    def test_valid_manifest(self):
        m = AgentManifest(
            id="test-agent", name="Test Agent", description="A test",
            version="0.1.0", runtime={"type": "local-stub"},
        )
        assert m.id == "test-agent"
        assert m.risk_level == RiskLevel.low

    def test_high_risk(self):
        m = AgentManifest(
            id="risky", name="Risky", description="A risky agent",
            version="0.1.0", runtime={"type": "docker"}, risk_level="high",
        )
        assert m.risk_level == RiskLevel.high

    def test_missing_required_field(self):
        with pytest.raises(Exception):
            AgentManifest(id="test", name="Test")

    def test_default_fields(self):
        m = AgentManifest(
            id="test", name="Test", description="desc",
            version="0.1.0", runtime={"type": "local-stub"},
        )
        assert m.skills == []
        assert m.tools == []
        assert m.permissions == {}
        assert m.tags == []
        assert m.author == ""


class TestLoadManifest:
    def test_valid_manifest(self, tmp_path):
        agent_dir = tmp_path / "my-agent"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text(
            "id: my-agent\nname: My Agent\ndescription: An agent\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        result, errors = load_manifest(manifest)
        assert result is not None
        assert result.id == "my-agent"
        assert len([e for e in errors if e.severity == "error"]) == 0

    def test_missing_yaml(self, tmp_path):
        agent_dir = tmp_path / "missing-agent"
        agent_dir.mkdir()
        result, errors = load_manifest(agent_dir / "agent.yaml")
        assert result is None
        assert any(e.severity == "error" for e in errors)

    def test_invalid_yaml(self, tmp_path):
        agent_dir = tmp_path / "bad-yaml"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text("id: bad\nname: [invalid")
        result, errors = load_manifest(manifest)
        assert result is None
        assert any(e.severity == "error" for e in errors)

    def test_missing_required_fields(self, tmp_path):
        agent_dir = tmp_path / "incomplete"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text("id: incomplete\nname: Incomplete\n")
        result, errors = load_manifest(manifest)
        assert result is None
        assert any(e.severity == "error" for e in errors)

    def test_warning_no_description(self, tmp_path):
        agent_dir = tmp_path / "no-desc"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text(
            "id: no-desc\nname: No Desc\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        result, errors = load_manifest(manifest)
        assert result is not None
        warnings = [e for e in errors if e.severity == "warning"]
        assert any("description" in w.message.lower() for w in warnings)

    def test_warning_id_mismatch(self, tmp_path):
        agent_dir = tmp_path / "dir-name"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text(
            "id: different-id\nname: Test\ndescription: test\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        result, errors = load_manifest(manifest)
        assert result is not None
        warnings = [e for e in errors if e.severity == "warning"]
        assert any("does not match directory" in w.message for w in warnings)

    def test_warnings_separate_from_errors(self, tmp_path):
        agent_dir = tmp_path / "warn-agent"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text(
            "id: warn-agent\nname: Warn Agent\ndescription: test\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        result, errors = load_manifest(manifest)
        assert result is not None
        errs = [e for e in errors if e.severity == "error"]
        warns = [e for e in errors if e.severity == "warning"]
        assert len(errs) == 0
        assert isinstance(warns, list)

    def test_invalid_manifest_does_not_crash(self, tmp_path):
        agent_dir = tmp_path / "crash-test"
        agent_dir.mkdir()
        manifest = agent_dir / "agent.yaml"
        manifest.write_text("not_a_map: true")
        result, errors = load_manifest(manifest)
        assert result is None
        assert any(e.severity == "error" for e in errors)
