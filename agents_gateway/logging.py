"""Structured logging for Agents Gateway.

Request-scoped fields (request_id, auth_user, path, method) are propagated
through `contextvars`, NOT through module globals or environment variables.
This means concurrent requests inside the same asyncio event loop / thread
pool see only their own request context, never a sibling's.
"""

from __future__ import annotations

import contextvars
import json
import logging
import uuid
from datetime import datetime, timezone
from typing import Any

_service_name = "agents-gateway"

# Module-level fallback environment (set once at startup). NOT used for
# request_id propagation.
import os
_environment = os.environ.get("AGW_ENV", "dev")

# ContextVars holding per-request metadata.
_request_id_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "request_id", default=None
)
_auth_user_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "auth_user", default=None
)
_method_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "method", default=None
)
_path_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "path", default=None
)
_url_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "url", default=None
)


def bind_request_context(
    request_id: str, auth_user: str, method: str, path: str, url: str
) -> None:
    """Bind per-request metadata into the current context."""
    _request_id_var.set(request_id)
    _auth_user_var.set(auth_user)
    _method_var.set(method)
    _path_var.set(path)
    _url_var.set(url)


def clear_request_context() -> None:
    """Reset per-request metadata (call at the end of a request)."""
    _request_id_var.set(None)
    _auth_user_var.set(None)
    _method_var.set(None)
    _path_var.set(None)
    _url_var.set(None)


def get_request_id() -> str:
    rid = _request_id_var.get()
    return rid or uuid.uuid4().hex


def get_auth_user() -> str:
    return _auth_user_var.get() or ""


def get_context_dict() -> dict[str, str]:
    """Return a dict of currently bound context keys (for logging)."""
    return {
        key: value
        for key, value in (
            ("request_id", _request_id_var.get()),
            ("auth_user", _auth_user_var.get()),
            ("method", _method_var.get()),
            ("path", _path_var.get()),
        )
        if value
    }


# Set of header names that must NEVER be logged. Use when adding header
# values to structured logs.
SENSITIVE_HEADERS = frozenset({
    "authorization", "cookie", "cf-access-jwt-assertion",
    "x-auth-internal-token", "x-confirm-high-risk",
})


def filter_headers(headers: dict[str, str]) -> dict[str, str]:
    """Return a redacted copy of `headers` safe to log."""
    return {k: ("<redacted>" if k.lower() in SENSITIVE_HEADERS else v)
            for k, v in headers.items()}


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
        # Pull context vars (per-request fields).
        ctx = get_context_dict()
        log_entry.update(ctx)
        # Pull LogRecord extra fields (task_id, agent_id, duration_ms, etc.).
        for field in ("duration_ms", "task_id", "agent_id", "runtime_type",
                      "error", "status_code"):
            value = getattr(record, field, None)
            if value is not None:
                log_entry[field] = value
        return json.dumps(log_entry)


class HumanFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S")
        event = getattr(record, "event", "")
        rid = _request_id_var.get()
        parts = [f"{ts} [{record.levelname}] {record.getMessage()}"]
        if event:
            parts.append(f"event={event}")
        if rid:
            parts.append(f"req={rid}")
        user = _auth_user_var.get()
        if user:
            parts.append(f"user={user}")
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
