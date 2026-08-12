# Agents Gateway v3 — Codebase Review and Cleanup Record

**Review date:** 2026-08-12

**Scope:** the complete v3 source tree, chart, images, Phase-0 harness, release
contracts, and v3 documentation. The review was read-only for the reviewers;
implementation fixes were applied only after the findings were checked locally.

## Review method

Six independent Luna Max reviews covered these disjoint areas:

| Reviewer | Scope |
|---|---|
| James | controller, admission, CRDs, immutable resolution, restart/idempotency |
| Hooke | security, RBAC, chart, NetworkPolicy, Pod/Sandbox posture, images |
| Plato | Argo/Phase-0 scripts, live E2E harness, cleanup and failure handling |
| Godel | broker, agentgateway seam, runtime protocol, adapters, context, skills |
| Laplace | Gate, verification, evidence, object store, attestation, effect ledger, publish |
| Beauvoir | CLI, release workflows, CI, image/dependency reproducibility, docs/config |

Three follow-up Luna Max audits then covered repository hygiene, implementation
reachability, and test/fixture lifecycle. Two focused implementation reviews
addressed publication/configuration findings discovered by that audit.

The findings were cross-checked against the current call graph, generated CRDs,
rendered Helm output, and tests. No hosted GitHub Actions run, production
deployment, provider call, registry push, or external mutation was used.

## High-confidence fixes applied

- Finalizer changes now use the `finalizers` subresource, matching the shipped
  RBAC boundary.
- ToolSet endpoint/argument security validation is shared by admission and
  immutable resolution, so a post-admission mutation cannot widen a run.
- Resolution is checkpointed before its immutable artifact is written. A
  controller restart recovers that checkpoint by digest instead of resolving
  mutable Agent/Gate/ToolSet/ModelRoute/ConfigMap references again.
- ConfigMap-backed instructions use the same bounded, UTF-8, non-empty text
  validation as inline instructions.
- Capture persists its patch descriptor before deleting the work child, and
  cancellation/deletion can recover deterministic children whose status ref was
  lost between create and status persistence.
- Verification clears stale retryable failure state after a successful retry;
  execution isolation failures are projected into bounded status conditions.
- Sandbox and Job deletion use UID preconditions; foreign run Secrets are not
  deleted; ordinary clients cannot remove the cleanup finalizer.
- Findings replay validates finding/PR/head/section identity, URL ownership,
  digest, and strict JSON end-of-input. Signed verification reports are bound to
  the run, resolved spec, base, and patch before attestation.
- Critic capability evidence is now bound to a canonical digest covering the
  endpoint, route, ServiceAccount, token lifetime, issuer/audience, policy
  reference, and pod selector; any configuration drift fails closed.
- The Argo Sandbox bridge treats a deterministic 404 as terminal instead of
  retrying a same-name replacement, and verification-attestation persistence
  reconciles an exact read-after-write before reporting success.
- Runtime completion requires the protocol terminal events (`turn.completed`
  for Codex and successful result data for Claude), and event streams must begin
  with `run.started`.
- Source Secret references and RuntimeClass/StorageClass selection are exact,
  operator-owned allowlists and empty by default.
- Argo lifecycle pods now carry explicit user namespaces, disabled process
  sharing/service links, and RuntimeDefault seccomp. The preflight firewall now
  uses the exact direct workload UID/port policy rather than allowing every
  loopback destination.
- Release/base image inputs and CI tools are digest/version pinned; release
  checks distinguish a real 404 from API/auth/network failure and run all image
  validators before promotion.
- Source pinning now classifies permanent versus transient GitHub outcomes without
  exposing response data, so invalid/unauthorized source refs fail terminally while
  bounded service failures retain the normal retry path.
- Object-store configuration now shares the 64 MiB general hard ceiling across
  the S3 adapter, workload, broker, and lifecycle parsers; the separate
  verify-fetch patch/archive ceiling remains explicit.
- OutputBoth publication persists and replays the created PR number, derives the
  findings target from that exact PR, and keeps patch and findings effects
  independent. Findings-only runs require a pre-existing target and no longer
  project `Published=True`/`PullRequestCreated`; a separate
  `FindingsPublished` condition records the review result. Resolution also rejects
  findings-bearing output when publication is disabled, so `publish.mode: none`
  cannot silently bypass the publication contract.

