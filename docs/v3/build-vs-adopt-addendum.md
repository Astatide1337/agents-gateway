# Agents Gateway v3 — accepted build-vs-adopt addendum

Status: accepted, gated implementation decision

Date: 2026-08-11

Extends: ADR-001 through ADR-024

Adds: ADR-025 through ADR-029

The supplied build-vs-adopt proposal is directionally correct: generic workflow,
credential-routing, event-ingress, and attestation machinery should not be rebuilt.
The upstream implementations do not, however, replace the Agents Gateway trust
contract. This addendum records the narrower adoption boundaries proven by the
current source and preserves the existing implementation until live Phase 0 evidence
supports each deletion.

The detailed evidence and falsification plans are in:

- [`build-vs-adopt-spike.md`](build-vs-adopt-spike.md)
- [`agentgateway-composition-spike.md`](agentgateway-composition-spike.md)

## Decision summary

| ADR | Decision | Stable boundary |
| --- | --- | --- |
| ADR-025 | Conditionally adopt Argo Workflows as the durable sequencing backend. | Keep a thin `AgentRun` translator/status mirror and a type-aware Agent Sandbox wait bridge. |
| ADR-026 | Conditionally adopt agentgateway as a per-run downstream routing and credential-injection sidecar. | Keep `agw-guard` as the exact-argument, ToolSet, effect-ledger, runtime-protocol, artifact, and identity authority. |
| ADR-027 | Adopt in-toto/DSSE and cosign as the portable Gate-evidence envelope. | Keep the canonical Gate predicate and independent verification. Use a private/KMS trust root for self-managed k3s; do not assume public Fulcio accepts its ServiceAccount issuer. |
| ADR-028 | Defer Argo Events. | `kubectl apply -f AgentRun.yaml` remains the canonical trigger. Add webhook infrastructure only for a concrete, authenticated event contract. |
| ADR-029 | Build only the completion and trust contract that upstream systems do not provide. | Gate, effect safety, runtime completion, policy compilation, immutable translation, preflight, and bounded status projection remain AGW-owned. |

Production manifests pin immutable image digests. Upgrade automation may discover the
latest release, but a running security boundary never floats on `latest`.

## ADR-025 — Argo is the workflow substrate, not the product state machine

Adopt Argo Workflows for:

- durable step/DAG sequencing;
- safe-compute retry and backoff;
- workflow deadlines and synchronization;
- parameter and artifact transfer;
- exit handlers; and
- Argo-owned Workflow, transient artifact, and PVC garbage collection.

Argo does not define whether agent work is coherent, accepted, rejected, safe to
publish, or ambiguous after an external mutation. The public `AgentRun` domain states
remain AGW-owned, including `Rejected` and `UnknownEffect`.

Agent Sandbox v0.5.4 exposes `Ready` and `Finished` as typed entries in
`status.conditions`; it has no summary `status.phase`. Argo Workflows v4.1.0 resource
conditions use label-selector requirements over field lookups and cannot safely select
an array entry by condition type. Positional conditions are forbidden. A small,
fail-closed bridge must watch the Sandbox and emit a bounded result only when the
required condition type has the required status.

The bridge also owns the child-artifact boundary. Creating a CRD through an Argo
resource template does not make files written by the CRD's child pod into Argo
artifacts. Capture must read the known workspace only after `Finished=True`, validate
the patch/evidence digest, and publish through the existing immutable artifact path.

Argo retries are enabled only for operations proven idempotent or repeatable. Publish
always remains behind the effect ledger. A process or Workflow restart after an
ambiguous external mutation produces `UnknownEffect`; it never produces an automatic
retry.

The direct controller backend remains available until the Argo backend has passed
side-by-side shadow runs and rollback tests. Deletion of old scheduling code is a
separate reviewed change.

## ADR-026 — agentgateway is downstream of an AGW guard

agentgateway v1.4.1 supports useful provider/MCP routing, credential injection, OAuth
exchange, coarse identity policy, streaming, and telemetry. It does not provide the
current v3 exact-JSON authorization contract. Its MCP request-time RBAC receives target
and tool identity, while arguments remain telemetry context. It also has no supported
request-lifecycle extension that implements claim-before-effect, durable outcome
commit, or terminal unknown effects.

The initial supported composition is per run:

```text
agent UID 1000
    | fixed loopback TCP/8081 only
    v
agw-guard UID 1337
    | fixed loopback TCP/8082 only
    v
agentgateway UID 1338
    | approved provider/MCP egress only
    v
provider or MCP backend
```

`agw-guard` owns:

- strict JSON parsing and exact-argument matching;
- phase-scoped ToolSet membership and effect classification;
- effect reservation, commit, and `unknown_effect` handling;
- run UID, resolved-spec digest, base SHA, request, and budget binding;
- `agw.runtime.v1` supervision and completion semantics; and
- artifact descriptors and scoped upload authorization.

agentgateway owns, after live equivalence tests:

- provider and MCP downstream routing;
- backend credential injection or OAuth exchange;
- provider protocol translation and connection management; and
- provider-facing telemetry and usage extraction that passes AGW's conservative
  budget tests.

The future three-container composition must not reuse a broad loopback allow rule.
Its airlock must allow UID 1000 only to the guard port, UID 1337 only to the
downstream gateway port, and UID 1338 only to explicitly permitted public destinations. IPv4,
IPv6, alternate loopback addresses, pod IP, Service paths, management listeners,
private ranges, metadata, and the Kubernetes API are tested as bypass paths. The
gateway is not adopted until that live invariant holds under `hostUsers: false`.

