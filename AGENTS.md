# AGENTS.md (Agents Gateway)

This project is operated by AI coding agents. Follow these rules.

## Model policy — tiered, task-type routed

Two billing modes, both first-class, tracked via `HarnessProfile.billing_mode`
in `agents_gateway/harness/profiles.py`:

- **`metered`** (`pi-coding-agent`, `opencode`) — pay-per-token via an
  OpenRouter API key. Bounded by the allowlist
  (`AGW_APPROVED_FREE_MODELS`, default
  `nvidia/nemotron-3-ultra-550b-a55b:free`) enforced by
  `validate_model_for_profile`, plus a monthly quota breaker
  (`conductor/circuit.py::evaluate_provider_quota_breaker`). If the
  `:free` tier returns 429, retry with backoff — do not silently swap
  in a different metered model outside the allowlist.
- **`subscription`** (`claude-code`, `codex`) — a CLI logged into a
  flat-rate subscription (`claude login` / `codex login`), not a
  metered API key. No per-token allowlist applies (`model_arg_name`
  is `None` for these profiles by design). Usage limits are enforced
  by the provider's own backend and surfaced only via a classifier
  marker (`agents_gateway/harness/classifier.py`'s
  `HarnessState.usage_limited`), not a cost ledger.

**Which provider runs a given task is decided by
`TASK_TYPE_ROUTES`/`resolve_route()`** in `harness/profiles.py` — an
ordered, per-task-kind provider list, not a single global default and
not free-text model guessing. The scheduler tries each profile in a
route in order, falling through on a tripped breaker (cost overrun or
usage-limit) or an unavailable harness binary. `"default"` MUST always
resolve to a free-tier, zero-configuration profile
(`pi-coding-agent`) so the system works with nothing but the
OpenRouter free tier configured. Adding a provider or changing a
route is a data edit to `TASK_TYPE_ROUTES`, not new branching code.

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
- Metered-tier credential env var is `OPENROUTER_API_KEY`. The auth
  file is `~/.pi/agent/auth.json` (key `openrouter`). Do **not**
  introduce `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `DEEPSEEK_API_KEY`
  / `NVIDIA_API_KEY` for the metered tier — OpenRouter is the only
  metered provider.
- Subscription-tier profiles (`claude-code`, `codex`) authenticate via
  their own CLI login (`claude login` / `codex login`), not a gateway-
  managed API key. Do not wire an `ANTHROPIC_API_KEY`/`OPENAI_API_KEY`
  into these profiles' env — that would silently convert a flat-rate
  subscription into a metered one and defeat the point of routing
  work to them.
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

This project does not ship silent, ungoverned model fallbacks.
Claude Code and Codex are legitimate, first-class `subscription`-tier
profiles reached only through `TASK_TYPE_ROUTES` — reject any PR that
hardcodes a paid model choice outside that table, bypasses
`billing_mode`/route resolution, or adds a *metered* paid model
(e.g. a pay-per-token `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` path)
without an explicit monthly quota breaker
(`evaluate_provider_quota_breaker`) guarding it. The deleted
`opencode-deepseek` profile was the historical source of silent
profile-substitution bugs (a hard-coded paid model that fell back to
itself when the dispatcher did not set `harness_profile`) — the fix
was structural (explicit routing, not an implicit fallback), and that
principle still applies: a task's provider must always be traceable
to a `TASK_TYPE_ROUTES` entry, never an ad hoc default buried in code.
