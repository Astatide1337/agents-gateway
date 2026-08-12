# Agents Gateway v3 requirements and evidence matrix

Audit date: 2026-08-12
Scope: accepted ADR-001 through ADR-029, as narrowed by the current v3 design,
quality, and build-vs-adopt documents.  This is an evidence audit of the current
worktree; it is not a release approval.

## How to read this matrix

The accepted requirements are defined by:

- `docs/v3/kubernetes-native-implementation-plan.md` — the current implementation
  contract and release gates;
- `docs/v3/quality-architecture-addendum.md` — ADR-015 through ADR-024;
- `docs/v3/build-vs-adopt-addendum.md` — ADR-025 through ADR-029;
- `docs/v3/build-vs-adopt-spike.md` and the phase-0 spike documents — falsification
  results and go/no-go conditions.

Evidence labels are deliberately strict:

| Classification | Meaning |
|---|---|
| **Proven** | The repository contains the required behavior and focused local evidence covers the stated requirement. This does not imply a live deployment unless the row is explicitly marked live. |
| **Partial** | A meaningful implementation or fail-closed boundary exists, but an accepted integration, runtime, release, or independent-production proof is still absent. |
| **Missing** | No acceptable implementation or required live proof was found. A unit test or a fail-closed error alone is not completion evidence. |
| **Deferred-by-design** | The accepted v3 scope explicitly postpones, rejects, or makes this optional. It is not a release defect unless the scope is later changed. |

The matrix distinguishes three kinds of evidence:

1. **Contract evidence** — types, validators, builders, and manifests.
2. **Local behavior evidence** — named tests and local scripts that execute without
   a cluster or external mutation.
3. **Live evidence** — a real two-node k3s environment, real upstream Sandbox,
   real object storage/trust root, real agentgateway request, GitHub App operation,
   and a disposable-repository shadow run. This audit also records a completed,
   disposable two-node k3s run nested in Docker; its live isolation evidence is
   valid for the contained k3s runtime but is explicitly not a substitute for
   two bare-metal target nodes. The earlier two-node Kind API/Phase-0 probe
   remains excluded from live-isolation evidence because `hostUsers:false` Pods
   could not start inside nested Docker.

The codebase-wide review and cleanup record is
[`codebase-review.md`](codebase-review.md). Its final local validation passed
unit/race tests, vet, reachable vulnerability scanning, chart/image contracts,
CRD drift, and production command builds. It also records why no v3 source or
test was deleted: every candidate was reachable or remains a documented
rollback/evidence boundary.

## Executive result

The current worktree is a substantial local implementation candidate, not a
production-complete v3 system. The strongest completed areas are the seven AGW CRD
contracts, admission invariants, deterministic workload construction, the
independent deterministic Gate, policy projections, bounded tool-result handling,
effect-ledger semantics, artifact identity binding, and local attestation tests.

The release blockers are external and integration-heavy: the disposable k3s run
does not prove the target two-node host floor; there is no real agentgateway
credential/model/MCP request, production object-store/KMS proof, GitHub App
disposable-repository PR, provider-backed critic, or labelled shadow window.
Agent Sandbox, UID airlock, and the lower-level Argo resource-template path now
have disposable live evidence, but not target-cluster or full artifact/publish
evidence.

The remaining implementation slices are correspondingly narrow. They are not a
rewrite of v2: production trust-root/reconciliation plumbing and an optional
reviewed symbol-provider image are the main remaining code/packaging areas. Most
remaining work is external validation and operational proof.

## Environment and evidence boundary

These checks made no production deployment, push, workflow dispatch, or
external-service mutation. One explicitly named disposable local Kind cluster
was created, mutated for API/Phase-0 probes, and deleted:

| Check | Observed evidence | Consequence |
|---|---|---|
| Kubernetes API smoke | Checksum-verified `kubectl` v1.36.3 and Kind v0.31.0 created disposable one- and two-node Kubernetes v1.35.0 clusters. The original API smoke established all seven AGW CRDs and all eight official Argo CRDs; the two-node probe additionally admitted live samples, rejected immutable execution-spec mutation, preserved monotonic `cancelRequested`, and passed server-side Phase-0 manifest checks. Both clusters were deleted after their runs. | The AGW/Argo CRD, admission, and manifest contracts are locally Proven; this is not target k3s, CNI, user-namespace, Agent Sandbox, or workload evidence. |
| Helm/Argo schema | Helm v4.2.3 rendered/linted both backends, the checksum-verified Argo v4.1.0 CLI passed strict offline WorkflowTemplate lint, and the official v4.1.0 full CRD Kustomize path installed server-side before the Argo chart dry-run. | Direct and Argo chart shape are locally Proven; no Workflow or Sandbox was executed. |
| Cosign | No host `cosign` binary is available; the exact pinned cosign binary was exercised from the local operator image in the prior local suite. | Local-key behavior is evidenced; production KMS/trust-root behavior is not. |
| Container runtime | Docker 29.7.1 is present. The disposable Kind nodes reported containerd 2.2.0, runc 1.3.4, and kernel 6.8.0, but `hostUsers:false` sandbox creation failed in nested Docker while mounting sysfs. | Local image/contract checks and API tests are possible; the nested runtime result is a negative Kind limitation and does not prove k3s/containerd behavior. |
| Disposable two-node k3s | A pinned `rancher/k3s:v1.35.0-k3s1` control-plane container and agent container became Ready on a private Docker network. The agent carried the required label and taint; the loopback-only API was removed after testing. | Substantive Phase-0 inventory, userns, airlock, credentials, and Sandbox checks are live-proven in this contained k3s runtime. This is not bare-metal node-floor or production-cluster evidence. |
| Disk | Root filesystem peaked at approximately 66% used with about 25 GB free while the disposable cluster and probe images were active. After teardown and removal of one unreferenced 1.35 GB Kind image, it returned to approximately 62% used with about 28 GB free. Active Docker/Coolify containers, application volumes, build cache, and logs were preserved. | Local validation can proceed without an image rebuild storm; Docker runtime state remains intentionally protected. |
| Hosted CI | No GitHub Actions workflow was dispatched. `.github/workflows/v3-ci.yml` is `workflow_dispatch`-only and optional for image/Kind work. | No hosted CI result is represented as evidence. |
| Phase-0 harness safety | Every Phase-0 script passed plan-mode rendering; explicit-context client dry-run passed for the applicable built-in resources; no-context dry-run safely skipped; cleanup confirmation, run-ID bounds, strict-mode object-store initialization, sanitizer adversarial cases, and the Kind worker label/taint were exercised locally. | Harness behavior is locally Proven. It does not prove a k3s API, CNI, user namespace, image digest, or Sandbox controller. |
| Git state | Existing cumulative user work includes untracked `v3/` and `docs/v3/` content plus workflow files. | This audit adds the Kind worker taint, shared Phase-0 safety/sanitizer fixes, and evidence updates; it does not interpret uncommitted work as a released artifact. |

