"""Skills Gateway client and validation for agent manifests."""

from __future__ import annotations

from typing import Any

import httpx

from agents_gateway.config import SkillsGatewayIntegrationConfig
from agents_gateway.manifest import ValidationResult


class SkillDefinition:
    def __init__(self, id: str, name: str = "") -> None:
        self.id = id
        self.name = name


class SkillsGatewayClient:
    def __init__(self, config: SkillsGatewayIntegrationConfig) -> None:
        self.config = config
        self._base_url = config.base_url.rstrip("/")
        self._timeout = config.timeout_seconds

    def fetch_skills(self) -> list[SkillDefinition]:
        url = f"{self._base_url}/skills"
        with httpx.Client(timeout=self._timeout) as client:
            resp = client.get(url)
            resp.raise_for_status()
            data: Any = resp.json()
        if isinstance(data, list):
            raw_list = data
        elif isinstance(data, dict):
            raw_list = data.get("skills", [])
        else:
            raw_list = []
        return [SkillDefinition(id=s["id"], name=s.get("name", "")) for s in raw_list]


def validate_agent_skills(
    agent_id: str,
    referenced_skills: list[str],
    skills_config: SkillsGatewayIntegrationConfig,
) -> list[ValidationResult]:
    results: list[ValidationResult] = []

    if not skills_config.enabled or not referenced_skills:
        return results

    client = SkillsGatewayClient(skills_config)

    try:
        available_skills = client.fetch_skills()
    except Exception as e:
        severity = "error" if skills_config.strict else "warning"
        results.append(ValidationResult(
            agent_id=agent_id,
            severity=severity,
            message=f"Skills Gateway fetch failed: {e}",
        ))
        return results

    available_ids = {s.id for s in available_skills}

    for skill_id in referenced_skills:
        if skill_id not in available_ids:
            severity = "error" if skills_config.strict else "warning"
            results.append(ValidationResult(
                agent_id=agent_id,
                severity=severity,
                message=f"Referenced skill '{skill_id}' not found in Skills Gateway",
            ))

    return results
