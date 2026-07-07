"""Auth mode handling for Agents Gateway.

Modes:
  - dev-none: No authentication (development only; refused in production)
  - internal-only: Shared-secret header based auth (X-Auth-Internal-Token)
  - cloudflare-access: Real Cloudflare Access JWT verification (RS256, JWKS,
    audience, issuer, expiration, signature).

The previous implementation accepted ANY bearer token in cloudflare-access
mode and only base64-decoded JWTs without signature verification. That is
not safe. This module replaces it with real PyJWT-based verification.
"""

from __future__ import annotations

import os
import secrets
import time
from typing import Any

import jwt
from jwt import PyJWKClient, PyJWTError

from agents_gateway.config import AuthConfig

VALID_MODES = {"dev-none", "cloudflare-access", "internal-only"}

CLOUDFLARE_CERT_ENV = "AGW_CLOUDFLARE_CERT"  # legacy, unused; kept for back-compat
INTERNAL_SECRET_ENV = "AGW_INTERNAL_SECRET"

CF_JWT_HEADER = "Cf-Access-Jwt-Assertion"
INTERNAL_AUTH_HEADER = "X-Auth-Internal-Token"
RISK_CONFIRM_HEADER = "X-Confirm-High-Risk"


class AuthResult:
    def __init__(self, allowed: bool, user: str = "", mode: str = "", error: str = "") -> None:
        self.allowed = allowed
        self.user = user
        self.mode = mode
        self.error = error


class AuthError(Exception):
    """Raised when the auth handler itself is misconfigured."""


class AuthHandler:
    def __init__(self, config: AuthConfig) -> None:
        if config.mode not in VALID_MODES:
            raise ValueError(
                f"Invalid auth mode: {config.mode!r}. Valid: {sorted(VALID_MODES)}"
            )
        self.mode = config.mode
        self._cfg = config
        # Internal secret can come from env or config field (config has priority).
        self._internal_secret = config.internal_secret or os.environ.get(INTERNAL_SECRET_ENV, "")
        # JWKS client for Cloudflare Access.
        if config.mode == "cloudflare-access":
            if not config.cloudflare_team_domain:
                raise AuthError(
                    "auth.mode=cloudflare-access requires auth.cloudflare_team_domain "
                    f"(env AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN)"
                )
            if not config.cloudflare_aud:
                raise AuthError(
                    "auth.mode=cloudflare-access requires auth.cloudflare_aud "
                    f"(env AGW_AUTH__CLOUDFLARE_AUD)"
                )
            team = config.cloudflare_team_domain.strip().rstrip("/")
            self._jwks_client: PyJWKClient | None = PyJWKClient(
                f"https://{team}/cdn-cgi/access/certs"
            )
            self._cf_issuer = f"https://{team}"
            self._cf_aud = config.cloudflare_aud
        else:
            self._jwks_client = None
            self._cf_issuer = ""
            self._cf_aud = ""

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
            return self._check_internal(client_host, internal_token)

        if self.mode == "cloudflare-access":
            return self._check_cloudflare(cf_jwt, internal_token)

        return AuthResult(allowed=False, error="No auth handler", mode=self.mode)

    def _check_internal(self, client_host: str, internal_token: str) -> AuthResult:
        if not self._internal_secret:
            return AuthResult(
                allowed=False,
                error=(
                    f"Internal auth not configured (set {INTERNAL_SECRET_ENV} or "
                    "auth.internal_secret)"
                ),
                mode="internal-only",
            )
        # Constant-time comparison of the shared secret.
        if internal_token and secrets.compare_digest(internal_token, self._internal_secret):
            return AuthResult(allowed=True, user="internal", mode="internal-only")
        # Optional explicit unsafe private-IP bypass. Off by default; operators
        # must opt in via auth.allow_unsafe_private_ip_bypass=true and understand
        # that anyone on the same RFC1918 network can call protected routes.
        if self._cfg.allow_unsafe_private_ip_bypass and _is_private_ip(client_host):
            return AuthResult(allowed=True, user=client_host, mode="internal-only")
        return AuthResult(
            allowed=False,
            error="Access denied: valid X-Auth-Internal-Token required",
            mode="internal-only",
        )

    def _check_cloudflare(self, cf_jwt_token: str, internal_token: str) -> AuthResult:
        # Allow internal service-to-service calls (worker -> gateway) when an
        # explicit shared secret is configured. This is the only way the
        # background task worker should authenticate.
        if internal_token and self._internal_secret and secrets.compare_digest(
            internal_token, self._internal_secret
        ):
            return AuthResult(allowed=True, user="internal", mode="cloudflare-access")
        if not cf_jwt_token:
            return AuthResult(
                allowed=False,
                error=f"Authentication required (provide {CF_JWT_HEADER} header)",
                mode="cloudflare-access",
            )
        # NOTE: We deliberately do NOT accept arbitrary bearer tokens. The
        # previous implementation returned allowed=True for any non-empty
        # bearer_token string here --- that was an auth-bypass footgun and is
        # now removed.
        payload = _verify_cf_jwt(
            cf_jwt_token,
            self._jwks_client,
            self._cf_aud,
            self._cf_issuer,
            self._cfg.jwt_leeway_seconds,
        )
        if payload is None:
            return AuthResult(
                allowed=False,
                error="Invalid or expired Cloudflare Access JWT",
                mode="cloudflare-access",
            )
        email = payload.get("email") or payload.get("sub") or "cf-user"
        return AuthResult(allowed=True, user=email, mode="cloudflare-access")

    def check_high_risk(self, headers: dict[str, str]) -> AuthResult:
        if self.mode == "dev-none":
            return AuthResult(allowed=True, user="dev", mode="dev-none")
        confirmed = headers.get(RISK_CONFIRM_HEADER, "").lower() in ("true", "1", "yes")
        if not confirmed:
            return AuthResult(
                allowed=False,
                error=f"High-risk agent requires {RISK_CONFIRM_HEADER}: true header",
                mode=self.mode,
            )
        return AuthResult(allowed=True, user="confirmed", mode=self.mode)

    @property
    def is_production_safe(self) -> bool:
        return self.mode != "dev-none"

    def require_production_safe(self) -> None:
        """Boot-time guard. Raises if auth config cannot run in production."""
        if not self.is_production_safe:
            raise RuntimeError(
                f"Auth mode '{self.mode}' is not safe for production. "
                f"Set auth.mode to 'internal-only' or 'cloudflare-access' and "
                f"configure the corresponding secret "
                f"({INTERNAL_SECRET_ENV} or AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN + "
                f"AGW_AUTH__CLOUDFLARE_AUD)."
            )
        if self.mode == "internal-only" and not self._internal_secret:
            raise RuntimeError(
                f"auth.mode=internal-only requires {INTERNAL_SECRET_ENV} or "
                "auth.internal_secret to be configured in production."
            )
        if self.mode == "cloudflare-access" and (
            not self._cfg.cloudflare_team_domain or not self._cfg.cloudflare_aud
        ):
            raise RuntimeError(
                "auth.mode=cloudflare-access requires AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN "
                "and AGW_AUTH__CLOUDFLARE_AUD to be configured in production."
            )


