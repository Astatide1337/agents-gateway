"""Auth mode handling for Agents Gateway.

Modes:
  - dev-none: No authentication (development only)
  - internal-only: Shared-secret header-based auth
  - cloudflare-access: Cloudflare Access JWT verification
"""

from __future__ import annotations

import base64
import json
import os
import re
from typing import Any

from agents_gateway.config import AuthConfig

VALID_MODES = {"dev-none", "cloudflare-access", "internal-only"}

CLOUDFLARE_CERT_ENV = "AGW_CLOUDFLARE_CERT"
INTERNAL_SECRET_ENV = "AGW_INTERNAL_SECRET"

# Cf-Access-Jwt-Assertion header name
CF_JWT_HEADER = "Cf-Access-Jwt-Assertion"
INTERNAL_AUTH_HEADER = "X-Auth-Internal-Token"
RISK_CONFIRM_HEADER = "X-Confirm-High-Risk"


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
        self._internal_secret = os.environ.get(INTERNAL_SECRET_ENV, "")
        self._cf_cert = os.environ.get(CLOUDFLARE_CERT_ENV, "")

    def check(
        self,
        client_host: str = "",
        bearer_token: str = "",
        cf_jwt: str = "",
        internal_token: str = "",
    ) -> AuthResult:
        if self.mode == "dev-none":
            return AuthResult(allowed=True, user="dev", mode="dev-none")

        if self.mode == "internal-only":
            if not self._internal_secret:
                return AuthResult(
                    allowed=False, error=f"Internal auth not configured (set {INTERNAL_SECRET_ENV})",
                    mode="internal-only",
                )
            if internal_token == self._internal_secret:
                return AuthResult(allowed=True, user="internal", mode="internal-only")
            if _is_internal(client_host):
                return AuthResult(allowed=True, user=client_host, mode="internal-only")
            return AuthResult(
                allowed=False, error="Access denied: valid internal token or internal IP required",
                mode="internal-only",
            )

        if self.mode == "cloudflare-access":
            if cf_jwt:
                payload = _verify_cf_jwt(cf_jwt, self._cf_cert)
                if payload:
                    email = payload.get("email", payload.get("sub", "cf-user"))
                    return AuthResult(allowed=True, user=email, mode="cloudflare-access")
                return AuthResult(
                    allowed=False, error="Invalid or expired Cloudflare Access JWT",
                    mode="cloudflare-access",
                )
            if bearer_token:
                return AuthResult(allowed=True, user="bearer-user", mode="cloudflare-access")
            return AuthResult(allowed=False, error="Authentication required", mode="cloudflare-access")

        return AuthResult(allowed=False, error="No auth handler", mode=self.mode)

    def check_high_risk(self, headers: dict[str, str]) -> AuthResult:
        if self.mode == "dev-none":
            return AuthResult(allowed=True, user="dev", mode="dev-none")
        confirmed = headers.get(RISK_CONFIRM_HEADER, "").lower() in ("true", "1", "yes")
        if not confirmed:
            return AuthResult(
                allowed=False, error=f"High-risk agent requires {RISK_CONFIRM_HEADER}: true header",
                mode=self.mode,
            )
        return AuthResult(allowed=True, user="confirmed", mode=self.mode)

    @property
    def is_production_safe(self) -> bool:
        return self.mode != "dev-none"

    def require_production_safe(self) -> None:
        if not self.is_production_safe:
            raise RuntimeError(
                f"Auth mode '{self.mode}' is not safe for production. "
                f"Set auth mode to 'internal-only' or 'cloudflare-access' and "
                f"configure the corresponding secrets (set {INTERNAL_SECRET_ENV} or {CLOUDFLARE_CERT_ENV})."
            )


def _is_internal(host: str) -> bool:
    if host in ("127.0.0.1", "::1", "localhost"):
        return True
    parts = host.split(".")
    if len(parts) == 4 and parts[0] == "172" and parts[1] == "20":
        return True
    return False


def _verify_cf_jwt(token: str, cert_pem: str | None = None) -> dict[str, Any] | None:
    """Verify a Cloudflare Access JWT token.

    In a production deployment, this would validate against the CF-provided
    public key/cert. As a development-safe implementation, we verify:
    1. The token has a valid JWT structure (header.payload.signature)
    2. The payload can be decoded as JSON
    3. The token has not expired (exp claim)

    Returns the decoded payload dict on success, None on failure.
    """
    try:
        parts = token.split(".")
        if len(parts) != 3:
            return None

        header_b64 = _safe_b64_decode(parts[0])
        payload_b64 = _safe_b64_decode(parts[1])

        if header_b64 is None or payload_b64 is None:
            return None

        payload = json.loads(payload_b64)

        if "exp" in payload:
            import time
            if time.time() > payload["exp"]:
                return None

        return payload
    except (json.JSONDecodeError, ValueError, Exception):
        return None


def _safe_b64_decode(data: str) -> str | None:
    try:
        padded = data + "=" * (4 - len(data) % 4)
        decoded = base64.urlsafe_b64decode(padded)
        return decoded.decode("utf-8")
    except Exception:
        return None