The local validation contract is documented in `v3/scripts/README.md` and
`v3/scripts/validate-local.sh`: it runs repository tests, race tests, vet,
vulnerability checks, static manifest/CRD checks, and builds, while explicitly not
claiming a cluster, GitHub, object-store, or push. The previously recorded local
suite and focused adapter checks passed. Those results are cited below only as
local evidence.

## ADR-001 through ADR-014: base architecture

| ADR | Requirement | Concrete worktree evidence | Classification | Smallest remaining proof or slice |
|---|---|---|---|---|
| ADR-001 | Kubernetes is the control plane; do not retain a hand-built API, queue, lease table, or node agent as the product. | Seven CRDs and generated schemas under `v3/api/v1alpha1/`; `v3/internal/controller/` reconciles `AgentRun`; `v3/README.md` documents kubectl-first operation. A disposable two-node k3s API was exercised with the AGW CRDs and live child-resource contracts; the direct controller/FSM remains the default rollback path. | **Partial** | Prove the full AgentRun lifecycle on the target two-node k3s cluster; only then retire equivalent legacy control-plane slices. |
| ADR-002 | Adopt `kubernetes-sigs/agent-sandbox`; depend only on `Sandbox` in v3.0, with a Job fallback behind one interface. | `go.mod` and `v3/api` import `sigs.k8s.io/agent-sandbox/api/v1beta1`; `v3/internal/sandbox/` has primary Agent Sandbox and Job backends. Pinned v0.5.4 live evidence covered a digest-referenced controller, typed `Ready=True`/`Finished=True/PodSucceeded`, `hostUsers=false`, UID maps, Pod/PVC/Service generation, suspend/resume, shutdown, and cleanup in disposable k3s. | **Partial** | Repeat the lifecycle on the target nodes and under the full AGW artifact/runtime path, including restart and timeout behavior. |
| ADR-003 | Trust is produced by an independent Gate, not by the harness completion event. | `v3/internal/gate/engine.go` evaluates independent evidence; `v3/internal/verifycontroller/verifycontroller.go` requires Finished plus identity-bound evidence; `gate/engine_test.go` and verify-controller tests reject missing, forged, malformed, or mismatched evidence. | **Partial** | Execute the work and verify pods as separate live workloads and retain a signed report/artifact from a real run. |
| ADR-004 | Verification is from a fresh checkout in a pod the agent did not touch. | `v3/internal/workload/workload.go` builds separate work/verify shapes; verifier code copies changed tests onto a pristine base and checks `newTestsMustFailOnBase`; `TestBuildProducesADR006ADR007WorkTopology`, `TestReconcileEnsuresAndWaitsForFinishedEvidence`, and `Test...DoesNotAcceptFinishedSandboxWithoutEvidence` provide local evidence. | **Partial** | Prove the PVC is not shared with verify and that a malicious/modified work workspace cannot affect the clean-checkout run in k3s. |
| ADR-005 | The agent never holds a publish credential; GitHub App credentials are controller-owned and short-lived. | `v3/internal/githubapp/`, `githubpublish/`, `publishcontroller/`, workload tests, and secret-materialization tests keep publish credentials outside the agent workload; publish tests cover request identity and replay. There is no live App installation token or disposable PR. | **Partial** | Configure a scoped GitHub App in a disposable repository, verify credential canaries are absent from work pods, and create exactly one PR. |
| ADR-006 | Egress is denied to the agent by UID while the broker/gateway can reach allowed upstreams; user namespaces make `NET_ADMIN` namespaced. | `v3/internal/airlock/policy.go`, `v3/config/networkpolicy/runs-boundary.yaml`, and `v3/test/e2e/phase0/30-uid-airlock.yaml` render UID 1000/1337, `hostUsers: false`, and lockdown rules. Disposable k3s packet probes blocked pod/service/API/metadata/public/DNS/UDP paths while allowing the broker's loopback listener; the final chained guard/gateway request is still pending. | **Partial** | Repeat on target nodes and prove the real guard/gateway request, including exact argument and denied-write policy. |
| ADR-007 | Ordered init containers provide the network airlock: clone → skills → context → lockdown, then execution. | The init-container ordering and lockdown script are built in `v3/internal/workload/workload.go`; disposable k3s executed the live two-container lockdown and recorded positive broker access plus negative agent bypasses. | **Partial** | Prove the complete clone/skills/context/runtime sequence with provider and artifact boundaries. |
| ADR-008 | Use user namespaces, RuntimeDefault seccomp, drop-ALL, read-only rootfs, and a runtime-class escape hatch; do not assume gVisor in v3.0. | Workload builder and tests set `hostUsers: false`, RuntimeDefault, non-root UIDs, dropped capabilities, read-only mounts/rootfs, and no host network/PID/IPC. Disposable k3s proved subordinate UID/GID mappings, distinct ranges, and namespaced airlock behavior; `runtimeClassName` remains an escape hatch. | **Partial** | Prove the same mapping and namespaced capability behavior on the actual target node image and filesystem. |
| ADR-009 | Evaluate agentgateway, but do not weaken exact argument policy; accepted composition keeps an AGW guard as authority and uses agentgateway only after live equivalence. | `docs/v3/agentgateway-composition-spike.md` records v1.4.1 review, exact-argument limitation, broad-loopback limitation, and no effect-ledger extension. `v3/phase0/agentgateway-guard/` contains static/container harness material; `v3/pkg/toolpolicy/` and `v3/pkg/strictjson/` remain authoritative. | **Partial** | Complete the real per-run guard → agentgateway → upstream request with denied-write and credential-canary tests; central/shared gateway is not required for v3.0. |
| ADR-010 | Admission fails closed until a preflight proves the node security floor. | `v3/cmd/agw-preflight/`, `v3/internal/admission/`, chart preflight templates, and admission tests reject missing/stale/failed preflight evidence. The preflight probe has not run on a real node. | **Partial** | Run the preflight Job on every target node and make admission consume its current result during a real API-server session. |
| ADR-011 | Hard ceiling of the accepted AGW-owned CRD set; adopted Sandbox is not counted. | The current API package and generated manifests contain exactly seven AGW CRDs: `AgentRun`, `Agent`, `Gate`, `ToolSet`, `ModelRoute`, `Policy`, and `ContextStrategy`. `schema_test.go`/`types_test.go` cover schema and bounds. `EvalSuite` is intentionally not a v3 CRD. | **Proven** for the API contract | Apply the generated CRDs to the target API server and verify conversion/admission/print columns; this is an external API-server gate, not a missing CRD. |
| ADR-012 | `Rejected` means coherent work declined by policy; `Failed` means no coherent result; `UnknownEffect` is distinct and terminal. | `v3/api/v1alpha1/types.go` defines all phases; `v3/internal/fsm/agentrun_test.go` covers terminal transitions; `agentrun_test.go` and effects tests cover rejected work, failed evidence, and unknown external effects. | **Proven** locally | Confirm the phase/condition projection during a live restart and ambiguous GitHub response. |
| ADR-013 | Retention must reap PVC/workspace/artifacts safely; dry-run is the default and effect tombstones preserve replay fences. | `v3/internal/retention/` implements dry-run, bounds, safe deletion, and ledger tombstones; retention tests cover candidate selection and safety. There is no live local-path PVC/object-store retention run. | **Partial** | Run dry-run then an explicitly enabled cleanup against disposable PVCs and artifacts; verify unknown-effect records survive retention. |
| ADR-014 | Shadow mode first; trust is per repo and Gate and is earned from measured false-accept data before enforcing or auto-merge. | `GateSpec.Mode` and publish controller tests distinguish shadow/enforcing; `docs/v3/shadow-mode-audit.md` documents the protocol. No labelled 50-run window, confusion matrix, or auto-merge decision exists. | **Partial** | Run and human-label the first real shadow window. Auto-merge remains outside current accepted scope. |

