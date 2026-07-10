"""Configuration loading with precedence: CLI > env > YAML > defaults."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field


class RateLimitConfig(BaseModel):
    enabled: bool = False
    requests_per_minute: int = 60


class ServiceConfig(BaseModel):
    host: str = "0.0.0.0"
    port: int = 8092
    mcp_path: str = "/mcp"
    rate_limiting: RateLimitConfig = Field(default_factory=RateLimitConfig)


class AuthConfig(BaseModel):
    mode: str = "dev-none"
    public_base_url: str = ""
    cloudflare_team_domain: str = ""
    cloudflare_aud: str = ""
    internal_secret: str = ""
    allow_unsafe_private_ip_bypass: bool = False
    jwt_leeway_seconds: int = 30


class RuntimeConfig(BaseModel):
    allow_process: bool = False
    docker_network: bool = False
    docker_memory: str = "512m"
    docker_cpus: float = 1.0
    docker_pids_limit: int = 128
    docker_tmpfs_size: str = "64m"
    task_timeout_seconds: int = 300


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


class SkillsGatewayIntegrationConfig(BaseModel):
    enabled: bool = False
    base_url: str = "http://localhost:8091"
    mcp_path: str = "/mcp"
    auth_mode: str = "dev-none"
    internal_token: str = ""
    strict: bool = False
    timeout_seconds: float = 5.0


class McpGatewayIntegrationConfig(BaseModel):
    enabled: bool = False
    base_url: str = ""
    auth_mode: str = "dev-none"
    internal_token: str = ""
    timeout_seconds: float = 5.0


class HarnessRuntimeConfig(BaseModel):
    """Configuration for the harness worktree runtime (harness_session tasks).

    Defaults are conservative and host-friendly:

      * workspace_root / worktree_root / artifacts_root under
        ``/tmp/agents-gateway/*`` so the gateway can boot read-only
        in a Compose container without explicit volume mounts (the
        Docker Compose example below mounts /data; users should
        override these paths via env vars to put worktrees/artifacts
        on a persistent volume).
      * auto_commit defaults to True (so verified work survives cleanly
        on a branch per task). auto_push / auto_pr default to False
        per the milestone spec (you opt in via env vars when you have
        push access configured and CI is enabled).
      * use_fake_tmux defaults to False in production-like setups.
        For the local E2E script + tests we set it True so no real
        tmux daemon is required.
    """
    workspace_root: str = "/tmp/agents-gateway/repos"
    worktree_root: str = "/tmp/agents-gateway/worktrees"
    artifacts_root: str = "/tmp/agents-gateway/artifacts"
    session_poll_interval_seconds: float = 10.0
    session_stall_seconds: int = 900
    auto_commit: bool = True
    auto_push: bool = False
    auto_pr: bool = False
    use_fake_tmux: bool = False
    command_timeout_seconds: int = 1800
    completion_wait_seconds: float = 0.5
    relay_max_time_seconds: float = 3600.0
    max_verify_iterations: int = 50


class IntegrationsConfig(BaseModel):
    skills_gateway: SkillsGatewayIntegrationConfig = Field(default_factory=SkillsGatewayIntegrationConfig)
    mcp_gateway: McpGatewayIntegrationConfig = Field(default_factory=McpGatewayIntegrationConfig)


class GatewayConfig(BaseModel):
    service: ServiceConfig = ServiceConfig()
    auth: AuthConfig = AuthConfig()
    runtime: RuntimeConfig = RuntimeConfig()
    agents: AgentsConfig = AgentsConfig()
    storage: StorageConfig = StorageConfig()
    observability: ObservabilityConfig = ObservabilityConfig()
    integrations: IntegrationsConfig = Field(default_factory=IntegrationsConfig)
    harness: HarnessRuntimeConfig = Field(default_factory=HarnessRuntimeConfig)
    profiles: dict[str, ProfileConfig] = Field(default_factory=dict)
    profile: str | None = None
    environment: str = "dev"


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
