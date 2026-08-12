# Agents Gateway v3 — Kubernetes-native implementation plan

Status: local implementation candidate; live Phase-0 and release gates remain

Prepared: 2026-08-11

Target: two-node k3s, one operator, trusted submitter

AGW API: `agents.astatide.com/v1alpha1`

Upstream sandbox API: `agents.x-k8s.io/v1beta1`

Architecture overlays accepted on 2026-08-11:

- [`quality-architecture-addendum.md`](quality-architecture-addendum.md) governs
  ADR-015 through ADR-024.
- [`build-vs-adopt-addendum.md`](build-vs-adopt-addendum.md) governs ADR-025
  through ADR-029 and supersedes any broader claim in this plan that Argo or
  agentgateway can replace the AGW trust/effect contract by itself.

## Implementation checkpoint — 2026-08-11

Implemented and locally verified:

- exactly seven AGW CRDs, generated manifests, CEL validation, fail-closed
  dynamic admission, and a second execution-time preflight check;
- canonical immutable resolved snapshots, exact base-SHA pinning, direct Agent
  Sandbox v1beta1 reconciliation, hardened work/capture/verify workloads, and
  immediate terminal Secret/Sandbox cleanup;
- the Codex runtime adapter, strict `agw.runtime.v1` event stream, Kubernetes-
  observed process-exit finalization, networkless patch capture, canonical
  changed-file manifests, and digest-bound object storage;
- the loopback-only broker, ToolSet exact-JSON policy, model/tool/cost limits,
  skills materialization, runtime artifact upload, and durable effect ledger;
- per-run, exact-prefix STS session policies with no ambient credential chain
  in the broker, plus phase-specific short-lived clone/artifact credentials;
  verification receives a fresh read-only lease whose lifetime bounds the Gate
  timeout, never the older work-phase Secret;
- independent fresh-checkout verification, offline patch apply, scope/diff/
  binary/coverage checks, `newTestsMustFailOnBase`, and operator-signed Gate
  reports;
- controller-owned GitHub App publication with deterministic branch/commit/PR,
  signed evidence verification, claim-before-mutation idempotency, and terminal
  `UnknownEffect` handling;
- Helm/RBAC/NetworkPolicy/quota boundaries, `kubectl-agw`, Phase-0 probes, a
  dry-run-first retention policy engine, and CI that builds all fifteen images;
- an explicit `agent-sandbox|job` execution-backend selector whose watches,
  RBAC, quota, child identity, restart fingerprint, process evidence, and CLI
  log resolution remain backend-specific; Job mode has no Sandbox-CRD REST
  mapping dependency;
- restart-safe controller-computed child plan fingerprints, fail-closed task
  ConfigMap validation, cleanup that reaps run credentials before loading
  object-store state, verifier-owned Go coverage/test-strength evidence, and a
  race-free Claude subprocess lifecycle;
- all fifteen local container images—including the Helm-deployed operator,
  recurring preflight attestor, and both runtime harnesses—build
  from the repository-root context,
  credential-bearing helpers were inspected for direct regular `0400` mount
  targets, and the networkless Codex container E2E passes the complete
  six-event runtime/artifact contract.

The stable local tree also passes the uncached full unit suite, full race suite,
`go vet`, `govulncheck`, all image/static contracts, both Helm backend renders,
and Kubernetes server-side dry-run for the CRDs, samples, and both chart modes.
The official Argo v4.1.0 full CRD Kustomize path must be installed with
server-side apply because client-side apply cannot store its large
last-applied annotation; the complete Argo chart then passes API-server dry-run.
The Kind cluster did not contain the upstream Sandbox CRD when Job mode was
validated; this proves API independence, not live workload isolation.

Still required before release-candidate or live deployment:

- provision/select the actual `node-cp` and `node-agents`, provide a kubeconfig,
  and run every Phase-0 probe on that real k3s/containerd/kernel combination;
- use those results to confirm the UID airlock or select the separate-broker-
  pod fallback, and prove the real Agent Sandbox/PVC lifecycle before enabling
  the already-wired retention applier;
- use the new scoped preflight command/image and Helm Job/CronJob to turn the
  Phase-0 contract into a recurring attestor that refreshes
  `agw-system/agw-preflight` on chart start/upgrade and on the recurring
  schedule; the real cluster still has to prove the assumptions before
  `preflight.airlockProven` is enabled;
- create the production bucket/IAM role, GitHub App Secret, Ed25519 signing
  Secret, build and push every image, and record registry manifest digests;
- execute the disposable-repository and Jobmark shadow-mode E2E, restart/fault
  injection, cleanup, restore, and rollback drills;
- collect the per-repo/per-Gate shadow confusion matrix before enabling
  enforcing mode. The bounded retention inventory/applier is wired in dry-run
  mode, but remains guarded until live lifecycle evidence makes deletion safe.

No v1/v2 deletion is authorized until those release gates pass.

This plan turns the v3 product proposal into an executable migration from the
repository that exists today. It is intentionally gated: Phase 0 proves the
security and lifecycle assumptions before an adopted backend replaces the
already-verified direct implementation or authorizes deletion.

## 1. Executive decision

Proceed with v3 only if the first target run can be stated concretely. The
recommended acceptance sentence is:

> Point Agents Gateway at one reproducible Jobmark bug, let Codex produce a
> patch, independently verify it, and open a shadow-mode pull request carrying
> enough evidence for the owner to judge the Gate without reconstructing the
> run.

Build v3 beside v2 in a new `v3/` Go module. Do not import v2 packages as a
dependency and do not delete either existing implementation until v3 completes
a release-candidate cycle, a rollback drill, and the shadow-mode threshold.

The planning baseline as of 2026-08-11 is:

- Go 1.26.5 or newer. The floor excludes four reachable standard-library
  vulnerabilities present in 1.26.3; CI runs `govulncheck` against reachable
  call paths.
- k3s `v1.36.3+k3s1`, currently bundling Kubernetes 1.36.3, containerd 2.3.2,
  and runc 1.4.2.