## Build/adopt component audit

| Component requirement | Concrete evidence | Classification | Remaining requirement |
|---|---|---|---|
| k3s server/agent, CoreDNS, local-path storage, kube-router NetworkPolicy | Disposable two-node k3s proved Ready nodes, worker label/taint, CoreDNS, local-path, and NetworkPolicy packet behavior. | **Partial** for live adoption | Provision and record the same node floor and policy behavior on the target two-node host topology. |
| `agent-sandbox` adopted dependency | Go dependency, API use, primary backend, and focused backend tests exist; pinned v0.5.4 controller/PVC/Service/shutdown lifecycle passed in disposable k3s. | **Partial** | Repeat on target nodes and through the full AGW workflow/artifact path. |
| `agw-operator` | `v3/cmd/agw-operator/` and `v3/internal/controller/` implement admission, reconciliation, status, cleanup, direct orchestration, verification, publish, retention, and optional Argo selection. | **Partial** | The accepted design requires a thin translator/status mirror only after adopted lifecycle equivalence; prove it live before deleting direct code. |
| `agw-broker` / AGW guard | `v3/internal/broker/` has phase profiles, result bounds, and structured errors; `v3/internal/effects/` has the custom ledger; strict JSON/tool policy remains local. `agentgateway-composition-spike.md` says agentgateway cannot replace exact argument/effect semantics without the guard. | **Partial** | Wire a real per-run guard and agentgateway sidecar chain and prove the upstream request/credential boundary. |
| `agentgateway` | Version and composition assumptions are documented in `v3/phase0/agentgateway-guard/` and `docs/v3/agentgateway-composition-spike.md`; no live request, model listener, or credential exchange exists. | **Partial** | Live Phase-0 equivalence tests; shared central gateway is deferred. |
| `agw-runtime-codex` | `v3/pkg/codexadapter/`, runtime image validation, private `CODEX_HOME`/loopback-provider contract, and container live tests exist. The local test uses the adapter/container contract, not a production provider run. | **Partial** | Run the exact digest in a work Sandbox with the configured model route and capture a real runtime completion frame. |
| `agw-runtime-claude` | `v3/pkg/claudeadapter/` and image validation/tests exist, including output-pipe race coverage. | **Partial** | Real provider-backed Claude run and completion-contract evidence; no provider secret may enter the agent. |
| Clone, skills, context, lockdown, capture, verify-fetch, verify-apply, verifier, and preflight images | `v3/images/*/Containerfile`/`validate.sh` contracts and corresponding packages/tests exist; they are locally buildable/validatable. | **Partial** | Push reviewed immutable digests to the intended registry and exercise them in k3s; no push was performed for this audit. |
| `kubectl-agw` | `v3/cmd/kubectl-agw/` provides kubectl-first run/logs/cancel/status behavior and local command tests. | **Proven** locally | Verify against a live API server and ensure output reflects bounded `AgentRun.status`, not hidden console state. |
| Runtime protocol and adapters | `v3/pkg/runtimeproto/`, `v3/pkg/codexadapter/`, and `v3/pkg/claudeadapter/` include frame validation, private auth/config, bounded output, and race-focused tests. | **Proven** as local contract | Provider-backed, sandboxed execution and restart/terminal-event proof. |
| Skills digest verifier/materializer | `v3/pkg/skills/gateway.go`, `materialize.go`, and tests reject traversal, links/special files, oversized content, and digest mismatch; `v3/images/skills/` exists. | **Proven** locally | Materialize a real digest-pinned skill set in a work Sandbox and verify no network/credential path is opened by the agent. |
| Artifact catalog/event stream/object-store boundary | `v3/internal/artifactcatalog/`, `artifacts/`, `runtimeevents/`, `objectstore/`, `artifactauth/`, and `runsecret/` enforce content-addressed keys, bounds, scoped sessions, and identity binding with local tests. `internal/objectstore/local_s3_conformance_test.go` exercises the real AWS SDK against an ephemeral loopback S3-shaped TLS fixture for conditional create, exact read, immutable conflict, restart, and ambiguous transport behavior. | **Partial** | Real S3-compatible endpoint, IAM/session policy, and retention/restore test; provider-free SDK behavior is now proven. |
| GitHub App publisher | `v3/internal/githubapp/`, `githubpublish/`, `publish/`, and `publishcontroller/` implement scoped request construction, ledger fencing, shadow/enforcing, and UnknownEffect tests. | **Partial** | Real App installation token, disposable repo branch/PR/review, canary and ambiguous-response tests. |
| Argo Workflows adoption | `v3/internal/argoworkflow/` and `agw-agent-run-lifecycle` implement prepare → UID-bound Ready/Finished waits → stage → capture → authenticated handoff, a run-level deadline, retained PVC ownership, and a tokenless exit marker; the trusted controller owns credential/Sandbox cleanup. A live pinned Argo v4.1.0 preflight proved resource-template conditions and Workflow-created Sandbox Ready/Finished behavior in disposable k3s. | **Partial** | Execute the full Workflow against live Agent Sandbox, runtime, object storage, and retry/restart semantics before making it the production backend. |
| Argo Events | No EventSource/Sensor/EventBus is in the accepted stable path; canonical trigger remains `kubectl apply AgentRun`. | **Deferred-by-design** | Revisit only if an event-trigger requirement is accepted. |
| in-toto/DSSE/cosign | `v3/internal/cosignattestation/`, `verificationattestation/`, and `evidenceattestation/` implement canonical envelope/report binding; pinned cosign was exercised locally with an ephemeral key. | **Partial** | Production self-managed/KMS trust root, object-store persistence, key rotation/rollback, and independent verification on k3s. |
| OTel/Grafana/Loki operational platform | No v3-managed OTel/Grafana/Loki deployment is part of the accepted implementation contract; this is not a completion claim for the AgentRun product. | **Deferred-by-design** | Operate using the existing observability platform; add v3-specific signals only if accepted as a product requirement. |

## CRD and API contract audit

The current accepted v3 API owns seven CRDs. `Sandbox` is adopted from
`sigs.k8s.io/agent-sandbox`; `EvalSuite` is explicitly deferred rather than silently
missing.

