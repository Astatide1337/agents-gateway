# Agents Gateway Codex runtime

This image is the digest-deployed `codex` runtime for a v3 work `Sandbox`.
Its entrypoint is `/agw/agent`. The wrapper deterministically creates the
single `run.start` frame from the immutable workload environment and posts
each validated runtime event synchronously to the loopback broker. It does
not depend on an interactive stdin or use stdout as a cross-container pipe.
Diagnostics go to stderr.

The entrypoint requires these per-run values:

| Variable | Purpose |
| --- | --- |
| `AGW_BROKER` | HTTP loopback broker base URL; the adapter derives model, MCP, and artifact routes from it. |
| `AGW_CODEX_WORKSPACE` | Absolute writable checkout path, normally `/workspace/repo`. |
| `AGW_CODEX_MODEL` | Exact model selected by the resolved `ModelRoute`. |
| `AGW_BASE_SHA` | Lowercase 40- or 64-character SHA resolved by the controller. |
| `AGW_SPEC_DIGEST` | Digest of the fully resolved immutable run contract. |
| `AGW_RUN_UID`, `AGW_AGENT_REF` | Server-bound runtime identity. |
| `AGW_TASK`, `AGW_INSTRUCTIONS` | Bounded prompt inputs resolved by the controller. |

`AGW_BASE_PATH` is optional for standalone use and recommended in the work
Sandbox. When provided, `/agw/agent` verifies both it and the agent checkout
with `git rev-parse --verify HEAD^{commit}`. Both must equal `AGW_BASE_SHA`.
The git check runs with a minimal environment that disables system/global Git
configuration, credential helpers, prompts, and remote access.

The adapter creates a fresh private `CODEX_HOME` for every invocation and
passes Codex an allowlisted child environment. Provider credentials stay in
the loopback broker sidecar. The command handles SIGINT/SIGTERM through a
canceling context, allowing the adapter to terminate the Codex process group
and emit `run.cancelled`.

The runtime only authors events. It cannot finalize completion: the operator
independently reads the Kubernetes `agent` container termination state and
combines that exit code with the durable event stream. A forged early
`run.completed` event is therefore insufficient.

The temporary private home is placed below the writable workspace parent. This
is intentional: the v3 work pod uses a read-only root filesystem and does not
make the image's `/tmp` writable.

The workload builder supplies the harness, workspace, base path, selected
model, immutable base SHA, and loopback broker contract. For standalone image
tests, supply `AGW_CODEX_MODEL` and `AGW_BASE_SHA` explicitly; the entrypoint
exits before launching Codex when either is absent or either checkout does not
match the pinned SHA.

## Build

From the repository root:

```bash
v3/images/runtime-codex/validate.sh
IMAGE=ghcr.io/astatide/agw-runtime-codex:local \
  CONTAINER_ENGINE=docker \
  v3/images/runtime-codex/build.sh
```

The build script resolves `@openai/codex@latest`, passes the exact version to
the multi-stage build, and records it in `org.opencontainers.image.version`
and `com.astatide.agw.codex.version`. Set `CODEX_CLI_VERSION` explicitly for
a reproducible rebuild after recording the version from the first build.

The image is not production-ready merely because it has a local tag. Push it
to a registry, resolve the resulting manifest digest, and put only that
digest-pinned reference in `Agent.spec.runtime.image`.

## Verification

Local command/package checks do not require a provider credential:

```bash
cd v3
go test ./cmd/agw-runtime-codex ./pkg/codexadapter
go test -race ./cmd/agw-runtime-codex ./pkg/codexadapter
go vet ./cmd/agw-runtime-codex ./pkg/codexadapter
```

The real image contract additionally requires a container engine. The
existing opt-in adapter container test can be run after building an image,
but a complete broker-backed Kubernetes E2E still requires a running v3
broker, a k3s workload Sandbox, and model credentials in the broker—not in
this image.
