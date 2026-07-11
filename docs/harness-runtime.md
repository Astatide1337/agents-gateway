# Harness Runtime

This document explains the runtime contract used by the `harness_session` execution mode.

## What the harness runtime is

A runtime plane in Agents Gateway that:

- Creates an isolated git worktree per task
- Starts a configured real coding harness (Claude Code, opencode, Codex, or a fake harness for tests) inside a tmux session
- Supervises the session, classifies its output, and waits for the harness to either ask a question or claim completion
- Routes harness questions to Composer via `composer_interactions`
- Runs mandatory verification commands in the worktree
- Feeds failed test output back into the harness so the agent can iterate
- Captures proof artifacts (logs, diffs, HTML report) and returns structured results to Composer

## What the harness runtime is NOT

- Not a planner / captain — Composer owns intent and strategy
- Not a UI — Composer (or any MCP-compatible client) drives the workflow
- Not a container runtime — sessions today run on host via tmux (container isolation is roadmap)
- Not a fixed protocol — only the runtime contract (worktree + tmux session + interaction queue + verification + artifacts) is fixed

## Harness profiles

Built-in profiles (registered in `harness/profiles.py`):

| Name | Harness | Binary | `supports_slash_goal` | Notes |
|------|---------|--------|-----------------------|-------|
| `opencode-deepseek` | opencode | `opencode` | yes | Default. Supports `/goal` slash command. |
| `claude-code` | claude | `claude` | no | Plain-text goal injection only. |
| `codex` | codex | `codex` | no | Plain-text goal injection only. |
| `fake-test` | fake | `python3 <repo>/agents/fake-test/run.py` | yes | Deterministic fake harness for tests + local E2E. Resolved at module load time so a worktree-path CWD can still find the runner. |

You can register custom profiles at runtime via `harness.profiles.register_profile(profile)`.

## Goal injection strategies

Each profile declares a default goal injection strategy. Composer can override the strategy per task via the task spec's `goal.strategy` field.

| Strategy | Behavior |
|----------|----------|
| `auto` | Use the profile's default. If `supports_slash_goal=true`, sends `/goal <text>`. Else falls back to `plain_prompt`+`file_based`. |
| `slash_goal` | Send `/goal <goal text>` verbatim. Errors early if the profile doesn't support slash commands. |
| `plain_prompt` | Send a multi-line direct instruction to the harness via tmux stdin. |
| `stdin_script` | Pipe a multi-line shell script into stdin. Future — currently equivalent to plain_prompt. |
| `file_based` | Write `.agent-task/{GOAL.md,SKILLS.md,TOOLS.md,VERIFICATION.md,CONTEXT.md,RESULT_SCHEMA.json}` into the worktree and tell the harness to read them. |

The recommended default for unknown / new harnesses is `file_based + plain_prompt` — most robust universal mode.

### Runtime files written per task

`.agent-task/GOAL.md`, `.agent-task/SKILLS.md`, `.agent-task/TOOLS.md`, `.agent-task/VERIFICATION.md`, `.agent-task/CONTEXT.md`, `.agent-task/RESULT_SCHEMA.json`.

`VERIFICATION.md` always begins with the warning:

> You may not mark this task complete until all required verification commands pass.

## Session supervision

The `SessionSupervisor` (a background figurative daemon thread — actually a transient thread per `execute_task` call) polls active sessions every `session_poll_interval_seconds` and classifies recent tmux output via `classify_state`:

| Classification | Meaning | Resulting action |
|----------------|---------|------------------|
| `running` | Harness is actively writing output | No transition |
| `waiting_for_reply` | Detected a question-like phrase ("I need clarification", "should I", "please provide", "can you confirm", "would you like") | Create a `needs_reply` interaction; transition session to `waiting_for_reply`; emit `composer.interaction.created` event |
| `completed_claimed` | Detected a completion marker ("DONE.", "completed.", "all tests passed") | Transition session to `verifying`; invoke `on_completed_claim` hook (runtime runs verification next) |
| `failed_claimed` | Detected a failure marker ("fatal error", "traceback") OR the tmux process is dead with no completion marker | Transition session to `failed`; the runtime surfaces the failure via `HarnessRunResult` |
| `stalled` | No new output for `session_stall_seconds` | Create an `ambiguous_harness_state` interaction; transition session to `stalled` — Composer decides how to proceed |
| `unknown` | Classifier was unable to determine state (process alive but no decisive marker) | Same as `running` — keep polling |

