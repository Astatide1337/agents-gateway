# Agents Gateway v3 — Release / Completion Audit

**Audit date:** 2026-08-12

**Scope:** `/home/ubuntu/Projects/agents-gateway` current worktree, accepted ADR-001 through ADR-029, v3 source, tests, charts, images, build-vs-adopt probes, disposable local Kind API/Phase-0 probes, and a disposable two-node k3s Phase-0 run.
**Release decision:** **Not release complete.** v3 is a substantial, locally green implementation candidate, but its production completion contract has not been proven on the target two-node cluster.

**CI and execution boundary:** v3 CI is manual-only (`workflow_dispatch`). The
hosted image-build matrix and hosted Kind API smoke are separate opt-in inputs;
neither runs automatically. This audit update used only local repository
evidence. No push, hosted Actions run, deployment, or production-cluster
mutation has occurred for this implementation. Disposable local Kind and
nested two-node k3s clusters were created and deleted for API/Phase-0
verification; neither touched the production or home-lab contexts.

The local release-contract guard now checks every `v3*.yml` workflow, not just
the release workflow: each must expose `workflow_dispatch` and none may add a
push, pull-request, schedule, repository-dispatch, workflow-run, or reusable
workflow trigger. The guard runs as part of `v3/scripts/validate-local.sh`, so a
future trigger change fails locally before it can spend hosted Actions minutes.

This audit began with the stable backend-selector implementation and is updated
for the accepted quality/build-vs-adopt addenda. Existing worktree changes were
preserved. “Locally proven” means source/contract/unit/static evidence only; it
does not mean production isolation or a live two-node deployment. The disposable
local Kind/API smoke covers the complete seven-CRD surface and both direct and
Argo chart backends; it remains local schema/admission evidence, not
target-cluster proof. A separate two-node Kind probe also exercised live API
admission and Phase-0 manifest schema, but its nested-container runtime could
not start `hostUsers:false` Pods; it is explicitly not target-cluster proof.
The Argo CRDs were installed through the official v4.1.0
full Kustomize path with server-side apply. Client-side apply is not supported
for these large CRDs because its last-applied annotation exceeds Kubernetes'
256 KiB annotation limit.

## Final codebase review and local validation — 2026-08-12

Six disjoint Luna Max reviews covered the controller/admission graph, security
and chart boundary, Argo/Phase-0 harness, broker/runtime/context seam, Gate and
publication/evidence path, and CLI/release/configuration surface. The review
record, exact cleanup list, and remaining-finding policy are in
[`codebase-review.md`](codebase-review.md).

The final constrained local validation passed after the review fixes:

```text
AGW_GOMAXPROCS=2 go test ./...
PATH=/home/ubuntu/.local/bin:$PATH AGW_GOMAXPROCS=2 \
  bash v3/scripts/validate-local.sh --skip-phase0
=> PASS: unit, race, vet, reachable vulnerability scan, Bash, Helm/chart,
         image contracts, CRD drift, and all production command builds
```

No image build, registry push, hosted GitHub Actions run, provider call, or
production mutation was used. Exact generated caches and the old v2 web
`node_modules` tree were removed. v1/v2 and the direct v3 fallback remain
because their deletion gates are not satisfied.

The final Phase-0 follow-up also corrected the upstream Agent Sandbox installer:
the v0.5.4 release manifest contains a CRD larger than Kubernetes' 256 KiB
client-side last-applied annotation limit. The installer now requires a
64-character manifest digest, uses explicit server-side apply with a bounded
field-manager, and verifies that the Sandbox CRD exists after apply. The
disposable k3s harness exposes this only through the explicit
`--install-upstream` flag. The corresponding interface test is
`v3/scripts/local-k3s-smoke.test.sh`.

The final follow-up fixes also closed the remaining local correctness findings:
source-pinning classifications are bounded and terminal only for permanent
failures; general object-store limits use the named shared
`objectstore.GeneralMaxObjectBytes` 64 MiB ceiling across all callers; the
separate verify-fetch patch path has an explicit 1 GiB ceiling; OutputBoth
persists/replays and uses the patch-created PR number;
findings-only status uses a separate `FindingsPublished` condition; and the
disposable direct gate cleans up exact generated namespaces by default with
`AGW_DIRECT_KEEP=1` as the explicit retention switch. The API/CRD validation
now requires a pre-existing target only for findings-only output, while
resolution rejects findings-bearing output unless `publish.mode` is
`pull-request`. The final internal reachability pass also removed the unused
work-secret alias and legacy opaque artifact-token compatibility shims; the
public Skills Gateway aliases remain because they are package-level API.

## Latest local implementation update

The guarded per-run agentgateway seam is now explicit across the pure workload
builder, run-plan, operator flags, and Helm chart. It is intentionally disabled
by zero-value configuration. Helm rejects both partial disabled configuration and
`agentGateway.enabled=true`; the run-plan rejects nonzero configuration before
reading the GitHub App Secret or materializing a run Secret. The workload still
emits exactly `agent` → `broker`; no unproven third container was added.

The controller/chart release-readiness audit found and corrected two local
fail-closed defects in the requested surface. The Argo deployment now passes
the operator's required WorkflowTemplate UID and canonical digest, and the
chart schema/render checks require both values whenever the Argo backend is
selected. Terminal cleanup now reads each deterministic per-run credential
Secret and deletes it only when its single controller owner is the exact
AgentRun (matching API version, name, UID, and owner flags); a same-name
foreign Secret retains the finalizer and is never deleted. The optional
agentgateway contract remains explicitly rejected until the guard-to-gateway
adapter and live capability evidence exist.

Focused local evidence after these fixes:

```text
GOTOOLCHAIN=go1.26.5 AGW_GOMAXPROCS=2 go test -count=1 \
  ./api/v1alpha1 ./internal/controller ./internal/runplan \
  ./internal/workload ./internal/sandbox
=> PASS

GOTOOLCHAIN=go1.26.5 AGW_GOMAXPROCS=2 go test -race -count=1 \
  ./api/v1alpha1 ./internal/controller ./internal/runplan \
  ./internal/workload ./internal/sandbox
=> PASS

PATH=/home/ubuntu/.local/bin:$PATH bash v3/charts/agw-operator/tests/validate.sh
=> PASS: schema, fail-closed render, direct/Argo wiring, and chart checks
```