| Kind | Required role | Concrete evidence | Classification |
|---|---|---|---|
| `AgentRun` | The only user-created run object: immutable resolved inputs, source/base, task, scope, workspace, output/publish, limits, bounded status and terminal distinctions. | `v3/api/v1alpha1/types.go`, `agentrun_types.go`; generated `v3/config/crd/agents.astatide.com_agentruns.yaml`; admission tests for task/reference/immutability/cancel; FSM/controller tests. | **Proven** for schema/local semantics; live API admission remains unproven. |
| `Agent` | Harness image, digest-pinned instructions, ToolSet/ModelRoute/context/Policy/skill references, sandbox template. | `v3/api/v1alpha1/agent_types.go`, generated CRD, schema tests, workload builder tests. | **Proven** for contract; **Partial** for runtime use because no live run resolves it. |
| `Gate` | Independent verification commands, clean checkout, scope/test-strength/coverage/diff/binary requirements, optional critic signals, policy references, shadow/enforcing. | `gate_types.go`, `gate_signal_types.go`, generated CRD, `gate/engine.go`, verifier, verifycontroller, scoring, and report tests. Mutation fields are intentionally absent. | **Proven** for deterministic contract; **Partial** for live independent execution/critic. |
| `ToolSet` | Exact MCP server/tool/argument policy plus phase profiles and bounded tool count. | `toolset_types.go`, generated CRD, `pkg/strictjson/`, `pkg/toolpolicy/`, `internal/broker/phase.go`, workload/security tests. | **Proven** locally; live upstream denial/credential boundary is unproven. |
| `ModelRoute` | Provider/model/family identity, credentials reference, priority and budget; critic must be a different family. | `modelroute_types.go`, generated CRD, admission tests for unknown route and family overlap, model budget validation. | **Proven** for admission contract; **Partial** for real provider request and spend enforcement. |
| `Policy` | One rule source: blocking/advisory context plus deterministic script check, digest, path and bounds. | `policy_types.go` CEL/validation; `internal/policycontract/` compile and projection tests; `contextpack` tests. | **Proven** for local compiler/validation; **Partial** for full live three-consumer execution. |
| `ContextStrategy` | Bounded lexical/structural/LSP/history/convention context with semantic tier disabled until measured, digest and budget. | `contextstrategy_types.go`, generated CRD, `contextpack/`, `contextmaterializer/`, local structural fallback, local symbol adapter and focused race/vet tests; the default context image contains no tree-sitter, LSP, or Serena provider and any explicit adapter must provide an executable SHA-256 pin. | **Partial**: deterministic/bounded machinery exists, but the optional parser/provider images and live materialization have not been proven. |
| Adopted `Sandbox` | Work/verify pod lifecycle, shutdown, PVC/service behavior under agent-sandbox. | `sigs.k8s.io/agent-sandbox` dependency; `internal/sandbox/` primary backend/tests; no AGW-generated Sandbox object has completed on a real cluster. | **Partial** |
| `EvalSuite` | Golden-set/judge/regression gate for changing Gate/Policy/Context/Model/image after labelled shadow data. | No `EvalSuite` type or accepted stable CRD; `docs/v3/quality-architecture-addendum.md` defers it until roughly 50 labelled shadow runs. | **Deferred-by-design** |

## Lifecycle phase audit

| Phase/transition | Requirement | Evidence | Classification |
|---|---|---|---|
| Pending → admission | Resolve refs, require fresh preflight, freeze resolved digest, materialize only required run inputs, fail closed. | `internal/admission/` tests; controller admission/preflight interfaces; `AgentRun` status resolved-spec fields. | **Proven** locally; live webhook/API session missing. |
| Cloning | Read-only setup credential, base SHA capture, pristine base, no agent credential. | Workload clone init builder/tests; source/base validation; publish credential separation tests. | **Partial** |
| Working | Agent runs with hardened workload, bounded phase tools, loopback broker/guard, runtime protocol. | Workload, broker, runtime, and security tests. | **Partial**: no live airlock/provider/Sandbox proof. |
| Capturing | Capture diff against recorded base, content-addressed patch and digest, bounded summary, delete work sandbox. | Controller capture path; `artifacts/`, catalog/objectstore tests; immutable snapshot and capture tests. | **Partial** |
| Verifying | Fresh verify workspace, apply expected patch, run configured commands, enforce independent evidence and policy. | `internal/verifier/`, `verifycontroller/`, gate/report tests; clean-checkout test-strength implementation. | **Partial**: no live verify pod/PVC and no live signed artifact. |
| Gated | Score candidates only after blocking checks; accept/reject distinct; evidence/report identities bound. | `gatescoring/`, `findingcorroboration/`, `gate/`, `verifycontroller/` tests. | **Proven** locally; **Partial** as production gate. |
| Publishing | Controller-owned GitHub App branch/commit/PR, never base push, effect-key fencing, UnknownEffect on ambiguity, shadow label. | `publishcontroller/` and `githubpublish/` tests cover replay/unknown/tampering/shadow. | **Partial** |
| Succeeded/Rejected/Failed/Cancelled/UnknownEffect | Terminal phases must not be retried or silently conflated. | `api/v1alpha1/types.go`, `fsm/agentrun_test.go`, controller terminal/retry tests. | **Proven** locally |
| Cleanup/finalizer | Delete work/verify resources and run Secret, retain/reap workspace/artifacts by policy, preserve effect tombstones, and never delete a same-name foreign Secret. | Controller cleanup/finalizer tests now require the exact single `AgentRun` owner (API version, name, UID, controller, and block-owner flags); `TestCleanupRefusesForeignPerRunCredentialSecret` proves the foreign object and finalizer are retained. `retention/` tests cover the separate retention policy. | **Partial**: no live PVC/object-store/restart proof. |

## Gate and trust invariants

