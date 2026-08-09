# Agents Gateway v2 implementation status

Last updated: 2026-08-09

This is the implementation ledger for the approved v2 architecture. The
design baseline remains [architecture-plan.md](architecture-plan.md); this file
records what exists in the repository and what still requires follow-up. The
[acceptance matrix](acceptance-matrix.md) maps each release promise to evidence.

## Operational today

- Versioned `agents.astatide.com/v1alpha1` resources with strict YAML/JSON
  decoding, production validation, canonical revision digests, deterministic
  plans, and conservative v1 manifest translation.
- The `agw` CLI supports `validate`, `plan`, `apply`, `run`, `cancel`,
  `approve`, `reply`, and `migrate-manifest`. Remote operations require HTTPS (except loopback
  development) and a runtime `AGW_TOKEN`; the CLI does not persist tokens.
- A Go control-plane API with tenant-scoped resource revisions, run creation,
  run lookup, bounded status-event history, long-lived SSE, audit records,
  OIDC, RBAC, dependency-aware readiness, idempotent Temporal dispatch, and
  bounded/idempotent cancellation, approval, and immutable-reply controls.
- PostgreSQL persistence with organization and project row-level security for
  immutable definition revisions, runs/events, and audit metadata. The schema
  also reserves approvals, effects, credentials, entitlements, artifacts,
  runners, leases, and service accounts; only service-account authentication
  lookup is operational from that latter group today.
- A PostgreSQL-backed local orchestration engine selected with
  `AGW_ORCHESTRATION_MODE=local`. It persists queued workflows, state,
  commands, leases, attempts, signal outcomes, and privacy-redacted runtime
  events. Runner event replay uses a task-qualified source cursor, bounded
  history, lease-fenced atomic persistence, and idempotent PostgreSQL
  uniqueness. Temporal remains an optional orchestration adapter.
- Human OIDC plus service-account credential exchange. Long-lived service
  credentials are accepted only at `/auth/token`; APIs receive short-lived
  Ed25519 tokens. Browser builds contain no API token and use a runtime token
  provider or same-origin HttpOnly session.
- Deterministic Temporal workflows with DAG ordering, retries, timeouts,
  durable internal approval/reply/cancellation signals, capacity waits,
  immutable artifact references, stable workflow IDs, stable activity
  idempotency keys, and durable run lifecycle transitions.
- A dedicated worker which dispatches activities to runners over an mTLS-only,
  bounded protocol. The runner starts and supervises a real sandbox process,
  enforces wall-clock timeouts, persists command idempotency, reconciles stale
  managed state on restart, and requires a valid immutable output reference.
- A standalone production execution profile using rootless Podman with an
  explicit shared-kernel isolation grade, plus optional enhanced isolation
  using containerd with gVisor. The local engine creates short-lived broker
  sessions for model/tool/skill/artifact routes; direct sandbox egress remains
  disabled and brokered access is limited to the run-specific Unix broker.
- OpenTelemetry trace/metric/log providers over OTLP HTTP or gRPC with W3C
  propagation, correlated/redacted log helpers, bounded attributes, secure
  endpoints, an explicit internal-collector plaintext allowlist, and HTTP
  server/client tracing. Domain-specific workflow/model/tool metrics and spans
  are not complete yet.
- A React console with API/SSE collections for definitions, runs, approvals,
  runners, entitlements, model routes, quotas, usage, and audit. It is
  live-by-default; demo fixtures require explicit `VITE_AGW_MODE=demo`. The
  owner connection screen accepts a bearer token through an in-memory runtime
  provider and never stores it in the URL, browser storage, cookies, or build
  output. It includes an authenticated Claude-style artifact library and
  workspace with version selection, lineage, sandboxed preview, escaped source,
  safe GitHub-flavored Markdown, and download. A latest-Chrome browser E2E
  covers desktop/mobile rendering, download, iframe policy, accessibility, and
  browser errors.
- Compose/Coolify assets, hardened images, PostgreSQL, a local-only bundled
  Temporal development server, OTel Collector profiles, migration handling,
  runner systemd assets, standalone installer/update handling, CI checks, and
  a deliberately separate MinIO development profile.

