# Argo lifecycle producer

Status: implemented and integration-validated; not deployed or live Sandbox
execution-verified.

The Argo backend is an opt-in sequencing backend. Argo owns ordering, retry,
workflow deadlines, and pod garbage collection. The lifecycle image performs
one bounded operation per template; it does not contain an AgentRun phase
machine. Gate evaluation, effect-ledger decisions, publication, and terminal
AGW success remain controller-owned.

## Workflow contract

The `agw-agent-run-lifecycle` WorkflowTemplate runs:

1. `prepare` loads the admitted AgentRun and the digest-bound resolved
   snapshot, then validates the operator-owned deterministic work Sandbox. It
   does not create a Sandbox or a run Secret. The Sandbox's existing
   clone/skills/context/lockdown/broker helpers perform the work setup. The
   step emits only deterministic names and the validated `spec.limits.timeout`
   for downstream waits.
2. `stage` reloads the resolved snapshot through the projected run-scoped
   object-store credentials and writes exactly:
   `/workspace/.agw/resolved-spec.json` and
   `/workspace/.agw/capture-spec.json`. Existing files must be byte-identical
   on retry. The stage and capture pods have no service-account token.
3. `wait-ready` and `wait-finished` call the existing condition-array bridge.
   They receive the timeout emitted by `prepare`; there is no hard-coded
   45-minute fallback. Finished also requires the authenticated ready marker.
4. `capture` uses the existing digest-pinned capture helper and the staged
   capture contract. It validates and persists patch, manifest, and result
   artifacts; it has an explicit empty egress policy and no DNS allowance.
5. `handoff` reloads the live AgentRun/Workflow identity and projected
   object-store credentials, authenticates the runtime event stream and
   completion record, validates capture artifacts, and writes the sole
   reserved `agw-lifecycle-output` parameter. Handoff retains its lifecycle
   service-account token because it must bind the live Workflow UID and
   generation.

The output is constructed from authenticated, content-addressed artifacts. No
agent-authored workspace file can supply the lifecycle JSON. Handoff verifies
the resolved snapshot identity, capture descriptors, exact runtime completion
bytes, terminal event digest, Workflow binding, and canonical output before it
writes `/tmp/agw-lifecycle-output.json`.

## Failure cleanup

The Workflow has `onExit: cleanup`, but that template is a tokenless lifecycle
marker only. Its ServiceAccount has no Role or RoleBinding, and the command
only validates that the immutable lifecycle identity was propagated. The
trusted AgentRun controller deletes per-run credential Secrets and owns
Sandbox/PVC cleanup; Argo retry pods never receive namespace-wide Secret
mutation. Patch/runtime artifacts and verification evidence are not deleted by
the exit step.

## Network boundary

Each lifecycle pod has a role label and the chart emits additive,
least-privilege policies in `agw-runs`:

- `prepare`, `wait`, and `handoff`: exact Kubernetes API CIDR/port only;
- `stage` and `handoff`: TCP 443 only to the configured object-store CIDRs;
- `capture`: explicit empty egress, with the shared DNS policy excluding its
  `capture` label.

The API CIDR comes from `preflight.apiServerCIDR`; the API port is the explicit
`argo.apiServerPort` value. The chart refuses an Argo render without
`argo.objectStoreEgressCIDRs` and rejects unrestricted `/0` entries. Use exact
stable endpoint CIDRs or a private egress path when the object-store provider's
addresses are dynamic.

## Configuration boundary

`prepare` alone receives GitHub, root object-store, artifact-STS, and helper
image configuration. `stage` and `handoff` receive only run identity,
non-secret object-store settings, and projected per-run object-store files.
`cleanup` receives only run identity and no Kubernetes API token. This keeps
the lifecycle templates aligned with the command's least-privilege config
loaders.

## Verification performed

Local checks cover Go unit/contract tests, exact staging paths, policy execution
and Gate projection, command/config dispatch, chart source/schema checks, and
lifecycle image immutability checks. Helm v4.2.3 is available locally and the
rendered chart assertions pass after the lifecycle security edits.

The provider-free lifecycle fixture
`TestProviderFreeLifecycleBindsSandboxEvidenceThroughArgo` composes the real
translator/backend, producer, runtime completion collector, capture validator,
Sandbox condition bridge, and reserved Workflow output. It proves the local
AgentRun -> Workflow -> Sandbox -> evidence -> status path, including the
fail-closed rejection of a `Succeeded` Workflow with no authenticated handoff.
It uses only an in-memory Kubernetes client/object store and does not claim
that an Argo controller or Agent Sandbox pod ran.

The current tree also has the following integration evidence from the review
environment:

- focused Go tests pass;
- Helm v4.2 lint passes;
- dependency-free chart checks pass;
- Argo CLI v4.1.0 strict offline WorkflowTemplate lint passes; and
- The direct chart backend passes Kubernetes server-side dry-run on a disposable
  local Kind API server.
- The official Argo Workflows v4.1.0 full CRD Kustomize path installs
  server-side, all eight Argo CRDs establish, and the complete Argo chart passes
  Kubernetes server-side dry-run. Client-side apply must not be used for these
  large CRDs because the last-applied annotation exceeds Kubernetes' 256 KiB
  annotation limit.

The reproducible upstream CRD installation is:

```bash
kubectl apply --server-side \
  -k 'https://github.com/argoproj/argo-workflows/manifests/base/crds/full?ref=v4.1.0'
```

These checks validate rendering, schema/API shape, RBAC, NetworkPolicies, and
WorkflowTemplate wiring. They do not prove that an Agent Sandbox can execute
on a real node. Live Sandbox creation, readiness/finish observation, capture,
and authenticated handoff remain explicitly pending. No deployment, live
cluster mutation, commit, push, or GitHub Actions run was performed by this
worktree.