| Invariant | Concrete evidence | Classification |
|---|---|---|
| Gate is independent from harness completion | `gate/engine.go` is pure and consumes evidence; verifycontroller refuses Finished without evidence. Tests include `TestReconcile...DoesNotAcceptFinishedSandboxWithoutEvidence`. | **Proven** locally; **Partial** live. |
| Fresh base checkout | Verifier contract and verify workload shape use a pristine base; `newTestsMustFailOnBase` copies only changed tests to the base. | **Proven** locally; **Partial** live. |
| Scope is respected | Gate engine checks included/forbidden paths and malformed path evidence; boundary tests cover forbidden and unlisted paths. | **Proven** |
| Maximum files and diff lines | `GateRequirements` and `gate/engine.go` enforce finite bounds; boundary tests cover equal/over limits. | **Proven** |
| No binary files | Gate engine and verifier reject binary/symlink/special-file evidence; tests cover these cases. | **Proven** locally; **Partial** live capture. |
| Commands are explicit and fail closed | `VerifyCommand` uses argv or explicit shell, verifier strict frames, command failure is evidence of failure, missing/timeout/overflow is not acceptance. | **Proven** |
| New tests must fail on base | `verifier/teststrength.go` and `Test...` tests require non-zero pristine-base result and prevent forged coverage markers. | **Proven** locally; **Partial** live toolchain. |
| Coverage cannot be forged | Coverage marker/digest/identity checks in verifier and report tests; no arbitrary accepted result shape. | **Proven** |
| Policy blocking rules are deterministic | `policy_types.go` requires script/exit0 for blocking rules; policy compiler produces separate self-check/Gate projections; tests reject advisory/blocking misuse. | **Proven** |
| Advisory findings cannot reject | `findingcorroboration` routes model-only assertions to advisory; `TestModelAssertionCanNeverBlock` and scoring tests cover it. | **Proven** |
| Critic is a different model family and fresh | Admission rejects family overlap; the production Job uses a digest-pinned critic plus agentgateway, a projected audience-specific JWT, exact central-gateway selector, per-run read-only verify Secret, full-Pod authentication, and controller-derived evidence. The operator/chart integration is fail-closed and off by default. | **Partial**: implementation proven locally; live different-family request missing. |
| Blocking critic finding needs machine evidence | Corroboration tests cover reproduction, static, symbol, and policy evidence; forged/duplicate/unknown/tampered findings are rejected. | **Proven** locally; **Partial** with no real critic. |
| Candidate score cannot override hard checks | `gatescoring/` fixed-point tests show score ranks only candidates passing blocking checks. | **Proven** |
| Exact tool arguments remain AGW-owned | `strictjson`/`toolpolicy` tests and composition spike document that agentgateway alone sees tool identity, not exact args. | **Proven** as design/local policy; **Partial** live chain. |
| Agent has no upstream egress | Workload/airlock static tests render UID rules and NetworkPolicy; the disposable k3s packet run passed the configured deny/allow probes, but target-node evidence is still absent. | **Partial** |
| Agent has no publish/provider credentials | Workload tests inspect mounts/env and publisher tests keep GitHub App path controller-side; no live canary test. | **Partial** |
| Report/artifact identity is immutable | `gate/report.go`, `argoworkflow/output.go`, attestation, and artifact tests bind run UID, generation, base, patch, workflow, event, and digest. | **Proven** locally; **Partial** production storage/trust root. |
| Signature verification is independent | Local cosign roundtrip and attestation tests exist; no production KMS/key rotation/independent verifier run. | **Partial** |
| Unknown external effect is terminal and fenced | `effects/ledger.go` tests cover claims, concurrency, tombstones, ambiguous result, and replay. | **Proven** locally; **Partial** live GitHub fault injection. |

## ADR-015 through ADR-024: quality architecture

| ADR | Requirement | Concrete worktree evidence | Classification | Remaining work |
|---|---|---|---|---|
| ADR-015 | Never fork Codex/Claude harnesses; own only context, tools/results, attempt selection, and feedback. | Runtime adapters remain in `v3/pkg/codexadapter/` and `claudeadapter/`; outer-loop code is in workload/broker/verifycontroller; no replacement harness is introduced. | **Proven** for architecture/local contracts | Provider-backed sandbox run and protocol evidence. |
| ADR-016 | One Policy artifact is compiled into context, deterministic self-checks, and independent Gate checks; blocking checks must be deterministic. | `v3/internal/policycontract/` has canonical rules, digest/bounds, `ContextPackRules`, `SelfChecks`, `GateChecks`; policy/context tests cover permutation determinism and advisory/blocking semantics. | **Proven** locally | Prove all three projections are consumed in one live run and that Gate scripts execute from pristine base. |
| ADR-017 | ContextPack uses bounded lexical, structural, symbol, history, and policy tiers; semantic embeddings/code graph are off; digest and budget are recorded. | `contextpack/`, `contextmaterializer/`, deterministic file-oriented structural fallback, bounded local symbol adapter, status context digest, and tests exist. `v3/images/context/README.md` documents the default-disabled provider contract and executable SHA-256 verification; `lsp-serena` does not claim Serena availability. | **Partial** | Supply and review a digest-pinned tree-sitter/parser or LSP provider image if those tiers are required, or intentionally run with the corresponding tier disabled; measure materialization on a real run. |
| ADR-018 | Candidate fan-out, testgen, Gate ranking, and repair attempts are optional quality levers, not default architecture. | No fan-out controller/CRD or testgen path is in the accepted stable contract; `gatescoring/` can rank already-present candidate evidence. | **Deferred-by-design** | Add only after an EvalSuite demonstrates benefit over cost. |
| ADR-019 | Hybrid verification is execution plus different-family critic; mutation signal is deferred; hard rules gate score. | Deterministic Gate/verifier, fixed-point scoring/corroboration, production critic Job/source, native Anthropic Messages forwarding through exact agentgateway v1.4.1, operator wiring, and chart configuration exist. No provider credential enters the run pod. Mutation is absent by design. | **Partial** | Prove a real fresh-sandbox, different-family request and authenticated evidence on target k3s. Mutation remains deferred. |
| ADR-020 | A critic finding blocks only with deterministic corroboration; model assertion is advisory. | `v3/internal/findingcorroboration/` and tests route evidence classes and reject forgery/tampering; verifycontroller binds canonical critic evidence. | **Proven** locally; **Partial** production | Run against a real critic response and real verifier/static/symbol evidence. |
| ADR-021 | Compile to open `AGENTS.md`, Agent Skills, policy scripts, and broker config; do not invent a harness config dialect. | Policy/context materializer writes standard paths and manifest; `pkg/skills/` digest-verifies skills; workload uses `AGENTS.md`, `.agents/skills`, `.agents/policies`, and context manifest conventions. | **Proven** locally | Verify generated artifacts inside an actual work Sandbox and test both supported harnesses. |
| ADR-022 | Phase-scoped tool profiles, max 12 tools, truncate/reference, structured errors, and incremental rejection feedback; do not claim unimplemented observation masking. | `internal/broker/phase.go` and tests implement profile transitions, counts, result byte bounds/reference files, and structured errors. The broker's phase transition primitive remains unbound; `cmd/agw-broker`, `docs/v3/trusted-phase-supervisor-contract.md`, workload tests, and chart schema keep deployment fail-closed because no independent supervisor is wired. | **Partial** | Connect the shaping path to the real MCP/model request and capture size/phase metrics; deploy and live-test the separate phase authority/process observer before exposing edit tools. Observation masking remains unclaimed. |
| ADR-023 | Findings are an output mode (`patch`, `findings`, `both`), with corroborated findings and explicit pre-existing review scope. | `AgentRunSpec.Output`, publish/findings parsing and digest tests, report/status types, and verifycontroller output handling exist. | **Partial** | Prove a real findings-mode run and GitHub review comment/PR evidence; no live reviewer provider exists. |
| ADR-024 | EvalSuite/golden set gates changes after shadow data; no enforcing/auto-merge claim before measured false-accept evidence. | `docs/v3/quality-architecture-addendum.md` defers the CRD until roughly 50 labelled shadow runs; `docs/v3/shadow-mode-audit.md` describes the protocol, but no labelled run records or suite implementation are present. | **Deferred-by-design** | Start only after real shadow runs; never represent current shadow scaffolding as a green EvalSuite. |

## ADR-025 through ADR-029: build-versus-adopt decisions