Important: the classifier NEVER marks a session `completed` from text alone — only verification (the runtime's verification loop) can grant `completed` status. A harness that claims done only transitions to `verifying`, where verification gates actual completion.

## Composer interaction protocol

When a harness appears to need a reply:

1. The supervisor captures the relevant output excerpt
2. It inserts a row into `composer_interactions` with `status=pending` and `type=needs_reply`
3. It emits events `agent.waiting_for_reply` and `composer.interaction.created`
4. The session transitions to `waiting_for_reply`; the supervisor resumes polling but doesn't reclassify a waiting session into a different state

Composer (or any MCP client) then:

1. Reads `GET /interactions?status=pending`
2. Picks one and calls `POST /interactions/{id}/reply` with body `{"reply": "..."}` (or via `harness_reply_interaction` MCP tool)
3. The gateway wraps the reply in an `ASSISTANT REPLY (from Composer):` header and sends it into the tmux session line-by-line, pressing Enter between lines
4. Marks the interaction `answered`; emits `composer.interaction.answered` and `agent.resumed` events
5. The session transitions back to `running`; the supervisor resumes its classified-driven loop

Interaction types:

- `needs_reply` — agent asked for guidance
- `needs_credentials` — verification blocked on missing env vars (Composer can supply or skip)
- `external_blocker` — runtime detected a non-credentials blocker
- `verification_failure_context` — runtime-generated diagnostic context (Composer can choose to ignore or intervene)
- `ambiguous_harness_state` — classifier stalled, no decisive signal output

## Completion + verification flow

```
harness prints DONE / "completed" / "all tests passed"
  ↓
classify_state = completed_claimed
  ↓
supervisor marks session verifying
  ↓
HarnessRuntime runs VerificationRunner:
  - checks each command's env_required (if any)
    - missing env → blocked: status=blocked_external,
      interaction type=needs_credentials
    - blocking command reports "missing_credentials: VAR1, VAR2"
  - else runs the command via subprocess.run in the worktree
    - with _safe_env() — no gateway secrets leak into subprocess
    - stdout+stderr captured to a per-command log artifact
    - exit_code recorded
  - first blocked command stops the run
  ↓
if pass:
  - capture git diff (artifact_diff.patch)
  - optional auto-commit (config flag)
  - optional auto-push + auto-PR (config flags)
  - generate HTML review report (artifact_kind=html_report)
  - write session log artifact
  - mark session completed + agent_run completed
  ↓
if fail:
  - feed failure summary back into tmux session
    as "VERIFICATION FEEDBACK:\n<failed command summary>"
  - session transitions back to running
  - harness sees the feedback and can iterate
  - Runtime's loop continues until pass / fail / stall / hard timeout
```

The harness is NEVER automatically marked `failed` for failed verification — only for crashed sessions. Verification failures are work, not blockers.

## Stall / hard timeout / loop cap

The runtime has three safety nets that mark a session `stalled` (not `failed`) so Composer can decide what to do next:

- `session_stall_seconds`: harness silent for that long → `ambiguous_harness_state` interaction; session becomes `stalled`
- `relay_max_time_seconds`: hard wallclock cap exceeded → `stalled`; runtime exits `execute_task` returning `status=stalled`
- `max_verify_iterations`: verification rounds exceeded (`50` by default; for an endlessly self-failing harness) → `stalled` with `ambiguous_harness_state` interaction

## Docker / containerization roadmap

Today sessions run on host via tmux. Long-term target is containerized harness sessions (`ContainerDriver`) where each worktree is mounted into a sandboxed container with no host contact except via the configured MCP Gateway and Skills Gateway URLs. The abstraction (`HarnessDriver.tmux`) is structured so a future `ContainerDriver` can slot in without changing the runtime contract.

## Restart reconciliation

When the gateway boots, `reconcile_harness_sessions()` (in
`harness/reconcile.py`) inspects all recoverable harness sessions:

- **Alive tmux sessions** → marked `recovered_after_restart` + status
  set to `running`. Composer can re-attach and let the session
  continue.
- **Missing tmux sessions** → marked `stalled` (NOT `failed`) so
  Composer can still intervene via interactions or cancel.

This replaces the old behavior where a gateway restart left orphaned
sessions with no gateway-side state. The reconciliation runs once at
boot, before the worker starts claiming new tasks.

## Retention cleanup

The `harness/cleanup.py` module implements artifact and worktree
retention pruning:

- **Time-based** — artifacts older than `artifact_retention_days` and
  worktrees older than `worktree_retention_days` are deletion
  candidates.
- **Size-based** — if total artifact bytes exceed
  `max_artifact_bytes`, the oldest artifacts beyond the budget are
  pruned first.
- **Active guard** — sessions with `status in (created, starting,
  running, waiting_for_reply, verifying)` and their worktrees are
  **never** touched, regardless of age.

Accessed via:

- `POST /cleanup/dry-run` — preview only, no disk changes.
- `POST /cleanup/run` — executes cleanup; honours
  `AGW_HARNESS__CLEANUP_DRY_RUN` (default `true`). Use
  `?force=true` to override.
- `scripts/cleanup-harness-artifacts.sh` — CLI wrapper for in-process
  or HTTP invocation.

Pruned artifacts are removed from disk but the DB row is retained for
audit trail.

## Known limitations (current state)

- Single per-task supervisor thread — no thread-pool reuse across tasks
- Sessions run on host (no per-task container isolation)
- Live E2E verification requires explicit env config (no auto-injection of `GITHUB_TOKEN` etc.)
- Classifier is heuristic — false positives create `needs_reply` interactions that Composer can safely cancel; false negatives are surfaced when the supervisor stops calling classify (session dies, stalls, or completes)
- No checkpoint/resume of the `execute_task` loop — if the gateway restarts mid-task, the harness session survives (tmux persists) but the runtime loop is gone. Reconciliation marks the session `recovered_after_restart` so Composer can re-attach, but the runtime does not automatically resume the supervision loop. Re-running the task starts a new session; the old session can be cleaned up via `POST /sessions/{id}/stop`.