A shared central gateway is deferred. It increases cross-run credential, session,
configuration, and outage blast radius without simplifying the completion contract.
An external gateway may be an optional transport adapter; it never replaces the local
guard.

## ADR-027 — cosign envelopes the Gate predicate

The independently produced Gate report becomes a versioned in-toto predicate wrapped
in DSSE and signed with cosign. The statement subject binds the patch digest. The
predicate continues to bind the run UID, resolved-spec digest, base SHA, Gate identity
and generation, image and skill digests, checks, evidence, score, and verdict.

Cryptographic authenticity and semantic correctness remain separate:

- SHA-256 proves retrieved bytes match a digest;
- DSSE/cosign proves a trusted signer covered the statement;
- the in-toto predicate gives the claims a portable shape; and
- AGW's independent Gate evaluation determines whether those claims are true.

Public Fulcio does not accept an arbitrary self-managed k3s ServiceAccount issuer by
default. The first deployment therefore uses a tightly scoped self-managed cosign key
or a supported KMS key and a pinned verification key/trust root. Self-hosting Fulcio
and Rekor is deferred because it creates a new availability, upgrade, and trust-root
system for a single-owner cluster.

Migration is dual-format. The current signed report and the DSSE statement are emitted
and verified over the same corpus. Tampering with the patch, predicate, verdict, or any
identity binding must fail both paths. The old envelope is deleted only after parity,
key rotation, rollback, and retained-bundle verification are proven.

Kyverno or Sigstore policy-controller may later verify runtime/helper images at
admission. Neither replaces the Gate or automatically turns an object-store report
into permission for a later Workflow step.

## ADR-028 — Argo Events is deliberately absent from stable v3

The stable product contract is one declarative `AgentRun`. It does not require an
EventBus, EventSource, Sensor, public webhook ingress, or another ServiceAccount that
can submit work.

When a specific GitHub issue, PR comment, or repository event is requested, a separate
change may add Argo Events. That change must define payload authentication, replay and
duplicate handling, deterministic event-to-`AgentRun` identity, least-privilege Sensor
RBAC, and rollback. Until then, no Argo Events components are installed.

## ADR-029 — the remaining custom core

Agents Gateway owns only responsibilities not supplied by the adopted systems:

1. Gate verification signals and evidence routing: scope, test strength, execution,
   critic corroboration, scoring, and future measured mutation checks.
2. The durable effect ledger and terminal unknown-outcome contract.
3. `agw.runtime.v1` plus hermetic Codex and Claude harness adapters.
4. The `Policy` compiler that produces one canonical rule consumed as context,
   deterministic self-check, and independent Gate check.
5. The immutable `AgentRun` resolver, Workflow translator, type-aware Sandbox bridge,
   and bounded status projection.
6. Node/runtime/airlock preflight for the actual k3s environment.
7. Shadow review evidence and the golden-set pipeline; an external eval engine may be
   evaluated only after real labelled runs exist.

The original design estimated a 3,000–4,500-line landing zone. The independent
2026-08-12 implementation audit measured 71,183 physical non-test Go lines and a
conservative unique-core lower bound of approximately 12,508 lines before several
required production responsibilities. The numeric estimate is therefore an open,
explicit architecture deviation: it must not be presented as achieved or quietly
waived. Argo and agentgateway must delete specific duplicated scheduling and
transport responsibilities after proof, but they do not replace the API, Gate,
runtime, artifact, security, or effect contracts. Every deletion is justified by a
passing equivalence test and rollback release, with the resulting total reported at
each cutover gate.

## Revised implementation sequence

1. Freeze the direct v3 behavior and retain its unit, race, report, effect, retention,
   and publish-ambiguity corpus.
2. Run the Argo/Agent Sandbox disposable-cluster probes: typed condition waiting,
   artifact handoff, deterministic naming, retry behavior, least-privilege RBAC, and
   cleanup.
3. Run the guard/agentgateway disposable-pod probes: credential canaries, exact JSON,
   denied writes, IPv4/IPv6 bypasses, sessions, restart ambiguity, routing, and budget
   accounting.
4. Add opt-in backends behind interfaces. Resolved `AgentRun` inputs remain identical
   across direct and Argo paths; guard decisions remain identical across direct and
   agentgateway transports.
5. Move safe compute and read-only traffic first. Move publication last and only by
   invoking the existing effect-ledger-backed publisher.
6. Emit DSSE evidence beside the current report and run parity/rotation tests.
7. Execute real two-node k3s, disposable-repository, and Codex Luna Max shadow E2E.
8. Delete only responsibilities proven redundant; retain a rollback release through
   the shadow window.

## Hard stop/go gates

The revised architecture is not production-ready until all of these are true:

- Sandbox `Ready` and `Finished` are selected by type, never array position.
- A child artifact is captured only after completion and retains its immutable digest.
- Argo retry/resubmit cannot create a second logical Sandbox, artifact, or publish
  effect.
- The agent can reach the guard and cannot reach agentgateway or any direct upstream.
- Exact-argument and denied-write matrices produce zero upstream calls on rejection.
- Credential canaries never appear in agent-visible state, logs, workspace, or
  retained artifacts.
- Workflow and run ServiceAccounts cannot read Secrets or mutate cluster-scoped
  resources.
- The cosign trust root works for self-managed k3s without relying on unsupported
  public-Fulcio issuer behavior.
- Direct and adopted paths agree on Gate, effect, and terminal-status semantics.
- Rollback and cleanup preserve all evidence needed to reconcile `UnknownEffect`.

Until those checks pass in the disposable and real target environments, this remains a
locally implemented architecture candidate—not a production isolation or correctness
claim.
