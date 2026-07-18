"""Tests for harness.client_mcp (McpGatewayClient).

Verifies:

  * Disabled client returns the expected summary.
  * Enabled client calls /version endpoint for health/version.
  * render_tools_md returns a non-empty markdown string when configured.
  * scrub_url_for_logs hides user tokens.
"""

from __future__ import annotations

import pytest

from agents_gateway.harness.client_mcp import (
    McpGatewayClient,
    McpGatewayConfig,
)


class TestConfig:
    def test_defaults(self):
        c = McpGatewayConfig()
        assert c.enabled is False
        assert c.base_url.startswith("http://") or c.base_url == ""

    def test_with_settings(self):
        c = McpGatewayConfig(
            enabled=True,
            base_url="http://mcp.example",
            auth_mode="internal-only",
            internal_token="secret",
        )
        assert c.enabled is True
        assert c.auth_mode == "internal-only"


class TestToolsSummary:
    def test_disabled_returns_unhealthy(self):
        cfg = McpGatewayConfig(enabled=False)
        client = McpGatewayClient(cfg)
        summary = client.tools_summary()
        assert summary["enabled"] is False
        assert summary["healthy"] is False

    def test_enabled(self, respx_mock):
        cfg = McpGatewayConfig(
            enabled=True, base_url="http://mcp.example",
        )
        client = McpGatewayClient(cfg)
        # Mock the /health endpoint to return OK
        respx_mock.get("http://mcp.example/health").respond(
            json={"status": "ok"},
        )
        # Mock /version
        respx_mock.get("http://mcp.example/version").respond(
            json={"version": "1.2.3"},
        )
        result = client.tools_summary()
        assert result["enabled"] is True
        assert "base_url" in result


class TestRenderToolsMd:
    def test_disabled_renders_default_text(self):
        cfg = McpGatewayConfig(enabled=False)
        client = McpGatewayClient(cfg)
        md = client.render_tools_md()
        assert isinstance(md, str)
        # Should mention MCP gateway somehow (or use default text)
        assert md  # non-empty

    def test_enabled_renders_with_base_url(self):
        cfg = McpGatewayConfig(
            enabled=True, base_url="http://mcp.example",
        )
        client = McpGatewayClient(cfg)
        md = client.render_tools_md()
        assert isinstance(md, str)
        assert len(md) > 0


class TestUrlRedaction:
    def test_scrub_url_token_in_userinfo_removed(self):
        from agents_gateway.harness.client_mcp import scrub_url_for_logs
        src = "https://user:supersecret@host.example/path?q=1"
        cleaned = scrub_url_for_logs(src)
        assert "supersecret" not in cleaned
        # The host should still be present.
        assert "host.example" in cleaned

    def test_scrub_url_without_credentials_passthrough(self):
        from agents_gateway.harness.client_mcp import scrub_url_for_logs
        src = "https://example.com/path"
        cleaned = scrub_url_for_logs(src)
        assert "example.com" in cleaned
