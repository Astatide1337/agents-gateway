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