These are local tests only. They do not establish Argo Workflow execution,
upstream Agent Sandbox behavior, user-namespace/`NET_ADMIN` isolation, or a
live restart/cleanup cycle on the target k3s nodes.

The following checks were rerun locally after that change, without hosted CI,
image pushes, or production mutation:

```text
PATH=/home/ubuntu/.local/bin:$PATH \
  AGW_GOMAXPROCS=2 bash v3/scripts/validate-local.sh \
  --skip-vulnerability-scan --skip-phase0
=> PASS: unit, race, vet, Helm, image contracts, CRD drift, and command builds

PATH=/home/ubuntu/.local/bin:$PATH \
  ./v3/scripts/local-api-smoke.sh --skip-argo
=> PASS: disposable Kind v0.31.0 / Kubernetes v1.35.0 API,
         seven AGW CRDs, sample admission, immutable-run validation,
         monotonic cancellation, and direct Helm server-side dry-run
```

The cluster was deleted by the smoke script's exit cleanup. This strengthens
local API/chart evidence only; it does not replace target-node, provider,
object-store, GitHub App, or shadow-run proof. Disposable k3s, Agent Sandbox,
UID-airlock, and lower-level Argo evidence is recorded in the live section below.

## Current follow-up audit — 2026-08-11

The local implementation gained three bounded operational improvements after
the disposable probe:

- `v3/scripts/phase0-run.sh` now orchestrates all six Phase-0 probes with one
  run ID, per-probe evidence directories, explicit context requirements for
  dry-run/apply/cleanup, and namespace/marker matching. It owns only the
  resources it created and does not delete namespaces; the disposable direct
  live gate deletes its own exact generated namespaces by default and offers
  `AGW_DIRECT_KEEP=1` for inspection. Its interface test and all six plan paths
  pass. This is a safer runbook, not live k3s evidence.
- `kubectl agw matrix` now fails closed when a terminal shadow run has no
  immutable human review record. It cannot report a partial sample as a valid
  confusion matrix.
- The chart has an optional, disabled-by-default Prometheus Operator
  `ServiceMonitor` for the existing operator metrics Service. It is schema
  validated and is not rendered unless explicitly enabled; the chart does not
  install or require Prometheus Operator CRDs by default.

The trusted phase-transition request also now binds `run_uid`, `spec_digest`,
and `base_sha` in the strict payload. Cross-run and stale requests are rejected
before the one-way transition. The deployment remains fail-closed because an
independent process supervisor is still not wired.

## Latest disposable two-node Kind probe

On 2026-08-11 America/New_York (2026-08-12 UTC), a dedicated Kind v0.31.0
cluster was created from
`v3/test/e2e/kind-live-config.yaml` with one control-plane node and one worker
labelled `agw.astatide.com/agents=true`. Both nodes became Ready on Kubernetes
v1.35.0 with containerd 2.2.0, runc 1.3.4, and the host kernel 6.8.0. The
probe used a temporary kubeconfig and never used the existing production or
home-lab contexts.

Live results:

- **PASS:** all seven AGW CRDs established; samples were admitted; immutable
  execution-spec mutation was rejected; `cancelRequested` could be set but not
  cleared; and server-side dry-run of the direct Phase-0 credential and
  airlock manifests succeeded.
- **PASS:** Phase-0 credential/airlock evidence rendering remained valid after
  sanitization, including generated Secret canaries and boolean Kubernetes
  fields. The Agent Sandbox manifest was rendered in plan mode only because
  the upstream CRD/controller and artifact repository were intentionally not
  installed in this memory-constrained disposable cluster.
- **EXPECTED NEGATIVE:** both `hostUsers:false` UID-map Pods reached the Kind
  worker but could not create a pod sandbox. Kubelet reported runc failing to
  mount `sysfs` with `operation not permitted`. This is a limitation of the
  nested Kind-in-Docker environment, not evidence that the target k3s airlock
  works. It invalidates any userns/UID-airlock claim from this run.

The three failed-probe namespaces and the entire Kind cluster were deleted
afterward. No existing Docker/Coolify container was stopped, restarted, or
pruned. The later disposable two-node k3s run provides nested live evidence;
the target bare-metal two-node k3s Phase-0 run remains a release gate.

## Disposable two-node k3s live evidence — 2026-08-11/12

The real local k3s experiment was run after the Kind probe. It used two
privileged Docker-contained nodes from the pinned `rancher/k3s:v1.35.0-k3s1`
image: one control plane and one agent with the production worker label and
`agw.astatide.com/agents=true:NoSchedule` taint. The API was published only on
loopback port 16443, so Coolify's production ports and containers were not
selected. Both nodes became Ready; the observed runtime was k3s containerd
2.1.5 on the host's 6.8 kernel. This is genuine disposable k3s evidence, but
it is not a substitute for proving the same assumptions on two bare-metal
target nodes because the nodes themselves were nested in Docker.

The final aggregate Phase-0 run (`final2-k3s-20260812`) produced:

- **PASS:** node inventory on both nodes; user namespaces with subordinate
  UID/GID maps and distinct mappings; absent service-account tokens; scoped
  credential canaries unavailable to the agent; and Agent Sandbox v0.5.4
  controller identity, `hostUsers=false`, UID mapping, generated Pod/PVC/
  Service, suspend/resume, shutdownTime, PVC, and local-path cleanup.
- **PASS:** the UID airlock's IPv4/IPv6 owner rules, agent/broker UID and
  shared-network assertions, broker-only loopback access, blocked pod IP,
  Service IP, Kubernetes API, metadata, public IPv4/IPv6, DNS, UDP, gateway,
  admin, and alternate loopback paths, plus no agent capabilities or firewall
  control.
- **INCONCLUSIVE by configuration:** IPv6 private targets were not exposed by
  this single-stack disposable cluster; no broker allowlist URL was configured;
  and no object-store endpoint/bucket/prefix was supplied. These are known
  missing inputs, not positive evidence.

