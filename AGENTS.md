# AGENTS.md (Agents Gateway)

This project is operated by AI coding agents. Follow these rules.

## Model policy

- **Only** use `openrouter/deepseek/deepseek-v4-flash` for LLM-driven
  work in this repo (PI coding harness, Composer planner LLM, smoke
  tests, etc.).
- Do **not** use any other model — not claude, not gpt, not
  moonshotai/kimi, not gemini, not minimax, not anything from
  openrouter beyond `deepseek/deepseek-v4-flash`.
- The thinking level must stay at `low` (or `minimal`). Do not escalate
  to `medium` / `high` / `xhigh` / `max`.
- OpenRouter is the only allowed provider. Other providers
  (Anthropic, OpenAI, NVIDIA, etc.) are forbidden.

## Practical settings

- PI binary: `/home/ubuntu/.local/bin/pi`
- Invoke PI as:
  ```
  pi --model openrouter/deepseek/deepseek-v4-flash --thinking low
  ```
- PI settings live at `~/.pi/agent/settings.json`. Pin
  `defaultModel: "openrouter/deepseek/deepseek-v4-flash"` there.
- The Agents Gateway `pi-coding-agent` profile must always carry
  `--model openrouter/deepseek/deepseek-v4-flash` and
  `--thinking low` in its `args` tuple. Edit
  `agents_gateway/harness/profiles.py` if you change this.
- The Composer/LLM configuration must use the model id
  `deepseek/deepseek-v4-flash`. The env var name is
  `CONDUCTOR_COMPOSER_LLM_MODEL`. Default in production config:
  `deepseek/deepseek-v4-flash`.
- The credential env var is `OPENROUTER_API_KEY`. The auth file is
  `~/.pi/agent/auth.json` (key `openrouter`). Do **not** introduce
  `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `DEEPSEEK_API_KEY`, or
  `NVIDIA_API_KEY` here.
- `_credential_env_names` for the `pi` harness entry must include
  `OPENROUTER_API_KEY` so AGW reports `credentials_present=true` for
  `pi-coding-agent` availability checks.

## Scaling knobs that prevent 402s

- Never let PI pick `auto`. Always pass `--model ...v4-flash` so it
  does not silently route to a larger model.
- If a verification command needs pytest, use `uvx pytest` (NOT
  `uv run pytest`). Worktrees sit under
  `git@github.com:owner/repo/...` paths whose `:` breaks uv's
  argument parser. Also avoid `pytest file.py::test_name`; use
  `-k pattern` instead.
- The Hosts allow OpenRouter credit consumption up to ~$5/run. Keep
  the design composed of short tasks (1–3 implementation tasks). Do
  not task a single PI session with build-migrate-everything.

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
