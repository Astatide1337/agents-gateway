"""Structured logging for Agents Gateway."""

from __future__ import annotations

import json
import logging
import os
import uuid
from datetime import datetime, timezone
from typing import Any

_service_name = "agents-gateway"
_environment = os.environ.get("AGW_ENV", "dev")
_request_id: str | None = None


def set_request_id(req_id: str | None) -> None:
    global _request_id
    _request_id = req_id


def get_request_id() -> str:
    return _request_id or uuid.uuid4().hex


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        event = getattr(record, "event", record.getMessage())
        log_entry: dict[str, Any] = {
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "level": record.levelname,
            "service": _service_name,
            "environment": _environment,
            "event": event,
            "message": record.getMessage(),
        }
        if _request_id:
            log_entry["request_id"] = _request_id
        for field in ("duration_ms", "task_id", "agent_id", "error"):
            value = getattr(record, field, None)
            if value is not None:
                log_entry[field] = value
        return json.dumps(log_entry)


class HumanFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
        event = getattr(record, "event", "")
        parts = [f"{ts} [{record.levelname}] {record.getMessage()}"]
        if event:
            parts.append(f"event={event}")
        task_id = getattr(record, "task_id", None)
        if task_id:
            parts.append(f"task={task_id}")
        agent_id = getattr(record, "agent_id", None)
        if agent_id:
            parts.append(f"agent={agent_id}")
        return " ".join(parts)


def setup_logging(log_level: str = "INFO", log_format: str = "json") -> logging.Logger:
    logger = logging.getLogger("agents-gateway")
    logger.setLevel(getattr(logging, log_level.upper(), logging.INFO))
    handler = logging.StreamHandler()
    if log_format == "json":
        handler.setFormatter(JSONFormatter())
    else:
        handler.setFormatter(HumanFormatter())
    logger.handlers.clear()
    logger.addHandler(handler)
    return logger


def log_event(
    event: str,
    message: str,
    level: str = "INFO",
    **kwargs: Any,
) -> None:
    logger = logging.getLogger("agents-gateway")
    log_level = getattr(logging, level.upper(), logging.INFO)
    record = logging.LogRecord(
        name="agents-gateway", level=log_level, pathname="", lineno=0,
        msg=message, args=(), exc_info=None,
    )
    record.event = event
    for k, v in kwargs.items():
        setattr(record, k, v)
    logger.handle(record)