The provider-free direct lifecycle gate then passed live: exact AgentRun owner
UIDs were checked on Jobs, ConfigMaps, and PVCs; work/capture/fresh-verify
boundaries were exercised; the verification-failure payload was exact and
non-retryable; and the final AgentRun owner-reference garbage collection
assertion passed. The fixture was corrected to wait for work materialization
before capture and to delete capture Pods before their work PVC, matching
Kubernetes PVC protection semantics.

The Argo live preflight also passed against the same disposable cluster with
pinned Argo Workflows v4.1.0 and Agent Sandbox v0.5.4 release assets: all CRDs
were Established, both resource-template success/failure conditions behaved
as expected, the Workflow-created Sandbox ran, preserved `hostUsers=false`,
and reported Ready/Finished conditions. The preflight's cleanup path was
hardened to wait for gracefully terminating, exact run-labelled Sandbox Pods.

After evidence was recorded, every exact test namespace was deleted, the two
k3s containers and their Docker network were removed, and the temporary
kubeconfig/evidence directory was removed. Production Coolify containers,
Traefik ports, and the host Docker network were not pruned or changed. No
hosted GitHub Actions pipeline, image push, deployment, provider call, object
store write, or GitHub App publication was used.

### Follow-up installer and lifecycle rerun — 2026-08-12

A second disposable two-node k3s run exercised the current installer after the
review fixes. It supplied the verified v0.5.4 manifest digest and the immutable
controller image reference recorded in `v3/phase0/argo-sandbox/versions.env`.
The release manifest was applied with server-side ownership, and the controller
deployment identity matched the digest reference before the Sandbox was
created.

The direct Sandbox probe then passed the typed lifecycle contract: `Ready=True`,
`Finished=True/PodSucceeded`, `hostUsers=false`, subordinate UID mapping,
generated Pod/PVC/Service, suspend/resume, shutdown, PVC cleanup, and
local-path host-directory cleanup. The run returned exit status `3` by design:
required checks passed, while single-stack IPv6 targets, the optional broker
allowlist URL, the object-store endpoint/bucket/prefix, and PV deletion were
not configured. The exact containers, network, kubeconfig, evidence directory,
and probe resources were removed by the harness.

## Phase-0 harness safety audit

The Phase-0-only audit on 2026-08-12 UTC covered every `phase0-*.sh` script,
the five Kubernetes fixtures, the disposable Kind topology, and this evidence
surface. No Argo or Go implementation scope was changed, and no hosted CI,
image push, or production API was used.

The following local defects were corrected in the shared harness boundary:

- `PHASE0_MODE=cleanup` now becomes an explicit cleanup action and is rejected
  unless `--yes` is also present; this closes the environment-variable bypass
  for both Kubernetes and object-store cleanup.
- Run IDs are limited to Kubernetes-label-safe values of at most 63 characters,
  preventing evidence-directory traversal and ambiguous label selectors.
- `kubectl apply --dry-run=client` is now bounded and requires an explicit
  `--context`; `--plan` remains the offline rendering mode. The harness never
  silently discovers against the current context.
- The object-store probe initializes its temporary AWS config under `set -u`.
- Evidence sanitization now covers quoted multi-word YAML values and shell-style
  credential assignments such as `AWS_SECRET_ACCESS_KEY=...`, while preserving
  boolean Kubernetes fields.
- The pinned Agent Sandbox installer now uses server-side apply because the
  v0.5.4 CRD exceeds the client-side annotation limit, and it verifies the CRD
  exists after a successful apply instead of trusting the apply exit code alone.
- The disposable k3s wrapper forwards upstream installation only when
  `--install-upstream` is explicitly requested, and returns exit status `3`
  when required inputs pass but optional checks (such as object storage) were
  not supplied.
- The Kind-only worker now carries the target agent taint
  `agw.astatide.com/agents=true:NoSchedule` in addition to its label. A fresh
  two-node Kind check observed the expected label and taint, then deleted the
  cluster; the worker was still converging to Ready at the first observation,
  so a real run must explicitly wait for both nodes.

Local proof after the changes:

```text
bash -n v3/scripts/phase0-*.sh
=> PASS

All six Phase-0 scripts in --plan mode
=> PASS; no Kubernetes API contacted

All Kubernetes Phase-0 scripts in --dry-run mode without --context
=> SAFE SKIP; no current kube context used

Airlock, credentials, and inventory in --dry-run mode with an explicit
temporary Kind context
=> PASS; Sandbox dry-run correctly failed without the upstream Sandbox CRD

PHASE0_MODE=cleanup without --yes
=> rejected; object-store strict-mode cleanup no longer aborts on an unset
   temporary config variable
```

### Remaining target-node and optional-input conditions

The harness still accepts an operator-supplied existing namespace and mutable
tag-based probe images by default for lightweight local planning. These are
deliberate operator inputs, not evidence of production safety. A target-node
run must use a fresh namespace such as
`agw-phase0-<timestamp>` (never `agw-system`, `agw-runs`, `default`, or
`kube-system`) and digest-pinned images. Before apply, prove the namespace is
absent; after every script, use its exact run label for cleanup and verify that
no matching `all`, `NetworkPolicy`, `Secret`, `ConfigMap`, `PVC`, or Sandbox
resource remains. Verify each pod's resolved `status.containerStatuses[*].imageID`
against the approved digest.

The minimum disposable-k3s command shape is:

Use `--require-digests` on the shared `phase0-run.sh` wrapper (or on each
individual probe) after supplying digest-pinned `PHASE0_*_IMAGE` values. The
flag rejects mutable tags before apply or dry-run; cleanup accepts it without
requiring image inputs.