def _is_private_ip(host: str) -> bool:
    """Conservative private-IP check. Used ONLY when an operator explicitly
    enables auth.allow_unsafe_private_ip_bypass. We do NOT trust private IPs
    by default because anyone on the same RFC1918 network (containers, LAN,
    VPN) would bypass auth."""
    if not host:
        return False
    if host in ("127.0.0.1", "::1", "localhost"):
        return True
    parts = host.split(".")
    if len(parts) == 4:
        try:
            a, b = int(parts[0]), int(parts[1])
        except ValueError:
            return False
        # 10.0.0.0/8
        if a == 10:
            return True
        # 172.16.0.0/12
        if a == 172 and 16 <= b <= 31:
            return True
        # 192.168.0.0/16
        if a == 192 and b == 168:
            return True
    return False


def _verify_cf_jwt(
    token: str,
    jwks_client: PyJWKClient | None,
    expected_aud: str,
    expected_issuer: str,
    leeway_seconds: int = 30,
) -> dict[str, Any] | None:
    """Verify a Cloudflare Access JWT.

    Required validation:
      * Signature is verified using the JWKS fetched from
        https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
      * Algorithm MUST be RS256 (Cloudflare Access only ever issues RS256;
        this also rejects alg=none).
      * Audience MUST match expected_aud.
      * Issuer MUST match expected_issuer.
      * exp is enforced by PyJWT (with leeway_seconds slack). We also
        require the exp claim to be present.
      * sub or email must be present (we use it as the user identifier).

    Returns the verified claims dict on success, None on any failure.
    """
    if jwks_client is None:
        return None
    if not token or token.count(".") != 2:
        return None
    try:
        signing_key = jwks_client.get_signing_key_from_jwt(token)
        claims = jwt.decode(
            token,
            signing_key.key,
            algorithms=["RS256"],
            audience=expected_aud,
            issuer=expected_issuer,
            leeway=leeway_seconds,
            options={"require": ["exp", "iss", "aud"]},
        )
    except PyJWTError:
        return None
    except Exception:
        # Defensive: any unexpected error in JWKS fetch / decode path is an
        # auth failure, not a 500.
        return None
    if not isinstance(claims, dict):
        return None
    if "sub" not in claims and "email" not in claims:
        return None
    return claims
