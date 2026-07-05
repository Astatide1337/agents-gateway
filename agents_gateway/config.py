"""Configuration loading with precedence: CLI > env > YAML > defaults."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field


class ServiceConfig(BaseModel):
    host: str = "0.0.0.0"
    port: int = 8092
    mcp_path: str = "/mcp"


class AuthConfig(BaseModel):
    mode: str = "dev-none"


class AgentsConfig(BaseModel):
    dir: str = "./agents"


class StorageConfig(BaseModel):
    sqlite_path: str = "./data/agents-gateway.db"
    artifacts_dir: str = "./data/artifacts"


class ObservabilityConfig(BaseModel):
    log_level: str = "INFO"
    log_format: str = "json"
    metrics_enabled: bool = True


class ProfileConfig(BaseModel):
    agents: list[str] = Field(default_factory=list)


class GatewayConfig(BaseModel):
    service: ServiceConfig = ServiceConfig()
    auth: AuthConfig = AuthConfig()
    agents: AgentsConfig = AgentsConfig()
    storage: StorageConfig = StorageConfig()
    observability: ObservabilityConfig = ObservabilityConfig()
    profiles: dict[str, ProfileConfig] = Field(default_factory=dict)
    profile: str | None = None


DEFAULT_CONFIG = GatewayConfig()


def _deep_merge(base: dict, override: dict) -> dict:
    result = dict(base)
    for key, value in override.items():
        if key in result and isinstance(result[key], dict) and isinstance(value, dict):
            result[key] = _deep_merge(result[key], value)
        else:
            result[key] = value
    return result


def _load_yaml(path: str | Path) -> dict:
    p = Path(path)
    if not p.exists():
        return {}
    with open(p) as f:
        data = yaml.safe_load(f)
    return data if isinstance(data, dict) else {}


def _env_overrides() -> dict:
    overrides: dict[str, Any] = {}
    prefix = "AGW_"
    for key, value in os.environ.items():
        if not key.startswith(prefix):
            continue
        config_key = key[len(prefix):].lower()
        parts = config_key.split("__")
        d = overrides
        for part in parts[:-1]:
            d = d.setdefault(part, {})
        d[parts[-1]] = _coerce_env(value)
    return overrides


def _coerce_env(value: str) -> Any:
    if value.lower() in ("true", "1", "yes"):
        return True
    if value.lower() in ("false", "0", "no"):
        return False
    try:
        return int(value)
    except ValueError:
        pass
    try:
        return float(value)
    except ValueError:
        pass
    return value


def load_config(yaml_path: str | Path | None = None) -> GatewayConfig:
    yaml_data = _load_yaml(yaml_path or "agents-gateway.yaml")
    env_data = _env_overrides()
    merged = _deep_merge(DEFAULT_CONFIG.model_dump(), yaml_data)
    merged = _deep_merge(merged, env_data)

    profile = os.environ.get("AGW_PROFILE") or merged.get("profile")
    if profile:
        merged["profile"] = profile

    return GatewayConfig(**merged)
