"""MCP Gateway downstream client.

This is a minimal client today: we only fetch ``/health`` to check
gateway availability, fetch ``/version`` for status reporting, and
format a concise TOOLS.md-style summary of available MCP Gateway tool
categories.

Executing arbitrary MCP Gateway tools on behalf of the harness is
EXPERIMENTAL in this milestone and disabled by default (the harness
itself, running inside tmux with its own agent loop, will set up its
own MCP connection to downstream gateways if needed). We just need to
let the runtime write TOOLS.md telling the harness where to find MCP
Gateway and how to authenticate.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import httpx

from agents_gateway.harness.client_skills import redact_token_in_text


@dataclass
class McpGatewayConfig:
    enabled: bool = False
    base_url: str = ""
    auth_mode: str = "dev-none"
    internal_token: str = ""
    timeout_seconds: float = 5.0


class McpGatewayClient:
    """Read-only MCP Gateway snapshot client."""

    def __init__(self, config: McpGatewayConfig) -> None:
        self.config = config

    def health(self) -> bool:
        if not self.config.enabled or not self.config.base_url:
            return False
        try:
            r = httpx.get(f"{self.config.base_url}/health",
                          timeout=self.config.timeout_seconds)
            return r.status_code == 200
        except Exception:
            return False

    def version(self) -> str:
        if not self.config.enabled or not self.config.base_url:
            return ""
        try:
            r = httpx.get(f"{self.config.base_url}/version",
                          timeout=self.config.timeout_seconds,
                          headers=self._auth_headers())
            if r.status_code == 200:
                return r.json().get("version", "")
        except Exception:
            return ""
        return ""

    def tools_summary(self) -> dict[str, Any]:
        """Return a summary block to embed into .agent-task/TOOLS.md."""
        base = self.config.base_url if self.config.enabled else ""
        return {
            "enabled": self.config.enabled,
            "base_url": base,
            "auth_mode": self.config.auth_mode
                         if self.config.enabled else "disabled",
            "healthy": self.health() if self.config.enabled else False,
            "version": self.version() if self.config.enabled else "",
        }

    def render_tools_md(self) -> str:
        """Render the TOOLS.md content for the worktree."""
        info = self.tools_summary()
        if not info["enabled"]:
            return (
                "# Available tools\n\n"
                "MCP Gateway is not configured for this run.\n"
                "If the harness needs GitHub access, it should request "
                "explicit credentials rather than fabricating them.\n"
            )
        body = [
            "# Available tools",
            "",
            f"- MCP Gateway base URL: `{info['base_url']}`",
            f"- Healthy: {info['healthy']}",
            f"- Version: {info['version'] or 'unknown'}",
            f"- Auth mode: `{info['auth_mode']}`",
            "",
            "Use the MCP Gateway for GitHub/drive/calendar/external tools "
            "when needed. Do not log Authorization headers in artifacts.",
            "",
            "If the gateway becomes unavailable, perform the task without "
            "external tools if possible — do not fabricate API responses.",
        ]
        return "\n".join(body)

    def _auth_headers(self) -> dict[str, str]:
        headers: dict[str, str] = {}
        if self.config.auth_mode == "internal-only" and self.config.internal_token:
            headers["X-Auth-Internal-Token"] = self.config.internal_token
        return headers


def scrub_url_for_logs(url: str) -> str:
    if not url:
        return ""
    # Strip any user:pass@ portion
    if "://" in url and "@" in url.split("://", 1)[1]:
        scheme, rest = url.split("://", 1)
        host = rest.split("@", 1)[-1]
        return f"{scheme}://<redacted>@{host}"
    return url


__all__ = [
    "McpGatewayClient",
    "McpGatewayConfig",
    "redact_token_in_text",
    "scrub_url_for_logs",
]