```bash
ctx=<disposable-k3s-context>
ns=agw-phase0-$(date -u +%Y%m%d%H%M%S)
out="$PWD/phase0-evidence"
run=phase0-$(date -u +%Y%m%d%H%M%S)

kubectl --context "$ctx" get nodes -o wide
kubectl --context "$ctx" get namespace "$ns" # must return NotFound

PHASE0_CONTEXT="$ctx" PHASE0_NAMESPACE="$ns" PHASE0_RUN_ID="${run}-airlock" \
  PHASE0_BASE_IMAGE=<digest-pinned-image> \
  PHASE0_NETWORK_IMAGE=<digest-pinned-netshoot-image> \
  PHASE0_LOCKDOWN_IMAGE=<digest-pinned-netshoot-image> \
  ./v3/scripts/phase0-airlock.sh --apply --yes --require-digests --context "$ctx" \
  --namespace "$ns" --output-dir "$out"

PHASE0_RUN_ID="${run}-airlock" PHASE0_NAMESPACE="$ns" \
  ./v3/scripts/phase0-airlock.sh --cleanup --yes --require-digests --context "$ctx" \
  --namespace "$ns" --output-dir "$out"

kubectl --context "$ctx" -n "$ns" get all,networkpolicy,secret,configmap,pvc \
  -l "agw.astatide.com/phase0-run=${run}-airlock" --ignore-not-found
```

The disposable nested-k3s run used the same exact-resource cleanup boundary and
installed Agent Sandbox v0.5.4 with the server-side path above. It passed the
substantive inventory, userns, credential, airlock, and Sandbox lifecycle
checks, but had no object-store endpoint/bucket/prefix and no configured
broker-allowlist URL, so its aggregate result was explicitly inconclusive.

Repeat the same pattern on the target nodes for inventory, userns, credentials,
and Sandbox after installing the pinned Agent Sandbox CRD/controller. The airlock command must
also receive the actual k3s DNS, Pod, Service, and API CIDRs; the Kind defaults
are not valid proof for k3s. Record the node-floor output, packet-level
allow/deny results, Sandbox Ready/Finished/shutdown/PVC cleanup, and sanitized
evidence directory as the P0-B through P0-D release artifacts.

## Status categories

- **Implemented + locally proven:** present and supported by local tests, static checks, or API-schema evidence.
- **Implemented but requiring live proof:** the implementation exists, but the relevant cluster, provider, credential, or end-to-end behavior has not been exercised.
- **Partially implemented:** important code exists, but a required path is missing, currently failing, or not release-grade.
- **Not implemented:** absent, intentionally deferred, or has no evidence of the required behavior.

## Summary

| Requirement | Status | Audit conclusion |
|---|---|---|
| Seven-CRD Kubernetes control plane | **Implemented + locally proven** | `Policy` and `ContextStrategy` extend the original five CRDs. All seven established on disposable Kubernetes v1.35.0; sample creation, server-side dry-run, immutable execution-spec CEL, and monotonic cancellation were exercised. This does not prove target k3s execution. |
| Work/verify completion contract and Gate | **Implemented but requiring live proof** | Work, capture, verify, scope, diff, test-strength, coverage, and `Accepted`/`Rejected` paths pass local unit and race tests; live clean-checkout verification is not yet proven. |
| Quality contract, hybrid scoring, and critic corroboration | **Implemented locally; live provider proof pending** | Policy compilation, integer scoring, family separation, evidence-or-advisory routing, and signed provenance are wired to a production critic Job/source. The pod uses a digest-pinned critic plus agentgateway, a projected audience-specific JWT, a per-run read-only verify Secret, full-Pod authentication, and no provider credential. Operator/chart wiring is disabled by default and fails closed without capability evidence. No live provider-backed critic pod has completed. |
| Deterministic `ContextPack` | **Implemented locally; live producer execution pending** | Policy/task/skill context plus local lexical metadata, deterministic file-oriented structural fallback, bounded Git history, and a bounded local LSP client are compiled, budgeted, digested, mounted read-only, persisted immutably, and independently projected into status. The default context image bundles no tree-sitter parser, language server, or Serena runtime; explicit providers must be executable-digest pinned and optional tiers remain disabled/fail closed until reviewed provider images supply them. |
| Argo Workflows sequencing backend | **Implemented + disposable live preflight proven; production proof pending** | The opt-in producer performs prepare, UID-bound Ready/Finished waits, stage, capture, and authenticated handoff. A live disposable Argo v4.1.0/Agent Sandbox v0.5.4 preflight proved resource-template conditions, Workflow-created Sandbox execution, `hostUsers=false`, and Ready/Finished observations. The full object-store/bridge/artifact Workflow and target-cluster run remain unproven. |
| `agentgateway` composition | **Implemented as guarded Phase-0 candidate; live gate pending** | The tested v1.4.1 digest is recorded. A UID airlock forces agent traffic through the AGW guard, which retains exact-argument policy, approval, and effect-ledger authority before forwarding to loopback agentgateway. No live k3s credential-canary/MCP handshake has passed. |
| Phase-0 orchestration and evidence runner | **Implemented + disposable live proven** | `v3/scripts/phase0-run.sh` runs all six existing probes under one run ID with per-probe evidence, explicit context/confirmation guards, and marker-checked cleanup without deleting namespaces. The separate direct live gate deletes only its exact generated namespaces by default and supports explicit retention for inspection. The final disposable two-node k3s run passed the substantive inventory, userns, credentials, airlock, and Sandbox checks; optional IPv6/allowlist/object-store checks were explicitly inconclusive because their inputs were absent. |
| Operator metrics scrape integration | **Implemented + locally proven** | The Helm chart exposes an optional, schema-validated `ServiceMonitor` for the existing metrics Service. It is disabled by default and is not rendered when Prometheus Operator CRDs are absent. |
| Real two-node Phase 0 | **Implemented + disposable k3s proven; target-node proof pending** | A Docker-contained two-node k3s v1.35.0 run established both nodes, the worker label/taint, and the substantive Phase-0 checks. This is stronger than the nested Kind API probe, but bare-metal node-floor, production CNI, and production artifact-store evidence remain required. |
| `agent-sandbox` compatibility and PVC lifecycle | **Implemented + disposable live proven** | v0.5.4 is manifest- and controller-image-digest pinned and the primary backend uses `agents.x-k8s.io/v1beta1` `Sandbox`; live evidence covered typed `Ready=True` and `Finished=True/PodSucceeded`, controller readiness, `hostUsers=false`, UID mapping, Service/PVC generation, suspend/resume, shutdown, and PVC/local-path cleanup. Target-node and production object-store execution remain pending. |
| UID airlock / network isolation | **Implemented + disposable live proven; target CNI proof pending** | `hostUsers: false`, ordered init containers, NetworkPolicies, and destination-specific IPv4/IPv6 UID/port rules were exercised live. The agent could reach only the broker's approved loopback listener; private/public/API/metadata/DNS/UDP bypasses and firewall inspection were blocked. Production-node/CNI behavior remains pending. |
| Object storage, IAM, and signed evidence | **Implemented locally; KMS/live storage proof pending** | The real AWS SDK/store path now has a loopback TLS S3-shaped conformance harness covering conditional create, exact read, immutable conflict, simulated restart, and persisted-before-response-drop ambiguity. Artifact digests, STS-scoped auth, report signing, a strict in-toto v1 projection, and a fail-closed cosign lifecycle also exist. A real cosign v3.1.3 local-key create/verify/persist round trip passed against the exact binary copied into the operator image. A real KMS, production object store, key rotation, retained-bundle procedure, and ambiguity drill have not passed. Public Fulcio/keyless trust is deliberately not assumed. See `docs/v3/objectstore-conformance.md`. |
| Registry-published image digests | **Partially implemented** | All 15 release-matrix image contracts, including `critic` and `agent-run-lifecycle`, have local validation and manual hosted build/release paths. No production images/manifests have been pushed or configured with immutable registry digests. |
| GitHub App publication | **Implemented but requiring live proof** | Controller-owned branch/commit/PR logic, immutable `patch`/`findings`/`both` projection, per-finding review/advisory effects, marker reconciliation, and effect-ledger idempotency exist; no live scoped GitHub App PR/review has been reconciled. |
| Claude runtime | **Implemented but requiring live proof** | The adapter's output-pipe race is fixed and repeated/race tests pass; no live Claude-provider run exists. |
| Shadow confusion matrix | **Implemented but not populated with release evidence** | `kubectl-agw review` stores immutable labels and `kubectl-agw matrix` computes the per-repo/per-Gate confusion matrix; no real 50-run evidence window exists yet. |
| Enforcing promotion / auto-merge | **Not run / intentionally deferred** | Promotion is deliberately manual and evidence-gated; auto-merge remains a separate future decision. |
| v1/v2 deletion | **Not implemented / intentionally deferred** | `v2/` and `agents_gateway/` remain. The design explicitly requires release gates and cutover evidence before deletion. |

