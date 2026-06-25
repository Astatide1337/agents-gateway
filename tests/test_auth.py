"""Tests for auth modes."""

import pytest

from agents_gateway.config import AuthConfig
from agents_gateway.auth import AuthHandler, AuthResult, _is_internal


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


class TestDevNone:
    def test_allows_all(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        result = handler.check()
        assert result.allowed is True

    def test_not_production_safe(self):
        handler = AuthHandler(AuthConfig(mode="dev-none"))
        assert handler.is_production_safe is False


class TestInternalOnly:
    def test_internal_allowed(self):
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(client_host="127.0.0.1")
        assert result.allowed is True

    def test_external_denied(self):
        handler = AuthHandler(AuthConfig(mode="internal-only"))
        result = handler.check(client_host="203.0.113.1")
        assert result.allowed is False


class TestCloudflareAccess:
    def test_bearer_allowed(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        result = handler.check(bearer_token="test-token")
        assert result.allowed is True

    def test_cf_jwt_allowed(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        result = handler.check(cf_jwt="test-jwt")
        assert result.allowed is True

    def test_no_auth_denied(self):
        handler = AuthHandler(AuthConfig(mode="cloudflare-access"))
        result = handler.check(client_host="1.2.3.4")
        assert result.allowed is False


class TestInvalidMode:
    def test_invalid_mode_raises(self):
        with pytest.raises(ValueError, match="Invalid auth mode"):
            AuthHandler(AuthConfig(mode="invalid-mode"))