- `kubernetes-sigs/agent-sandbox` v0.5.4 and its `Sandbox` v1beta1 API.
- Direct `Sandbox` resources only in v3.0. Do not depend on
  `SandboxTemplate`, `SandboxClaim`, or `SandboxWarmPool`.
- Local-path PVCs are disposable workspace storage, never authoritative
  evidence storage.
- One k3s server plus one k3s agent is not highly available. This is an
  accepted single-operator tradeoff, not an HA claim.
- Production manifests use versions and image digests proven by Phase 0.
  Automation may propose upgrades to the latest release, but production never
  floats on `latest`.

Current primary sources:

- [K3s v1.36.3 release](https://github.com/k3s-io/k3s/releases/tag/v1.36.3+k3s1)
- [Agent Sandbox v0.5.4 release](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.4)
- [Kubernetes user namespaces](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/)
- [Agent Sandbox API migration guide](https://agent-sandbox.sigs.k8s.io/docs/getting_started/api-migration-guide/)

## 2. Amendments required before implementation

These are corrections to the supplied proposal, not optional refinements.

| Area | Amendment |
|---|---|
| Upstream API | Use `agents.x-k8s.io/v1beta1`. The current upstream API is no longer alpha-first. |
| Sandbox dependency | Generate a complete hardened `Sandbox.spec.podTemplate` internally. `SandboxTemplate` is an extension CRD and conflicts with ADR-002's core-`Sandbox`-only dependency. |
| Seven-CRD ceiling | The stable quality API owns seven **AGW-owned** CRDs. The adopted upstream `Sandbox` CRD does not count as an AGW CRD; `EvalSuite` remains deferred. |
| User namespace field | `hostUsers: false` belongs directly on `PodSpec`, not under `securityContext`. |
| Lockdown user | The `lockdown` init container must run as UID 0 inside the user namespace with only `NET_ADMIN`; the agent and broker remain non-root. A pod-wide `runAsNonRoot: true` would prevent lockdown from working. |
| Preflight state | A Kubernetes `Lease` has no custom `status.conditions`. Store the attested result in `agw-system/agw-preflight` ConfigMap and node labels; use Events and metrics for diagnostics. |
| Preflight timing | The experimental preflight is Phase 0. The production preflight and fail-closed admission webhook ship in Phase 1, before the first admitted run—not Phase 5. |
| Immutable revisions | A digest alone does not preserve a referenced object after that object changes. Persist the complete canonical resolved-run snapshot to object storage and record its URI and digest in status. |
| Status | `.status` is a bounded projection, not an event database. Logs, patches, full reports, resolved specs, and ledgers stay in object storage. Target less than 64 KiB per `AgentRun`. |
| Network policy | Cover IPv4 and IPv6, Pod/Service CIDRs, node addresses, `100.64.0.0/10`, metadata, link-local, UDP, DNS, and the API server. RFC1918 exclusions alone are insufficient. |
| PVC lifecycle | Do not assume `shutdownTime` preserves PVCs. Prove pod, Service, PVC, PV, and local-path-directory behavior with the selected upstream release. |
| Verification apply | Disable Git hooks, filters, credential helpers, submodules, and smudge processing before applying an untrusted patch. Applying data must not execute repository code. |
| Capture networking | Capture is networkless and credential-free. It emits one bounded stdout frame; the operator validates and uploads the patch and canonical publish manifest. |
| Signed report | The verify pod emits normalized observations. The operator validates and signs the canonical report; the signing key is never mounted into the verify pod. |
| Cancellation | Add terminal phase `Cancelled`. Cancellation is operationally distinct from `Failed`. |
| Codex port | Preserve behavior and tests, but do not claim a literal directory copy. The standalone v3 adapter now uses v3 runtime, artifact, and broker contracts; the remaining live gate is provider/runtime acceptance. |

## 3. Product and trust boundary

The product is the completion contract:

```text
agent says complete
        |
        v
patch captured and content-addressed
        |
        v
fresh verifier applies patch to exact base SHA
        |
        v
Gate emits machine-checkable verdict
        |
        v
controller publishes evidence and, if configured, a PR
```

The submitter and cluster owner are trusted. Agent-authored code, repository
scripts, downloaded skills, tests, and artifact content are not trusted.

The minimum security claims are:

1. Agent code cannot directly reach external networks, the Kubernetes API,
   node services, metadata endpoints, or other runs.
2. Agent code cannot read clone, model, MCP, object-store, signing, or publish
   credentials.
3. Only the policy broker can create approved external effects.
4. Verification starts from the recorded base SHA and patch digest and never
   mounts the work PVC.
5. A missing or stale preflight result prevents new runs.
6. An ambiguous external mutation is never retried blindly.

If UID-based egress filtering fails, the fallback is selected immediately:

```text
setup Job -> credential-free Agent Sandbox -> per-run Broker Pod -> upstreams
                              |
                              +-> independent Verify Sandbox
```

The fallback gives the agent and broker separate network namespaces. A Service
and per-run NetworkPolicies allow only agent-to-broker traffic and broker
egress to approved upstreams. No phase after P0 assumes which topology won;
both implement the same `SandboxPlan` and broker interfaces.

## 4. Cluster and namespace plan

```text
node-cp                                 node-agents
k3s server                             k3s agent
control plane is not HA                taint: agw.astatide.com/agents=true:NoSchedule
                                       label: agw.astatide.com/agents=true

agw-system                             agw-runs
- agw-operator                         - Agent/AgentRun/Gate/ToolSet/ModelRoute
- agent-sandbox controller             - work and verify Sandboxes
- webhook Service                      - capture Jobs
- preflight result                     - per-run Secrets
- GitHub App private key               - workspace PVCs
- provider/source credentials          - optional separate Broker Pods
```

k3s is installed with Traefik and ServiceLB disabled. Cloudflared is not a
dependency of the execution path; add it only if a later operator endpoint
needs ingress. The v3 product itself has no public HTTP API or web console.

`agw-runs` receives ResourceQuota and LimitRange objects. Pod creation is not
granted to normal AGW submitters. Because lockdown requires namespaced
`NET_ADMIN`, Pod Security Admission may require an exception for this namespace;
if so, a validating policy must admit only operator/agent-sandbox-created pods
whose immutable shape and labels match the resolved run snapshot.

All seven AGW CRDs are namespaced. v3.0 supports the single `agw-runs` namespace,
but the API does not hard-code that name. Credential references are logical
names resolved by the operator from `agw-system`; direct cross-namespace owner
references are never used. Kubernetes forbids cross-namespace namespaced owner
references, so all per-run children live with their `AgentRun` in `agw-runs`.

## 5. API contract

The AGW-owned CRDs remain:

1. `AgentRun`
2. `Agent`
3. `Gate`
4. `ToolSet`
5. `ModelRoute`
6. `Policy`
7. `ContextStrategy`

The quality contract and its deferred features are tracked in
[`quality-architecture-addendum.md`](quality-architecture-addendum.md).

CRD OpenAPI schemas and `x-kubernetes-validations` handle static shape,
mutual-exclusion, ranges, enums, and transition rules. The webhook handles
dynamic preflight state, referenced-resource existence, allowed images, and
cluster-dependent policy.

### Production preflight attestor

The production implementation is intentionally outside the operator reconcile
loop. `v3/cmd/agw-preflight` is a bounded one-shot command used by a Helm
post-install/post-upgrade Job and a `Forbid` CronJob. The Job has no retries and
has a 120-second Kubernetes deadline; the command itself has a 90-second hard
deadline. Both run on the selected `agw.astatide.com/agents=true` node with
`hostUsers:false`.

The pod has one init container, and only that init container has namespaced
`NET_ADMIN`; it installs the v3 IPv4/IPv6 owner rules. The attestor writer runs as UID 1337 with an
explicit 10-minute projected API token and can update only the named
`agw-preflight` ConfigMap. The attestor and second UID-1000 container are
regular containers and start concurrently after lockdown; the UID-1000 probe has no token mount,
Secret, host mount, or ambient credential and must fail DNS and API-server
egress. The shared evidence volume uses a bounded fsGroup. The writer checks
both UID/GID maps, host namespace identity, the kubelet filesystem family, and
selected kernel/k3s/containerd/runc fingerprints.

The writer first records a non-passing result, then publishes a passing
`result.json` only after every check and the final ConfigMap update succeed.
The strict five-field JSON shape remains the contract consumed by admission;
bounded diagnostic details are kept separately in `checks.json`. Missing
evidence, missing host files, failed network enforcement, a fingerprint
mismatch, an unavailable API, or a malformed probe report cannot produce a
passing result.

The hostPath mounts remain in this workload because the node/runtime checks
need them. Kubernetes requires every volume filesystem in a `hostUsers:false`
Pod to support idmapped mounts, including hostPath filesystems; the command
fails closed for an unknown filesystem family and the chart requires
`hostPathIdmapRequired: true`. See the primary
[Kubernetes user namespace filesystem documentation](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/#filesystem-support).

The implementation does not claim live proof. The actual kernel's
`hostUsers:false` behavior, namespaced `NET_ADMIN`, `xt_owner`, hostPath
readability and idmapped-mount support, k3s service CIDR, DNS policy, and configured runtime binary paths
remain Phase-0 assertions on the real cluster. Helm refuses to render the
attestor until `preflight.airlockProven` is explicitly set after that review.

### AgentRun

Keep the supplied spec shape with these changes:

- `workspace.existingClaim` is disabled in v3.0. It weakens cleanup and
  isolation guarantees before there is a demonstrated need.
- `spec.cancelRequested` is an optional one-way boolean used by
  `kubectl agw cancel`.
- `source.repo` is parsed into a canonical host/owner/repository tuple; free-form
  clone URLs and embedded credentials are rejected.
- `baseRef` is resolved by the controller to `baseSHA` before a sandbox exists.
- Runtime, verifier, and helper images must be digest-pinned in the resolved
  snapshot even if a friendly tag appears in a source `Agent`.

Status adds these bounded fields:

```yaml
resolvedSpecRef: { uri: s3://..., digest: sha256:... }
eventStreamRef:  { uri: s3://..., digest: sha256:... }
failure:         { code: "", message: "" }
startedAt:       ""
completedAt:     ""
```

Terminal phases are `Succeeded`, `Rejected`, `Failed`, `Cancelled`, and
`UnknownEffect`. `Published=True` may coexist with terminal `Rejected` in
shadow mode.

### Agent

`Agent` selects the harness, digest-pinned runtime image, instructions,
ToolSet, ModelRoute, and digest-pinned skills. Remove `sandboxTemplateRef` from
v3.0. Keep optional `runtimeClassName` as the future gVisor/Kata upgrade point.
The operator owns the secure pod template.

Codex and Claude Code have separate, locally verified runtime adapters and
digest-built images. Codex remains the recommended first live harness because
its six-event container contract has already been exercised end to end. Claude
Code routes its native Anthropic Messages protocol through the same loopback
broker boundary with dummy child credentials; it becomes production-supported
only after a real provider and two-node shadow run pass.

### Gate

`Gate.verify` includes a digest-pinned verifier image, command list, timeout,
and normalized result adapters. Commands use an explicit `argv` form by
default. A shell command is allowed only as an explicit `{shell: "..."}` entry
executed by the verifier's controlled shell with a fixed environment and
bounded output.

`newTestsMustFailOnBase` is implemented by a language-aware test-file adapter.
Go is the first adapter. The generic controller does not guess test files or
parse arbitrary coverage formats.

The report schema includes:

- run UID, `specDigest`, and resolved-spec digest
- base SHA and patch digest
- runtime, verifier, and helper image digests
- skill digests and Gate UID/generation
- every command, bounded output digest, exit code, and duration
- every require-rule observation and verdict
- report schema version, report digest, and operator signature

### ToolSet and ModelRoute

ToolSet exact argument constraints keep v2's strict semantics. The Phase-0
agentgateway spike rejected adoption for authorization (P0-E): evaluated v1.4.1
captures `mcp.tool.arguments` into its telemetry context, but its MCP
authorization evaluator constructs a fresh request-time `MCPInfo` from a
`ResourceType` containing only target and tool name. Argument values therefore
cannot participate in the authorization decision, so it cannot preserve the
v2 exact matcher. agentgateway remains an optional future routing and
observability component, not the v3.0 capability boundary.

ModelRoute uses explicit ordered providers and one budget. It does not silently
fall through to an unlisted paid model. Credential values never enter CRDs,
status, logs, or resolved snapshots.

## 6. Repository layout

Create a standalone module:

```text
v3/
  go.mod
  api/v1alpha1/
  cmd/
    agw-operator/
    agw-broker/
    agw-capture/
    agw-verify/
    agw-runtime-codex/
    agw-runtime-claude/
    kubectl-agw/
  internal/
    admission/
    artifacts/
    canonical/
    controller/
    credentials/
    effects/
    fsm/
    githubapp/
    preflight/
    retention/
    sandbox/
    status/
    verification/
  pkg/
    artifactcatalog/
    proto/
    runtimeproto/
    strictjson/
    toolpolicy/
  config/
    crd/
    rbac/
    webhook/
    networkpolicy/
    samples/
  charts/agw-operator/
  images/
  test/
    contract/
    integration/
    e2e/
    fixtures/
```

Portable contract packages under `pkg/` have no Kubernetes imports. The v3
module does not import `github.com/Astatide1337/agents-gateway/v2`; contract
files and tests are copied, import paths are changed, and dependencies are cut
at explicit interfaces.

The sandbox abstraction is:

```go
type SandboxBackend interface {
    Ensure(context.Context, SandboxPlan) (SandboxRef, error)
    Observe(context.Context, SandboxRef) (SandboxObservation, error)
    Delete(context.Context, SandboxRef) error
}
```

The primary backend creates upstream `Sandbox` v1beta1 resources. The Job
fallback is retained and tested, but not advertised as equivalent persistence
unless its acceptance suite passes.

## 7. Port, adapt, and delete map

The repository currently contains exactly 28,135 non-test Go LOC in v2. The
Python deletion estimate of 25,572 LOC includes its tests.

### Port contract and tests first

| Source | v3 destination | Treatment |
|---|---|---|
| `v2/proto/runtime.go` | `v3/pkg/proto` | Copy contract and tests. |
| `v2/pkg/runtimeproto` | `v3/pkg/runtimeproto` | Copy behavior; preserve strict JSONL bounds. |
| `v2/pkg/strictjson` | `v3/pkg/strictjson` | Preserve for ToolSet exact matching. |
| `v2/pkg/artifactcatalog` | `v3/pkg/artifactcatalog` | Preserve descriptor and safe-rendering contract. |
| `v2/pkg/toolpolicy/policy.go` | `v3/pkg/toolpolicy` | Preserve evaluation semantics. |
| `v2/pkg/skills/materialize.go` | `v3/pkg/skills` | Preserve traversal, link, type, size, and digest checks. |
| `v2/pkg/toolbroker/audit.go` | `v3/internal/broker` | Preserve privacy and fail-closed audit behavior. |

Preserve, at minimum, the runtime protocol, strict JSON, artifact contract,
skill materialization, exact arguments, tool fault-injection, and upstream
security tests before moving implementations.

### Adapt behind new interfaces

- `v2/pkg/codexadapter`: retain private `CODEX_HOME`, loopback provider,
  cancellation, event sequencing, bounded diagnostics, and artifact handling;
  replace v2 workflow/artifact types and Unix broker assumptions.
- `v2/pkg/skills/gateway.go`: retain MCP session, endpoint, redirect, response,
  digest, and credential-lifetime hardening for the skills init container.
- `v2/pkg/toolbroker`: retain authorization ordering, MCP behavior, exact
  matching, audit, and unknown-effect semantics; replace PostgreSQL policy,
  credential, and ledger implementations.
- `v2/pkg/modelbroker` and `modelroute`: adapt to resolved `ModelRoute` and
  per-run Secret inputs.
- `v2/pkg/artifact`: retain bounded immutable S3-compatible uploads and adapt
  keys to namespace/run UID.
- `v2/pkg/spec`: retain canonical digest and one-file compiler ideas; replace
  API types and most validators with CRDs/CEL.
- `agents_gateway/harness/cleanup.py`: port behavior, not code, into the
  retention controller.
- `agents_gateway/harness/verification.py`: retain bounded execution and
  failure-classification ideas in the independent verifier.

### Delete only after cutover

Delete Python v1, the v2 console/API/store/orchestration/runner/sandbox code,
Compose assets, PostgreSQL migrations, and obsolete CI in one dedicated
post-cutover PR. Tag the last known-good v2 release first and retain its image
digests and deployment manifest for rollback.

## 8. Reconcile and durability design

The controller uses deterministic child names derived from the immutable run
UID. Every child carries the run UID, `specDigest`, and controller version.
`CreateOrGet` accepts an existing child only when its immutable digest matches;
otherwise the run fails closed.

```text
Pending
  -> add finalizer
  -> verify fresh preflight attestation
  -> resolve and canonicalize references
  -> write immutable resolved-spec artifact
  -> resolve baseSHA
Cloning/Working
  -> create per-run Secret and workspace PVC
  -> create work Sandbox
  -> observe Sandbox/Pod conditions
Capturing
  -> create a zero-egress, credential-free capture Job on the workspace node
  -> operator reads one bounded frame, validates it, and uploads patch plus
     canonical changed-file manifest and independent digests
  -> stop work Sandbox only after evidence is durable
Verifying
  -> create fresh verify Sandbox with no work PVC
  -> operator reads and persists bounded normalized observations
Gated
  -> operator evaluates and signs canonical report
  -> Accepted or Rejected
Publishing
  -> claim publish effect
  -> create deterministic GitHub branch/commit/PR
Terminal
  -> flush evidence, delete Secrets and active children
  -> retention policy owns remaining PVC/artifact lifecycle
```

Reconciliation never blocks waiting for a process and never sleeps. It watches
child resources, uses deadlines, and requeues. Controller restarts are injected
after every transition in E2E.

### Effect ledger without PostgreSQL

The authoritative ledger is an immutable object-store protocol, not status and
not a best-effort local file:

```text
PUT claims/<effectHash>.json  If-None-Match: *
  -> durable claim before any external mutation
call upstream once
PUT outcomes/<effectHash>/<state>.json  If-None-Match: *
  -> succeeded | failed | unknown
```

An existing claim with the same request digest returns its recorded terminal
result. A different digest is a hard conflict. A claim without a terminal
outcome after restart becomes `unknown` and is never retried automatically.
Large results are separate content-addressed objects.

This design requires strong read-after-write and conditional-create behavior.
Phase 0 runs a conformance test against the selected production S3-compatible
backend. MinIO is the CI/E2E fixture, not an automatically added production
stateful service. If the chosen backend cannot meet the contract, the fallback
is a narrow operator-owned ledger service backed by Kubernetes ConfigMaps;
`AgentRun.status` is never the ledger.

Publishing uses the same effect interface plus deterministic GitHub branch and
PR markers. A timed-out GitHub call is reconciled only through read-only lookup;
if the outcome cannot be proven, the run enters terminal `UnknownEffect`.

## 9. Work and verify pod rules

### Work Sandbox

Init order is `clone -> skills -> lockdown`. Clone and skills receive separate,
single-purpose credentials mounted only into their own containers. Clone uses
an installation token reduced to `contents:read`, checks out exact `baseSHA`,
removes credentials from Git configuration, disables hooks, and leaves no token
in the workspace.

The lockdown container:

- runs as UID 0 inside `hostUsers: false`
- has only namespaced `NET_ADMIN`
- installs loopback allow, broker UID allow, then default drop
- exits before the agent and broker start
- never grants `NET_ADMIN` to a long-lived container

The agent is UID 1000 with drop-all capabilities, read-only root filesystem,
no Secret mounts, no service-account token, and no direct egress. The broker is
UID 1337 with only the exact per-run Secret mounts and upstream policy.

The runtime synchronously POSTs one strict `agw.runtime.v1` event at a time to
the loopback broker, which persists the bounded stream to object storage;
stdout is not used as a sidecar pipe. The operator independently observes the
Kubernetes `agent` container termination state and finalizes the persisted
stream. A runtime-authored terminal event alone cannot create a completion
record. Status stores only counters, terminal summaries, and an event-stream
reference.

### Verify Sandbox

The verify flow is `fresh clone -> fetch and digest-check patch -> safe apply ->
lockdown -> execute`. Network-capable steps do not execute repository code.
The execution container has unconditional zero egress, no broker, no skills,
no work PVC, no publish credential, and no signing key.

If verification needs dependencies, a trusted init step may fetch only from the
recorded base revision and lockfiles before the patch is applied. Install scripts
and repository hooks remain disabled. Agent-authored code starts only after
lockdown; otherwise the Gate fails closed and requires a prebuilt verifier image.

`newTestsMustFailOnBase` uses a second pristine base tree. It copies only files
classified as tests by the Gate adapter and requires the configured test command
to fail on base while the full patch passes. A missing adapter is a failed Gate,
not a skipped check.

## 10. GitHub publish path

Use a GitHub App with `contents:write` and `pull_requests:write`, installed only
on opted-in repositories. GitHub installation tokens are repository/permission
scoped and expire after one hour. The private key remains mounted only in the
operator. See [GitHub installation tokens](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app).

Capture produces both `patch.diff` and a content-addressed changed-file manifest
for accepted non-binary changes. The operator can publish through GitHub's Git
data/commit APIs without executing repository code:

1. Claim `sha256(runUID | baseSHA | patchDigest | "publish/pr")`.
2. Re-read and verify `baseSHA`.
3. Create `agw/<run-name>-<uid-short>` from `baseSHA`.
4. Create the commit from the verified changed-file manifest.
5. Open the PR and add `agw/accepted` or `agw/rejected`; add `agw/shadow` when
   applicable.
6. Put the report digest, resolved-spec digest, image/skill digests, base SHA,
   patch digest, and effect marker in the PR body.
7. Commit the ledger outcome and project the URL into status.

Never push to or merge `baseRef`.

## 11. Phase 0 — prove assumptions

Duration: 5–7 focused engineer-days. No operator implementation beyond a
throwaway probe harness and repository scaffold.

### P0-A: preserve the baseline

- Tag and record the current v2 commit, image digests, deployment manifest, and
  `go test ./...` result.
- Copy the portable contract tests into a quarantined v3 test package.
- Record actual node candidates; do not assume the proposal's Dokploy/Coolify
  wording identifies the machines correctly.

### P0-B: node and runtime floor

- Install the current supported k3s release on disposable target nodes.
- Record kernel, k3s, Kubernetes, containerd, runc, cgroup, CNI, iptables
  backend, Pod/Service CIDRs, and actual kubelet root.
- Verify idmapped mounts for kubelet pods, tmpfs/emptyDir, Secret, ConfigMap,
  and local-path PVC filesystems.
- Confirm the agent node label/taint and resource floor.

### P0-C: user namespace and airlock

Run a two-container matrix using UIDs 0, 1000, and 1337. Prove:

- the pod UID map is non-host and unique
- the lockdown rule changes only the pod network namespace
- UID 1337 reaches a controlled public endpoint
- UIDs 0 and 1000 cannot use IPv4/IPv6 TCP, UDP, DNS, HTTPS, raw sockets, or
  alternate proxy variables
- the agent cannot change UID, gain a useful capability, alter rules, or run a
  setuid escape
- the API server, Pod/Service CIDRs, node addresses, RFC1918, CGNAT,
  link-local/ULA, and metadata endpoints are unreachable
- rules survive container restart and are recreated on Sandbox replacement
- credential canaries are absent from environment, arguments, `/proc`, logs,
  workspace, and Git config

### P0-D: Agent Sandbox lifecycle

- Install Agent Sandbox v0.5.4 core only.
- Create direct v1beta1 `Sandbox` resources with user namespaces, multiple
  containers, local-path PVCs, `shutdownTime`, and `shutdownPolicy` variants.
- Record Pod, Service, PVC, PV, host-directory, Finished/Ready/Suspended, and
  deletion behavior.
- Verify that work/capture node affinity is preserved.
- Exercise the `Job` backend fallback once.

### P0-E: agentgateway decision

**Current decision overlay.** The latest evaluated release is agentgateway
v1.4.1 at revision `163ea2146acb7b82082acea30ed691b29079095f`. The complete
source-backed delta, secure composition, deletion boundary, and live test
matrix are in
[`agentgateway-composition-spike.md`](agentgateway-composition-spike.md).
Adopt it only as a per-run downstream routing/credential sidecar behind
`agw-guard`; it does not replace strict exact-argument authorization, the
effect ledger, runtime supervision, or artifact authorization. The v1.4.0
analysis below is retained as the original falsification record.

The fresh spike is pinned to the upstream v1.4.0 release and source revision
`83c952731ee79b4372e3a031382c4ff419ddfee1` (2026-07-27). The release is recorded
at the [v1.4.0 release page](https://github.com/agentgateway/agentgateway/releases/tag/v1.4.0).
The decisive checked-in test is
[`test_rbac_mcp_context_is_identity_only`](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/http/authorization_tests.rs#L288-L299),
and the implementation path is
[`McpAuthorizationSet::validate`](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/rbac.rs#L51-L59)
plus
[`MCPInfo::from(ResourceType)`](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/mod.rs#L413-L423).

| Question | v1.4.0 evidence | v3 consequence |
|---|---|---|
| Are arguments request-time authorization input? | No. The current CEL architecture says payload fields, including `mcp.tool.arguments`, are for post-request telemetry; request-time RBAC is identity-only. The session captures arguments for logging, then authorizes a `ResourceType` containing only target and tool name. See the [CEL architecture](https://github.com/agentgateway/agentgateway/blob/v1.4.0/architecture/cel.md#L6-L10) and [call path](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/session.rs#L543-L575). | Native agentgateway authorization cannot implement `exactArguments`; do not translate the field into CEL. |
| Missing versus `null`, extra keys, nested objects/arrays | Not evaluated by request-time authorization because the argument map is absent. Its telemetry model is an optional `serde_json::Map`, not the v3 contract. | Retain `strictjson` and broker-side matching. v3 tests require missing/`null`, extras, nested values, and array order to remain distinct where specified. |
| Duplicate keys, number lexical/canonical distinctions, arbitrary precision, depth/size limits | No v1.4 request-time policy input or conformance guarantee was found for these cases. A parsed map cannot preserve duplicate members for an authorization rule; a generic body/telemetry expression is not an equivalent fail-closed parser boundary. | v3 must continue rejecting duplicates and bounded invalid payloads before policy evaluation and comparing bounded decimal values without float conversion. |
| Target/tool multiplexing | Yes, identity-level. Multiplexing prefixes names and the policy resource carries target/name; see the [multiplex example](https://github.com/agentgateway/agentgateway/blob/v1.4.0/examples/mcp-multiplex/README.md#L14-L29) and [resource model](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/rbac.rs#L66-L109). | Useful routing primitive, but not a replacement for the v3 server/tool plus exact-argument decision. |
| `tools/list` filtering | Yes, identity-level filtering before the merged list is returned; see the [handler](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/handler.rs#L622-L668). | Keep it out of the capability authority unless exact call authorization remains broker-owned. |
| Credential injection | Yes, backend authentication can inject a credential; the upstream test verifies `Bearer my-key` reaches the backend in [stream-to-stream](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/mcp_tests.rs#L2024-L2050). | Potentially removes a transport/helper slice, but it does not replace policy validation, effect claims, or credential selection after policy. |
| Failure mode | MCP fanout defaults to `FailClosed` but also supports `FailOpen` for failed targets; see the [MCP failure-mode definition](https://github.com/agentgateway/agentgateway/blob/v1.4.0/crates/agentgateway/src/mcp/mod.rs#L38-L50). | This is upstream availability behavior, not v3's terminal `UnknownEffect`/no-retry contract. v3 remains fail-closed for policy and external effects. |
| Meaningful boundary removed | Partial: routing, identity authorization, list filtering, backend auth, and telemetry can be delegated. | The strict parser/matcher, effect ledger, runtime protocol, artifact/evidence contract, Gate, and controller-owned publication remain AGW responsibilities. The added proxy/configuration dependency does not justify removing the broker boundary. |

**Decision (2026-08-11): do not adopt agentgateway as the v3 authorization
authority.** Keep `pkg/strictjson` and broker-side ToolSet policy as the source
of truth. The preservation question is negative before the duplicate-key,
arbitrary-precision-number, and bounded depth/size cases are even reachable.
Agentgateway may be evaluated later as an optional routing, backend-credential,
or telemetry adapter behind an explicit interface; it must not receive or
reinterpret `exactArguments` as CEL policy.

The targeted upstream Rust test was inspected at the pinned release but was not
claimed as a local runtime pass: this environment had no Rust toolchain, and an
isolated Rust 1.97 container exhausted the available filesystem while compiling
before tests could run. The decision therefore rests on the tagged source,
checked-in conformance assertion, and v3's existing strictjson/ToolSet tests—not
on an unverified live deployment. Revisit only when upstream provides
request-time argument authorization plus conformance coverage for every case in
this table.

### P0-F: object-store conformance

- Select the production S3-compatible backend.
- Prove conditional create, read-after-write, digest integrity, conflict
  behavior, outage handling, and lifecycle retention.
- Prove that an incomplete effect claim recovers as `unknown` and cannot issue
  a second mutation.

### P0-G: Argo and evidence-envelope adoption

- Pin Argo Workflows v4.1.0 and Agent Sandbox v0.5.4 in a disposable cluster.
- Prove a type-aware bridge records an identity-bound `Ready=True` observation,
  then accepts the upstream v0.5.4 terminal shape
  `Finished=True/PodSucceeded` + `Ready=False/PodSucceeded`; never select a
  condition by array position or require terminal `Ready=True`.
- Prove child-workspace artifact capture, deterministic names, retry behavior,
  exit cleanup, and least-privilege Workflow ServiceAccount RBAC.
- Exercise process termination at every boundary and prove Argo cannot create a
  second logical Sandbox, artifact, or external effect.
- Emit the current Gate report and an in-toto/DSSE cosign envelope over the
  same predicate, then prove tamper rejection and dual-reader parity.
- Use a self-managed/KMS key trust model for self-managed k3s. Do not assume
  public Fulcio accepts an arbitrary k3s ServiceAccount issuer.

The complete source audit and stop/go gates are in
[`build-vs-adopt-spike.md`](build-vs-adopt-spike.md). Argo Events remains
deferred until a concrete authenticated webhook contract is requested.

### Phase 0 exit gate

Any of these is a hard no-go for the sidecar design:

- agent direct egress succeeds
- any credential reaches agent-visible state
- `hostUsers: false` is ignored or idmapped volumes fail
- API, metadata, node, or another run is reachable
- rules can be changed by the agent
- CNI policies are not enforced
- Sandbox/PVC lifecycle cannot be reconciled deterministically
- effect claims are not durable before upstream mutation

Record each result under `docs/v3/evidence/phase-0/` with manifest digests,
versions, commands, sanitized logs, and the chosen sidecar/fallback decision.

## 12. Delivery roadmap

| Phase | Deliverables | Acceptance gate | Estimate |
|---|---|---|---:|
| P0 — assumptions | Real k3s probes, airlock, Sandbox lifecycle, agentgateway and object-store decisions | Every critical assumption has evidence or a selected fallback | 5–7 days |
| P1 — thin slice | v3 module; all seven schemas; `AgentRun`/`Agent` behavior; webhook/preflight; Sandbox backend; resolved snapshot; GitHub App read-token minting; clone; Codex; capture; patch artifact | Operator restarts at every phase create one patch and no duplicate child/effect | 8–10 days |
| P2 — broker | `ToolSet`, `ModelRoute`, runtime protocol, skills, model/MCP proxy, object ledger, audit, per-run Secrets | Exact policy, credential canaries, connected MCP/skills, cost/tool limits, restart and unknown-effect tests pass | 10–12 days |
| P3 — Gate | independent verifier, Go adapter, diff/scope rules, test strength, coverage facts, signed report, shadow records | Out-of-scope, fake-test, tampered-patch, binary, oversized, and malicious verifier fixtures are rejected | 12–15 days |
| P4 — publish | GitHub App, deterministic branch/commit/PR, labels/evidence, manual effect reconciliation | Disposable-repo live E2E opens exactly one PR and handles injected ambiguity safely | 5–7 days |
| P5 — operate | retention, `kubectl-agw`, Helm chart, quotas, metrics/events/logs, upgrades, datastore recovery, runbooks | Restart, cancellation, node loss, disk pressure, cleanup, chart upgrade, restore, and rollback evidence passes | 8–10 days |
| Cutover | v2-to-v3 bundle translator, golden Jobmark comparison, 50-run shadow sample, v2 freeze/removal | Zero false accepts for one repo/Gate and tested rollback | 5–7 days plus shadow window |

Expected total: roughly 55–70 focused engineer-days. Calendar time can be
8–10 weeks with the parallel workstreams below, but Phase 0 and integration
gates remain serial.

## 13. Subagent execution plan

Use `gpt-5.6-luna` with maximum reasoning for implementation subagents. The
integration agent owns architecture, contracts, review, merges, and live E2E;
subagents own bounded non-overlapping worktrees.

| Workstream | Ownership | Starts |
|---|---|---|
| Luna A — API/operator | CRD types, CEL, webhook, status budget, FSM, reconciliation skeleton | After P0 decision |
| Luna B — sandbox/security | P0 manifests, preflight probes, Sandbox backend, NetworkPolicies, credential-canary suite | Immediately; owns P0 |
| Luna C — broker/runtime | port contracts, Codex image, model/MCP proxy, skills, effect ledger | Contract freeze after P0 |
| Luna D — Gate | capture format, verify image, Go adapter, require rules, report/signature | P1 artifact contract stable |
| Luna E — delivery | Helm, RBAC, CI, kind tests, real-k3s harness, kubectl plugin, release/runbooks | P1 scaffold stable |

Rules:

- One worktree and branch per workstream; no shared file ownership.
- Contract changes require integration-owner approval before implementation.
- P0 security evidence is reviewed manually and is never accepted solely from
  a mocked or nested Kubernetes environment.
- Subagent output is merged only with tests and exact acceptance evidence.
- No subagent deploys to the live cluster, creates a GitHub App, or deletes v2
  without explicit integration-stage authorization.

## 14. CI and live E2E

### Unit and envtest

- OpenAPI/CEL and webhook validation
- canonical resolution and digest golden files
- phase-transition table, cancellation, finalizer, and status-size budget
- deterministic names and create-or-get conflicts
- Gate rules and report canonicalization/signature
- effect ledger claim/outcome state machine
- retention selection and dry-run behavior
- GitHub effect/branch/marker generation

### Container integration

- copied runtime, strict JSON, artifact, skill, and tool-policy contracts
- Codex adapter against fake model and broker endpoints
- fake MCP/model/S3 services with fault injection
- credential non-leakage and redirect/private-address rejection
- broker crash before call, during call, and before outcome persistence
- capture and verify helpers with malicious patch fixtures

### Kubernetes integration

Use kind/k3d for ordinary controller, CRD, webhook, owner-reference, finalizer,
and Helm tests. Use the actual two-node k3s environment for user namespaces,
containerd/runc, kube-router/Flannel policy, iptables owner matching, local-path
storage, and shutdown behavior. Nested CI does not substitute for that runner.

The release E2E must:

1. Apply one `AgentRun` YAML.
2. Resolve an exact private-repo base SHA.
3. Run real Codex through the configured model route.
4. Exercise one allowed MCP read and reject a forbidden call.
5. Materialize one digest-pinned skill.
6. Capture and independently verify the patch.
7. Produce a signed report and immutable artifacts.
8. Open exactly one shadow PR through the GitHub App.
9. Show logs through `kubectl agw logs -f`.
10. Remove Sandboxes, Secrets, Jobs, and expired PVCs without deleting evidence.
11. Repeat with operator restart and injected GitHub/object-store failures.

## 15. Retention and operations

Retention remains operator configuration, not a sixth CRD. Initial defaults:

```yaml
artifactRetentionDays: 14
workspaceRetentionDays: 7
ledgerRetentionDays: 30
unknownEffectRetentionDays: 0   # zero means retain until human resolution
maxArtifactBytes: 20Gi
maxWorkspaceBytes: 40Gi
dryRun: true
```

The bounded inventory/applier and its safety tests are implemented and wired as
a leader-elected controller-runtime `Runnable`. Each cycle lists a capped,
complete set of `AgentRun`, Secret, PVC, and object-store metadata; an
incomplete inventory fails closed and cannot produce delete calls. The planner
and applier fence every action to the exact AGW owner/UID/label/spec-digest
identity, exact run-object prefix, Kubernetes UID precondition, or object-store
ETag. Actions are deterministic and idempotent, and never target active runs,
`UnknownEffect`, unflushed ledger/events, or evidence before expiry.

Retention defaults to `dryRun: true`; actual deletion also requires the
explicit enforcement flag and `lifecycleAttested: true`. The attestation flag
is deliberately disabled until a real Phase-0 probe proves the selected Agent
Sandbox release, local-path provisioner, PVC owner references, PV, and host
directory lifecycle. The applier does not patch finalizers, so deletion is
finalizer-safe and remains conservative if Kubernetes is terminating an object.

The live lifecycle probe and object-store conditional-delete proof have not
been run in this workspace because no target k3s kubeconfig or production
object store was available. Until those proofs are recorded, the operator
continues to inventory and report without mutating retention targets.

Phase 5 also adds:

- controller-runtime metrics, structured logs, Kubernetes Events, and bounded
  reason codes
- alerts for preflight stale/failed, run stuck, agent node unavailable, disk
  pressure, cleanup failure, and `UnknownEffect`
- k3s datastore backup/rebuild procedure and a restore drill
- agent-sandbox and k3s upgrade conformance before version changes
- chart rollback and last-known-good image digest runbook

## 16. Shadow-mode cutover and deletion

1. Freeze v2 interfaces and preserve current connected acceptance tests.
2. Build v3 beside v2.
3. Translate one v2 AgentBundle into the seven v3 resources plus `AgentRun`.
4. Run the same mechanical Jobmark task through both systems.
5. Run v3 in shadow mode and read every diff.
6. Record Gate agreement per repo/Gate; do not tune the prompt to hide a Gate
   weakness.
7. Require zero false accepts across approximately 50 runs for that repo/Gate.
8. Freeze new v2 runs and let active runs finish.
9. Archive required v2 artifacts/audit evidence and verify the rollback image.
10. Remove Python v1, obsolete Go v2, console, Compose, and database assets in a
    separate deletion PR.

`enforcing` and later auto-merge are separate, evidence-based decisions. v3.0
never merges a PR itself.

## 17. Owner inputs and recommended defaults

These are not needed to approve this plan, but are required at the indicated
implementation phase.

| Decision/input | Needed | Recommended default |
|---|---|---|
| One-sentence target run | Before P0 closes | The Jobmark shadow-PR sentence in §1 |
| Exact `node-cp` and `node-agents` hosts | P0 | New/dedicated nodes, not the current production Coolify host |
| Production object store/bucket | P0-F | Existing S3-compatible managed storage; MinIO only in tests |
| Sidecar vs separate Broker Pod | P0 exit | Sidecar only if every airlock test passes |
| Agent Sandbox extension CRDs | P0 exit | Core `Sandbox` only |
| `existingClaim` | API freeze | Disabled in v3.0 |
| First harness | P1 | Codex first; Claude Code is implemented but requires its live provider/cluster acceptance run |
| First model route | P2 | Explicit current OpenRouter route; no silent fallback |
| GitHub App creation/installation | P1 | Selected repositories only; App has contents/PR permissions, while clone tokens are reduced to `contents:read` |
| `Cancelled` terminal phase | API freeze | Yes |
| Unknown-effect resolution | P4 | Terminal; inspect GitHub and the ledger manually, never retry blindly |
| Shadow threshold | Cutover | Zero false accepts over about 50 runs per repo/Gate |
| Retention | P5 | Defaults in §15, dry-run first |

## 18. Definition of done

Agents Gateway v3 is stable enough to replace v2 only when all of the following
are true:

- Phase 0 evidence proves the selected isolation topology on the real nodes.
- A clean cluster install creates exactly seven AGW CRDs and the adopted Sandbox
  dependency, with least-privilege RBAC and fail-closed admission.
- One YAML drives clone, work, capture, independent verification, Gate verdict,
  evidence upload, and one shadow PR.
- The agent cannot reach the network or any credential outside broker policy.
- Operator, broker, node, object-store, and GitHub fault injection never creates
  a blind duplicate mutation.
- Patch, report, resolved spec, runtime stream, image/skill digests, and effect
  history are independently retrievable and digest-valid.
- `kubectl agw run -f`, `logs -f`, and `cancel` work against
  the Kubernetes API without a separate AGW server.
- Cleanup, disk pressure, upgrade, restore, and rollback drills pass.
- The chosen repo/Gate records zero false accepts across the agreed shadow
  window.
- v2 remains recoverable until the dedicated deletion PR is merged.

Anything less is an alpha milestone, not completion.