| ADR | Requirement | Concrete worktree evidence | Classification | Remaining work |
|---|---|---|---|---|
| ADR-025 | Argo Workflows may own durable sequencing/retries/artifacts/exit handling; AGW retains a thin identity translator/status mirror and a type-aware Sandbox wait bridge; direct backend is fallback until equivalence. | `v3/internal/argoworkflow/` translates/binds/observes identity-bound lifecycle output; the controller bootstraps the operator-owned work child before creating the Workflow; orchestration tests reject replay/tamper and never treat Argo phase as the AgentRun verdict. The chart template is opt-in. | **Partial** | Execute the identity-bound Workflow against live Sandbox/artifact/runtime infrastructure, then compare direct/adopted semantics before deleting the direct path. |
| ADR-026 | Adopt agentgateway only as a guarded, per-run downstream sidecar for credential injection/routing/translation/telemetry; AGW guard retains exact args, effect ledger, runtime protocol, artifact, and identity authority. | `docs/v3/agentgateway-composition-spike.md`, the provider-free `v3/phase0/agentgateway-guard/gateway/` chain, the real-image Phase-0 pod, `pkg/strictjson/`, `pkg/toolpolicy/`, `internal/effects/`, and workload UID constants encode this boundary. The provider-free chain passes locally; no live agentgateway/provider request or credential canary exists. | **Partial** | Prove UID 1000 → guard 1337 → real gateway 1338 and exact-argument/denied-write/credential-canary behavior with the reviewed image. Shared central gateway is deferred. |
| ADR-027 | Use in-toto/DSSE/cosign with a self-managed/KMS trust root; retain canonical Gate predicate and independent verification; dual format only during measured migration. | `cosignattestation/`, `verificationattestation/`, and `evidenceattestation/` bind reports; pinned cosign is copied from the operator image and local ephemeral-key roundtrip passed. No KMS, object-store, rotation, rollback, or dual-format live proof. | **Partial** | Configure a production trust root and independent verifier; exercise rotation/rollback and object-store ambiguity before any deletion of existing evidence format. |
| ADR-028 | Do not build a webhook receiver now; Argo Events is deferred and canonical trigger is `kubectl apply AgentRun`. | No EventSource/Sensor/EventBus in accepted stable manifests/docs; `kubectl-agw` and AgentRun API are the trigger surface. | **Deferred-by-design** | Revisit only with an accepted event-trigger requirement. |
| ADR-029 | Keep only unique value: Gate/evidence, effect ledger, runtime adapters/protocol, Policy compiler, immutable resolver/translator/bridge/status, preflight, and shadow data. Delete/reduce equivalent code only after live equivalence/release/rollback proof. | These packages exist under `v3/internal/gate`, `effects`, `pkg/runtimeproto`, adapters, `policycontract`, `argoworkflow`, admission/controller, preflight, and shadow docs. `docs/v3/build-vs-adopt-spike.md` records that the original 3,000–4,500 estimate is falsified for the accepted scope and lists G0–G7 deletion gates. | **Partial / architecture deviation** | Finish external gates and equivalence proof; delete proven redundant paths in a separate reviewed change and report the measured total. |

## Phase-by-phase completion

| Phase | Accepted deliverable | Evidence and status |
|---|---|---|
| Phase 0 / P0-A baseline | Preserve a local baseline and avoid wasting hosted CI. | `v3/scripts/validate-local.sh`, `scripts/README.md`, and test/build reports provide local baseline. No Actions run or deployment was made. **Proven locally.** |
| P0-B node/runtime floor | Two nodes; kernel/containerd/runc/idmap filesystem checks. | Disposable k3s reported two Ready nodes, kernel 6.8, containerd 2.1.5, and live subordinate UID mappings. **Partial live proof:** bare-metal target node floor and full runc/filesystem inventory remain pending. |
| P0-C user namespace/airlock | Prove UID owner filtering, namespaced `NET_ADMIN`, blocked API/node metadata, and allowed guard path. | Disposable k3s packet run passed agent/broker UID rules, userns, blocked API/metadata/private/public/DNS/UDP paths, and broker loopback access. **Partial live proof:** final guard→agentgateway/provider request remains pending. |
| P0-D Agent Sandbox lifecycle | Install upstream controller; create Sandbox/PVC; prove Ready/Finished/shutdown/reap. | Pinned v0.5.4 manifest and immutable controller image were live-installed in disposable k3s; typed `Ready=True`/`Finished=True/PodSucceeded`, Sandbox Pod/PVC/Service, `hostUsers=false`, suspend/resume, shutdown and cleanup passed. **Partial live proof:** target nodes and full AGW artifact path remain pending. |
| P0-E agentgateway decision | Real Model A/per-run guard composition, exact argument and credential canaries. | Provider-free guard → fixed gateway contract → recording MCP chain passes locally, including notification framing, one-time handshake, exact-argument denial, unapproved-write denial, and canary absence. The real agentgateway image/provider path remains unproven. **Missing live proof.** |
| P0-F object-store conformance | Conditional content-addressed writes, bounds, IAM/session prefix, restart/ambiguity. | Local objectstore/artifact/auth tests. No real endpoint or IAM. **Partial implementation; Missing live proof.** |
| P0-G Argo/evidence envelope | Prove Argo output binding, retry identity, artifact and report evidence. | Live pinned Argo preflight proved CRDs/controllers, resource-template success/failure conditions, and Workflow-created Sandbox Ready/Finished plus `hostUsers=false`; full object-store/bridge/artifact/retry evidence remains missing. **Partial live proof.** |
| Phase 0 exit gate | All P0 checks green before production implementation/cutover. | The required external checks are not green/recorded. **Missing.** |
| Phase 1 thin end-to-end slice | One AgentRun clones, runs, captures a patch artifact, and cleans up. | Controller/backend/capture code and local tests exist; no real work Sandbox or artifact exists. **Partial.** |
| Phase 2 broker | ToolSet/ModelRoute, guarded credentials, runtime protocol, ledger, artifact upload, skills. | Direct broker/strict policy/runtime/ledger/skills implementations and tests exist; real agentgateway composition is not wired/proven. **Partial.** |
| Phase 3 Gate | Clean verify Sandbox, scope/test strength/diff/policy checks, independent critic, evidence/report. | Deterministic Gate plus the production critic workload/source/operator wiring are locally strong; no live verify/critic pod or provider request. **Partial.** |
| Phase 4 publish | GitHub App branch/commit/PR, ledger/UnknownEffect, bounded PR evidence. | Publisher/controller tests cover replay and ambiguity; no real App/repo/PR. **Partial.** |
| Phase 5 operate | Preflight, retention, fault/restart/cleanup/rollback, shadow measurement. | Preflight/retention and failure tests exist; no node, PVC, restart, rollback, or labelled shadow run. **Partial.** |
| Phase 6 EvalSuite | Golden set and regression gate after actual shadow labels. | Explicitly deferred; no suite or labelled corpus. **Deferred-by-design.** |
| Phase 7+ fan-out/mutation/semantic | Add only when measured quality/cost numbers justify it. | Explicitly deferred in quality addendum; no fan-out/testgen/mutation/semantic graph implementation in accepted core. **Deferred-by-design.** |

## Live proof and release-gate audit

These are not satisfied by unit tests, static YAML, or an error path that refuses to
continue.

