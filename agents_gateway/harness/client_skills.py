"""Skills Gateway downstream client.

Used by the harness runtime plane to:

  * Resolve skill names -> skill descriptions/contents.
  * Validate that requested skills exist when the task spec lists them.

Best-effort: if the Skills Gateway is unreachable or unconfigured and
the strict flag is OFF, we proceed with the textual skill references
that the dispatcher already composed into .agent-task/SKILLS.md. If the
strict flag is ON, we surface ``blocked_external`` for the agent_run.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

import httpx


TOKEN_LOG_RE = re.compile(
    r"(token=|secret=|Authorization:\s*Bearer\s+)[A-Za-z0-9._\-]+",
    re.I,
)


class SkillsGatewayError(Exception):
    pass


@dataclass
class SkillsGatewayConfig:
    enabled: bool = False
    base_url: str = "http://localhost:8091"
    mcp_path: str = "/mcp"
    auth_mode: str = "dev-none"
    internal_token: str = ""
    strict: bool = False
    timeout_seconds: float = 5.0


@dataclass
class SkillsValidationResult:
    valid: bool
    known: list[str]
    unknown: list[str]
    blocked_reason: str = ""


class SkillsGatewayClient:
    """Lightweight Skills Gateway client.

    Calls the Skills Gateway's HTTP API to validate/list skills. We do
    NOT run a full MCP handshake here (the harness itself can do that
    once it's launched) — we just check that the requested skills exist.
    """

    def __init__(self, config: SkillsGatewayConfig) -> None:
        self.config = config

    def health(self) -> bool:
        if not self.config.enabled:
            return False
        try:
            r = httpx.get(f"{self.config.base_url}/health",
                          timeout=self.config.timeout_seconds)
            return r.status_code == 200
        except Exception:
            return False

    def list_skills(self) -> list[dict[str, Any]]:
        if not self.config.enabled:
            return []
        try:
            headers = self._auth_headers()
            r = httpx.get(f"{self.config.base_url}/skills",
                          headers=headers,
                          timeout=self.config.timeout_seconds)
            if r.status_code != 200:
                return []
            data = r.json()
            return list(data.get("skills", []))
        except Exception:
            return []

    def validate_required_skills(self,
                                  required: list[str]
                                  ) -> SkillsValidationResult:
        """Return ``SkillsValidationResult`` for a skill-name list.

        Empty list -> always valid. Unknown skills -> ``unknown``.
        If gateway is unavailable and ``strict=True`` we return
        ``blocked_reason='skills_gateway_unavailable'``.
        """
        if not required:
            return SkillsValidationResult(True, [], [])
        if not self.config.enabled:
            return SkillsValidationResult(
                True if not self.config.strict else False,
                [], list(required),
                blocked_reason="skills_gateway_unconfigured"
                if self.config.strict else "",
            )
        skills = self.list_skills()
        known_names = {s.get("id") or s.get("name") for s in skills}
        known = [s for s in required if s in known_names]
        unknown = [s for s in required if s not in known_names]
        if unknown and self.config.strict:
            return SkillsValidationResult(
                False, known, unknown,
                blocked_reason="skills_missing",
            )
        # Best-effort + disabled strict -> proceed but flag unknown.
        return SkillsValidationResult(True, known, unknown)

    def _auth_headers(self) -> dict[str, str]:
        headers: dict[str, str] = {}
        if self.config.auth_mode == "internal-only" and self.config.internal_token:
            headers["X-Auth-Internal-Token"] = self.config.internal_token
        return headers


def redact_token_in_text(text: str) -> str:
    """Helper util: strip obvious token patterns from text.

    Shared between Skills + MCP Gateway clients and the report generator.
    """
    if not text:
        return ""
    return TOKEN_LOG_RE.sub(
        lambda m: (m.group(1) or "") + "<redacted>", text,
    )


__all__ = [
    "SkillsGatewayClient",
    "SkillsGatewayConfig",
    "SkillsGatewayError",
    "SkillsValidationResult",
    "redact_token_in_text",
]
