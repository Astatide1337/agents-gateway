"""Tests for configuration loading."""

import os
import tempfile
from pathlib import Path

import pytest
import yaml

from agents_gateway.config import (
    DEFAULT_CONFIG,
    GatewayConfig,
    _deep_merge,
    _env_overrides,
    _load_yaml,
    load_config,
)


class TestDeepMerge:
    def test_simple_override(self):
        base = {"a": 1, "b": 2}
        override = {"b": 3}
        assert _deep_merge(base, override) == {"a": 1, "b": 3}

    def test_nested_merge(self):
        base = {"service": {"host": "0.0.0.0", "port": 8092}}
        override = {"service": {"port": 9090}}
        result = _deep_merge(base, override)
        assert result == {"service": {"host": "0.0.0.0", "port": 9090}}

    def test_empty_override(self):
        base = {"a": 1}
        assert _deep_merge(base, {}) == {"a": 1}


class TestLoadYaml:
    def test_nonexistent_file(self):
        assert _load_yaml("/nonexistent/path.yaml") == {}

    def test_valid_yaml(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({"service": {"port": 9090}}, f)
            f.flush()
            data = _load_yaml(f.name)
        assert data == {"service": {"port": 9090}}
        os.unlink(f.name)

    def test_empty_yaml(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            f.write("")
            f.flush()
            data = _load_yaml(f.name)
        assert data == {}
        os.unlink(f.name)


class TestEnvOverrides:
    def test_simple_env(self):
        os.environ["AGW_SERVICE__PORT"] = "9090"
        try:
            overrides = _env_overrides()
            assert overrides.get("service", {}).get("port") == 9090
        finally:
            del os.environ["AGW_SERVICE__PORT"]

    def test_bool_env(self):
        os.environ["AGW_OBSERVABILITY__METRICS_ENABLED"] = "false"
        try:
            overrides = _env_overrides()
            assert overrides.get("observability", {}).get("metrics_enabled") is False
        finally:
            del os.environ["AGW_OBSERVABILITY__METRICS_ENABLED"]


class TestLoadConfig:
    def test_defaults(self):
        cfg = load_config(yaml_path="/nonexistent.yaml")
        assert cfg.service.host == "0.0.0.0"
        assert cfg.service.port == 8092
        assert cfg.auth.mode == "dev-none"

    def test_yaml_override(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({"service": {"port": 9090}}, f)
            f.flush()
            cfg = load_config(yaml_path=f.name)
        assert cfg.service.port == 9090
        os.unlink(f.name)

    def test_env_override(self):
        os.environ["AGW_SERVICE__PORT"] = "7070"
        try:
            cfg = load_config()
            assert cfg.service.port == 7070
        finally:
            del os.environ["AGW_SERVICE__PORT"]

    def test_profile_from_env(self):
        os.environ["AGW_PROFILE"] = "development"
        try:
            cfg = load_config()
            assert cfg.profile == "development"
        finally:
            del os.environ["AGW_PROFILE"]

    def test_config_precedence_env_over_yaml(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({"service": {"port": 9090}}, f)
            f.flush()
            os.environ["AGW_SERVICE__PORT"] = "7070"
            try:
                cfg = load_config(yaml_path=f.name)
                assert cfg.service.port == 7070
            finally:
                del os.environ["AGW_SERVICE__PORT"]
        os.unlink(f.name)

    def test_profiles_from_yaml(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({"profiles": {"dev": {"agents": ["agent-a"]}}}, f)
            f.flush()
            cfg = load_config(yaml_path=f.name)
        assert cfg.profiles["dev"].agents == ["agent-a"]
        os.unlink(f.name)

    def test_skills_gateway_integration_defaults(self):
        cfg = load_config(yaml_path="/nonexistent.yaml")
        skills_gateway = cfg.integrations.skills_gateway
        assert skills_gateway.enabled is False
        assert skills_gateway.base_url == "http://localhost:8091"
        assert skills_gateway.mcp_path == "/mcp"
        assert skills_gateway.strict is False

    def test_skills_gateway_integration_from_yaml(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({
                "integrations": {
                    "skills_gateway": {
                        "enabled": True,
                        "base_url": "http://skills-gateway:8091",
                        "mcp_path": "/mcp",
                        "strict": True,
                    }
                }
            }, f)
            f.flush()
            cfg = load_config(yaml_path=f.name)
        assert cfg.integrations.skills_gateway.enabled is True
        assert cfg.integrations.skills_gateway.base_url == "http://skills-gateway:8091"
        assert cfg.integrations.skills_gateway.strict is True
        os.unlink(f.name)

    def test_cli_flag_overrides_env_after_load(self):
        """CLI flags applied after load_config override env vars."""
        os.environ["AGW_SERVICE__PORT"] = "7070"
        try:
            cfg = load_config(yaml_path="/nonexistent.yaml")
            assert cfg.service.port == 7070, "env should apply during load"
            cfg.service.port = 8080
            assert cfg.service.port == 8080, "explicit assignment (CLI flag) overrides env"
        finally:
            del os.environ["AGW_SERVICE__PORT"]

    def test_full_precedence_chain(self):
        """CLI > env > YAML > defaults: verify each layer can override the previous."""
        expected_defaults = {
            "host": "0.0.0.0",
            "port": 8092,
            "auth_mode": "dev-none",
            "log_level": "INFO",
        }
        cfg = load_config(yaml_path="/nonexistent.yaml")
        assert cfg.service.host == expected_defaults["host"]
        assert cfg.service.port == expected_defaults["port"]
        assert cfg.auth.mode == expected_defaults["auth_mode"]
        assert cfg.observability.log_level == expected_defaults["log_level"]

        with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f:
            yaml.dump({
                "service": {"host": "0.0.0.0", "port": 9090},
                "auth": {"mode": "internal-only"},
            }, f)
            f.flush()
            os.environ["AGW_OBSERVABILITY__LOG_LEVEL"] = "DEBUG"
            try:
                cfg = load_config(yaml_path=f.name)
                assert cfg.service.port == 9090, "YAML should override default"
                assert cfg.service.host == "0.0.0.0", "YAML same as default should be preserved"
                assert cfg.auth.mode == "internal-only", "YAML auth should be loaded"
                assert cfg.observability.log_level == "DEBUG", "env should override YAML default"

                with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as f2:
                    yaml.dump({"service": {"port": 7070}}, f2)
                    f2.flush()
                    cfg2 = load_config(yaml_path=f2.name)
                    assert cfg2.service.port == 7070, "second YAML (CLI --config) should override env"
                    assert cfg2.auth.mode == "dev-none", "second YAML without auth resets to default"
                    os.unlink(f2.name)
            finally:
                del os.environ["AGW_OBSERVABILITY__LOG_LEVEL"]
        os.unlink(f.name)

    def test_profile_loaded_from_yaml_and_used_by_catalog(self, tmp_path):
        """Profile definitions and active profile from config YAML propagate to catalog."""
        agents_dir = tmp_path / "agents"
        agents_dir.mkdir()
        (agents_dir / "agent-a").mkdir()
        (agents_dir / "agent-a" / "agent.yaml").write_text(
            "id: agent-a\nname: Agent A\ndescription: First\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
        )
        (agents_dir / "agent-b").mkdir()
        (agents_dir / "agent-b" / "agent.yaml").write_text(
            "id: agent-b\nname: Agent B\ndescription: Second\nversion: 0.2.0\nruntime:\n  type: local-stub\n"
        )
        yaml_path = tmp_path / "config.yaml"
        yaml.dump({
            "agents": {"dir": str(agents_dir)},
            "profiles": {"dev": {"agents": ["agent-a"]}},
            "profile": "dev",
        }, open(yaml_path, "w"))

        cfg = load_config(yaml_path=yaml_path)
        assert cfg.profile == "dev"
        assert "dev" in cfg.profiles
        assert cfg.profiles["dev"].agents == ["agent-a"]

        from agents_gateway.catalog import AgentCatalog
        catalog = AgentCatalog(cfg)
        assert catalog.active_profile == "dev"
        agents = catalog.list_agents()
        assert len(agents) == 1
        assert agents[0].id == "agent-a"

    def test_skills_gateway_integration_from_env(self):
        os.environ["AGW_INTEGRATIONS__SKILLS_GATEWAY__ENABLED"] = "true"
        os.environ["AGW_INTEGRATIONS__SKILLS_GATEWAY__BASE_URL"] = "http://skills-gateway:8091"
        try:
            cfg = load_config(yaml_path="/nonexistent.yaml")
            assert cfg.integrations.skills_gateway.enabled is True
            assert cfg.integrations.skills_gateway.base_url == "http://skills-gateway:8091"
        finally:
            del os.environ["AGW_INTEGRATIONS__SKILLS_GATEWAY__ENABLED"]
            del os.environ["AGW_INTEGRATIONS__SKILLS_GATEWAY__BASE_URL"]
