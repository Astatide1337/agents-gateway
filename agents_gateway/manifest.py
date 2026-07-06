"""Agent manifest schema and validation."""

from __future__ import annotations

from enum import Enum
from pathlib import Path
from typing import Any

import yaml
from pydantic import BaseModel, Field, model_validator


class RiskLevel(str, Enum):
    low = "low"
    medium = "medium"
    high = "high"


class RuntimeConfig(BaseModel):
    type: str
    command: str = ""
    docker_image: str = ""


class AgentManifest(BaseModel):
    id: str
    name: str
    description: str = ""
    version: str = "0.1.0"
    runtime: RuntimeConfig
    skills: list[str] = Field(default_factory=list)
    tools: list[str] = Field(default_factory=list)
    permissions: dict[str, Any] = Field(default_factory=dict)
    risk_level: RiskLevel = RiskLevel.low
    tags: list[str] = Field(default_factory=list)
    author: str = ""

    @model_validator(mode="after")
    def id_matches_dir(self) -> "AgentManifest":
        return self


class ValidationResult(BaseModel):
    agent_id: str
    severity: str  # "error" or "warning"
    message: str


def load_manifest(path: Path) -> tuple[AgentManifest | None, list[ValidationResult]]:
    results: list[ValidationResult] = []
    agent_id = path.parent.name

    if not path.exists():
        return None, [ValidationResult(
            agent_id=agent_id, severity="error",
            message=f"agent.yaml not found at {path}",
        )]

    try:
        with open(path) as f:
            raw = yaml.safe_load(f)
    except yaml.YAMLError as e:
        return None, [ValidationResult(
            agent_id=agent_id, severity="error",
            message=f"Invalid YAML: {e}",
        )]

    if not isinstance(raw, dict):
        return None, [ValidationResult(
            agent_id=agent_id, severity="error",
            message="agent.yaml must be a mapping",
        )]

    try:
        manifest = AgentManifest(**raw)
    except Exception as e:
        return None, [ValidationResult(
            agent_id=agent_id, severity="error",
            message=f"Schema validation failed: {e}",
        )]

    if manifest.id != agent_id:
        results.append(ValidationResult(
            agent_id=agent_id, severity="warning",
            message=f"Manifest id '{manifest.id}' does not match directory name '{agent_id}'",
        ))

    if not manifest.description:
        results.append(ValidationResult(
            agent_id=agent_id, severity="warning",
            message="No description provided",
        ))

    if not manifest.author:
        results.append(ValidationResult(
            agent_id=agent_id, severity="warning",
            message="No author specified",
        ))

    return manifest, results
