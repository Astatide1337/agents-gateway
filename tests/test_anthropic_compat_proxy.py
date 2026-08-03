from __future__ import annotations

import json

import httpx
import pytest
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse
from starlette.routing import Route

from agents_gateway.harness.anthropic_compat_proxy import (
    _anthropic_to_openai,
    _openai_to_anthropic,
    build_app,
    resolver_from_env,
)


class TestResolverFromEnv:
    def test_resolves_configured_provider(self):
        resolve = resolver_from_env("key-a,key-b", "https://openrouter.ai/api/v1,https://integrate.api.nvidia.com/v1")
        base_url, key, model = resolve("nvidia-nim/z-ai/glm-5.2")
        assert base_url == "https://integrate.api.nvidia.com/v1"
        assert key == "key-b"
        assert model == "z-ai/glm-5.2"

    def test_unknown_provider_raises(self):
        resolve = resolver_from_env("key-a", "https://openrouter.ai/api/v1")
        with pytest.raises(ValueError, match="unknown provider"):
            resolve("nvidia-nim/z-ai/glm-5.2")

    def test_missing_slash_raises(self):
        resolve = resolver_from_env("key-a", "https://openrouter.ai/api/v1")
        with pytest.raises(ValueError, match="must be"):
            resolve("no-provider-prefix")


class TestAnthropicToOpenAI:
    def test_system_and_text_message(self):
        out = _anthropic_to_openai({
            "system": "You are helpful.",
            "messages": [{"role": "user", "content": "hi"}],
        })
        assert out["messages"][0] == {"role": "system", "content": "You are helpful."}
        assert out["messages"][1] == {"role": "user", "content": "hi"}

    def test_tool_use_block_becomes_tool_call(self):
        out = _anthropic_to_openai({
            "messages": [{"role": "assistant", "content": [
                {"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"cmd": "ls"}},
            ]}],
        })
        tc = out["messages"][0]["tool_calls"][0]
        assert tc["id"] == "toolu_1"
        assert tc["function"]["name"] == "bash"
        assert json.loads(tc["function"]["arguments"]) == {"cmd": "ls"}

    def test_tool_result_becomes_tool_role_message(self):
        out = _anthropic_to_openai({
            "messages": [{"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
            ]}],
        })
        assert out["messages"][0] == {"role": "tool", "tool_call_id": "toolu_1", "content": "done"}

    def test_tools_translated_to_openai_functions(self):
        out = _anthropic_to_openai({
            "messages": [],
            "tools": [{"name": "bash", "description": "run a command",
                       "input_schema": {"type": "object"}}],
        })
        assert out["tools"] == [{"type": "function", "function": {
            "name": "bash", "description": "run a command", "parameters": {"type": "object"}}}]


class TestOpenAIToAnthropic:
    def test_text_response(self):
        out = _openai_to_anthropic({
            "id": "chatcmpl-1",
            "choices": [{"message": {"content": "hello"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 3},
        }, model_field="nvidia-nim/z-ai/glm-5.2")
        assert out["content"] == [{"type": "text", "text": "hello"}]
        assert out["stop_reason"] == "end_turn"
        assert out["usage"] == {"input_tokens": 5, "output_tokens": 3}

    def test_tool_call_response(self):
        out = _openai_to_anthropic({
            "choices": [{"message": {"content": None, "tool_calls": [
                {"id": "call_1", "function": {"name": "bash", "arguments": '{"cmd": "ls"}'}},
            ]}, "finish_reason": "tool_calls"}],
        }, model_field="x/y")
        block = out["content"][0]
        assert block == {"type": "tool_use", "id": "call_1", "name": "bash", "input": {"cmd": "ls"}}
        assert out["stop_reason"] == "tool_use"


def _fake_upstream_app(captured: list) -> Starlette:
    async def chat_completions(request: Request) -> JSONResponse:
        body = await request.json()
        captured.append(body)
        return JSONResponse({
            "id": "chatcmpl-fake",
            "choices": [{"message": {"content": f"echo:{body['model']}"}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 2},
        })
    return Starlette(routes=[Route("/chat/completions", chat_completions, methods=["POST"])])


class TestBuildAppEndToEnd:
    @pytest.mark.asyncio
    async def test_non_streaming_round_trip(self):
        captured: list = []
        upstream = _fake_upstream_app(captured)
        upstream_transport = httpx.ASGITransport(app=upstream)

        def resolve(model_field):
            return "http://upstream", "test-key", model_field.split("/", 1)[1]

        app = build_app(resolve, transport=upstream_transport)
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://proxy") as client:
            resp = await client.post("/v1/messages", json={
                "model": "nvidia-nim/z-ai/glm-5.2",
                "messages": [{"role": "user", "content": "hi"}],
            })

        assert resp.status_code == 200
        data = resp.json()
        assert data["content"] == [{"type": "text", "text": "echo:z-ai/glm-5.2"}]
        assert captured[0]["model"] == "z-ai/glm-5.2"

    @pytest.mark.asyncio
    async def test_unknown_provider_returns_400(self):
        def resolve(model_field):
            raise ValueError(f"unknown provider in {model_field!r}")

        app = build_app(resolve)
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://proxy") as client:
            resp = await client.post("/v1/messages", json={
                "model": "nope/model", "messages": [{"role": "user", "content": "hi"}],
            })
        assert resp.status_code == 400