| Required live proof | Evidence found | Classification |
|---|---|---|
| Phase-0 harness safety and cleanup | Local plan/client-dry-run matrix, explicit-context requirement, cleanup confirmation, bounded run IDs, adversarial sanitization, Kind label/taint check, and disposable k3s run. | **Proven in disposable k3s; target execution still required** |
| Target two-node k3s, tainted agent node, node floor | Disposable Docker-contained k3s provided two Ready nodes, required label/taint, CoreDNS, local-path, and packet-level evidence. | **Partial live proof; target bare-metal floor missing** |
| Preflight proves user namespace mapping and namespaced `NET_ADMIN` | Disposable k3s proved subordinate mappings, distinct ranges, namespaced firewall setup, and agent capability/firewall denial. | **Partial live proof; target node result missing** |
| Agent can reach only the intended guard/gateway; no upstream/API/node metadata | Disposable k3s airlock blocked pod/service/API/node metadata/public/DNS/UDP bypasses and allowed broker loopback. | **Partial live proof; real guard→gateway/provider request missing** |
| Upstream Agent Sandbox reaches Ready and Finished and is reaped | Pinned v0.5.4 live controller, installed with manifest and controller-image digests, created a Sandbox Pod/PVC/Service, reported typed `Ready=True` and `Finished=True/PodSucceeded`, preserved `hostUsers=false`, and passed suspend/resume/shutdown/PVC cleanup; Argo preflight also observed Ready/Finished. | **Partial live proof; target/full workflow missing** |
| Work/verify isolation and fresh checkout on actual PVCs | Provider-free direct live gate exercised separate work/capture/fresh-verify PVC boundaries, exact AgentRun owner UIDs, and an independent failure child; the real runtime/clean clone path is still pending. | **Partial live proof** |
| Real model request through configured ModelRoute | A no-paid-call two-gateway recording chain proves native request shape and one-attempt forwarding, but no real provider credential/request was used. | **Missing live proof** |
| Real MCP request through guard/agentgateway with exact argument denial | Provider-free Go chain now exercises guard → fixed gateway contract → recording MCP and proves the credential canary is injected only downstream; no real agentgateway binary, Kubernetes pod, or provider request has run. | **Missing real-agentgateway proof** |
| Credential canaries never appear in agent environment/results/logs | Disposable k3s credential Phase-0 passed clone/broker scoped canaries, agent absence scans, no credential env, no Secret/service-account mounts, and credential-free Git config. | **Partial live proof**: provider/runtime logs and full GitHub App path remain pending. |
| Real S3-compatible object store with scoped session/IAM and conditional writes | Local object-store fake/contract tests; no endpoint or IAM result. | **Missing** |
| Production cosign/in-toto trust root, verification, rotation and rollback | Explicit provider/KMS-or-absolute-file reference contract, separate verification-key wiring, and immutable read-after-write reconciliation are locally tested and documented. | **Partial** |
| Argo successful lifecycle producer and retry idempotency | Disposable k3s Argo preflight executed resource-template condition probes and a Workflow-created Sandbox with Ready/Finished observation; full AGW artifact/retry/idempotency path remains unexecuted. | **Partial live proof** |
| Argo and direct backend semantic equivalence | Unit-level identity/replay tests; no paired live run/rollback evidence. | **Missing** |
| GitHub App disposable-repository PR/review | Publisher tests only; no App installation or PR URL. | **Missing** |
| Restart/fault injection: controller, worker, verify, object-store and GitHub ambiguity | Local failure/idempotency tests; no live fault run. | **Missing** |
| Cleanup/retention preserves UnknownEffect and removes PVC/artifacts as configured | Retention/controller tests only; no live local-path/object-store cleanup. | **Missing** |
| Real first shadow run and human labels | Shadow protocol is documented; zero evidence of a labelled run/window. | **Missing** |
| 50-run per-repo/per-Gate false-accept window and enforcing decision | No EvalSuite/corpus/confusion matrix. | **Missing** |
| Immutable image/chart digests are published and deployable | Local image validators and pinned references exist; no registry release or deployment. | **Missing** |

## Deletion and reduction audit

The accepted build-vs-adopt documents explicitly corrected the original aggressive
deletion budget: deletion is conditional on live equivalence, release, rollback, and
shadow evidence. The presence of old code is therefore not automatically a defect.

| Original deletion/reduction item | Current state | Classification | Safe next action |
|---|---|---|---|
| v2 HTTP API/store/authz/identity/credential control plane | Legacy v2 and `agents_gateway/` material remain; v3 has new CRDs/controller and direct local path. | **Deferred-by-design** until cutover proof | Do not delete before direct/adopted equivalence and rollback evidence. |
| v2 localengine/workflow/orchestration/runstate | v3 still has a direct FSM/controller plus an optional Argo translator. | **Deferred-by-design** for now; duplicate lifecycle is **Partial** | Prove Argo lifecycle first; then remove only redundant implementation. |
| v2 runner/sandbox/node-agent logic | v3 has custom lifecycle adapters because direct backend is still the safe fallback; adopted Sandbox backend is present. | **Deferred-by-design** | Keep the fallback until upstream lifecycle and outage semantics are proven. |
| Hand-built retry/backoff/finalizer | Controller/FSM and cleanup remain in the direct backend; tests cover idempotency and terminal phases. | **Deferred-by-design** pending Argo migration | Do not remove the safety behavior merely because Argo is declared. |
| Hand-built webhook receiver | No new receiver in accepted path. | **Deferred-by-design** | Keep kubectl apply as canonical trigger. |
| Custom signing scheme | Current attestation uses cosign/in-toto-style envelope packages; old compatibility is retained until parity. | **Partial** | Complete KMS/rotation/dual-format evidence before removing compatibility. |
| `toolpolicy`/`strictjson` exact argument matcher | Still present and required by ADR-009/026 because agentgateway does not provide exact argument authority. | **Proven / retained by design** | Do not delete or delegate without a live equivalence proof. |
| Effect ledger | Still present in `v3/internal/effects/`; no adopted replacement exists. | **Proven / retained by design** | Keep as product core. |
| `runtimeproto` and Codex/Claude adapters | Still present under `v3/pkg/`; accepted as unique completion-contract code. | **Proven / retained by design** | Keep; validate with real providers. |
| Skills/artifact/artifact-catalog code | Still present and tested; accepted as required core. | **Proven / retained by design** | Keep; prove live materialization/storage. |
| Telemetry subsystem | No v3 OTel operator/Grafana deployment in accepted scope. | **Deferred-by-design** | Do not expand the v3 release to recreate the separate observability platform. |
| Web console | `kubectl-agw` is the accepted interface; web console is a non-goal. | **Deferred-by-design** | Keep CLI/k9s workflow. |
| Python v1 and legacy gateway | Existing legacy directories remain in the worktree; no cutover/release gate has passed. | **Deferred-by-design** | Delete in a separately authorized cleanup after v3 release/rollback readiness. |
| Total LOC target | The original addendum estimated 3,000–4,500 non-test Go lines. The 2026-08-12 audit measured 71,183 current lines and a conservative unique-core lower bound of approximately 12,508 before several required responsibilities. | **Open architecture deviation** | Remove proven duplicate plumbing at the live equivalence gates, publish each measured reduction, and never represent the original estimate as achieved. |

