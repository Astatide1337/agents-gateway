"""Tests for auth modes.

Replaces the previous test_auth.py suite which relied on a fake JWT
verifier that did NOT actually verify signatures. We now generate a
real RSA keypair, present a stubbed PyJWKClient that returns our test
public key, and assert that ONLY signature-verified JWTs with the
correct audience and issuer are accepted.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path
from typing import Any
from unittest.mock import patch

import jwt
import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPrivateKey

from agents_gateway.auth import (
    CF_JWT_HEADER,
    INTERNAL_AUTH_HEADER,
    INTERNAL_SECRET_ENV,
    AuthError,
    AuthHandler,
    _is_private_ip,
    _verify_cf_jwt,
)
from agents_gateway.config import AuthConfig


# ---------------------------------------------------------------------------
# RSA keypair fixture for "fake JWKS server"
# ---------------------------------------------------------------------------


@pytest.fixture(scope="session")
def rsa_keypair() -> tuple[RSAPrivateKey, bytes]:
    """Generate one RSA keypair per test session and return (priv, pub_pem)."""
    priv = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pub_pem = priv.public_key().public_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PublicFormat.SubjectPublicKeyInfo,
    )
    return priv, pub_pem


class _StubSigningKey:
    """Mimics the object returned by PyJWKClient.get_signing_key_from_jwt()."""

    def __init__(self, pub_pem: bytes) -> None:
        self.key = pub_pem


@pytest.fixture
def stub_jwks_client(rsa_keypair):
    """A fake PyJWKClient-like object whose get_signing_key_from_jwt
    returns the public key of the test RSA keypair.

    Used to stand in for the Cloudflare certs endpoint."""
    _, pub_pem = rsa_keypair

    class FakeJWKS:
        def get_signing_key_from_jwt(self, token: str) -> _StubSigningKey:
            return _StubSigningKey(pub_pem)

    return FakeJWKS()


def _make_jwt(priv_key: RSAPrivateKey, aud: str, iss: str,
              exp_offset: int = 3600, extra_claims: dict[str, Any] | None = None,
              alg: str = "RS256", headers: dict[str, Any] | None = None) -> str:
    claims: dict[str, Any] = {
        "sub": "user@example.com",
        "email": "user@example.com",
        "exp": int(time.time()) + exp_offset,
        "iat": int(time.time()),
        "aud": aud,
        "iss": iss,
    }
    if extra_claims:
        claims.update(extra_claims)
    return jwt.encode(claims, priv_key, algorithm=alg, headers=headers)


# ---------------------------------------------------------------------------
# Config fixtures
# ---------------------------------------------------------------------------


@pytest.fixture
def cf_config() -> AuthConfig:
    return AuthConfig(
        mode="cloudflare-access",
        public_base_url="https://agents.test.invalid",
        cloudflare_team_domain="test-team.cloudflareaccess.com",
        cloudflare_aud="test-aud",
    )


@pytest.fixture
def internal_config() -> AuthConfig:
    return AuthConfig(mode="internal-only", internal_secret="s3cr3t")


@pytest.fixture
def dev_none_config() -> AuthConfig:
    return AuthConfig(mode="dev-none")


@pytest.fixture
def patch_internal_secret(monkeypatch):
    monkeypatch.setenv(INTERNAL_SECRET_ENV, "s3cr3t")


# ---------------------------------------------------------------------------
# AuthHandler wiring tests
# ---------------------------------------------------------------------------


class TestAuthHandlerWiring:
    def test_invalid_mode_rejected(self):
        with pytest.raises(ValueError):
            AuthHandler(AuthConfig(mode="totally-fake"))

    def test_cf_missing_team_domain_rejected(self):
        with pytest.raises(AuthError):
            AuthHandler(AuthConfig(
                mode="cloudflare-access",
                cloudflare_aud="aud",
            ))

    def test_cf_missing_aud_rejected(self):
        with pytest.raises(AuthError):
            AuthHandler(AuthConfig(
                mode="cloudflare-access",
                cloudflare_team_domain="team.cloudflareaccess.com",
            ))

    def test_dev_none_allows_all(self, dev_none_config):
        h = AuthHandler(dev_none_config)
        result = h.check(client_host="203.0.113.1")
        assert result.allowed
        assert result.user == "dev"


# ---------------------------------------------------------------------------
# Internal-only mode tests
# ---------------------------------------------------------------------------


class TestInternalOnly:
    def test_no_secret_rejects_everything(self):
        h = AuthHandler(AuthConfig(mode="internal-only", internal_secret=""))
        r = h.check(client_host="8.8.8.8", internal_token="anything")
        assert not r.allowed

    def test_correct_secret_allowed(self, internal_config):
        h = AuthHandler(internal_config)
        r = h.check(client_host="8.8.8.8", internal_token="s3cr3t")
        assert r.allowed, r.error
        assert r.user == "internal"

    def test_wrong_secret_denied(self, internal_config):
        h = AuthHandler(internal_config)
        r = h.check(client_host="8.8.8.8", internal_token="wrong")
        assert not r.allowed

    def test_constant_time_compare(self, internal_config, monkeypatch):
        """We must use secrets.compare_digest so the comparison does not
        leak length info. We assert at least that the correct token of
        different prefix length rejects cleanly."""
        h = AuthHandler(internal_config)
        assert not h.check(client_host="8.8.8.8", internal_token="x").allowed
        assert h.check(client_host="8.8.8.8", internal_token="s3cr3t").allowed

    def test_unsafe_ip_bypass_off_by_default(self, internal_config):
        h = AuthHandler(internal_config)
        # Without allow_unsafe_private_ip_bypass=True, internal IPs alone
        # are not enough.
        assert not h.check(client_host="127.0.0.1").allowed
        assert not h.check(client_host="10.0.0.1").allowed
        assert not h.check(client_host="172.16.0.1").allowed

    def test_unsafe_ip_bypass_opt_in_enabled(self):
        cfg = AuthConfig(
            mode="internal-only",
            internal_secret="s3cr3t",
            allow_unsafe_private_ip_bypass=True,
        )
        h = AuthHandler(cfg)
        assert h.check(client_host="10.0.0.1").allowed


# ---------------------------------------------------------------------------
# Cloudflare Access JWT verification tests
# ---------------------------------------------------------------------------


class TestCloudflareAccessJWT:
    def test_valid_jwt_allowed(self, cf_config, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="test-aud", iss="https://test-team.cloudflareaccess.com")
        h = AuthHandler(cf_config)
        # Inject stub JWKS client (instead of the real Cloudflare endpoint).
        with patch.object(h, "_jwks_client", stub_jwks_client):
            r = h.check(cf_jwt=token)
        assert r.allowed, r.error
        assert r.user == "user@example.com"

    def test_unsigned_jwt_rejected(self, cf_config, stub_jwks_client):
        # Hand-built JWT with alg=none. PyJWT refuses to decode it under
        # algorithms=["RS256"].
        import base64
        header = base64.urlsafe_b64encode(
            json.dumps({"alg": "none", "typ": "JWT"}).encode()
        ).rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": "u@e.com", "exp": int(time.time()) + 3600,
                        "aud": "test-aud", "iss": "https://test-team.cloudflareaccess.com"}).encode()
        ).rstrip(b"=").decode()
        unsigned_token = f"{header}.{payload}."
        h = AuthHandler(cf_config)
        with patch.object(h, "_jwks_client", stub_jwks_client):
            r = h.check(cf_jwt=unsigned_token)
        assert not r.allowed

    def test_expired_jwt_rejected(self, cf_config, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="test-aud", iss="https://test-team.cloudflareaccess.com",
                          exp_offset=-3600)
        h = AuthHandler(cf_config)
        with patch.object(h, "_jwks_client", stub_jwks_client):
            r = h.check(cf_jwt=token)
        assert not r.allowed

    def test_wrong_audience_rejected(self, cf_config, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="different-aud",
                          iss="https://test-team.cloudflareaccess.com")
        h = AuthHandler(cf_config)
        with patch.object(h, "_jwks_client", stub_jwks_client):
            r = h.check(cf_jwt=token)
        assert not r.allowed

    def test_wrong_issuer_rejected(self, cf_config, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="test-aud", iss="https://evil-team.cloudflareaccess.com")
        h = AuthHandler(cf_config)
        with patch.object(h, "_jwks_client", stub_jwks_client):
            r = h.check(cf_jwt=token)
        assert not r.allowed

    def test_malformed_jwt_rejected(self, cf_config, stub_jwks_client):
        h = AuthHandler(cf_config)
        with patch.object(h, "_jwks_client", stub_jwks_client):
            assert not h.check(cf_jwt="not.a.jwt").allowed
            assert not h.check(cf_jwt="xx").allowed
            assert not h.check(cf_jwt="").allowed

    def test_random_bearer_token_rejected(self, cf_config, stub_jwks_client):
        """The previous implementation accepted ANY bearer string in
        cloudflare-access mode. That bug is now closed."""
        h = AuthHandler(cf_config)
        # We don't pass cf_jwt; simulate a Bearer "Authorization" header by
        # attempting to authorise with a random-looking token and proving the
        # AuthHandler returns allowed=False. The new AuthHandler.check
        # signature takes bearer_token but ignores it for security.
        r = h.check(bearer_token="abc123randomtoken", cf_jwt="")
        assert not r.allowed

    def test_no_auth_header_rejected(self, cf_config):
        h = AuthHandler(cf_config)
        r = h.check()
        assert not r.allowed
        assert "Authentication required" in r.error


# ---------------------------------------------------------------------------
# _verify_cf_jwt unit tests (low-level helper)
# ---------------------------------------------------------------------------


class TestVerifyCfJwtHelper:
    def test_returns_claims_for_valid_token(self, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="aud1", iss="https://team.cloudflareaccess.com")
        claims = _verify_cf_jwt(
            token,
            jwks_client=stub_jwks_client,
            expected_aud="aud1",
            expected_issuer="https://team.cloudflareaccess.com",
        )
        assert claims is not None
        assert claims["sub"] == "user@example.com"

    def test_returns_none_for_no_jwks_client(self, rsa_keypair):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="aud1", iss="https://team.cloudflareaccess.com")
        claims = _verify_cf_jwt(token, jwks_client=None,
                               expected_aud="aud1",
                               expected_issuer="https://team.cloudflareaccess.com")
        assert claims is None

    def test_returns_none_for_wrong_aud(self, rsa_keypair, stub_jwks_client):
        priv, _ = rsa_keypair
        token = _make_jwt(priv, aud="right", iss="https://team.cloudflareaccess.com")
        claims = _verify_cf_jwt(token, jwks_client=stub_jwks_client,
                               expected_aud="wrong",
                               expected_issuer="https://team.cloudflareaccess.com")
        assert claims is None


# ---------------------------------------------------------------------------
# Production boot assertions
# ---------------------------------------------------------------------------


class TestProductionBootAssertion:
    def test_dev_none_refused_in_production(self):
        h = AuthHandler(AuthConfig(mode="dev-none"))
        with pytest.raises(RuntimeError):
            h.require_production_safe()

    def test_internal_only_without_secret_refused(self):
        h = AuthHandler(AuthConfig(mode="internal-only", internal_secret=""))
        with pytest.raises(RuntimeError):
            h.require_production_safe()

    def test_internal_only_with_secret_ok(self):
        h = AuthHandler(AuthConfig(mode="internal-only", internal_secret="s3cr3t"))
        h.require_production_safe()  # must not raise

    def test_cloudflare_access_without_team_refused(self):
        with pytest.raises(AuthError):
            AuthHandler(AuthConfig(mode="cloudflare-access",
                                    cloudflare_aud="x"))
        # Also test via require_production_safe: if someone bypassed the
        # constructor (e.g. through unconfigured env), the boot guard still
        # catches them.
        h = AuthHandler(AuthConfig(mode="cloudflare-access",
                                    cloudflare_team_domain="team.cloudflareaccess.com",
                                    cloudflare_aud="x"))
        # Pretend the team_domain got cleared post-construction:
        h._cfg.cloudflare_team_domain = ""
        with pytest.raises(RuntimeError):
            h.require_production_safe()

    def test_cloudflare_access_without_aud_refused(self):
        with pytest.raises(AuthError):
            AuthHandler(AuthConfig(mode="cloudflare-access",
                                    cloudflare_team_domain="team.cloudflareaccess.com"))
        h = AuthHandler(AuthConfig(mode="cloudflare-access",
                                    cloudflare_team_domain="team.cloudflareaccess.com",
                                    cloudflare_aud="x"))
        h._cfg.cloudflare_aud = ""
        with pytest.raises(RuntimeError):
            h.require_production_safe()

    def test_cloudflare_access_complete_ok(self, cf_config, rsa_keypair):
        h = AuthHandler(cf_config)
        h.require_production_safe()  # must not raise


# ---------------------------------------------------------------------------
# _is_private_ip helper tests
# ---------------------------------------------------------------------------


class TestIsPrivateIp:
    def test_localhost(self):
        assert _is_private_ip("127.0.0.1")
        assert _is_private_ip("::1")
        assert _is_private_ip("localhost")

    def test_rfc1918_10(self):
        assert _is_private_ip("10.0.0.1")
        assert _is_private_ip("10.255.255.255")

    def test_rfc1918_172(self):
        assert _is_private_ip("172.16.0.1")
        assert _is_private_ip("172.31.255.254")
        assert not _is_private_ip("172.15.0.1")
        assert not _is_private_ip("172.32.0.1")

    def test_rfc1918_192_168(self):
        assert _is_private_ip("192.168.1.1")

    def test_public_rejected(self):
        assert not _is_private_ip("8.8.8.8")
        assert not _is_private_ip("203.0.113.1")

    def test_empty(self):
        assert not _is_private_ip("")
        assert not _is_private_ip("not-an-ip")