## Implemented + locally proven

### Kubernetes API and control-plane shape

The v3 surface is seven namespaced CRDs: `AgentRun`, `Agent`, `Gate`, `ToolSet`,
`ModelRoute`, `Policy`, and `ContextStrategy`. The source has no replacement API
server, queue, database, or web console. Argo is an opt-in orchestration
candidate and is not yet a deployed dependency. Relevant evidence:

- `v3/api/v1alpha1/types.go` — API types, phases, immutable-spec CEL validation, Gate modes and requirements.
- `v3/config/crd/` — seven generated CRDs.
- `v3/internal/admission/` — reference, preflight, and task validation.
- `v3/internal/controller/agentrun.go` — phase reconciliation, child ownership, capture, verification, publication, and `UnknownEffect` handling.
- `v3/cmd/kubectl-agw/` — `run`, `logs`, and `cancel` commands.

The complete seven-CRD local smoke passed:

```text
kubectl apply --server-side --force-conflicts -k v3/config/crd
kubectl wait --for=condition=Established ... all seven AGW CRDs
kubectl create namespace agw-system
kubectl create namespace agw-runs
kubectl apply --server-side --dry-run=server -k v3/config/samples
helm template ... v3/charts/agw-operator ... --set orchestrationBackend=direct | kubectl apply --server-side --dry-run=server -f -
```

Result: all seven AGW CRDs and all eight official Argo CRDs established; samples
and both direct and Argo chart backends passed server-side dry-run on a
disposable local Kind cluster. Actual sample objects were admitted,
execution-spec mutation was rejected, and cancellation remained monotonic. The
cluster was deleted afterward. This proves API shape only.

The Argo chart also renders and passes strict offline Argo CLI lint locally. The
official CRD installation uses server-side apply to avoid the large-CRD
last-applied annotation limit.

### Local static and contract checks

The supported local entrypoint is `v3/scripts/validate-local.sh` (run from the
`v3/` directory). Its default mode is explicitly non-deploying: it does not
contact a Kubernetes API, create Kind, submit Argo, invoke `gh` or a GitHub
API, push a container, or clean up Docker/Podman state. It covers formatting
and whitespace, serialized unit and race tests, vet, vulnerability scanning,
Bash syntax, chart/image contracts, Phase-0 static/plan checks, CRD drift, and
production command builds. `--build-images` and `--build-image NAME` are
explicit local opt-ins.

Focused local tests for the current context/LSP, critic, Argo-output, and
cosign seams passed. After the final integration edits, the complete local
entrypoint also passed end to end. It remains the repeatable
release-candidate gate.

The final repository-wide evidence includes:

```text
./scripts/validate-local.sh
```

That run passed formatting/whitespace, serialized unit and race tests, vet,
`govulncheck` (`No vulnerabilities found.`), Bash syntax, static chart checks,
all 15 local image contracts, both Phase-0 static guards, all six Phase-0 plan
runs, generated-CRD drift, and command builds. A follow-up check added the
previously omitted `agw-attest` entrypoint to discovery and built it, bringing
the discovered command count to 16. Helm rendering, Argo CLI schema lint,
container image builds, Kubernetes API smoke, and live Phase-0 remained
explicitly skipped for the reasons recorded below.

The current final local run also passed the manual-only v3 workflow/release
contract and the fail-closed trusted phase-supervisor deployment contract.
The focused broker/command/workload/lifecycle tests and their race variants
passed after the unproven private-socket path was removed. `govulncheck`
returned `No vulnerabilities found.` in a separate local run, and all six
Phase-0 scripts completed in plan mode; neither check contacted a cluster or
provider.

