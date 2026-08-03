"""Anthropic Messages API <-> OpenAI chat-completions translator.

Lets a harness whose CLI only speaks Anthropic's wire format run
against any OpenAI-compatible provider already configured via the
generic API_KEY/API_URL list (provider_config.parse_provider_list) —
no per-provider special-casing. The client sends "<provider-id>/<model>"
as the model field, same convention opencode already uses; provider-id
resolves to a base_url + key from that same list.
"""
from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import time
import uuid
from typing import Callable

import httpx
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, Response, StreamingResponse
from starlette.routing import Route

from agents_gateway.harness.provider_config import parse_provider_list

ResolveFn = Callable[[str], tuple[str, str, str]]


def resolver_from_env(api_keys: str, api_urls: str) -> ResolveFn:
    providers = {p["id"]: p for p in parse_provider_list(api_keys, api_urls)}

    def resolve(model_field: str) -> tuple[str, str, str]:
        if "/" not in model_field:
            raise ValueError(f"model {model_field!r} must be '<provider-id>/<model>'")
        provider_id, upstream_model = model_field.split("/", 1)
        p = providers.get(provider_id)
        if p is None:
            raise ValueError(f"unknown provider {provider_id!r}; configured: {list(providers)}")
        return p["url"], p["key"], upstream_model

    return resolve


_STOP_REASON = {
    "stop": "end_turn",
    "length": "max_tokens",
    "tool_calls": "tool_use",
    "content_filter": "end_turn",
    None: "end_turn",
}


def _anthropic_to_openai(body: dict) -> dict:
    messages = []
    system = body.get("system")
    if system:
        text = system if isinstance(system, str) else "\n".join(
            b.get("text", "") for b in system if isinstance(b, dict))
        if text:
            messages.append({"role": "system", "content": text})

    for m in body.get("messages", []):
        role = m["role"]
        content = m.get("content", "")
        if isinstance(content, str):
            messages.append({"role": role, "content": content})
            continue

        text_parts, tool_calls, tool_results = [], [], []
        for block in content:
            btype = block.get("type")
            if btype == "text":
                text_parts.append(block.get("text", ""))
            elif btype == "tool_use":
                tool_calls.append({
                    "id": block["id"], "type": "function",
                    "function": {"name": block["name"], "arguments": json.dumps(block.get("input", {}))},
                })
            elif btype == "tool_result":
                tool_results.append(block)

        if tool_results:
            for tr in tool_results:
                rc = tr.get("content", "")
                if isinstance(rc, list):
                    rc = "\n".join(b.get("text", "") for b in rc if isinstance(b, dict))
                messages.append({"role": "tool", "tool_call_id": tr["tool_use_id"], "content": rc or ""})
            continue

        msg: dict = {"role": role, "content": "\n".join(text_parts) or None}
        if tool_calls:
            msg["tool_calls"] = tool_calls
        messages.append(msg)

    out: dict = {"messages": messages}
    if body.get("max_tokens"):
        out["max_tokens"] = body["max_tokens"]
    if body.get("temperature") is not None:
        out["temperature"] = body["temperature"]
    tools = body.get("tools")
    if tools:
        out["tools"] = [
            {"type": "function", "function": {
                "name": t["name"], "description": t.get("description", ""),
                "parameters": t.get("input_schema", {}),
            }}
            for t in tools
        ]
    return out


def _openai_to_anthropic(resp: dict, model_field: str) -> dict:
    choice = resp["choices"][0]
    msg = choice.get("message", {})
    content: list[dict] = []
    text = msg.get("content")
    if text:
        content.append({"type": "text", "text": text})
    for tc in msg.get("tool_calls") or []:
        try:
            args = json.loads(tc["function"]["arguments"])
        except (TypeError, ValueError):
            args = {}
        content.append({
            "type": "tool_use", "id": tc.get("id") or f"toolu_{uuid.uuid4().hex[:16]}",
            "name": tc["function"]["name"], "input": args,
        })
    usage = resp.get("usage") or {}
    return {
        "id": resp.get("id") or f"msg_{uuid.uuid4().hex[:16]}",
        "type": "message", "role": "assistant", "model": model_field,
        "content": content,
        "stop_reason": _STOP_REASON.get(choice.get("finish_reason"), "end_turn"),
        "stop_sequence": None,
        "usage": {
            "input_tokens": usage.get("prompt_tokens", 0),
            "output_tokens": usage.get("completion_tokens", 0),
        },
    }


