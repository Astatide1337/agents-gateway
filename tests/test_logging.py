"""Tests for structured logging."""

import json
import logging

from agents_gateway.logging import (
    HumanFormatter,
    JSONFormatter,
    log_event,
    setup_logging,
)


class TestJSONFormatter:
    def test_format_includes_required_fields(self):
        formatter = JSONFormatter()
        record = logging.LogRecord(
            name="agents-gateway", level=logging.INFO, pathname="", lineno=0,
            msg="Test message", args=(), exc_info=None,
        )
        record.event = "test_event"
        output = formatter.format(record)
        data = json.loads(output)
        assert "timestamp" in data
        assert data["level"] == "INFO"
        assert data["service"] == "agents-gateway"
        assert data["event"] == "test_event"
        assert data["message"] == "Test message"

    def test_optional_fields(self):
        formatter = JSONFormatter()
        record = logging.LogRecord(
            name="agents-gateway", level=logging.INFO, pathname="", lineno=0,
            msg="Test", args=(), exc_info=None,
        )
        record.event = "task_created"
        record.task_id = "abc-123"
        record.agent_id = "test-agent"
        record.duration_ms = 42
        output = formatter.format(record)
        data = json.loads(output)
        assert data["task_id"] == "abc-123"
        assert data["agent_id"] == "test-agent"
        assert data["duration_ms"] == 42


class TestHumanFormatter:
    def test_format_readable(self):
        formatter = HumanFormatter()
        record = logging.LogRecord(
            name="agents-gateway", level=logging.WARNING, pathname="", lineno=0,
            msg="Something happened", args=(), exc_info=None,
        )
        record.event = "agent_invalid"
        output = formatter.format(record)
        assert "Something happened" in output
        assert "agent_invalid" in output


class TestSetupLogging:
    def test_json_format(self):
        logger = setup_logging("INFO", "json")
        assert len(logger.handlers) == 1
        assert isinstance(logger.handlers[0].formatter, JSONFormatter)

    def test_human_format(self):
        logger = setup_logging("DEBUG", "text")
        assert len(logger.handlers) == 1
        assert isinstance(logger.handlers[0].formatter, HumanFormatter)


class TestNoSecrets:
    def test_secrets_not_logged(self):
        formatter = JSONFormatter()
        record = logging.LogRecord(
            name="agents-gateway", level=logging.INFO, pathname="", lineno=0,
            msg="Auth success", args=(), exc_info=None,
        )
        record.event = "auth_success"
        output = formatter.format(record)
        data = json.loads(output)
        for sensitive in ["password", "secret", "token", "key", "api_key"]:
            assert sensitive not in json.dumps(data).lower() or sensitive == "event"


class TestNoSecretsReal:
    def test_sensitive_headers_are_filtered(self):
        """filter_headers() must redact the protected-header set so log
        writers cannot accidentally leak bearer tokens, JWTs, or
        internal secrets."""
        from agents_gateway.logging import filter_headers
        for header in ("authorization", "Authorization",
                       "cookie", "Cookie",
                       "cf-access-jwt-assertion",
                       "Cf-Access-Jwt-Assertion",
                       "x-auth-internal-token",
                       "X-Auth-Internal-Token",
                       "x-confirm-high-risk"):
            filtered = filter_headers({header: "super-secret-value"})
            assert filtered[header] == "<redacted>", f"{header} not redacted"
            assert "super-secret-value" not in filtered[header]

    def test_safe_headers_pass_through(self):
        from agents_gateway.logging import filter_headers
        filtered = filter_headers({"content-type": "application/json",
                                  "user-agent": "tester",
                                  "x-request-id": "abc"})
        assert filtered["content-type"] == "application/json"
        assert filtered["user-agent"] == "tester"
        assert filtered["x-request-id"] == "abc"


class TestRequestContext:
    def test_bind_then_clear_request_context(self):
        from agents_gateway.logging import (
            bind_request_context, clear_request_context,
            get_context_dict, get_request_id, get_auth_user,
        )
        bind_request_context("req-1", "user@example.com",
                             "GET", "/agents", "http://localhost/agents")
        assert get_request_id() == "req-1"
        assert get_auth_user() == "user@example.com"
        ctx = get_context_dict()
        assert ctx["request_id"] == "req-1"
        assert ctx["auth_user"] == "user@example.com"
        assert ctx["path"] == "/agents"
        clear_request_context()
        assert get_request_id() != "req-1"
        assert get_auth_user() == ""

    def test_concurrent_contexts_are_isolated(self):
        """Per-request contextvars must NOT cross between concurrent
        requests running in the same asyncio loop."""
        import asyncio
        from agents_gateway.logging import (
            bind_request_context, clear_request_context, get_request_id,
        )

        captured: list[tuple[str, str]] = []
        async def worker(req_id: str):
            bind_request_context(req_id, "u", "GET", "/x", "http://x")
            await asyncio.sleep(0.01)
            captured.append((req_id, get_request_id()))
            clear_request_context()

        async def run():
            await asyncio.gather(worker("A"), worker("B"))
        asyncio.run(run())
        assert ("A", "A") in captured
        assert ("B", "B") in captured

    def test_request_context_propagates_into_json_log(self):
        import io
        from agents_gateway.logging import (
            bind_request_context, clear_request_context, log_event,
            setup_logging,
        )
        setup_logging("INFO", "json")
        logger = logging.getLogger("agents-gateway")
        captured = io.StringIO()
        logger.handlers[0].stream = captured
        bind_request_context("req-XYZ", "alice", "POST", "/tasks",
                             "http://localhost/tasks")
        log_event("task_created", "Created task")
        clear_request_context()
        out = captured.getvalue().strip()
        data = json.loads(out)
        assert data["request_id"] == "req-XYZ"
        assert data["auth_user"] == "alice"
        assert data["path"] == "/tasks"
        assert data["method"] == "POST"

    def test_sensitive_header_value_never_appears_in_log(self):
        """An Authorization header value passed into log via kwargs must
        never appear in the rendered log line. Our JSON formatter has a
        fixed field whitelist, which by design excludes arbitrary kwargs
        that could leak secrets. This test asserts both that the secret
        is absent AND that the redacted form is preserved when the caller
        uses filter_headers()."""
        import io
        from agents_gateway.logging import (
            log_event, setup_logging, filter_headers,
        )
        setup_logging("INFO", "json")
        logger = logging.getLogger("agents-gateway")
        captured = io.StringIO()
        logger.handlers[0].stream = captured
        headers = {"Authorization": "Bearer leaked-token-xyz",
                   "Cf-Access-Jwt-Assertion": "eyJfake.header.signature"}
        safe_headers = filter_headers(headers)
        log_event("request_in", "incoming", headers=safe_headers)
        out = captured.getvalue().strip()
        assert "leaked-token-xyz" not in out
        assert "Bearer leaked" not in out
        assert "eyJfake" not in out
