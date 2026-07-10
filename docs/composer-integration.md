# Composer Integration

This document describes the contract between Composer (the orchestration brain) and Agents Gateway (the execution substrate).

## Roles

- **Composer** holds intent: which tasks exist, which repo/task/harness/skills/tools to use, what replies to give to agents when they ask for guidance, whether the final result satisfies the spec.
- **Agents Gateway** handles execution: where the task runs (worktree), how it runs (tmux session), how the harness gets the goal, how to capture output, how to detect waiting/completed/failed states, how to run verification, how to capture artifacts, how to report everything back.

## Endpoint map for Composer

Composer uses these HTTP endpoints (all under the same auth as the rest of the gateway):

| Composer wants to | HTTP call |
|-------------------|-----------|
| Create a task | `POST /tasks` (with `execution.mode=harness_session` + full task spec) |
| Enqueue a task | `POST /tasks/{id}/run` (returns 202; the worker picks the task up asynchronously) |
| Check task status | `GET /tasks/{id}` |
| Stream events | `GET /tasks/{id}/events` |
| List pending interactions | `GET /interactions?status=pending` |
| Pick a specific task's interactions | `GET /interactions?task_id=<id>&status=pending` |
| Reply to an agent question | `POST /interactions/{id}/reply` (body `{"reply": "..."}`) |
| Cancel an interaction | `POST /interactions/{id}/cancel` |
| Look at session output | `GET /sessions/{id}/capture` |
| Manually inject text | `POST /sessions/{id}/send` (body `{"text": "...", "submit": true}`) |
| Stop a session | `POST /sessions/{id}/stop` |
| Re-run verification | `POST /agent-runs/{id}/verify` |
| Fetch artifacts | `GET /agent-runs/{id}/artifacts` and then `GET /artifacts/{id}?view=true` for the raw bytes |
| Browse harness profiles | `GET /harness-profiles` to see what's configured |
| Validate a profile against a goal strategy | `POST /harness-profiles/validate` |

The MCP protocol (mounted at `POST /mcp`) exposes equivalent `harness_*` tools for Composer to call without an HTTP round-trip — see `docs/api.md` for the full tool inventory.

## Task spec accepted by `POST /tasks`

```json
{
  "objective_id": "obj_123",
  "composer_task_id": "comp_task_456",
  "title": "Implement Guide Timeline endpoint",
  "brief": "Add GET /objectives/{id}/timeline and MCP conductor_get_timeline. Include tests and docs.",
  "repo": {
    "url": "https://github.com/Astatide1337/conductor.git",
    "owner": "Astatide1337",
    "name": "conductor",
    "base_branch": "master"
  },
  "execution": {
    "mode": "harness_session",
    "harness_profile": "opencode-deepseek",
    "isolation": "worktree",
    "runtime": "tmux",
    "containerized": false
  },
  "goal": {
    "strategy": "auto",
    "slash_command": "/goal",
    "text": "Implement the timeline endpoint exactly as specified. Work only in your assigned worktree. Run all required verification before completion."
  },
  "required_skills": [
    "test-driven-development",
    "verification-before-completion",
    "systematic-debugging"
  ],
  "required_tools": [
    "github.read",
    "github.create_pr"
  ],
  "verification": {
    "required": true,
    "commands": [
      {"name": "unit tests", "command": "uv run pytest -q", "required": true},
      {"name": "local e2e", "command": "bash scripts/e2e-local.sh", "required": true}
    ],
    "live_e2e": {
      "required": false,
      "command": "bash scripts/e2e-live-gateway-hub.sh",
      "env_required": ["CONDUCTOR_BASE_URL", "CONDUCTOR_INTERNAL_TOKEN"]
    }
  },
  "artifacts": {"html_report": true, "screenshots": true, "videos": false,
                "terminal_capture": true},
  "metadata": {}
}
```

The full spec is preserved in `task.input` (JSON-serialized) so future operations (verification re-run, inspection) always use the original request.

## Reply protocol

When an agent asks a question, the runtime creates a `composer_interactions` row with `status=pending`. Composer should:

1. Poll `GET /interactions?status=pending` (or listen to `composer.interaction.created` events).
2. For each pending interaction, decide whether to reply or cancel.
3. To reply: `POST /interactions/{id}/reply` with body `{"reply": "<text>"}`. The reply text is wrapped in an `ASSISTANT REPLY (from Composer):` header and sent into the tmux session line-by-line. The interaction is marked `answered`; the session transitions back to `running`.
4. To cancel: `POST /interactions/{id}/cancel`. The interaction is marked `cancelled`. The harness is not auto-stopped — Composer should `POST /sessions/{id}/stop` separately if it wants the session terminated.

Interaction types and their meaning:

| `type` | Composer's expected response |
|--------|------------------------------|
| `needs_reply` | Decide what to tell the agent; reply or stop the session. |
| `needs_credentials` | Composer can supply the missing env, OR cancel the interaction and let the session stay `blocked_external`. |
| `external_blocker` | Surface to the human user; cancel or wait. |
| `ambiguous_harness_state` | Composer can ignore (the session is `stalled` but harness may still be working), inject a "continue" reply, or stop the session. |
| `verification_failure_context` | Optional diagnostic. The runtime also fires a `VERIFICATION FEEDBACK` block into the session automatically; Composer does NOT need to refuse-failed-feedback. |

## When the agent finishes

When verification passes, `HarnessRunResult` carries everything Composer needs:

```json
{
  "agent_run_id": "run_...",
  "task_id": "task_...",
  "status": "completed",
  "repo": {"url": "...", "branch": "agent/task-abc-feature", "base_branch": "master",
           "worktree_path": "/var/lib/agents-gateway/worktrees/.../task_abc"},
  "harness": {"profile": "opencode-deepseek", "session_id": "session_...",
              "tmux_session": "agw_..."},
  "verification": {"status": "passed", "commands": [...]},
  "artifacts": [
    {"id": "artifact_...", "kind": "html_report", "path": "..."},
    {"id": "artifact_...", "kind": "patch", "path": "..."},
    {"id": "artifact_...", "kind": "log", "path": "..."}
  ],
  "git": {"changed_files": [...], "insertions": N, "deletions": M,
          "commit_sha": "abc123", "pushed": false, "pr_url": null},
  "summary": "All required verification commands passed.",
  "blockers": []
}
```

Composer can then decide whether the work satisfies the spec, accept the result, push the branch, open a PR, request human review, or re-dispatch.

## When the agent gets stuck

Three outcome paths exist:

1. **`stalled`** — Composer sees `status=stalled` + an `ambiguous_harness_state` interaction. Composer can:
   - Tell the agent to continue (reply with `"Continue with the most-likely interpretation per spec."`)
   - Stop the session entirely (`POST /sessions/{id}/stop`)
   - Cancel the interaction + ignore; the session remains `stalled` indefinitely
2. **`blocked_external`** — Composer sees `status=blocked_external` + a `needs_credentials` (or `external_blocker`) interaction with `missing_env` listed. Composer can:
   - Provide the missing env via a re-dispatch (new task with `env_required` reply configured for the next run)
   - Cancel the interaction and accept `blocked_external` as terminal
3. **`failed`** — Only when the harness process died with a failure marker OR exited unexpectedly without a completion marker. Composer sees `status=failed` and the failure reason in the result's `summary` + `blockers` field. The session is stopped.

Verification failure is NONE of these — a failed verification just feeds the failure back into the session and the agent keeps working. Composer does not need to do anything.