## Deferred features audit

| Feature | Accepted disposition | Evidence | Classification |
|---|---|---|---|
| Hostile multi-tenant execution | Non-goal: one owner/trusted submitter. | Base design non-goals and current namespace/RBAC posture. | **Deferred-by-design** |
| Web console | Non-goal; kubectl/k9s is the interface. | `v3/README.md`, `kubectl-agw`. | **Deferred-by-design** |
| A custom DAG/workflow engine | Replaced conditionally by Argo; no custom v3 DAG product. | `docs/v3/build-vs-adopt-addendum.md`, `internal/argoworkflow/`. | **Deferred-by-design** |
| Long-lived conversational agent serving | Out of scope; AgentRun is batch-like. | Base product statement/non-goals. | **Deferred-by-design** |
| Replacing agent-sandbox or agentgateway | Both are adopted/evaluated, not reimplemented wholesale. | Build/adopt addendum and composition spike. | **Deferred-by-design** |
| Candidate edit fan-out and test-generation rollouts | Wait for measured best-of-N benefit/cost. | Quality addendum ADR-018. | **Deferred-by-design** |
| Mutation testing | Add only if false-accept data identifies weak tests. | Quality addendum ADR-019; not represented in `GateSignalSpec`. | **Deferred-by-design** |
| Semantic embeddings or a custom code graph | Tiers 1–3 first; semantic off; buy/use symbol tooling instead of building graph. | `ContextStrategy`, context image README, ADR-017. | **Deferred-by-design** |
| EvalSuite CRD/golden judge | Requires actual labelled shadow data first. | Quality addendum ADR-024 and no `EvalSuite` type. | **Deferred-by-design** |
| Auto-merge | Separate later decision after false-accept evidence. | ADR-014/quality addendum. | **Deferred-by-design** |
| Unbounded repair loops | Maximum one repair attempt initially; no open-ended loop. | Quality addendum and bounded status/types. | **Deferred-by-design** |
| Shared central agentgateway | v3.0 uses guarded per-run composition; shared gateway requires a new trust boundary. | Build-vs-adopt addendum and composition spike. | **Deferred-by-design** |
| Argo Events | Deferred; kubectl AgentRun is canonical trigger. | ADR-028. | **Deferred-by-design** |
| Public Fulcio/Rekor keyless trust root | Not assumed; self-managed/KMS trust root is the accepted direction. | ADR-027. | **Deferred-by-design** |
| gVisor/Kata | `runtimeClassName` is an upgrade field, not a v3.0 requirement. | ADR-008 and workload types. | **Deferred-by-design** |
| `SandboxClaim`/`SandboxWarmPool`/`SandboxTemplate` dependency | v3.0 depends only on upstream `Sandbox`; fallback remains. | ADR-002 and API/dependency code. | **Deferred-by-design** |
| Broader observability platform inside v3 | Existing centralized observability is outside AgentRun acceptance. | Current v3 docs and absence of OTel/Grafana manifests. | **Deferred-by-design** |

## Smallest remaining code slices versus external gates

The following are the minimum slices suggested by the evidence. They are deliberately
not a proposal to edit them in this audit.

### Remaining code or packaging slices

| Slice | Why it remains | Smallest boundary |
|---|---|---|
| Critic live capability evidence | The runner/source/operator/chart path exists, but its central JWT policy and real provider route are deliberately enabled only by reviewed evidence digests. | Deploy the exact central route, prove invalid/missing JWT rejection and one real different-family request, then record those digests in deployment values. |
| Argo/direct semantic proof | The type-aware condition bridge and lifecycle producer exist, but no live Sandbox/object-store Workflow has run and direct remains the rollback path. | Run paired disposable direct/Argo jobs, compare evidence/status/cleanup, and retain direct until the results and rollback drill match. |
| Production attestation/reconciliation plumbing | CLI, chart, cosign adapter, and lifecycle now fail closed on relative/ambient key references and read-after-write identity mismatch. | Prove the selected KMS/file trust root, external key mounting, object-store behavior, rotation/rollback, retained-bundle verification, and independent verification on k3s. |
| Reviewed symbol-provider image, if symbols are required | Local LSP adapter is bounded and verifies `AGW_CONTEXT_SYMBOL_ADAPTER_SHA256`, but `v3/images/context/README.md` says no Serena/LSP server is bundled and symbols are disabled without a separately digest-pinned provider. | Supply one reviewed digest-pinned provider image/config or explicitly run tier 3 disabled; no semantic index should be added yet. |
| Release manifest/registry packaging | Local validators and pinned references exist, but no immutable image/chart release was published or deployed. | Build/publish the reviewed digests and render the release chart in a controlled environment; this is a release operation, not needed for the matrix itself. |

### Remaining external live gates

| Gate | Why code/tests cannot close it |
|---|---|
| Two-node k3s and node floor | Kernel, containerd/runc, idmap filesystem, tainting, DNS, storage, and NetworkPolicy are properties of the target cluster. |
| User-namespace/UID airlock | Only a packet-level run on the target runtime can prove that agent UID 1000 cannot use upstream egress while guard/gateway UIDs can. |
| Upstream Agent Sandbox | Controller version, CRD behavior, condition shape, PVC lifecycle, shutdown/reap, and API defaults are external runtime behavior. |
| Real per-run agentgateway chain | Exact argument visibility, provider/model routing, credential injection, and canary absence require a real request through the selected versions. |
| Real object store and trust root | Conditional writes, IAM scope, KMS signing, artifact persistence, outage ambiguity, and independent verification cannot be established by an in-memory fake. |
| Real Codex/Claude provider execution | Adapter/container tests prove local protocol contracts, not provider authorization, model response, quota, or completion on a Sandbox. |
| GitHub App disposable repository | Branch/commit/PR/review permission scope, one-hour token behavior, rate limits, and ambiguous API response handling need a real installation. |
| Argo lifecycle and retry | The current producer is intentionally non-successful; a live Argo server and Sandbox controller must prove no duplicate logical Sandbox, artifact, or publish effect. |
| Fault/restart/cleanup/rollback | Kubernetes rescheduling, object-store/GitHub ambiguity, PVC cleanup, and UnknownEffect recovery require induced failures in disposable infrastructure. |
| Shadow quality window | False-accept rate, false-reject cost, critic precision, and Gate calibration require human labels from real runs; no amount of unit coverage substitutes for that dataset. |

## Final release decision

**Current status: Partial, not production-complete.**

The worktree has enough local implementation to justify the next validation phase and
the architecture has correctly narrowed the custom product surface. It is not honest
to mark ADR-001 through ADR-029 complete because the requirements that make this a
production system — isolated live execution, independent verification, credential
boundaries, durable artifacts/attestations, idempotent publishing, rollback/cleanup,
and measured shadow quality — have not yet been demonstrated in their real runtime.

The next smallest step is not a broad rewrite or a hosted CI run. It is to provision a
disposable two-node k3s test environment, run P0-B through P0-G in order, and record
the required live evidence. Until those gates pass, keep the direct backend and
fail-closed critic/Argo boundaries, and do not delete v2/direct compatibility code.
