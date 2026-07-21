"""Agent catalog, profiles, and search."""

from __future__ import annotations

import shutil
from pathlib import Path

from pydantic import BaseModel

from agents_gateway.config import GatewayConfig
from agents_gateway.harness.profiles import (
    HarnessProfile,
    get_profile as _get_harness_profile,
    list_profiles as _list_harness_profiles,
)
from agents_gateway.manifest import AgentManifest, ValidationResult, load_manifest
from agents_gateway.skills_validation import validate_agent_skills


class CatalogEntry(BaseModel):
    id: str
    name: str
    description: str
    version: str
    path: str
    runtime: dict[str, str]
    risk_level: str = "low"


class HarnessCatalogEntry(BaseModel):
    """A first-class catalog entry for a harness profile.

    Harness entries are surfaced alongside manifest-backed agents so
    Composer can discover every runnable `agent_id` — including the
    harness-session ones — through the same ``/agents`` API. They're
    distinguishable by ``kind == 'harness'``.
    """

    id: str
    kind: str = "harness"
    harness: str
    runtime_type: str = "harness_session"
    profile: str
    display_name: str
    command: str
    supports_slash_goal: bool
    capabilities: list[str] = []
    enabled: bool = True
    availability: dict[str, object] = {}
    metadata: dict[str, object] = {}


# The standard capability set advertised by every harness profile.
# Individual profiles may override/extend this via their description
# field; this is the floor for "this is what a harness can do".
_HARNESS_CAPABILITIES = [
    "code.edit",
    "code.review",
    "tests.run",
    "verification.run",
    "artifacts.produce",
]


def _profile_to_entry(p: HarnessProfile) -> HarnessCatalogEntry:
    return HarnessCatalogEntry(
        id=p.name,
        harness=p.harness,
        profile=p.name,
        display_name=p.name,
        command=p.command,
        supports_slash_goal=p.supports_slash_goal,
        capabilities=list(_HARNESS_CAPABILITIES),
        metadata={"description": p.description,
                   "args": list(p.args),
                   "goal_command": p.goal_command,
                   "input_mode": p.input_mode,
                   "goal_strategy": p.goal_strategy,
                   "default": p.default},
    )


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

    # ------------------------------------------------------------------
    # Harness profile catalog surface
    # ------------------------------------------------------------------
    #
    # The harness profiles (opencode-deepseek, claude-code, codex,
    # fake-test, ...) are first-class catalog entries alongside the
    # manifest-backed agents above. The milestone spec requires that
    # Composer can ``GET /agents`` and discover every runnable
    # `agent_id`, including harness-session ones; these methods make
    # that possible without a second API concept.

    def list_harness_profiles(self) -> list[HarnessCatalogEntry]:
        """Return all known harness profiles as catalog entries."""
        return [_profile_to_entry(p) for p in _list_harness_profiles()]

    def get_harness_profile_entry(self, name: str) -> HarnessCatalogEntry | None:
        p = _get_harness_profile(name)
        return _profile_to_entry(p) if p is not None else None

    def get_harness_profile(self, name: str) -> HarnessProfile | None:
        return _get_harness_profile(name)

    def resolve_agent_id_to_runtime(self, agent_id: str) -> str | None:
        """Return the runtime_type for an agent_id.

        Manifest agents resolve to ``manifest.runtime.type``; harness
        profiles resolve to ``harness_session``. ``None`` means the
        agent_id is unknown to both the manifest catalog and the harness
        profile catalog.
        """
        manifest = self.get_agent(agent_id)
        if manifest is not None:
            return manifest.runtime.type
        if _get_harness_profile(agent_id) is not None:
            return "harness_session"
        return None

    def check_harness_availability(self, name: str) -> dict[str, object]:
        """Return a structured availability report for a harness profile.

        This never raises — a missing profile is reported as
        ``{"runnable": false, "error": "unknown profile"}``. The check
        shells out to ``shutil.which`` (cheap, no LLM call) so it's
        safe to call from the request path.
        """
        from agents_gateway.logging import log_event
        p = _get_harness_profile(name)
        if p is None:
            log_event("harness_availability_checked",
                      f"harness availability checked: {name} (unknown profile)",
                      profile=name, runnable=False)
            return {"profile": name, "configured": False,
                    "binary_present": False, "credentials_present": None,
                    "runnable": False, "command": "",
                    "error": "unknown profile"}
        binary_present = bool(p.command) and shutil.which(p.command) is not None
        # Credential presence is detected by checking known env vars
        # per-profile type without ever printing their value.
        credentials_present: bool | None = None
        for env_name in _credential_env_names(p.harness):
            present = bool(__import__("os").environ.get(env_name))
            if present:
                credentials_present = True
                break
        # If no env matched, credentials_present remains None; map to False.
        if credentials_present is None:
            credentials_present = False
        runnable = binary_present and (credentials_present is not False)
        log_event("harness_availability_checked",
                  f"harness availability checked: {name} runnable={runnable}",
                  profile=name, harness=p.harness,
                  binary_present=binary_present,
                  credentials_present=credentials_present,
                  runnable=runnable)
        return {
            "profile": name,
            "harness": p.harness,
            "configured": True,
            "binary_present": binary_present,
            "credentials_present": credentials_present,
            "runnable": runnable,
            "command": p.command,
            "version": None,
            "error": None if runnable else (
                "missing_binary" if not binary_present else "missing_credentials"
            ),
        }

    def validate_all(self) -> list[ValidationResult]:
        errors = list(self._errors)
        agents_dir = Path(self.config.agents.dir)
        if not agents_dir.exists():
            errors.append(ValidationResult(
                agent_id="_global", severity="error",
                message=f"Agents directory does not exist: {self.config.agents.dir}",
            ))

        skills_config = self.config.integrations.skills_gateway
        for agent_id, manifest in self._agents.items():
            if manifest.skills:
                errors.extend(validate_agent_skills(
                    agent_id=agent_id,
                    referenced_skills=manifest.skills,
                    skills_config=skills_config,
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


def _credential_env_names(harness: str) -> list[str]:
    """Return env-var-name hints for credential presence checks.

    We never print the *values* of these vars — only a boolean that
    at least one is set. The specific env vars per harness are
    intentionally conservative so a misconfiguration is reported as
    "missing credentials" rather than a false positive.
    """
    if harness == "opencode":
        return ["DEEPSEEK_API_KEY", "OPENROUTER_API_KEY",
                "OPENAI_API_KEY", "ANTHROPIC_API_KEY"]
    if harness == "pi":
        return ["DEEPSEEK_API_KEY", "OPENAI_API_KEY",
                "ANTHROPIC_API_KEY", "NVIDIA_API_KEY",
                "OPENROUTER_API_KEY"]
    if harness == "claude":
        return ["ANTHROPIC_API_KEY"]
    if harness == "codex":
        return ["OPENAI_API_KEY"]
    # fake-test / unknown harnesses don't need credentials.
    return []


__all__ = ["AgentCatalog", "CatalogEntry", "HarnessCatalogEntry"]

