# Runtime

Agents Gateway supports two parallel execution runtimes. Both share the same SQLite store and the same FastMCP/ASGI server.

## 1. Legacy task/runtime adapters (`local-stub`, `process`, `docker`)

Used by `agent_id`-keyed tasks registered via `agents/*/agent.yaml`. The runtime adapter is chosen by the manifest's `runtime.type` field.

| Adapter | What it does |
|--------|--------------|
| `local-stub` | Records the task as completed within a few hundred ms. No-op adapter used for circuit-break tests and dev. |
| `process` | Runs a configured shell command on the host (or container host). No sandboxing — opt-in only via `AGW_RUNTIME__ALLOW_PROCESS=true`. |
| `docker` | Runs a hardened Docker container (`--cap-drop ALL`, `--read-only`, `--network none` by default, `--user 65534:65534`, etc.). Requires Docker daemon socket access. |

The legacy adapter family is preserved untouched by the harness-runtime milestone.

## 2. Harness worktree runtime (`harness_session`)

Drives a Composer-controlled task through the full lifecycle:

```
Composer receives spec
  |
  v
Composer breaks spec into tasks
  |
  for each task:
    Agents Gateway creates isolated git worktree
    Agents Gateway starts selected harness in tmux
    Harness works on the goal
    Harness can use Skills Gateway + MCP Gateway
    Harness may ask for input / clarification / next instruction
    Agents Gateway captures that state
    Composer replies automatically when necessary
    Harness continues working
    Verification runs
    Proof artifacts are captured
    Final result is returned to Composer
```

### Execution topology

```
repo workspace
  +-- base clone / cached repo
  |
  +-- worktrees/
        +-- task_<task_id>/
              +-- git worktree on branch agent/<task_id>-<slug>
              +-- harness session
                    +-- tmux session/window/pane
                    +-- claude / opencode / codex / future harness
```

Mapping:

```
objective -> repo workspace
task      -> git worktree
agent_run -> harness session
session   -> tmux process running selected harness
```

Multiple tasks may run in parallel:

```
objective: build gateway hub
  +-- task A -> worktree A -> opencode session A
  +-- task B -> worktree B -> opencode session B
  +-- task C -> worktree C -> claude-code review session C
```

No two agent runs ever edit the same working directory.

### Runtime behavior

- `HarnessRuntime.execute_task` is the synchronous entry point invoked by `TaskWorker` when the task's metadata `runtime_type == "harness_session"`.
- The runtime prepares the workspace (`RepoWorkspaceManager.get_or_create`), creates an isolated git worktree, starts a tmux-backed HarnessDriver session, injects the goal, runs a transient `SessionSupervisor` thread to classify output, and drives verification until pass or terminal failure.
- Verification is mandatory before `completed` status is granted.
- Failed verification is fed back into the harness session as `VERIFICATION FEEDBACK` so the agent can iterate.
- A hard time cap (`relay_max_time_seconds`) and verify-iterations cap (`max_verify_iterations`) limit runaway sessions. Exceeding either cap marks the session `stalled` (NOT `failed`) so Composer can decide.
- A full HTML review report (`review-report.html`) is generated when verification passes.

### Result shape

`HarnessRunResult` carries the structured return path back to Composer:

```json
{
  "agent_run_id": "run_...",
  "task_id": "task_...",
  "status": "completed|failed|blocked_external|stalled|...",
  "repo": {"url": "...", "branch": "agent/...", "base_branch": "master",
           "worktree_path": "..."},
  "harness": {"profile": "opencode-deepseek",
              "session_id": "session_...",
              "tmux_session": "agw_..."},
  "verification": {"status": "passed", "commands": [...]},
  "artifacts": [{"id": "artifact_...", "kind": "html_report", "path": "..."}],
  "git": {"changed_files": [...], "diff_artifact_id": "...",
          "commit_sha": "...", "pushed": false, "pr_url": null},
  "summary": "All required verification commands passed.",
  "blockers": []
}
```

When blocked:

```json
{
  "status": "blocked_external",
  "blockers": [
    {"type": "missing_credentials",
     "message": "Live E2E requires GITHUB_TOKEN",
     "missing_env": ["GITHUB_TOKEN"]}
  ]
}
```

### Configuration

| Env var | Default | Purpose |
|---------|---------|---------|
| `AGW_HARNESS__USE_FAKE_TMUX` | `false` | Use in-memory FakeTmuxDriver (no real tmux, for tests/local E2E) |
| `AGW_HARNESS__WORKSPACE_ROOT` | `/tmp/agents-gateway/repos` | Base clone cache |
| `AGW_HARNESS__WORKTREE_ROOT` | `/tmp/agents-gateway/worktrees` | Worktree directory root |
| `AGW_HARNESS__ARTIFACTS_ROOT` | `/tmp/agents-gateway/artifacts` | Per-run artifacts root |
| `AGW_HARNESS__SESSION_POLL_INTERVAL_SECONDS` | `10` | Supervisor poll interval |
| `AGW_HARNESS__SESSION_STALL_SECONDS` | `900` | Stall threshold (15 minutes) |
| `AGW_HARNESS__AUTO_COMMIT` | `true` | Auto-commit verified changes on the worktree branch |
| `AGW_HARNESS__AUTO_PUSH` | `false` | Auto-push the branch (requires remote write access) |
| `AGW_HARNESS__AUTO_PR` | `false` | Auto-open a GitHub PR (requires `gh` CLI) |
| `AGW_HARNESS__COMMAND_TIMEOUT_SECONDS` | `1800` | Per-verification-command timeout |
| `AGW_HARNESS__RELAY_MAX_TIME_SECONDS` | `3600` | Hard wallclock for one full task |
| `AGW_HARNESS__MAX_VERIFY_ITERATIONS` | `50` | Max verification rounds before `stalled` |
| `AGW_HARNESS__COMPLETION_WAIT_SECONDS` | `0.5` | Loop wait between completion checks |

### Skills + MCP Gateway integration

- `SkillsGatewayClient` (in `harness/client_skills.py`) validates required skills against a Skills Gateway URL. If the gateway is disabled in strict mode, the run moves to `blocked_external` with `blocked_reason=skills_gateway_unconfigured`.
- `McpGatewayClient` (in `harness/client_mcp.py`) renders `TOOLS.md` content advertising available MCP Gateway tools. The rendered markdown is written into the worktree's `.agent-task/TOOLS.md` so the harness can read it.
- Both clients support `auth_mode=internal-only` with an `internal_token` header. Tokens are never written to logs or HTML reports (see `SECURITY.md`).
