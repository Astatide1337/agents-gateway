# Personal agent workflow

The personal product surface is one file and one launch command:

```text
AgentBundle YAML
      ↓
agw run -f agent.yaml
      ↓
validate + compile + apply immutable revisions
      ↓
rootless Podman sandbox with brokered model/MCP/Skills access
      ↓
minimal event stream + primary/supporting artifacts
      ↓
terminal cleanup
```

`AgentBundle` is a declarative authoring format. It compiles into the existing
immutable `Agent`, `SkillSet`, `ToolSet`, `ModelRoute`, and `SandboxProfile`
resources. It does not contain arbitrary executable configuration or secret
values. Credentials are references to separately provisioned `Credential`
resources whose `secretRef` points to the host/deployment secret source.

## Example

Start with [personal-agent.yaml](../../v2/examples/personal-agent.yaml). It
defines:

- an explicit OpenRouter model route;
- a digest-pinned runtime image and rootless Podman limits;
- one pinned Skills Gateway revision;
- one read-only MCP grant (`get_me`); and
- a primary Markdown report plus a supporting text verification artifact.

The checked-in runtime image is an intentionally unusable zero-digest
placeholder. Before a live run, set `AGW_PERSONAL_RUNTIME_IMAGE` to the exact
image reference visible to your runner, or use a private copy of the file.
Never replace the digest with a floating tag.

The two credential names in the bundle (`personal-openrouter` and
`personal-mcp`) must already exist in the target project. The acceptance script
creates those reference definitions with `env://` secret references; it never
puts the secret values into the bundle or output.

## Launching an agent

Build the CLI or point `AGW_PERSONAL_CLI` at an existing `agw` binary. The CLI
reads its owner token from `AGW_TOKEN` and does not persist it:

```sh
cd /path/to/agents-gateway/v2
go build -o /tmp/agw ./cmd/agw
export AGW_PERSONAL_CLI=/tmp/agw
export AGW_PERSONAL_API_URL=https://agents.example.invalid
export AGW_PERSONAL_API_TOKEN='owner-token-from-your-secret-manager'
export AGW_PERSONAL_ORGANIZATION=00000000-0000-4000-8000-000000000001
export AGW_PERSONAL_PROJECT=00000000-0000-4000-8000-000000000002
export AGW_PERSONAL_RUNTIME_IMAGE='runner-visible/image@sha256:...'
export AGW_PERSONAL_SKILL_FILE_SHA256=sha256:...
```

The direct CLI UX is:

```sh
AGW_TOKEN="$AGW_PERSONAL_API_TOKEN" \
  /tmp/agw run -f v2/examples/personal-agent.yaml \
  --server "$AGW_PERSONAL_API_URL" \
  --organization "$AGW_PERSONAL_ORGANIZATION" \
  --project "$AGW_PERSONAL_PROJECT" \
  --idempotency-key personal-agent-$(date -u +%Y%m%d%H%M%S)
```

By default, `agw run -f` follows the run and prints only a compact event
timeline. Add `--follow=false` when another client will poll the run. Optional
immutable task input can be supplied with `--input FILE` or `--input -`; input
is bounded and becomes part of the compiled Agent revision.

The lower-level `validate`, `plan`, `apply`, and resource-revision commands
remain available for operators who want to inspect the compiled definitions
before launch. `agw run -f` is the personal convenience path over that same
resource and run API.

## Minimal status and logs

The run status and event history are the troubleshooting surface:

```sh
BASE="$AGW_PERSONAL_API_URL/api/v1alpha1/organizations/$AGW_PERSONAL_ORGANIZATION/projects/$AGW_PERSONAL_PROJECT"
curl --fail --silent --show-error \
  -H "Authorization: Bearer $AGW_PERSONAL_API_TOKEN" \
  "$BASE/runs/RUN_ID"

curl --fail --silent --show-error \
  -H "Authorization: Bearer $AGW_PERSONAL_API_TOKEN" \
  "$BASE/runs/RUN_ID/events?after=0"
```

Events are bounded and redacted. Useful event types include model selection,
tool authorization/outcome, sandbox lifecycle, verification, artifact
creation, and terminal status. This is intentionally not Sentry, a metrics
platform, distributed tracing, or a general production log system.

The console uses the same event API and SSE stream for a small Activity panel:
it loads historical events, follows new events, supports errors/tools/model/
sandbox filters, and exposes bounded safe details.

## Artifacts

The agent writes strict descriptors under `.agw/artifacts/`. The example authors
two files:

- `Personal agent primary report` — Markdown, the human-facing result;
- `Personal agent supporting verification` — text, supporting evidence.

The run-specific API exposes the relationship:

```sh
curl --fail --silent --show-error \
  -H "Authorization: Bearer $AGW_PERSONAL_API_TOKEN" \
  "$BASE/runs/RUN_ID/artifacts"
```

The response has `data.primary`, `data.supporting`, and `data.artifacts`. The
current alpha derives roles from authored-artifact event/order. The descriptor
contract does not accept arbitrary `output_role` fields, so keep the primary
artifact first and later authored files supporting. The generic immutable run
output remains a technical fallback.

Preview behavior is defensive: Markdown is rendered without active HTML,
HTML/SVG uses a sandboxed iframe, and interactive scripts require an explicit
capability. Keep secrets out of artifact content even though broker and event
paths redact sensitive material.

## Stop and cleanup

Stop an active run with an idempotent cancel command:

```sh
AGW_TOKEN="$AGW_PERSONAL_API_TOKEN" \
  /tmp/agw cancel \
  --server "$AGW_PERSONAL_API_URL" \
  --organization "$AGW_PERSONAL_ORGANIZATION" \
  --project "$AGW_PERSONAL_PROJECT" \
  --run-id RUN_ID \
  --idempotency-key personal-agent-stop-RUN_ID \
  --reason 'operator requested stop'
```

Terminal runs clean their managed sandbox, workspace, broker session, and
temporary materialized secrets. The applied resource revisions are durable
definitions and are intentionally not deleted by the acceptance script; the
current v1alpha1 API has no resource-delete operation. Use stable personal
resource names and review their revision history.

## Sandbox boundary

The default standalone runner is rootless Podman with a shared kernel. Each run
gets an ephemeral workspace, read-only root filesystem, bounded CPU/memory/
disk/PID limits, no-new-privileges, dropped capabilities, and no direct
internet. Model, MCP, Skills Gateway, and artifact credentials remain outside
the sandbox behind the per-run broker.

This is an owner-operated trusted-user profile, not a hostile public
multi-tenant boundary. Do not execute arbitrary code submitted by unknown users
on a shared host. Stronger isolation profiles are optional/experimental.

## Acceptance test

The acceptance script never prints tokens, response bodies, prompts, skill
contents, raw tool arguments, or artifact contents. Its temporary files are
private and removed on exit.

Static acceptance runs the bundle compiler tests and probes the actual
`agw run -f` compilation path without contacting a server:

```sh
./v2/scripts/e2e-personal-agent.sh
```

It reports:

```text
PERSONAL_AGENT_ACCEPTANCE=STATIC_PASS bundle=compiled sandbox=not-run
```

For the live chain, provide the API values and exact skill/runtime pins:

```sh
export AGW_PERSONAL_API_URL=https://agents.example.invalid
export AGW_PERSONAL_API_TOKEN='owner-token-from-your-secret-manager'
export AGW_PERSONAL_ORGANIZATION=00000000-0000-4000-8000-000000000001
export AGW_PERSONAL_PROJECT=00000000-0000-4000-8000-000000000002
export AGW_PERSONAL_RUNTIME_IMAGE='runner-visible/image@sha256:...'
export AGW_PERSONAL_SKILL_REF=agent-manager/matt-pocock/codebase-design
export AGW_PERSONAL_SKILL_DIGEST=sha256:...
export AGW_PERSONAL_SKILL_FILE_SHA256=sha256:...
export AGW_PERSONAL_MCP_URL=https://dockermcp.example.invalid/mcp
export AGW_PERSONAL_MODEL=cohere/north-mini-code:free

./v2/scripts/e2e-personal-agent.sh
```

The live result is compact:

```text
PERSONAL_AGENT_ACCEPTANCE=PASS run=... status=Succeeded stop=Cancelled sandbox=not-run
```

The live path validates the real bundle compile/apply/launch, rootless
sandbox-backed run, pinned skill proof, read-only MCP scope and audit, primary
and supporting artifact retrieval, event logs, cancellation, and cleanup. Set
`AGW_PERSONAL_CHECK_LOCAL_PODMAN=1` only when running as the same rootless
runner user that owns the local containers. Set
`AGW_PERSONAL_RUN_SANDBOX_E2E=1` without live variables to run the existing
fixture-based rootless acceptance separately; that low-level script remains
unchanged.

## Scope boundary

Stable personal scope is deliberately small: one owner, one host/profile,
explicit model selection, pinned skills, narrow MCP grants, rootless sandbox,
durable events, and private artifacts. Durable human approval for MCP side
effects, remote runner fencing, quotas, public sharing, Kubernetes, Temporal,
S3/R2, OTLP semantic tracing, Firecracker/Kata, and hostile multi-tenant
execution remain advanced/experimental capabilities.