## Security invariants

- Agent sandboxes never receive a Docker, Podman, or containerd socket.
- Logical mounts have exact destinations and access modes: writable
  `/workspace`, read-only `/artifacts`, and read-only `/skills`. Duplicate
  kinds/destinations and arbitrary targets such as `/etc` fail validation.
- Owner-operated standalone execution supports rootless Podman and reports
  `standard/shared-kernel`. It is not a hostile multi-tenant boundary. Shared
  untrusted submitters require an explicitly selected stronger runner profile.
- Runner control traffic requires mutual TLS. Temporal TLS is supported;
  plaintext Temporal is rejected unless `AGW_ENVIRONMENT=development`.
- Direct sandbox internet is denied. Credential-bearing model, tool, and
  artifact calls use the out-of-sandbox per-run broker. Unsupported broker
  configuration fails closed.
- Mutating MCP calls are not blindly retried after ambiguous failures. Their
  ledger state becomes `unknown` for reconciliation.
- Subscription credentials are owner-bound and are not pooled to evade quotas
  or account restrictions.
- PostgreSQL application transactions set both organization and project scope;
  authentication lookup uses a separate `BYPASSRLS` role.
- The first release supports one authoritative runner. Cross-runner lease
  fencing and safe task reassignment are not yet implemented.
- Public run controls use a tenant/run/key unique PostgreSQL claim and a
  30-second recovery lease. PostgreSQL acceptance and Temporal delivery are
  separate systems; a crash between them can redeliver the same semantically
  duplicate-safe command, and the API reports that outcome as ambiguous.

- Agent environment references use opaque `secret://<id>` values only. The
  trusted runner maps `<id>` to one exact `0600` file named `<id>` below
  `/var/lib/agw-runner/secrets` (the directory is `0700`, runner-owned), then
  writes a private per-run environment file below
  `/run/agw-runner/materialized` with mode `0600`. The file is passed to the
  sandbox and removed after success, failure, or cancellation. Literal
  production environment values remain forbidden, and secret values never
  enter manifests, events, errors, audit records, or logs.

## Implemented execution integrations and remaining boundaries

The local-engine path connects model routing, MCP capability policy/effect
ledgers, Skills Gateway materialization, local immutable artifact storage, and
the Codex runtime adapter through a per-run broker. Provider credentials remain
outside the sandbox, MCP catalogs are exact, staged skills are read-only, and
MCP authorization/effect outcomes are appended to the durable audit sink. The
local model factory supports only explicit OpenAI and OpenRouter Responses
providers, with their respective default Responses endpoints and host-side
credential resolution. It does not convert a Codex/ChatGPT subscription into
an API key.

Skill materialization verifies exact canonical content digests, preserves
source revision metadata, rejects traversal, links, and special files, bounds
expanded bytes and file count, honors cancellation, and leaves read-only
output. The configured Skills Gateway is an allowlisted out-of-sandbox fetch
path; its credential remains host-only.

The current runnable slice accepts an image pinned by digest, inline bounded
instructions and verification commands, a network-disabled sandbox profile,
and immutable input/output references. Rootless Podman provisions each
workspace on a per-container tmpfs sized to the requested disk limit; the live
standalone E2E proves exhaustion, cleanup, concurrent isolation, and the exact
kernel-enforced CPU, memory, and PID cgroup values under the dedicated
`agw-runner` service identity.

The v2 Codex CLI adapter image, loopback broker bridge, and immutable run-output
handoff are implemented. The standalone rootless-Podman E2E passed on
2026-08-09 with concurrent sandboxes, network denial, secret materialization,
tmpfs quota exhaustion, and cleanup. The connected Skills Gateway test passed
canonical resolution/materialization, and the connected MCP test passed a
real read, one approved write, and duplicate denial. The durable MCP audit sink
is mandatory, privacy-preserving, and covered by its fail-closed and adapter
tests; a combined live run that inspects that audit alongside a real provider
remains pending.
The current adapter E2E uses a fake local Responses provider, so the single
combined local-engine run against a real OpenAI or OpenRouter provider is still
pending. The containerd/gVisor path and hostile multi-tenant boundary are not
live release evidence.