`v3/internal/workload/workload_test.go` covers the intended pod contract: non-root agent, `RuntimeDefault`, drop-all capabilities, read-only root filesystem, no service-account token, restricted Secret mounts, ordered `clone → skills → context → lockdown` init containers, a read-only context mount that does not block writable artifact output, and broker UID 1337.

The trusted phase-supervisor deployment audit is explicitly fail-closed in
[`trusted-phase-supervisor-contract.md`](trusted-phase-supervisor-contract.md).
The work Sandbox renders `shareProcessNamespace: false`, carries a disabled
supervisor annotation, projects no phase credential, and renders only the agent
and broker regular containers. The Helm schema/helper rejects
`phaseSupervisor.enabled=true`, and the lifecycle image has no supervisor mode.
This is a deployment contract, not a claim that Explore-to-Edit authorization
or independent process observation is wired.

The current Go dependency graph returned `No vulnerabilities found.` This is a
point-in-time result, not a substitute for image/SBOM/registry scanning at release.
The v3 workflow definitions remain present for deliberate manual use, but no
hosted v3 CI or release workflow was invoked for this implementation.

The opt-in `v3/scripts/local-api-smoke.sh --skip-argo` was also executed against
a disposable Kind v1.35.0 API server using server-side apply. It established all
seven AGW CRDs, admitted the samples, rejected an execution-spec mutation,
rejected clearing `cancelRequested` after it was set, and accepted the direct
Helm server-side dry-run. The exact cluster and temporary kubeconfig were
deleted by the script's exit cleanup. Complete Argo mode requires a locally
stored v4.1.0 CRD bundle; the official Argo CRD/API smoke was separately run
earlier with server-side apply, but no Argo Workflow or workload was submitted.

### Quality architecture and authority boundaries

The accepted quality addendum is no longer documentation-only. The local tree
contains these independently tested contracts:

- `v3/internal/policycontract/` compiles one `Policy` into bounded standard
  `AGENTS.md`, Agent Skills, deterministic self-check scripts, and Gate inputs.
- `v3/internal/contextpack/`, `v3/internal/contextmaterializer/`, and
  `v3/internal/contextartifact/` make context a canonical, budgeted, immutable
  artifact rather than ambient prompt state. Repository traversal rejects
  symlinks, special files, malformed UTF-8, credential-like inputs, dirty Git
  state, and budget overflow. The structural tier has a deterministic local
  fallback. The symbol tier has an actual bounded local LSP client, but no
  language server or Serena runtime is bundled. An explicitly requested symbol
  tier fails closed until a reviewed local server is supplied, and the adapter
  verifies the exact executable SHA-256 before starting it.
- `v3/internal/broker/` enforces explore/edit/verify tool profiles and stores
  oversized results as content-addressed references with bounded structured
  diagnostics. `TransitionToEdit` is a host-authorized, one-way primitive with
  fail-closed tests, but `cmd/agw-broker` rejects the unproven phase-supervisor
  configuration and exposes no phase endpoint. Production agents therefore
  remain in `explore` until an independent deployment adapter is implemented
  and live-tested. Observation masking remains deliberately absent because the
  current runtime protocol cannot represent it truthfully.
- The deployment-side phase-supervisor audit found no safe implementation to
  enable without a deployment-proven broker adapter: the work pod does not
  share a PID namespace, and no workload, chart, or lifecycle resource binds a
  supervisor to the broker. The broker protocol tests pass locally, but the
  deployment binding is absent. The current lifecycle `wait`
  step observes Sandbox state from a separate Argo pod; it is not a trusted
  in-pod process supervisor. The chart therefore fails closed and agents remain
  in `explore`.
- `v3/internal/findingcorroboration/` and `v3/internal/gatescoring/` keep all
  arithmetic in integer basis points and route model-only assertions to
  advisory output. A critic cannot turn an uncorroborated assertion into a
  blocking result. The external critic supplies only canonical input; the
  trusted verification controller and publisher independently recompute the
  corroboration result, so the critic cannot grade its own findings.
- `v3/cmd/agw-critic/`, `v3/images/critic/`, and
  `v3/internal/criticworkload/` provide the production Job/source contract.
  The agentgateway sidecar forwards native Anthropic Messages once, while the
  central gateway retains provider credentials. Full-Pod authentication and a
  controller-derived per-run verify Secret bind the output; live provider and
  central-JWT evidence remain required before enablement.
- `v3/internal/gate/`, `v3/internal/verifycontroller/`, and
  `v3/internal/evidenceattestation/` bind the resolved Gate revision, critic
  route/provider family, corroboration artifact, score result, patch, images,
  skills, and deterministic checks into the signed evidence chain.

These packages prove the local contract, not the quality claim itself. No Gate
may move from shadow to enforcing until its per-repo golden set has the required
zero-false-accept evidence window.

### Build-vs-adopt implementation boundaries

The current adopted-component policy is deliberately narrower than the original
proposal:

- Argo Workflows owns durable sequencing only. `AgentRun` domain completion,
  `Rejected` versus `Failed`, signed Gate acceptance, and `UnknownEffect` remain
  AGW-owned.
- agentgateway owns downstream routing and credential injection only after the
  AGW guard authorizes the exact request and effect state. It is not the ToolSet
  capability authority.
- cosign/in-toto provides a portable evidence envelope. The canonical Gate
  predicate and trusted AGW report verification remain authoritative, and the
  self-managed cluster requires an explicit key/KMS trust root. The operator
  now refuses to complete a configured attestation until the signed report is
  rebound to its immutable reference, cosign writes a bounded bundle, the
  bundle passes `verify-blob-attestation`, and both statement and bundle are
  persisted content-addressably. File-backed signing can use a separate public
  verification key; KMS deployments may use one URI for both operations.
- Argo Events remains deferred. `kubectl apply -f AgentRun.yaml` is the stable
  trigger until an authenticated webhook/replay contract is actually needed.

The pinned Phase-0 candidates are Argo Workflows v4.1.0, Agent Sandbox v0.5.4,
and agentgateway v1.4.1. The latter is bound to the tested image digest recorded
in `v3/phase0/agentgateway-guard/versions.env`; a floating tag is not accepted
as production evidence.

