"""Tests for auth modes."""

import json
import time

import pytest

from agents_gateway.config import AuthConfig
from agents_gateway.auth import (
    INTERNAL_AUTH_HEADER,
    INTERNAL_SECRET_ENV,
    CLOUDFLARE_CERT_ENV,
    AuthHandler,
    AuthResult,
    _is_internal,
    _verify_cf_jwt,
    _safe_b64_decode,
)


class TestInternalCheck:
    def test_localhost(self):
        assert _is_internal("127.0.0.1") is True

    def test_ipv6_localhost(self):
        assert _is_internal("::1") is True

    def test_localhost_name(self):
        assert _is_internal("localhost") is True

    def test_docker_internal(self):
        assert _is_internal("172.20.0.5") is True

    def test_external(self):
        assert _is_internal("10.0.0.1") is False

    def test_public_ip(self):
        assert _is_internal("203.0.113.1") is False


class TestSafeB64Decode:
    def test_valid_b64(self):
        result = _safe_b64_decode("eyJmb28iOiAiYmFyIn0")  # {"foo": "bar"}
        assert result is not None
        assert "foo" in result

    def test_invalid_b64_returns_something(self):
        result = _safe_b64_decode("???")
        assert isinstance(result, str)

    def test_empty_string(self):
        result = _safe_b64_decode("")
        assert result is not None
        assert result == ""


class TestVerifyCfJwt:
    def test_valid_jwt_structure(self):
        import base64
        header = base64.urlsafe_b64encode(json.dumps({"alg": "RS256"}).encode()).rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": "user@example.com", "exp": time.time() + 3600, "email": "user@example.com"}).encode()
        ).rstrip(b"=").decode()
        token = f"{header}.{payload}.signature"
        result = _verify_cf_jwt(token)
        assert result is not None
        assert result["email"] == "user@example.com"

    def test_expired_jwt(self):
        import base64
        header = base64.urlsafe_b64encode(json.dumps({"alg": "RS256"}).encode()).rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": "user@example.com", "exp": time.time() - 3600}).encode()
        ).rstrip(b"=").decode()
        token = f"{header}.{payload}.signature"
        result = _verify_cf_jwt(token)
        assert result is None

    def test_malformed_jwt(self):
        assert _verify_cf_jwt("not-a-jwt") is None

    def test_empty_token(self):
        assert _verify_cf_jwt("") is None

    def test_invalid_json_payload(self):
        token = "header.invalid-payload.signature"
        result = _verify_cf_jwt(token)
        assert result is None


class TestDevNone:
    def test_allows_all(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        result = handler.check()
        assert result.allowed is True

    def test_not_production_safe(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        assert handler.is_production_safe is False

    def test_require_production_safe_raises(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        with pytest.raises(RuntimeError, match="not safe for production"):
            handler.require_production_safe()

    def test_high_risk_allowed_in_dev(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        result = handler.check_high_risk({})
        assert result.allowed is True


class TestInternalOnly:
    def test_internal_ip_allowed(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(client_host="127.0.0.1")
        assert result.allowed is True

    def test_correct_token_allowed(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(internal_token="test-secret")
        assert result.allowed is True

    def test_wrong_token_denied(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(internal_token="wrong-secret")
        assert result.allowed is False

    def test_external_denied(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(client_host="203.0.113.1")
        assert result.allowed is False

    def test_no_secret_configured_denied(self):
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(internal_token="anything")
        assert result.allowed is False
        assert "not configured" in result.error

    def test_production_safe(self):
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        assert handler.is_production_safe is True

    def test_high_risk_missing_header(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check_high_risk({})
        assert result.allowed is False
        assert "X-Confirm-High-Risk" in result.error

    def test_high_risk_confirmed(self, monkeypatch):
        monkeypatch.setenv(INTERNAL_SECRET_ENV, "test-secret")
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check_high_risk({"X-Confirm-High-Risk": "true"})
        assert result.allowed is True


class TestCloudflareAccess:
    def test_bearer_allowed(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        result = handler.check(bearer_token="test-token")
        assert result.allowed is True

    def test_cf_jwt_valid_structure_allowed(self):
        import base64
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        header = base64.urlsafe_b64encode(json.dumps({"alg": "RS256"}).encode()).rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": "user@example.com", "exp": time.time() + 3600}).encode()
        ).rstrip(b"=").decode()
        token = f"{header}.{payload}.sig"
        result = handler.check(cf_jwt=token)
        assert result.allowed is True

    def test_expired_cf_jwt_denied(self):
        import base64
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        header = base64.urlsafe_b64encode(json.dumps({"alg": "RS256"}).encode()).rstrip(b"=").decode()
        payload = base64.urlsafe_b64encode(
            json.dumps({"sub": "user@example.com", "exp": time.time() - 3600}).encode()
        ).rstrip(b"=").decode()
        token = f"{header}.{payload}.sig"
        result = handler.check(cf_jwt=token)
        assert result.allowed is False

    def test_no_auth_denied(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        result = handler.check(client_host="1.2.3.4")
        assert result.allowed is False

    def test_production_safe(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        assert handler.is_production_safe is True


class TestInvalidMode:
    def test_invalid_mode_raises(self):
        with pytest.raises(ValueError, match="Invalid auth mode"):
            AuthHandler(AuthConfig(mode="invalid-mode"))


class TestRequireProductionSafe:
    def test_dev_none_raises(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        with pytest.raises(RuntimeError, match="not safe for production"):
            handler.require_production_safe()

    def test_internal_only_passes(self):
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        handler.require_production_safe()

    def test_cloudflare_passes(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        handler.require_production_safe()