The Claude-style artifact path is implemented end to end: strict agent-authored
descriptors, private broker uploads, immutable local/S3-compatible storage,
tenant-scoped PostgreSQL catalog persistence, authenticated list/detail/content
APIs, digest verification before content response, and a safe console viewer.
Interactive scripts require an explicit capability and remain in a
network-denied, non-same-origin iframe. Edit/remix mutations, public sharing,
and cursor pagination are not implemented.

## Configuration profiles

The target standalone profile requires a generated local owner token, bundled
PostgreSQL, the local durable engine, local artifact storage, and a rootless
Podman runner. The one-command installer creates the runner-owned secret
source root `/var/lib/agw-runner/secrets` and ephemeral materialization root
`/run/agw-runner/materialized`, and writes the corresponding `secrets` block
into the runner configuration. The separate integration Compose profile still
exercises the bundled Temporal development server.

The Coolify Git-backed profile is ready as a Compose Application profile and
keeps the runner outside the Coolify Docker daemon. It has not yet been
created, deployed, or live-verified on the Coolify instance.

Optional distributed-profile values include:

- Managed database URLs and a separate authorization lookup role.
- OIDC issuer, audience, and initial bootstrap subject.
- A base64 Ed25519 service-token private key held in deployment secrets.
- Temporal address, TLS settings, and task queue.
- Remote runner endpoint, server name, CA, client certificate, and client key.
- OTLP endpoint, protocol, headers, and any allowed internal plaintext host.
- S3-compatible endpoint, bucket, region, and credentials.

## Release verification

```sh
cd v2
go test ./...
go test -race ./...
go vet ./...

cd web
npm ci
npm test
npm audit --audit-level=high
npm run build

cd ../..
./v2/scripts/e2e-artifacts-browser.sh

docker compose -f v2/deploy/compose.yaml --profile bundled config
docker compose -f v2/deploy/compose.yaml --profile bundled up --build
./v2/scripts/e2e-control-plane.sh
```

The current integration gate also runs a disposable PostgreSQL migration/RLS test and a
live mTLS runner lifecycle test. The control-plane E2E exercises PostgreSQL 18,
all migrations, Temporal, authenticated apply/run/cancel, and signal audit
events, then deletes its test containers, volumes, and images. The standalone
Docker container E2E and live rootless-Podman standalone E2E are verified. The
connected Skills/MCP tests and fault-injection suite are also release evidence.
The combined real-provider local-engine chain, live Coolify deployment, live
Temporal brokered execution, and live containerd/gVisor test remain
profile-specific or pending release gates.

## Deliberately deferred

- Firecracker/Kata strong-isolation backends and Kubernetes integration. A
  microVM backend is required only before advertising hostile public
  multi-tenant execution, not for the owner-operated standalone release.
- A visual workflow authoring canvas; declarative resources remain the source
  of truth.
- A general autonomous planner. Conductor may call the API but v2 has no
  runtime dependency on it.
- Connector-specific resource adapters beyond generic MCP enforcement.
- High-availability multi-region automation and remote runner fleets.
- Cross-runner lease fencing/reassignment and team-profile quotas.
- Artifact edit/remix mutation APIs, public sharing, cursor pagination, and
  orphan-object garbage collection.
- One combined local-engine run using a real OpenAI or OpenRouter provider,
  Skills Gateway materialization, MCP read/write, artifact authoring, and
  durable audit inspection.
- Live creation, deployment, and end-to-end verification of the Coolify
  Git-backed profile.
- Full Temporal-worker wiring for the brokered model/tool/artifact/skill path.
- Complete semantic telemetry for workflow, sandbox, model, tool, approval,
  verification, and artifact operations.

## License

Agents Gateway is distributed under the repository-level MIT License, matching
the permissive model used by Cloudflare's Agents SDK. Component-specific third-
party notices and dependency licenses continue to apply.