## Validation evidence

The final constrained local validation passed:

```text
AGW_GOMAXPROCS=2 go test ./...
PATH=/home/ubuntu/.local/bin:$PATH AGW_GOMAXPROCS=2 \
  bash v3/scripts/validate-local.sh --skip-phase0
```

That run passed unit tests, race tests, `go vet`, the reachable Go vulnerability
scan, Bash syntax checks, chart/schema/render checks, the trusted supervisor
contract, all image contracts, CRD generation/drift comparison, and every
production command build. Image builds were intentionally skipped to protect
RAM/disk and no hosted Actions minutes were spent.

Earlier disposable tests also passed the direct provider-free lifecycle gate,
the lower-level Argo/Agent Sandbox preflight, and a two-node nested-k3s Phase-0
run. Those clusters, namespaces, containers, networks, and temporary evidence
directories were removed after testing. The results are recorded in
[`completion-audit.md`](completion-audit.md); they are not a substitute for the
target bare-metal k3s, real provider, object-store, GitHub App, or shadow-window
gates.

## Cleanup decision

The reachability audit found no v3 production package, command, chart template,
image contract, or Phase-0 probe that could be deleted safely: the direct
backend remains the rollback path, Argo/Phase-0 assets are explicit validation
boundaries, and the v3 package graph includes the retained runtime/effect/Gate/
policy contracts. It did identify four unreferenced local wrappers/tests, which
were removed after a second reference scan:

The following generated artifacts were removed because they are not source or
release inputs:

- `v3/scripts/fixtures/objectstore/__pycache__/`
- `tests/__pycache__/`
- `agents_gateway/__pycache__/`
- `agents_gateway/harness/__pycache__/`
- `v2/web/node_modules/`

The following unreferenced v3 helpers were also removed:

- `v3/phase0/agentgateway-guard/Containerfile.agentgateway`
- `v3/phase0/agentgateway-guard/Containerfile.alpine`
- `v3/phase0/agentgateway-guard/Containerfile.netshoot`

The focused `v3/scripts/local-k3s-smoke.test.sh` interface test was retained:
it protects the explicit upstream-install forwarding and invalid-argument
guards in the disposable harness.

The final reachability cleanup also removed unused internal aliases and wrappers
for static AgentRun validation, work/verify Sandbox builders, and findings
artifact encoding, the unused cosign dry-run predicate helper, and the
test-only pre-agentgateway critic Job builder. Critic contract tests now exercise
the production builder directly. The remaining public skills aliases are kept
because `pkg/skills` is an external package surface; internal legacy secret-name
and opaque-token shims were removed after confirming they had no production
callers.

The Phase-0 Sandbox installer also now rejects unchecked local manifest copies,
requires an immutable controller image reference when it installs upstream,
pins that image in the verified release manifest, and fails before lifecycle
checks if installation did not complete. The condition probe records both
`Ready=True` and `Finished=True/PodSucceeded` before exercising shutdown cleanup.

The disposable direct live gate now rejects existing generated namespace names,
deletes only its exact generated namespaces by default, and supports
`AGW_DIRECT_KEEP=1` for explicit inspection. The local Kind smoke checks for an
exact cluster-name collision before enabling its exact-name cleanup trap.

The legacy `agents_gateway/` and `v2/` trees were deliberately retained. The
accepted design still requires direct-vs-Argo equivalence, rollback readiness,
release evidence, and shadow data before deleting them. Removing them now would
destroy the documented fallback rather than clean dead code.

## Remaining findings / external gates

The review did not justify speculative rewrites or deletion of safety code. The
remaining work is explicitly tracked as partial or external in the completion
audit: target-node isolation proof, full Argo/direct equivalence, real
agentgateway/provider and object-store/GitHub App paths, KMS/trust-root and
rotation evidence, fault/restart drills, and a human-labelled shadow window.
The Argo chart README now also documents that Workflow submission must remain
operator-only; an external RBAC grant for arbitrary Workflow creation is outside
the chart's safe boundary and must be blocked or separately admitted.