## Implemented but requiring live proof

### Phase 0, `agent-sandbox`, and the airlock

The six Phase 0 scripts are present and expose explicit plan/dry-run/apply/cleanup modes:

- `v3/scripts/phase0-inventory.sh` — node/kernel/container-runtime floor.
- `v3/scripts/phase0-userns.sh` — live user-namespace and UID-map proof.
- `v3/scripts/phase0-airlock.sh` — agent/broker egress and metadata/API/DNS assertions.
- `v3/scripts/phase0-credentials.sh` — setup-only and broker-only credential canaries.
- `v3/scripts/phase0-sandbox.sh` — upstream Sandbox/PVC/service/suspend/shutdown lifecycle.
- `v3/scripts/phase0-objectstore.sh` — S3 conditional-write, read, delete, and ambiguity checks.

The disposable Kind run used live apply for the API/admission probes and
server-side manifest checks. Userns-dependent apply failed at the nested runtime
boundary described above, so that run is useful falsification evidence rather
than Phase-0 completion. The later disposable nested-k3s run installed the
pinned upstream controller and exercised the live Sandbox lifecycle, but target
bare-metal node, provider, and artifact-store evidence is still required.

`v3/go.mod` pins `sigs.k8s.io/agent-sandbox v0.5.4`; `v3/internal/sandbox/backend.go` uses the upstream `Sandbox` API and `v3/internal/workload/workload.go` builds its pod template. The Helm chart does not install the upstream controller for the operator, so the target cluster still needs a compatible, independently managed Agent Sandbox installation and a live lifecycle test.

The UID airlock implementation is visible in `v3/internal/airlock/policy.go`,
`v3/internal/workload/workload.go`,
`v3/config/networkpolicy/runs-boundary.yaml`, and
`v3/test/e2e/phase0/30-uid-airlock.yaml`. The production direct topology now
permits agent UID 1000 only to the broker's exact IPv4/IPv6 loopback address and
TCP port; broker UID 1337 retains intentionally broad egress. A separate pure
renderer captures the future guard-to-agentgateway chain without enabling it in
production. A real two-node k3s node must still prove `hostUsers: false`,
namespaced `NET_ADMIN`, CNI policy, IPv4/IPv6 routing, and UID ownership.

**Important boundary:** the local Kind API-schema smoke and the two-node Kind
probe are **not** workload-isolation proof. The nested runtime failure prevents
claims about user namespaces, UID-owned iptables, CNI enforcement,
metadata/API blocking, PVC lifecycle, or Agent Sandbox controller behavior.

The Argo compatibility subtree at `v3/phase0/argo-sandbox/` now proves the
condition-array interpretation and manifest contract locally. It uses a
type-aware bridge because Agent Sandbox v0.5.4 exposes `Ready` and `Finished`
through `status.conditions`, not a trustworthy summary phase. Static and race
checks pass. The later disposable two-node k3s run proved the lower-level
Workflow-created Sandbox Ready/Finished path and cleanup behavior. It did not
prove the full AGW output-artifact/retry workflow or target-node equivalence,
so the direct backend remains the rollback path.
The operator's `direct|argo` selector, create-once Workflow backend, immutable
UID/reference/status binding, generic bounded messages, conditional namespace
RBAC, and rollback-preserving default pass local unit/race/static checks. The
Argo output boundary is now a strict canonical JSON contract under
`v3/internal/argoworkflow/output.go`: it binds the AgentRun UID/generation,
resolved spec digest, live Workflow UID/generation, base SHA, patch references,
runtime terminal/completion evidence, and verification inputs. The controller
persists the exact bytes under their SHA-256 content address and resumes only
at AGW-owned `Verifying`; it never derives Gate/effect/publication success from
`status.phase`. Duplicate, stale, non-canonical, oversized, and conflicting
outputs fail closed. The producer now implements the complete identity-bound
prepare/wait/stage/capture/handoff sequence described in
`docs/v3/argo-lifecycle-producer.md`, with a run-level deadline, retained PVC
ownership, and UID-preconditioned Secret cleanup. A disposable API server
validated the rendered object, but target k3s still needs to prove output
parameter delivery, Agent Sandbox behavior, object-store readback, and cleanup
before this backend can replace direct execution operationally.

The guarded agentgateway subtree at `v3/phase0/agentgateway-guard/` passes its
static, race, container-build, recording-upstream, credential-canary, and
airlock-render tests against the recorded v1.4.1 image digest. That does not
prove real CNI/user-namespace behavior or a real credential-injected MCP call.
The agentgateway path therefore remains opt-in and fail-closed.

### GitHub App publication

`v3/internal/githubapp/`, `v3/internal/githubpublish/`, and `v3/internal/publish/` implement controller-owned installation tokens, deterministic branch/commit/PR publication, labels, claim-before-mutation, and terminal `UnknownEffect` handling. Unit tests cover the GitHub adapter, App token construction, and effect-ledger behavior.

The release gate is still missing: a disposable repository must be cloned with the short-lived read credential, published through a real GitHub App restricted to the intended repository, and reconciled through a real PR. The App private key must remain in `agw-system`; it must never enter `agw-runs`.

Findings publication has a separate effect per blocking review and one advisory
section effect. Reconciliation is marker- and commit-bound, and a full
unpaginated GitHub response page fails closed rather than claiming absence of an
older marker. The exact verified findings artifact is now selected from bounded
status and projected from the immutable resolved output mode/target through the
top-level controller. The remaining gate is exercising it against a disposable
pull request with the scoped GitHub App.

## Partially implemented

### Local validation entrypoint; live validation remains partial

After the backend-selector fixtures and Claude subprocess lifecycle were fixed,
the stable tree passed:

```text
go test -count=1 -p 1 ./...
go test -race -count=1 -p 1 ./...
go vet ./...
GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
```

The Claude regression additionally passed 20 repeated normal runs and five
race-enabled repeats. The primary Agent Sandbox mode and built-in Job/PVC mode
both pass Helm lint and Kubernetes server-side dry-run. The Job render was
validated on a Kind API server that does not have the upstream Sandbox CRD,
which proves manifest/API independence but still does not prove a complete
operator startup or workload execution.

