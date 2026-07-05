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
