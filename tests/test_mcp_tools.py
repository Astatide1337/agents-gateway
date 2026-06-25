"""Tests for MCP tools."""

import json
from pathlib import Path

import pytest

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.mcp_tools import create_mcp_server
from agents_gateway.storage import TaskStorage


@pytest.fixture
def setup(tmp_path):
    agents_dir = tmp_path / "agents"
    agents_dir.mkdir()
    (agents_dir / "tool-agent").mkdir()
    (agents_dir / "tool-agent" / "agent.yaml").write_text(
        "id: tool-agent\nname: Tool Agent\ndescription: Agent for testing tools\nversion: 0.1.0\nruntime:\n  type: local-stub\n"
    )
    config = GatewayConfig(
        agents={"dir": str(agents_dir)},
        storage={"sqlite_path": str(tmp_path / "test.db"), "artifacts_dir": str(tmp_path / "artifacts")},
    )
    mcp = create_mcp_server(config)
    return mcp, config


class TestMCPTools:
    def test_tools_registered(self, setup):
        mcp, _ = setup
        tool_names = [t.name for t in mcp._tool_manager._tools.values()]
        expected = [
            "agents_list", "agents_search", "agents_inspect",
            "agent_task_create", "agent_task_get", "agent_task_events",
            "agent_task_artifacts", "agent_task_cancel",
        ]
        for name in expected:
            assert name in tool_names, f"Missing tool: {name}"

    def test_agents_list(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        list_tool = next(t for t in tools if t.name == "agents_list")
        result = list_tool.fn()
        data = json.loads(result)
        assert isinstance(data, list)

    def test_agents_search(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        search_tool = next(t for t in tools if t.name == "agents_search")
        result = search_tool.fn(query="testing")
        data = json.loads(result)
        assert isinstance(data, list)

    def test_agents_inspect(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        inspect_tool = next(t for t in tools if t.name == "agents_inspect")
        result = inspect_tool.fn(agent_id="tool-agent")
        data = json.loads(result)
        assert data.get("id") == "tool-agent"

    def test_agents_inspect_not_found(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        inspect_tool = next(t for t in tools if t.name == "agents_inspect")
        result = inspect_tool.fn(agent_id="nonexistent")
        data = json.loads(result)
        assert "error" in data

    def test_agent_task_create(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        create_tool = next(t for t in tools if t.name == "agent_task_create")
        result = create_tool.fn(agent_id="tool-agent", input_data="test")
        data = json.loads(result)
        assert data.get("agent_id") == "tool-agent"
        assert data.get("status") == "created"

    def test_agent_task_create_invalid_agent(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        create_tool = next(t for t in tools if t.name == "agent_task_create")
        result = create_tool.fn(agent_id="nonexistent")
        data = json.loads(result)
        assert "error" in data

    def test_agent_task_get(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        create_tool = next(t for t in tools if t.name == "agent_task_create")
        result = create_tool.fn(agent_id="tool-agent", input_data="")
        task_id = json.loads(result)["id"]

        get_tool = next(t for t in tools if t.name == "agent_task_get")
        result = get_tool.fn(task_id=task_id)
        data = json.loads(result)
        assert data.get("id") == task_id

    def test_agent_task_events(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        create_tool = next(t for t in tools if t.name == "agent_task_create")
        result = create_tool.fn(agent_id="tool-agent", input_data="")
        task_id = json.loads(result)["id"]

        events_tool = next(t for t in tools if t.name == "agent_task_events")
        result = events_tool.fn(task_id=task_id)
        data = json.loads(result)
        assert isinstance(data, list)

    def test_agent_task_cancel(self, setup):
        mcp, _ = setup
        tools = list(mcp._tool_manager._tools.values())
        create_tool = next(t for t in tools if t.name == "agent_task_create")
        result = create_tool.fn(agent_id="tool-agent", input_data="")
        task_id = json.loads(result)["id"]

        cancel_tool = next(t for t in tools if t.name == "agent_task_cancel")
        result = cancel_tool.fn(task_id=task_id)
        data = json.loads(result)
        assert data.get("status") == "cancelled"
