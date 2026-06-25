"""Auth mode handling for Agents Gateway."""

from __future__ import annotations

from typing import Any

from agents_gateway.config import AuthConfig

VALID_MODES = {"dev-none", "cloudflare-access", "internal-only"}


class AuthResult:
    def __init__(self, allowed: bool, user: str = "", mode: str = "", error: str = "") -> None:
        self.allowed = allowed
        self.user = user
        self.mode = mode
        self.error = error


class AuthHandler:
    def __init__(self, config: AuthConfig) -> None:
        if config.mode not in VALID_MODES:
            raise ValueError(f"Invalid auth mode: {config.mode}. Valid: {VALID_MODES}")
        self.mode = config.mode

    def check(self, client_host: str = "", bearer_token: str = "", cf_jwt: str = "") -> AuthResult:
        if self.mode == "dev-none":
            return AuthResult(allowed=True, user="dev", mode="dev-none")

        if self.mode == "internal-only":
            if _is_internal(client_host):
                return AuthResult(allowed=True, user=client_host, mode="internal-only")
            return AuthResult(allowed=False, error="Access denied: internal-only mode", mode="internal-only")

        if self.mode == "cloudflare-access":
            if cf_jwt:
                return AuthResult(allowed=True, user="cf-user", mode="cloudflare-access")
            if bearer_token:
                return AuthResult(allowed=True, user="bearer-user", mode="cloudflare-access")
            return AuthResult(allowed=False, error="Authentication required", mode="cloudflare-access")

        return AuthResult(allowed=False, error="No auth handler", mode=self.mode)

    @property
    def is_production_safe(self) -> bool:
        return self.mode != "dev-none"


def _is_internal(host: str) -> bool:
    if host in ("127.0.0.1", "::1", "localhost"):
        return True
    parts = host.split(".")
    if len(parts) == 4 and parts[0] == "172" and parts[1] == "20":
        return True
    return False