The Job fallback is operationally selectable with
`--sandbox-backend=agent-sandbox|job`. Controller watches, RBAC, quota,
status child kind, persisted plan fingerprint, process/evidence discovery, and
`kubectl-agw logs` are backend-aware. Provider-free direct and lower-level
Argo/Sandbox live fixtures exist, but no provider-backed full acceptance run
exists for either backend.

### Object storage, IAM, and evidence

`v3/internal/objectstore/`, `v3/internal/artifacts/`, `v3/internal/artifactauth/`, `v3/internal/runsecret/`, and `v3/internal/runtimeevents/` provide content-addressed artifacts, conditional creation, bounded reads, per-run materialization, durable event frames, and STS/session-policy hooks. `v3/internal/artifactauth/` also contains an explicitly opted-in static-credential fallback; that is not equivalent to revocable, short-lived production credentials.

Production values remain deployment inputs in `v3/charts/agw-operator/values.yaml`; bucket, credentials, STS role, signer, and related fingerprints are blank or disabled by default. The operator startup checks in `v3/cmd/agw-operator/main.go` intentionally refuse incomplete production configuration. A real bucket/IAM test must prove exact-prefix access, report signing, conditional writes, read-after-write behavior, and ambiguous-outcome handling.

The verification-attestation lifecycle is wired through
`v3/internal/verificationattestation/` and the Helm/operator configuration. A
real cosign v3.1.3 local-key round trip passed against the exact binary copied
into the operator image. The standalone CLI now accepts an explicit separate
verification-key reference, the adapter rejects relative/ambient/remote key
references, and lifecycle writes require exact read-after-write reconciliation
on both new and replayed objects. These prove local contract behavior; they do
not prove the target KMS identity, key policy, external key mounting,
object-store availability, or retained-bundle rotation procedure. See
`docs/v3/attestation-trust-root.md` for the boundary.

### Image supply chain

`v3/internal/workload/workload.go` validates immutable `@sha256:` images and
the chart schema requires runtime/helper image digests. The 15 release-matrix
images are `operator`, `agent-run-lifecycle`, `broker`, `capture`, `clone`,
`context`, `critic`, `lockdown`, `preflight`, `runtime-claude`, `runtime-codex`,
`skills`, `verifier`, `verify-apply`, and `verify-fetch`. Their hosted
build/release paths are manual-only and have not been invoked. Both new image
contracts were exercised locally. `v3/charts/agw-operator/values.yaml` contains empty
digest inputs by default. No production images/manifests have been pushed or
deployed, and release still requires digest-pushed images plus a deployment
manifest containing those exact digests.

### Retention and operations

`v3/internal/retention/` protects evidence and workspace deletion behind effect-ledger checks and supports dry-run-oriented cleanup. Effect-ledger retention is now an independent, bounded contract: only canonical claim plus terminal successful/failed outcome pairs whose run/evidence/workspace conditions are proven can be planned. The applier creates a permanent canonical tombstone replay fence, then deletes outcome before claim with ETag fences. Unknown, pending, incomplete, malformed, mismatched, duplicate, truncated, or ambiguous records remain protected. The fence is intentionally retained forever so replay safety survives the retained AgentRun/evidence window.

The v3 code emits controller metrics, Kubernetes Events, and durable runtime event streams (`v3/internal/retention/controller.go`, `v3/internal/runtimeevents/`), but no v3-managed Grafana/Loki/OTel dashboards or alert rules were found. That is an operational completeness gap, not a reason to add a larger platform before the core release gates pass.

## Not implemented / intentionally deferred

### Shadow evidence and enforcement promotion

`Gate.spec.mode` supports `shadow` and `enforcing`, and publication tests cover
shadow/rejected labels (`v3/api/v1alpha1/types.go`,
`v3/internal/publish/publish_test.go`). `v3/internal/shadowreview/` plus
`kubectl-agw review`/`matrix` provide immutable human labels and a
per-repo/per-Gate confusion matrix. The required operating evidence remains
absent:

- no measured zero-false-accept window across approximately 50 runs;
- no evidence-backed shadow-to-enforcing promotion procedure has been run;
- no auto-merge path.

The design intentionally makes promotion a human decision after reviewing the
per-repo/per-Gate measurements; automatic promotion is not a v3.0 requirement.
Until that evidence exists, enforcing and especially auto-merge must not be
presented as complete.

### v1/v2 deletion

The legacy `v2/` and `agents_gateway/` trees, including the v2 web/deployment surface, remain in the repository. The v3 plan explicitly defers deletion until the release gates, cutover, rollback, and shadow evidence pass. This is correct for the current state; deleting them now would remove the fallback before v3 is proven.

## Release blockers and proof sequence

1. Provision the actual two-node k3s topology and pass the kernel/containerd/runc/filesystem floor checks on both nodes.
2. Install a compatible Agent Sandbox controller and prove Sandbox/PVC/service/suspend/shutdown cleanup live.
3. Run the user-namespace, UID airlock, CNI, metadata/API, and credential-canary probes on the agent node. Fail closed on any mismatch.
4. Configure production object storage, exact-prefix IAM/STS, signing, GitHub App credentials, and digest-pushed release-matrix images; execute the ambiguity and restart drills. Enable the critic and select Argo only after their separate live capability gates pass.
5. Run a disposable-repository end-to-end job through a real Gate, including a real Claude runtime test if Claude is supported, then verify the independent clean-checkout pod and PR publication.
6. Re-run the full unit, race, vulnerability, image-contract, Helm, and Kubernetes API-server suites after any release-candidate change.
7. Run shadow mode per repository/Gate, record the confusion matrix, and keep enforcement/auto-merge disabled until the stated evidence threshold is met.
8. Only after those gates pass, perform the v1/v2 cutover with rollback and retention evidence.

The honest current conclusion is: **the Kubernetes-native shape is substantially implemented, but v3 is not yet a stable, isolated, production release.** No push, hosted Actions run, or deployment changes that conclusion.
