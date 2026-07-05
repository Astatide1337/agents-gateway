"""Agent catalog, profiles, and search."""

from __future__ import annotations

from pathlib import Path

from pydantic import BaseModel

from agents_gateway.config import GatewayConfig
from agents_gateway.manifest import AgentManifest, ValidationResult, load_manifest


class CatalogEntry(BaseModel):
    id: str
    name: str
    description: str
    version: str
    path: str
    runtime: dict[str, str]
    risk_level: str = "low"


class AgentCatalog:
    def __init__(self, config: GatewayConfig) -> None:
        self.config = config
        self._agents: dict[str, AgentManifest] = {}
        self._errors: list[ValidationResult] = []
        self._profiles: dict[str, list[str]] = {
            name: list(profile.agents)
            for name, profile in config.profiles.items()
        }
        self._scan()

    def _scan(self) -> None:
        agents_dir = Path(self.config.agents.dir)
        if not agents_dir.exists():
            return
        for entry in sorted(agents_dir.iterdir()):
            if not entry.is_dir():
                continue
            manifest_path = entry / "agent.yaml"
            manifest, results = load_manifest(manifest_path)
            for r in results:
                if r.severity == "error":
                    self._errors.append(r)
            if manifest is not None:
                self._agents[manifest.id] = manifest

    @property
    def active_profile(self) -> str | None:
        return self.config.profile

    def _filter_by_profile(self, agent_ids: list[str]) -> list[str]:
        if not self.active_profile:
            return agent_ids
        profile_agents = self._profiles.get(self.active_profile)
        if profile_agents is None:
            raise ValueError(f"Unknown profile: {self.active_profile}")
        return [a for a in agent_ids if a in profile_agents]

    def list_agents(self) -> list[AgentManifest]:
        ids = list(self._agents.keys())
        filtered = self._filter_by_profile(ids)
        return [self._agents[a] for a in filtered if a in self._agents]

    def get_agent(self, agent_id: str) -> AgentManifest | None:
        ids = self._filter_by_profile([agent_id])
        if not ids:
            return None
        return self._agents.get(agent_id)

    def search_agents(self, query: str) -> list[AgentManifest]:
        query_lower = query.lower()
        results = []
        for agent in self.list_agents():
            searchable = f"{agent.id} {agent.name} {agent.description} {' '.join(agent.tags)}".lower()
            if query_lower in searchable:
                results.append(agent)
        return results

    def catalog_entries(self) -> list[CatalogEntry]:
        return [
            CatalogEntry(
                id=a.id,
                name=a.name,
                description=a.description,
                version=a.version,
                path=a.id,
                runtime={"type": a.runtime.type},
                risk_level=a.risk_level.value,
            )
            for a in self.list_agents()
        ]

    def validate_all(self) -> list[ValidationResult]:
        errors = list(self._errors)
        agents_dir = Path(self.config.agents.dir)
        if not agents_dir.exists():
            errors.append(ValidationResult(
                agent_id="_global", severity="error",
                message=f"Agents directory does not exist: {self.config.agents.dir}",
            ))
        return errors

    @property
    def total_count(self) -> int:
        return len(self._agents)

    @property
    def invalid_count(self) -> int:
        return len(self._errors)

    @property
    def profiles(self) -> dict[str, list[str]]:
        return dict(self._profiles)
