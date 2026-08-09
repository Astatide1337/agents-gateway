# Agents Gateway v2 architecture plan

Status: approved implementation baseline
Canonical API group: `agents.astatide.com/v1alpha1`

This is the target architecture and compatibility contract. The current
implementation ledger, including evidence and release blockers, is in
[implementation-status.md](implementation-status.md).

## Product definition

Agents Gateway is an open-source, self-hosted control plane for defining and
running reproducible AI agents on infrastructure owned by the installer.

> Define an agent like infrastructure, run it like a job, and govern it like a
> production workload.

The default must be useful to one developer on one ordinary Linux machine.
Advanced distributed and enterprise features are additive configuration, not
installation prerequisites. Cloudflare, Kubernetes, Temporal, S3, OIDC,
gVisor, and a dedicated VM are never required for the baseline product.

V2 is independent of Conductor. Conductor or any other system may call its
public API, but V2 does not import or require Conductor.

## Deployment profiles

The concise operator-facing comparison is in
[deployment-profiles.md](deployment-profiles.md).

### Standalone (default)

```text
Browser / CLI
      |
      v
+---------------- Agents Gateway ----------------+
| API + console + local durable orchestration     |
| PostgreSQL metadata/queue + local artifacts     |
+-----------------------+-------------------------+
                        | local runner protocol
                        v
             rootless Podman runner
                        |
                 per-run sandbox
```

The installation owns all required services and exposes only the UI/API. A
single setup command generates local credentials, starts the control plane,
installs or verifies the rootless runner, and runs a real sandbox smoke test.
PostgreSQL is bundled because it already provides durable state, concurrency,
and a clean upgrade path. Artifacts use a local volume. Telemetry writes to
normal structured logs unless an OTLP endpoint is configured.

The runner is a separate process boundary because the web/API container must
never receive a container-runtime socket. The installer hides that operational
detail; users should not have to design a runner fleet to execute one agent.

The current owner-operated alpha uses this profile through a Coolify Git-backed
Compose Application in the `Gateways` project. Its live console/API is
`https://agents.astatide.com`. This is an operational deployment of the alpha,
not a claim that the platform is ready for hostile public multi-tenancy.
Release commit `4d05ed12a17e5e40a0ab4d7a6bc6c63ac97a754a` is deployed as
Coolify deployment `yf0tuzljbgjwn0otluqpyhbb`; `/healthz` and `/readyz` both
returned 200.

### Distributed (optional)

```text
                       managed or self-hosted dependencies
                      +-----------+---------+----------+
Browser / CLI -> API ->| Postgres | Temporal| S3 / OTLP|
                      +-----------+---------+----------+
                            |
                      mTLS runner protocol
                     /          |          \
              Podman runner  gVisor runner  microVM runner
```

The distributed profile enables Temporal, S3-compatible artifacts, OIDC,
organization/project tenancy, RLS, quotas, remote mTLS runners, and external
OpenTelemetry. Operators select only the capabilities they need. The same API
and declarative resources work in both profiles.

## Stable component boundaries

- `agw-server`: API, console backend, auth, definition compilation, run-engine
  interface, policy brokers, events, and audit.
- Run engine: `local` by default; `temporal` is an optional adapter. Both obey
  the same start/signal/cancel and readiness contract.
- `agw-runner`: owns sandbox lifecycle. Local transport is a Unix socket;
  remote transport is HTTPS with mTLS. Neither path exposes a runtime socket
  to the control plane or sandbox.
- Sandbox backend: rootless Podman by default; gVisor and future microVM
  backends advertise stronger isolation capabilities.
- Store: bundled PostgreSQL initially. The application uses a store interface,
  but a second database is not required merely to claim portability.
- Artifacts: local filesystem by default; S3-compatible storage is optional.
- Identity: generated local administrator token by default; OIDC and scoped
  service accounts are optional.
- Telemetry: structured logs by default; OTLP export is optional and
  provider-neutral.

## Agents as code

YAML/JSON is canonical. Typed SDK builders compile to the same representation
and digest; the control plane never evaluates configuration as arbitrary code.
Core resources are `Agent`, `Workflow`, `SkillSet`, `ToolSet`, `SandboxProfile`,
and `ModelRoute`. A run records immutable revisions for every referenced
resource so it can be explained and reproduced.

```yaml
apiVersion: agents.astatide.com/v1alpha1
kind: Agent
metadata:
  name: issue-fixer
spec:
  runtime:
    harness: codex
    image: ghcr.io/example/agent-codex@sha256:IMAGE_DIGEST
  sandboxProfileRef: rootless-default
  instructions:
    file: ./instructions/issue-fixer.md
  skillSetRef: coding-skills
  toolSetRef: github-issue-fixer
  limits:
    timeout: 45m
```

Workflows are deterministic DAGs with stable step IDs, typed references,
timeouts, retries, approval nodes, cancellation, and resumable state. An
optional Auto-CD/GitOps controller may reconcile definitions and enqueue runs,
but it is not part of the execution kernel.

## Security and isolation

The installer trusts the owner who submits work but treats agent workloads as
untrusted. Rootless Podman is the standard/shared-kernel baseline. It uses
non-root execution, user namespaces, read-only root filesystems, dropped
capabilities, `no-new-privileges`, cgroup/PID/time limits, isolated per-run
workspaces, and no network by default.

Tool/model credentials remain in brokers outside the sandbox. ToolSets grant
exact capabilities; Skills and MCP annotations may request but never grant
authority. Approved external writes use effect IDs and idempotency controls.
See [threat-model.md](threat-model.md) for guarantees and non-guarantees.

## Durability