def _sse(event: str, data: dict) -> bytes:
    return f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()


def _fake_stream_events(msg: dict):
    yield _sse("message_start", {"type": "message_start", "message": {**msg, "content": []}})
    for i, block in enumerate(msg["content"]):
        start_block = ({"type": "text", "text": ""} if block["type"] == "text" else
                       {"type": "tool_use", "id": block["id"], "name": block["name"], "input": {}})
        yield _sse("content_block_start", {"type": "content_block_start", "index": i, "content_block": start_block})
        if block["type"] == "text":
            delta = {"type": "text_delta", "text": block["text"]}
        else:
            delta = {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}
        yield _sse("content_block_delta", {"type": "content_block_delta", "index": i, "delta": delta})
        yield _sse("content_block_stop", {"type": "content_block_stop", "index": i})
    yield _sse("message_delta", {"type": "message_delta",
               "delta": {"stop_reason": msg["stop_reason"], "stop_sequence": None},
               "usage": {"output_tokens": msg["usage"]["output_tokens"]}})
    yield _sse("message_stop", {"type": "message_stop"})


def build_app(resolve: ResolveFn, timeout: float = 120.0,
              transport: httpx.BaseTransport | None = None) -> Starlette:
    """`transport` overrides the upstream HTTP transport (tests only)."""
    async def messages(request: Request) -> Response:
        body = await request.json()
        model_field = body.get("model", "")
        try:
            base_url, api_key, upstream_model = resolve(model_field)
        except ValueError as exc:
            return JSONResponse(
                {"type": "error", "error": {"type": "invalid_request_error", "message": str(exc)}},
                status_code=400)

        openai_body = _anthropic_to_openai(body)
        openai_body["model"] = upstream_model

        async with httpx.AsyncClient(timeout=timeout, transport=transport) as client:
            resp = await client.post(
                base_url.rstrip("/") + "/chat/completions",
                headers={"Authorization": f"Bearer {api_key}"},
                json=openai_body,
            )
        if resp.status_code >= 400:
            return JSONResponse(
                {"type": "error", "error": {"type": "api_error", "message": resp.text}},
                status_code=resp.status_code)

        anthropic_msg = _openai_to_anthropic(resp.json(), model_field)
        if body.get("stream"):
            return StreamingResponse(_fake_stream_events(anthropic_msg), media_type="text/event-stream")
        return JSONResponse(anthropic_msg)

    return Starlette(routes=[Route("/v1/messages", messages, methods=["POST"])])


def _port_open(port: int, host: str = "127.0.0.1") -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.settimeout(0.2)
        return s.connect_ex((host, port)) == 0


def ensure_proxy_running(port: int, ready_timeout: float = 10.0) -> None:
    """Best-effort: spawn this module as a background process if nothing
    is already listening on `port`. Idempotent — a second call while the
    first is still starting just waits for the same port to open."""
    if _port_open(port):
        return
    subprocess.Popen(
        [sys.executable, "-m", "agents_gateway.harness.anthropic_compat_proxy", str(port)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    deadline = time.monotonic() + ready_timeout
    while time.monotonic() < deadline:
        if _port_open(port):
            return
        time.sleep(0.2)


def main() -> None:
    import uvicorn
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8199
    app = build_app(resolver_from_env(
        os.environ.get("API_KEY", ""), os.environ.get("API_URL", "")))
    uvicorn.run(app, host="127.0.0.1", port=port, log_level="warning")


if __name__ == "__main__":
    main()


__all__ = ["resolver_from_env", "build_app", "ensure_proxy_running"]
