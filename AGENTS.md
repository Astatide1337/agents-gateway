# AGENTS.md (Agents Gateway)

This project is operated by AI coding agents. Follow these rules.

## Model policy

- **Only** use `nvidia/nemotron-3-ultra-550b-a55b:free` for LLM-driven
  work in this repo (PI coding harness, Composer planner LLM, smoke
  tests, etc.). The `:free` suffix is mandatory — it routes to the
  zero-cost OpenRouter tier.
- Do **not** use any other model — not claude, not gpt, not
  moonshotai/kimi, not gemini, not minimax, not deepseek, not anything
  from openrouter beyond `nvidia/nemotron-3-ultra-550b-a55b:free`.
- OpenRouter is the only allowed provider. Other providers
  (Anthropic, OpenAI, NVIDIA direct, etc.) are forbidden — the model
  is accessed **via OpenRouter** only, not via NVIDIA's own API.
- If the `:free` tier returns 429 (rate-limited), retry with backoff.
  Do not fall back to a paid model variant.

## Model configurability (per-task model override)

Harness profiles do **NOT** hardcode a model in their `args`.

- Each profile that supports a model override declares a CLI flag name
  via `model_arg_name` on the `HarnessProfile` dataclass:
    - `pi-coding-agent` → `model_arg_name="--model"`
    - `opencode`        → `model_arg_name="-m"`
- Profiles without `model_arg_name` (`claude-code`, `codex`,
  `fake-test`) ignore the override and launch with their own
  defaults.
- At dispatch time, `task_spec.execution.model` is read by
  `agents_gateway/harness/runtime.py` (~line 217) and forwarded to
  `HarnessDriver.start_session(model_override=...)`. The driver calls
  `profile.effective_args(model_override=...)` and prepends
  `[model_arg_name, model]` to the spawn argv.
- If the dispatcher omits `task_spec.execution.model`, the profile's
  `default_model` (if set) is used. If neither is set, the harness is
  launched without a model flag and picks its own runtime default.
- The Conductor forwards `composer.llm_model` into every dispatched
  task's `execution.model`. Set
  `CONDUCTOR_COMPOSER_LLM_MODEL=nvidia/nemotron-3-ultra-550b-a55b:free`
  in production env.

## Built-in harness profiles

Defined in `agents_gateway/harness/profiles.py` (`BUILTIN_PROFILES`):

- **pi-coding-agent** (default) — PI Coding Agent CLI. Model via
  `--model <id>`. Use this for live E2E and normal dispatch.
- **opencode** — opencode CLI. Model via `-m <provider/model>`.
  Supports `/goal` slash command.
- **claude-code** — Anthropic Claude Code CLI. No model override.
- **codex** — OpenAI Codex CLI. No model override.
- **fake-test** — in-tree deterministic fake harness for tests and
  the local E2E script.

The `opencode-deepseek` profile was **deleted** — it hardcoded a paid
DeepSeek model and was the source of silent profile-substitution bugs.
Do not reintroduce it.

## Practical settings

- PI binary: `/home/ubuntu/.local/bin/pi`.
- Invoke PI for a one-off:
  ```
  pi --model nvidia/nemotron-3-ultra-550b-a55b:free --thinking off
  ```
- PI settings live at `~/.pi/agent/settings.json`. Pin
  `defaultModel: "nvidia/nemotron-3-ultra-550b-a55b:free"` there.
- The Agents Gateway `pi-coding-agent` profile intentionally has
  **no** model in its `args` — the model is supplied per-task by the
  dispatcher. Do NOT re-hardencode it. To set a default model
  statically on the profile, set the `default_model` attribute of
  the `HarnessProfile` in `agents_gateway/harness/profiles.py`.
- The Composer/LLM configuration must use the model id
  `nvidia/nemotron-3-ultra-550b-a55b:free`. The env var name is
  `CONDUCTOR_COMPOSER_LLM_MODEL`. Set it in `.env.production`.
