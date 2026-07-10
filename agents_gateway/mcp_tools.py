"""MCP tools for Agents Gateway.

Includes both the legacy agent discovery / task lifecycle tools AND
harness-runtime tools that expose the worktree/session/interaction/
verification/artifact surfaces to MCP-compatible cockpits (Composer).
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from fastmcp import FastMCP

from agents_gateway.catalog import AgentCatalog
from agents_gateway.config import GatewayConfig
from agents_gateway.harness.profiles import (
    get_profile as _get_harness_profile,
    list_profiles as _list_harness_profiles,
)
from agents_gateway.harness.storage import HarnessStorage
from agents_gateway.storage import TaskStorage, TransitionError


def create_mcp_server(config: GatewayConfig) -> FastMCP:
    mcp = FastMCP(
        "Agents Gateway",
        instructions=(
            "Gateway tools for agent discovery + task lifecycle + the "
            "harness worktree runtime (workspace, worktree, session, "
            "verification, composer interactions, artifacts)"
        ),
    )
    catalog = AgentCatalog(config)
    storage = TaskStorage(config.storage.sqlite_path)
    harness_storage = HarnessStorage(config.storage.sqlite_path)

    # ===================================================================
    # Legacy agent catalog + task lifecycle MCP tools
    # ===================================================================

    @mcp.tool()
    def agents_list() -> str:
        agents = catalog.list_agents()
        entries = catalog.catalog_entries()
        return json.dumps([e.model_dump() for e in entries])

    @mcp.tool()
    def agents_search(query: str) -> str:
        results = catalog.search_agents(query)
        entries = [
            {
                "id": a.id, "name": a.name, "description": a.description,
                "version": a.version, "runtime": {"type": a.runtime.type},
                "risk_level": a.risk_level.value,
            }
            for a in results
        ]
        return json.dumps(entries)

    @mcp.tool()
    def agents_inspect(agent_id: str) -> str:
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return json.dumps({"error": f"Agent '{agent_id}' not found"})
        return json.dumps(agent.model_dump())

    @mcp.tool()
    def agent_task_create(agent_id: str, input_data: str = "") -> str:
        agent = catalog.get_agent(agent_id)
        if agent is None:
            return json.dumps({"error": f"Agent '{agent_id}' not found or not available in active profile"})
        task = storage.create_task(agent_id, input_data)
        return json.dumps(task.model_dump())

    @mcp.tool()
    def agent_task_get(task_id: str) -> str:
        task = storage.get_task(task_id)
        if task is None:
            return json.dumps({"error": f"Task '{task_id}' not found"})
        return json.dumps(task.model_dump())

    @mcp.tool()
    def agent_task_events(task_id: str) -> str:
        events = storage.list_events(task_id)
        return json.dumps([e.model_dump() for e in events])

    @mcp.tool()
    def agent_task_artifacts(task_id: str) -> str:
        artifacts = storage.list_artifacts(task_id)
        return json.dumps([a.model_dump() for a in artifacts])

    @mcp.tool()
    def agent_task_cancel(task_id: str) -> str:
        try:
            task = storage.cancel_task(task_id)
            return json.dumps(task.model_dump())
        except Exception as e:
            return json.dumps({"error": str(e)})

    # ===================================================================
    # Harness worktree runtime MCP tools
    # ===================================================================

    @mcp.tool()
    def harness_profiles_list() -> str:
        """List all configured harness profiles (opencode/claude/codex/fake-test)."""
        return json.dumps([p.to_dict() for p in _list_harness_profiles()])

    @mcp.tool()
    def harness_profile_get(name: str) -> str:
        """Get details of one harness profile by name."""
        profile = _get_harness_profile(name)
        if profile is None:
            return json.dumps({"error": f"Profile '{name}' not found"})
        return json.dumps(profile.to_dict())

    @mcp.tool()
    def harness_task_create(title: str = "", brief: str = "",
                             repo_url: str = "",
                             base_branch: str = "master",
                             harness_profile: str = "opencode-deepseek",
                             goal_text: str = "",
                             verification_commands_json: str = "[]",
                             required_skills_json: str = "[]",
                             required_tools_json: str = "[]",
                             live_e2e_command: str = "",
                             live_e2e_required: bool = False,
                             live_e2e_env_json: str = "[]",
                             repo_owner: str = "", repo_name: str = ""
                             ) -> str:
        """Create a composer-controlled harness_session task.

        Verification commands are supplied as a JSON array of
        ``[{"name": ..., "command": ..., "required": true}, ...]``.
        Live E2E, when configured, is included automatically and
        requires the env vars in ``live_e2e_env_json``.
        """
        try:
            verif_cmds = json.loads(verification_commands_json)
            skills = json.loads(required_skills_json)
            tools = json.loads(required_tools_json)
            live_e2e_env = json.loads(live_e2e_env_json)
        except json.JSONDecodeError as e:
            return json.dumps({"error": f"Invalid JSON arg: {e}"})
        spec = {
            "title": title,
            "brief": brief,
            "repo": {"url": repo_url, "owner": repo_owner,
                     "name": repo_name, "base_branch": base_branch},
            "execution": {"mode": "harness_session",
                          "harness_profile": harness_profile,
                          "isolation": "worktree", "runtime": "tmux"},
            "goal": {"strategy": "auto", "slash_command": "/goal",
                     "text": goal_text},
            "required_skills": skills,
            "required_tools": tools,
            "verification": {"required": True,
                             "commands": verif_cmds},
            "artifacts": {"html_report": True, "screenshots": False,
                          "videos": False, "terminal_capture": True},
        }
        if live_e2e_required:
            spec["verification"]["live_e2e"] = {
                "required": True, "command": live_e2e_command,
                "env_required": live_e2e_env,
            }
        else:
            spec["verification"]["live_e2e"] = {"required": False}
        task = storage.create_harness_task(
            agent_id="harness_session", task_spec=spec,
            metadata={"composer_task_id": "", "objective_id": ""},
        )
        return json.dumps(task.model_dump())

    @mcp.tool()
    def harness_task_run(task_id: str) -> str:
        """Enqueue a harness_session task for execution. Returns task status."""
        task = storage.get_task(task_id)
        if task is None:
            return json.dumps({"error": f"Task '{task_id}' not found"})
        try:
            storage.update_task_status(task_id, "queued")
        except TransitionError as e:
            return json.dumps({"error": str(e)})
        return json.dumps({"task_id": task_id, "status": "queued"})

    @mcp.tool()
    def harness_list_worktrees() -> str:
        """List all known worktrees + their task and branch mapping."""
        return json.dumps([w.__dict__ for w in harness_storage.list_worktrees()])

    @mcp.tool()
    def harness_list_sessions(task_id: str = "",
                              status: str = "") -> str:
        """List harness sessions (optional task_id/status filter)."""
        sessions = harness_storage.list_sessions(
            status=status or None,
            task_id=task_id or None,
        )
        return json.dumps([s.__dict__ for s in sessions])

    @mcp.tool()
    def harness_get_session(session_id: str) -> str:
        """Get one harness session by id."""
        session = harness_storage.get_session(session_id)
        if session is None:
            return json.dumps({"error": f"Session '{session_id}' not found"})
        return json.dumps(session.__dict__)

    @mcp.tool()
    def harness_get_session_capture(session_id: str,
                                    lines: int = 2000) -> str:
        """Capture the recent tmux output of one harness session."""
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(session_id)
        if session is None:
            return json.dumps({"error": f"Session '{session_id}' not found"})
        driver = HarnessDriver(storage=harness_storage)
        try:
            output = driver.capture_output(session, lines=lines)
        except Exception as e:
            return json.dumps({"error": f"capture failed: {e}"})
        return json.dumps({"session_id": session_id, "output": output})

    @mcp.tool()
    def harness_send_to_session(session_id: str, text: str,
                                submit: bool = True) -> str:
        """Send text into a harness session (Composer/ system use)."""
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(session_id)
        if session is None:
            return json.dumps({"error": f"Session '{session_id}' not found"})
        driver = HarnessDriver(storage=harness_storage)
        try:
            driver.tmux.send_text(driver._ref(session), text)
            if submit:
                driver.tmux.send_enter(driver._ref(session))
        except Exception as e:
            return json.dumps({"error": f"send_text failed: {e}"})
        return json.dumps({"session_id": session_id, "status": "sent"})

    @mcp.tool()
    def harness_stop_session(session_id: str) -> str:
        """Stop a harness session (forcibly)."""
        from agents_gateway.harness.driver import HarnessDriver
        session = harness_storage.get_session(session_id)
        if session is None:
            return json.dumps({"error": f"Session '{session_id}' not found"})
        driver = HarnessDriver(storage=harness_storage)
        driver.stop_session(session)
        return json.dumps({"session_id": session_id, "status": session.status})

    @mcp.tool()
    def harness_list_interactions(status: str = "",
                                   task_id: str = "") -> str:
        """List Composer interactions (pending by default)."""
        interactions = harness_storage.list_interactions(
            status=status or "pending",
            task_id=task_id or None,
        )
        return json.dumps([i.__dict__ for i in interactions])

    @mcp.tool()
    def harness_get_interaction(interaction_id: str) -> str:
        """Get one Composer interaction by id."""
        interaction = harness_storage.get_interaction(interaction_id)
        if interaction is None:
            return json.dumps({"error": f"Interaction '{interaction_id}' not found"})
        return json.dumps(interaction.__dict__)

    @mcp.tool()
    def harness_reply_interaction(interaction_id: str, reply: str) -> str:
        """Composer sends a reply to a pending interaction; the reply is
        injected into the associated harness session."""
        from agents_gateway.harness.models import ComposerInteractionStatus
        interaction = harness_storage.get_interaction(interaction_id)
        if interaction is None:
            return json.dumps({"error": f"Interaction '{interaction_id}' not found"})
        if interaction.status != "pending":
            return json.dumps({"error": f"Interaction not pending: {interaction.status}"})
        session = harness_storage.get_session(interaction.session_id)
        delivered = False
        if session is not None:
            from agents_gateway.harness.driver import HarnessDriver
            driver = HarnessDriver(storage=harness_storage)
            driver.send_reply(session, reply)
            delivered = True
        harness_storage.update_interaction_status(
            interaction_id, ComposerInteractionStatus.answered.value,
            composer_reply=reply,
        )
        if interaction.task_id:
            storage.append_event(interaction.task_id,
                                "composer.interaction.answered",
                                {"interaction_id": interaction_id,
                                 "delivered_to_session": delivered})
        return json.dumps({"interaction_id": interaction_id,
                           "status": "answered",
                           "delivered_to_session": delivered})

    @mcp.tool()
    def harness_get_verification(agent_run_id: str) -> str:
        """Get the latest verification run for an agent_run."""
        vr = harness_storage.get_verification_run_by_agent_run(agent_run_id)
        if vr is None:
            return json.dumps({"error": "no verification run found"})
        return json.dumps({
            "id": vr.id, "agent_run_id": vr.agent_run_id,
            "task_id": vr.task_id, "status": vr.status,
            "started_at": vr.started_at, "completed_at": vr.completed_at,
            "commands": [c.__dict__ for c in vr.commands],
        })

    @mcp.tool()
    def harness_list_artifacts(agent_run_id: str = "",
                                task_id: str = "") -> str:
        """List proof artifacts for a harness agent_run (or all for a task)."""
        artifacts = harness_storage.list_harness_artifacts(
            agent_run_id=agent_run_id or None,
            task_id=task_id or None,
        )
        return json.dumps(artifacts)

    @mcp.tool()
    def harness_get_artifact(artifact_id: str,
                             view: bool = False) -> str:
        """Get artifact metadata. If view=True, return file contents (text only)."""
        artifact = harness_storage.get_harness_artifact(artifact_id)
        if artifact is None:
            return json.dumps({"error": f"Artifact '{artifact_id}' not found"})
        if view:
            try:
                p = Path(artifact["path"])
                if not p.exists():
                    return json.dumps({"error": "artifact file missing on disk"})
                content = p.read_text(errors="replace")
                return json.dumps({"content": content[:50000]})
            except Exception as e:
                return json.dumps({"error": f"read failed: {e}"})
        return json.dumps(artifact)

    return mcp
