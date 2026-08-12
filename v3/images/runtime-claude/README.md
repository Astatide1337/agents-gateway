# Agents Gateway Claude Code runtime

This image is the digest-deployed Claude Code runtime for a v3 work Sandbox.
It runs Claude Code headlessly with `-p`, `--output-format stream-json`,
`--max-turns`, `--model`, and `--dangerously-skip-permissions`.

The runtime sets `ANTHROPIC_BASE_URL` to the loopback broker and supplies only
`agw-loopback-dummy` auth values. The broker selects and authenticates the
configured `anthropic-messages` or `openrouter-anthropic-messages` provider.
The real provider credential is never present in the agent container.

The wrapper creates a private `CLAUDE_CONFIG_DIR`, `HOME`, and `TMPDIR` below
the writable run workspace, writes a credential-free HTTP MCP config, verifies
the checkout SHA, emits bounded `agw.runtime.v1` events, and uploads the same
run-output and `.agw/artifacts` evidence contract as the Codex runtime.

## Build

```bash
v3/images/runtime-claude/validate.sh
IMAGE=ghcr.io/astatide/agw-runtime-claude:local \
  CONTAINER_ENGINE=docker \
  v3/images/runtime-claude/build.sh
```

The build script resolves `@anthropic-ai/claude-code@latest` at build time.
Record the resolved version, build and scan the image, push it, resolve the
registry manifest digest, and put only that digest-pinned reference in
`Agent.spec.runtime.image`. A local tag is not a deployment identity.

No paid/live provider is needed for unit or contract tests; use the fake Claude
binary tests in `pkg/claudeadapter` and the fake broker fixtures.