- The credential env var is `OPENROUTER_API_KEY`. The auth file is
  `~/.pi/agent/auth.json` (key `openrouter`). Do **not** introduce
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, or
  `NVIDIA_API_KEY` here — OpenRouter is the only allowed provider.
- `_credential_env_names` for the `pi` harness entry must include
  `OPENROUTER_API_KEY` so AGW reports `credentials_present=true` for
  `pi-coding-agent` availability checks.

## Scaling knobs that prevent 402s

- Never let PI pick `auto`. Always pass `--model ...` so it does not
  silently route to a larger model. This is now enforced structurally
  — the dispatcher must populate `task_spec.execution.model`.
- If a verification command needs pytest, use `uvx pytest` (NOT
  `uv run pytest`). Worktrees sit under
  `git@github.com:owner/repo/...` paths whose `:` breaks uv's
  argument parser. Also avoid `pytest file.py::test_name`; use
  `-k pattern` instead.
- The Hosts allow OpenRouter credit consumption up to ~$5/run. Keep
  the design composed of short tasks (1–3 implementation tasks). Do
  not task a single PI session with build-migrate-everything.
- The `:free` tier has stricter rate limits — use `--thinking off`
  for PI and keep token budgets small. The Conductor's
  `conductor.composer.llm_max_tokens` defaults to 2048; raise only if
  the spec warrants it.

## Repo-specific pointers

- Agent runtime: `agents_gateway/harness/{driver,tmux,verification,
  profiles,goal}.py`.
- Verification runner: `agents_gateway/harness/verification.py`.
  Commands containing shell metacharacters
  (`&&`, `||`, `;`, `|`, `>`, `>>`, `<`, command substitution)
  MUST be routed through `/bin/bash -c` rather than passed directly
  to `subprocess.run` — `cd` is a shell builtin otherwise.
- Tmux driver: `agents_gateway/harness/tmux.py`. Use `--` separator
  before any text with leading dashes (markdown list items like
  `- `) — `tmux send-keys` otherwise parses leading `-` as a flag.
- Profile table (built-in harness profiles):
  `agents_gateway/harness/profiles.py` (`BUILTIN_PROFILES`).
- MCP `harness_task_create` tool: see `agents_gateway/mcp_tools.py`
  — accepts an optional `model` parameter that flows into
  `task_spec.execution.model`.
- Model override plumbing (read top-down):
  - Conductor `composer/scheduler.py` and `composer/integration.py`
    populate `task_spec["execution"]["model"]` from `node.model` /
    `config.llm_model`.
  - Conductor composer models `TaskNode.model`, `IntegrationNode.model`,
    `LLMTaskNode.model` carry per-task overrides (default empty).
  - AGW `harness/runtime.py:execute_task` reads
    `task_spec.execution.model` and passes it as `model_override` to
    `HarnessDriver.start_session`.
  - AGW `harness/driver.py:start_session` calls
    `profile.effective_args(model_override=...)`.
  - AGW `harness/profiles.py:HarnessProfile.effective_args` injects
    `[model_arg_name, model]` into the spawn argv.

## Health gate before any LLM-driven work

```bash
curl -sf -H "X-Auth-Internal-Token: $CONDUCTOR_INTERNAL_TOKEN" \
  http://localhost:8093/health
curl -sf -H "X-Auth-Internal-Token: $TOK" \
  http://localhost:8092/harness-profiles/pi-coding-agent/availability
```

If either fails, do not dispatch more tasks; surface in the report.

## What this repo is not

This project does **not** ship `minimax` / `claude-sonnet` / `gpt-4o`
fall-backs. If a third-party pull request adds one, reject the PR.
The deleted `opencode-deepseek` profile was the historical source of
silent profile-substitution bugs (a hard-coded paid model that fell
back to itself when the dispatcher did not set `harness_profile`).
Do not reintroduce hardcoded model profiles — use the
`task_spec.execution.model` override mechanism or the profile's
`default_model` attribute.