The local engine persists queued work, transitions, timers, signals, attempts,
and leases in PostgreSQL. It may run inside `agw-server` for a one-process
standalone experience. Restarting the server must resume queued/running work or
record an accurate terminal/unknown state; it must never create a duplicate
external effect.

The Temporal adapter implements the same contract for operators who need
distributed workers, long-running histories, advanced retry/timer semantics,
or multi-host failover. Large payloads never live in orchestration history;
they are immutable artifact references.

## Runtime and gateway integration

The runner and harness adapter use the versioned JSONL protocol in
[runtime-protocol.md](runtime-protocol.md). A Codex CLI adapter and runtime
image are implemented and run inside the sandbox image, not on the control
plane host. Additional harness adapters remain separate work.

Skills Gateway is an MCP source for discovering and fetching pinned skills.
MCP Gateway is a tool transport. In the local-engine path, Agents Gateway
resolves immutable model/tool/skill references, stages skills read-only, and
creates a per-run policy broker. At run time, credentials remain outside the
sandbox and calls are policy checked. Environment references use opaque
`secret://<id>` values. On the trusted runner, the ID maps to one runner-owned
`0600` file below `/var/lib/agw-runner/secrets`; the resolved value is written
only to a private per-run `0600` env file below `/run/agw-runner/materialized`,
passed to the sandbox, and removed at run cleanup. Neither gateway is modified
or made a hard dependency: operators may configure any compatible MCP/skill
source or none at all.

The connected production evidence uses the direct MCP origin
`https://dockermcp.astatide.com/mcp`; the OAuth-facing MCP portal is a separate
client entry point. Model selection remains explicit and deployment-specific.
`nvidia/nemotron-3-ultra-550b-a55b:free` was attempted first and returned a
provider-side HTTP 502; `cohere/north-mini-code:free` was an explicit retry,
not an automatic production fallback. The system does not advertise silent
model fallback.

The shared `strictjson` boundary is used for API, capability, persistence, and
replay data. It accepts one JSON value only and rejects unknown fields,
duplicate keys, invalid UTF-8/NULs, explicit `null` for ToolSet argument
constraints, and out-of-budget numeric values. Optional ToolSet `arguments`
means either omitted/unconstrained or one exact JSON object; explicit `null`
is invalid. Exact matching ignores object key order, preserves array order, and
compares numbers mathematically. Budgets are 1 MiB documents, 16 KiB number
lexemes, 8,192 mantissa digits, absolute exponent 4,096, 12,288 integer
digits, 4,096 fractional digits, depth 128, 1,024 object members, 1,024 array
items, and 64 KiB strings. Authorization and approval denials are audited
before credentials/upstream access; unavailable audit persistence fails closed.
The `approve`/`propose/commit` broker workflow remains fail-closed and is not
advertised as complete.

## What remains optional

- Cloudflare Access/Tunnel and any other reverse proxy.
- OIDC, organizations/projects, RLS, quotas, and team approvals.
- Temporal and separately deployed workers.
- S3-compatible artifact storage.
- OTLP Collector, Grafana, or another observability backend.
- Remote mTLS runners and runner scheduling.
- gVisor, Firecracker/Kata, Kubernetes, and dedicated runner hosts.
- GitOps reconciliation and Terraform/provider integrations.

Optional capabilities must fail closed when selected but misconfigured. They
must not complicate or weaken the standalone defaults.

## Implementation sequence

1. **Standalone vertical slice:** one-command Compose control plane, bundled
   PostgreSQL, local durable engine, local artifacts, rootless Podman runner,
   local auth, one real harness, and a clean-machine E2E. The owner-operated
   rootless-Podman E2E is verified; a clean-host repeat remains a release
   hardening step.
2. **Agent capabilities:** pinned Skills Gateway materialization, MCP Gateway
   policy broker, model routing, approvals, verification, and artifact export.
   The local-engine broker, opaque secret materialization, durable MCP audit,
   and Claude-style artifact catalog/viewer paths are implemented. The
   connected real-provider run passed as gate 2 in production run
   `run-3686c06ef7ead0b0a1a5b13b7220df7ff0bb282ad1ebda5fe0b4506acbf6d937`.
   Gates 1 through 6 are passed for the owner-operated alpha; this is not a
   claim of final CI/release completion or public multi-tenant readiness.
3. **Workflow completeness:** DAGs, retries, signals, crash recovery,
   idempotent external effects, and full failure injection.
4. **Optional distributed profile:** Temporal adapter, S3, OIDC/RLS, OTLP,
   remote mTLS runners, gVisor, quotas, and scheduling.
5. **Ecosystem:** GitOps, SDKs, packaging, and optional microVM/Kubernetes
   integrations.

## Acceptance gates

- A new operator can install and run the sample agent on ordinary Ubuntu
  without Cloudflare, Kubernetes, Temporal, S3, OIDC, or a dedicated VM.
- The acceptance test executes a real rootless Podman sandbox and real harness
  adapter; Docker container E2E is already verified, but it does not replace
  the rootless-Podman test. The connected real-provider local-engine chain
  passed as gate 2 in the cited production run. The live Coolify deployment is
  verified separately in the `Gateways` project; final CI/cleanup completion
  remains a separate claim.
- A restart during queued and running work has a tested, accurate outcome.
- Sandboxes cannot access runtime sockets, host credentials, another run's
  files/processes, or the network unless an explicit broker policy allows it.
- Resource limits, cancellation, bounded output, immutable inputs, audit, and
  duplicate-effect handling are tested.
- Enabling an optional profile does not change the public API or manifest
  format and fails closed when its dependencies are unavailable.
