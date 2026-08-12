# Agents Gateway v3: build-vs-adopt architecture spike

Status: research and plan correction only. No implementation, deployment, deletion, or
release is authorized by this document.

Research snapshot: 2026-08-12 (America/New_York). Upstream release metadata and source
were checked on this date. The source revision is recorded whenever a claim depends on
implementation behavior.

## Decision summary

| Proposal | Decision | Correction |
| --- | --- | --- |
| ADR-025: Argo Workflows replaces the v3 lifecycle operator | **Hybrid, conditional adopt** | Adopt Argo for durable sequencing, retries around safe compute, parameters, artifacts, synchronization, and garbage collection. Do not delete the current lifecycle controller or claim that a generic Argo resource template can currently wait safely for `Sandbox` `Ready=True` **and** `Finished=True`. The condition-array bridge and child-artifact path must pass Phase 0 first. |
| ADR-027: cosign replaces the signed Gate report | **Adopt the format selectively; reject the public-keyless assumption** | in-toto Statement + DSSE + cosign is a good portable envelope. Public Fulcio does not accept an arbitrary self-managed k3s ServiceAccount issuer by default. Keep a self-managed key/KMS trust model for this cluster, and keep the canonical Gate predicate and independent verifier. |
| ADR-028: Argo Events replaces a webhook receiver | **Defer** | Argo Events is capable, but webhook triggering is not part of the current `kubectl apply AgentRun` contract. Adding EventBus, EventSource, Sensor, public ingress, replay handling, and new RBAC now adds an unrequested control plane. |

The original proposal estimated a `3,000–4,500` non-test Go landing zone. A fresh
independent count on 2026-08-12 disproved that estimate: the current v3 tree contains
**71,183 physical non-test Go lines across 169 files** (including 1,415 generated
deepcopy lines; the older Phase-0/probe subtotal is no longer a current snapshot).
This is an explicit architecture
deviation, not a silently waived success criterion. The retained runtime contract
alone is approximately 3,897 lines, and a conservative lower bound for the unique
typed API, immutable identity, Gate/verifier/corroboration, effect ledger, and Policy
compiler is already approximately 12,508 lines before admission, artifact, sandbox,
retention, publication, and CLI responsibilities are counted.

Argo and agentgateway must still remove duplicated platform plumbing after live
equivalence proof, but forcing the total below 4,500 would require deleting accepted
product contracts. Track reduction by owned responsibility and measured deletion,
and report the resulting total at each cutover gate; do not claim the original
numeric estimate has been met.

## Version and revision ledger

These are the current stable releases returned by the upstream GitHub release APIs on
2026-08-11. The commit is the dereferenced commit where the tag is annotated; where the
tag is lightweight it is the tag commit.

| Component | Release | Published | Revision | Primary source |
| --- | --- | --- | --- | --- |
| Argo Workflows | `v4.1.0` | 2026-08-11 | `e5ed20d5cb54d4708d5aeb29148b3e49922f795c` | [release](https://github.com/argoproj/argo-workflows/releases/tag/v4.1.0), [resource executor source](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/workflow/executor/resource.go) |
| Agent Sandbox | `v0.5.4` | 2026-07-30 | `6e2b7617310e3bf084b6d1a1cffbeb141a5e37fe` | [release](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v0.5.4), [v1beta1 API source](https://github.com/kubernetes-sigs/agent-sandbox/blob/v0.5.4/api/v1beta1/sandbox_types.go) |
| Argo Events | `v1.9.11` | 2026-07-13 | `c8ae7fc2dea2e368f44c8d361b73d7712b100cec` | [release](https://github.com/argoproj/argo-events/releases/tag/v1.9.11), [webhook docs](https://argoproj.github.io/argo-events/eventsources/setup/webhook/) |
| cosign | `v3.1.3` | 2026-08-06 | `2f3a85b04907df5b770eb049d7e4d08d4b018d86` | [release](https://github.com/sigstore/cosign/releases/tag/v3.1.3), [attest-blob source](https://raw.githubusercontent.com/sigstore/cosign/v3.1.3/cmd/cosign/cli/attest/attest_blob.go) |
| Fulcio | `v1.8.8` | 2026-07-08 | `501ea9d9711b1c24a86217822b8d115fe5b21bb4` | [release](https://github.com/sigstore/fulcio/releases/tag/v1.8.8), [public identity config](https://raw.githubusercontent.com/sigstore/fulcio/v1.8.8/config/identity/config.yaml) |
| Sigstore policy-controller | `v0.15.1` | 2026-03-26 | `cf6bc4f1c0817e0c40f703c6d8b4ae8c33d26b96` | [release](https://github.com/sigstore/policy-controller/releases/tag/v0.15.1), [official overview](https://docs.sigstore.dev/policy-controller/overview/) |
| Kyverno | `v1.18.2` | 2026-07-10 | `18b41b2505c8b16c34892691f9739848d25b5447` | [release](https://github.com/kyverno/kyverno/releases/tag/v1.18.2), [validate policy docs](https://kyverno.io/docs/policy-types/cluster-policy/validate/) |

The existing v3 module pins `sigs.k8s.io/agent-sandbox v0.5.4`, which is current, not
stale. `v0.5.3` is the preceding stable release. Both versions expose only a
`SandboxStatus.Conditions []metav1.Condition` status field; neither adds a summary
`status.phase`. The official v0.5.3 release page was therefore an insufficient basis
for calling the current v0.5.4 pin stale.

## A. Argo Workflows (ADR-025)

### Proven facts from v4.1.0

Argo Workflows resource templates are current supported API surface. The current API
still defines `ResourceTemplate`; the current documentation describes it as able to
create, update, delete, patch, replace, or get a single arbitrary Kubernetes resource,
including a CRD. There is no deprecation marker on this template in the v4.1.0 API or
documentation. That establishes “supported,” not a blanket recommendation that every
CRD lifecycle should be driven through it.

Primary references: [Kubernetes resource walkthrough](https://argo-workflows.readthedocs.io/en/latest/walk-through/kubernetes-resources/),
[v4.1.0 `ResourceTemplate` type](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/pkg/apis/workflow/v1alpha1/workflow_types.go),
[v4.1.0 resource executor](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/workflow/executor/resource.go).

The exact wait behavior in v4.1.0 is:

1. `successCondition` and `failureCondition` are parsed with Kubernetes
   `labels.Parse`, not CEL and not a general JSONPath evaluator.
2. A field lookup is implemented by `gjson.GetBytes` against the serialized resource.
3. Comma-separated selector requirements are ANDed.
4. Failure requirements are evaluated first; any matching failure terminates the
   resource step as failed.
5. Every success requirement must match before the step succeeds.
6. The resource is polled until the workflow context is cancelled or a condition is
   met. The kubectl operation also has a client-go transient-error retry wrapper.

Argo documents the same language explicitly: conditions use Kubernetes label-selection
syntax, may target fields beyond labels, and use commas for AND. See the [current
resource-template example](https://argo-workflows.readthedocs.io/en/latest/walk-through/kubernetes-resources/)
and the [v4.1.0 `WaitResource` implementation](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/workflow/executor/resource.go).

### Can it wait for Agent Sandbox `Ready` and `Finished`?

**Proven:** Argo can submit an `agents.x-k8s.io/v1beta1` `Sandbox` manifest because a
resource template accepts CRDs.

**Proven:** Agent Sandbox v0.5.4 represents lifecycle through a Kubernetes condition
array. Its `SandboxStatus` has `Conditions []metav1.Condition`; it has no summary
`status.phase`. The release notes document the `Finished` lifecycle condition and the
API source shows the array shape. The condition type is the semantic key, not the array
position.

**Not proven and currently unsafe:** Argo’s resource selector language cannot express a
portable “find the condition whose `type` is `Ready` and require its `status` to be
`True`, AND find the condition whose `type` is `Finished` and require its `status` to
be `True`.” The following is JSONPath-style syntax, not the language Argo implements:

```text
status.conditions[?(@.type=='Ready')].status == True
```

Using `status.conditions[0]` and `status.conditions[1]` would be position-dependent and
would silently bind the workflow to controller ordering. It is not an acceptable
production proof of the two condition types. A `status.phase` field would solve this,
but the upstream `Sandbox` API does not provide one. A small derived-status bridge or a
normal Argo wait/observer step is therefore still required unless the Phase 0 test
proves a safe alternative.

This is a source-level conclusion, not a claim that every possible GJSON expression is
invalid. The exact acceptance and runtime behavior must be tested against the real
v0.5.4 CRD/controller before adoption.

### Argo capability matrix

| Capability | v4.1.0 result | AGW consequence |
| --- | --- | --- |
| `WorkflowTemplate` and parameters | Supported. Templates can be referenced and parameterized. | Good fit for resolved `Agent`/`Gate` libraries, but the translator must resolve and freeze inputs before submission. |
| PVC `volumeClaimTemplates` | Supported. Argo creates claims at workflow start and manages deletion according to `volumeClaimGC`; default behavior is not the same as “retain forever.” | Choose one owner for the workspace. Do not let both Argo and Agent Sandbox independently create/manage the same PVC. Preserve AGW retention policy for evidence and workspaces. |
| Workflow `activeDeadlineSeconds` | Supported at workflow level. Template-level deadline is for container/script-style templates. | Use workflow deadline for the whole run; do not assume it supplies Sandbox-specific shutdown semantics. |
| Template `retryStrategy` | Supported, including backoff and retry policies. | Useful for clone/capture/verify only after idempotency review. A retry is a new execution attempt, not a semantic effect ledger. |
| Synchronization | Supported at workflow/template scope. | Useful for limiting agent concurrency and serializing scarce model or repository capacity. |
| `onExit` | Supported and invoked after the main workflow finishes, fails, or errors. | Good cleanup hook, but cleanup must be idempotent and must not erase `UnknownEffect` evidence. |
| Workflow TTL | Supported through `ttlStrategy`. | Reaps Workflow objects, not necessarily every AGW artifact, ledger tombstone, or external effect. |
| Volume-claim GC | Supported through `volumeClaimGC`. | Does not replace the current retention safety checks and PVC lifecycle attestation. |
| Artifact GC | Supported through `artifactGC` and artifact-level overrides. | Useful for Argo-owned transient artifacts; keep immutable Gate reports, patch evidence, and ledger records under AGW’s retention contract. |
| Resource output parameters | Supported for fields of the resource object, using JSONPath/JQ in the output definition. | Can return the Sandbox object/status, but does not automatically discover arbitrary files emitted by the child pod. |
| Indirect child-pod logs | Supported as a log-selection aid when child pods carry workflow labels; the official example says Argo does not know generic CRD child-pod behavior. | Useful for troubleshooting only. It is not artifact capture or completion evidence. |
| Indirect child-pod artifacts | Not automatic from a resource template. | Use a normal Argo capture pod, a shared PVC, or a small AGW bridge that waits for Sandbox completion and reads the known workspace. |

Primary references for the feature rows are the [v4.1.0 API field definitions](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/pkg/apis/workflow/v1alpha1/workflow_types.go),
[artifacts walkthrough](https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/),
[retry walkthrough](https://argo-workflows.readthedocs.io/en/latest/walk-through/retrying-failed-or-errored-steps/),
and the [resource-created child-pod log example](https://raw.githubusercontent.com/argoproj/argo-workflows/v4.1.0/examples/k8s-resource-log-selector.yaml).

### Retry and idempotency implications

Argo retries:

- transient kubectl/client errors inside the resource executor;
- a failed or errored template when its `retryStrategy` permits it;
- a whole workflow execution when an operator explicitly retries or resubmits it.

Argo does **not** know whether a model call, GitHub write, object-store conditional
write, or cleanup operation is safe to repeat. In particular:

- a resource template using `generateName` can create a second Sandbox on a retry;
- a fixed-name `create` can turn a retry into `AlreadyExists`, which is not the same as
  “the original Sandbox is known healthy”;
- a capture step can duplicate or overwrite evidence unless its key is content-addressed
  and writes are conditional;
- a publish step can create a duplicate branch/commit/PR if it is retried after the
  external system applied the mutation but before Argo observed the response;
- an `onExit` step may run after an error and must not delete evidence needed to resolve
  an ambiguous effect.

The existing AGW effect ledger is still required. Its claim-before-call,
request-digest binding, terminal outcome, permanent tombstone, and `UnknownEffect`
semantics are not supplied by Argo. The current implementation explicitly treats an
ambiguous external result as terminal rather than retrying blindly; see
[`v3/internal/effects/ledger.go`](../../v3/internal/effects/ledger.go).

### Can Argo represent the current phases?

Argo can execute a steps/DAG sequence corresponding to:

```text
Cloning -> Working -> Capturing -> Verifying -> Gated -> Publishing
```

It cannot represent the AGW contract *faithfully by itself*:

- Argo’s workflow/node phases are execution states, not AGW’s `Accepted`, `Rejected`,
  `Failed`, `Cancelled`, and `UnknownEffect` domain outcomes.
- `Gated` contains a signed, independently produced semantic verdict; an Argo node
  success is not that verdict.
- `UnknownEffect` is deliberately terminal even though the Argo node may be `Error` or
  `Failed`; those states cannot be substituted without losing the safety meaning.
- the `Sandbox` condition-array problem and child-artifact bridge remain outside a
  generic resource template.

The current FSM is not accidental scaffolding. It is tested for the exact phase and
terminal transitions, including `UnknownEffect`; see
[`v3/internal/fsm/agentrun.go`](../../v3/internal/fsm/agentrun.go) and
[`v3/internal/fsm/agentrun_test.go`](../../v3/internal/fsm/agentrun_test.go).

### Minimum translator and status mirror that remains

The proposed “few hundred lines” translator is too optimistic if it is expected to
preserve the current contract. The remaining responsibilities are:

| Responsibility | Required behavior |
| --- | --- |
| Admission and resolution | Validate preflight, refs, permissions, immutable input boundaries, and resolve `Agent`, `Gate`, `ToolSet`, `ModelRoute`, image, and skill digests. |
| Immutable run snapshot | Produce/freeze the resolved spec digest, base SHA, workflow identity, and all artifact/effect identities before execution. |
| Workflow rendering | Render only trusted server-side templates, deterministic names, owner references, ServiceAccount, namespace, deadlines, quotas, and artifact/PVC policy. Do not pass arbitrary raw resource manifests from an untrusted `AgentRun` into a privileged workflow. |
| Submission and recovery | Create or find the one workflow for the run; handle already-exists, API conflicts, cancellation, and restart without submitting a second logical run. |
| Status projection | Map workflow/node/Sandbox/bridge results into the public `AgentRun` phase, conditions, refs, evidence digests, Gate verdict, and effect summary using bounded status writes. |
| Domain outcome | Preserve `Rejected` versus `Failed`, shadow behavior, no-publish behavior, and `UnknownEffect` as a terminal state. |
| Cleanup and retention | Remove owned children only after evidence is durable; retain ledger tombstones and reports according to AGW policy. |
| Compatibility and rollback | Keep the direct backend or a fallback path until Argo has passed the live gates. |

That is a small control-plane adapter compared with the original FSM, but it is not a
200-line CRUD shim. A realistic planning range is **roughly 2,000–5,000 production Go
lines plus tests**, depending on whether the condition/artifact bridge is a script,
resource, or small controller. This is a responsibility estimate, not a promise to
write that many lines. The existing domain packages remain even if the adapter is
smaller.

### RBAC and blast radius

Argo’s official [workflow RBAC documentation](https://argo-workflows.readthedocs.io/en/latest/workflow-rbac/)
says workflow pods use `workflow.spec.serviceAccountName` and that the workflow
ServiceAccount needs create permission for any resource the workflow deploys. The
v4.1.0 namespace controller Role grants Argo control-plane permissions for workflows,
pods, PVCs, Secrets, and related Argo resources; it does not magically grant the
workflow pod permission to create the Agent Sandbox CRD.

For an AGW resource step, the workflow ServiceAccount needs only the narrow permissions
required in `agw-runs`, for example:

- `create/get/delete` on `sandboxes.agents.x-k8s.io` (and `patch/update` only if the
  chosen apply/owner-reference flow requires them);
- `get` on the status needed for the wait/output path;
- the minimum Argo `workflowtaskresults` permissions required by the executor;
- narrowly scoped PVC/Pod/Job permissions for capture/verify templates, if those are
  normal workflow resources.

It must not get Secrets, Nodes, ClusterRoles, arbitrary namespaces, or cluster-wide
CRD creation. A Role and RoleBinding in `agw-runs` are preferable to a ClusterRole.
The translator must be the only path from `AgentRun` to privileged workflow templates;
otherwise a submitter who can create Workflows can use the resource template as a
general-purpose Kubernetes API client within the ServiceAccount’s permissions.

### Argo Events assessment

Argo Events v1.9.11 provides an EventSource for external webhooks and a Sensor that
resolves event dependencies and triggers workflows. Its [webhook documentation](https://argoproj.github.io/argo-events/eventsources/setup/webhook/)
describes an HTTP server; its [Sensor documentation](https://argoproj.github.io/argo-events/concepts/sensor/)
describes event dependencies and triggers; its [ServiceAccount documentation](https://argoproj.github.io/argo-events/service-accounts/)
requires workflow create/list or resource-create permissions for triggers.

That is a good later integration, not a stable-v3 prerequisite. It introduces an
EventBus, EventSource and Sensor controllers, ingress/TLS/authentication, GitHub replay
and duplicate-event handling, new RBAC, and another path that can submit work. Keep
direct `AgentRun` submission as the canonical interface. Add Argo Events only when a
specific webhook trigger is requested and its event identity can deterministically map
to one `AgentRun`/effect key.

### ADR-025 correction and Phase 0 falsification test

**Corrected ADR-025:** Adopt Argo Workflows as a backend orchestration engine, not as a
replacement for the AGW completion contract. Keep a thin translator/status mirror and
the current domain safety code. Treat the Sandbox readiness/artifact bridge as a hard
compatibility boundary.

Run this in a disposable test cluster; do not use the live cluster:

1. Pin Argo Workflows `v4.1.0` and Agent Sandbox `v0.5.4` by the revisions above.
2. Install only the core Agent Sandbox controller and create a disposable `agw-runs`
   namespace, dedicated Workflow ServiceAccount, Role, and RoleBinding.
3. Submit a Workflow resource template that creates one named `Sandbox`, sets an owner
   reference, and attempts to wait for both condition types. Test the documented
   selector language, the JSONPath-filter expression, and a positional-index expression
   separately. Record parse errors, false success, and missed completion.
4. Force a Sandbox to pass through `Ready=True` before `Finished=True`. Assert that the
   workflow cannot succeed between those events and cannot succeed when only one type is
   true. If no type-safe expression works, test a derived-status/wait bridge and record
   its owner, RBAC, and failure behavior.
5. Verify that the Sandbox’s child pod can write a patch/evidence file to the intended
   workspace and that a normal Argo capture pod can read it only after the Sandbox is
   actually finished. Persist and re-read the digest through the existing artifact
   store; do not count logs as evidence.
6. Kill the executor at each boundary and retry the workflow. Confirm no duplicate
   Sandbox, workspace, patch, report, or publish attempt is treated as a new logical
   run. Confirm the effect ledger produces `UnknownEffect` for an ambiguous publish.
7. Run `kubectl auth can-i` as the workflow ServiceAccount and prove that it cannot
   create Secrets, Nodes, namespaces, or arbitrary cluster-scoped resources.

**STOP:** no full ADR-025 migration if steps 3–4 cannot prove type-safe readiness or a
small, auditable bridge. **GO:** only after all seven checks pass and the direct v3 path
remains available for rollback.

## B. Sigstore and cosign (ADR-027)

### Public Fulcio and a self-managed k3s ServiceAccount

**Proven:** public Fulcio authenticates OIDC tokens from configured issuers. The current
Fulcio v1.8.8 public identity configuration contains Kubernetes issuer patterns for
managed Kubernetes providers such as EKS, GKE, and AKS. It does not contain a generic
self-managed k3s issuer.

The [official Fulcio OIDC documentation](https://docs.sigstore.dev/certificate_authority/oidc-in-fulcio/)
says Kubernetes tokens must use an issuer present in Fulcio configuration and documents
cloud-based Kubernetes support. The [v1.8.8 public configuration](https://raw.githubusercontent.com/sigstore/fulcio/v1.8.8/config/identity/config.yaml)
shows the concrete issuer patterns. The same Fulcio [integration guide](https://raw.githubusercontent.com/sigstore/fulcio/v1.8.8/docs/oidc.md)
says adding a non-CI issuer requires Fulcio identity/configuration code and tests.

**Conclusion:** a projected ServiceAccount token from an arbitrary self-managed k3s
cluster will not be accepted by public Fulcio merely because it is a Kubernetes token.
Its `iss` must match a configured public issuer; the k3s issuer is not covered by the
current public configuration.

Real options are:

| Option | What it really means | Assessment for current AGW |
| --- | --- | --- |
| Self-managed cosign key in Kubernetes Secret | A private key is held in a tightly scoped Secret and verification uses a pinned public key. | Lowest operational complexity; closest to the current design. Keep the immutable Secret and trusted-key checks, or use cosign’s supported `k8s://` key provider with equivalent hardening. |
| KMS/key-managed cosign | cosign signs through AWS/GCP/Azure KMS, Vault/OpenBao, OVHcloud KMS, or another supported provider. | Stronger key custody if a suitable KMS is available; no public Fulcio OIDC dependency. See [cosign key management](https://docs.sigstore.dev/cosign/key_management/overview/). |
| Supported external OIDC | Sign in GitHub Actions, a supported cloud Kubernetes issuer, or another public Fulcio-supported environment. | Changes the trust boundary and is not the same as signing inside self-managed k3s. |
| Self-hosted Sigstore | Operate and trust Fulcio plus Rekor, trust-root/TUF material, and the required transparency/timestamp infrastructure; add the k3s issuer to the private Fulcio configuration. | Possible, but a new platform to operate. Defer until public auditability is a real requirement. |
| Sigstore policy-controller | Verify signed images/attestations at Kubernetes admission. | Useful later for AGW runtime/helper image policy; not a replacement for signing or evaluating a Gate report. |

Self-hosting only Fulcio is not equivalent to “using public Sigstore privately.” The
private instance needs a trust-root and verifier distribution story. For the current
single-owner k3s deployment, managed signing key material is the pragmatic choice.

### What `cosign attest-blob` actually does

The current cosign v3.1.3 [attest-blob implementation](https://raw.githubusercontent.com/sigstore/cosign/v3.1.3/cmd/cosign/cli/attest/attest_blob.go)
reads a blob or accepts an externally supplied digest with `--hash`, constructs an
in-toto Statement whose subject is that digest, signs the DSSE payload, optionally
uploads a Rekor entry, and writes a local signature/attestation/bundle. The current
command reference documents `--bundle`, `--statement`, `--predicate`, `--key`, and
`--hash`; a bundle is verification material, not an OCI upload.

The OCI path is separate:

- [`cosign upload blob`](https://raw.githubusercontent.com/sigstore/cosign/v3.1.3/cmd/cosign/cli/upload/blob.go)
  uploads a file as an OCI image/artifact at a registry reference.
- [`cosign attach attestation`](https://raw.githubusercontent.com/sigstore/cosign/v3.1.3/cmd/cosign/cli/attach/attach.go)
  parses an image reference, resolves its digest, and writes an attestation to that
  signed OCI entity/referrer path.

Therefore an arbitrary patch or report can be attested and stored as a detached bundle
in AGW’s existing immutable object store **without** OCI packaging. It cannot be
discovered through OCI referrers without first representing the bytes as an OCI
artifact/image (for example through `cosign upload blob` or an equivalent OCI
manifest). Do not claim that `attest-blob` itself uploads arbitrary files to an OCI
referrer index.

### Integrity, authenticity, and semantics are different

| Mechanism | Establishes | Does not establish |
| --- | --- | --- |
| Object-store SHA-256 digest | The bytes retrieved match the expected content and can be addressed immutably. | Who produced the bytes or whether the Gate’s conclusion is correct. |
| Ed25519/cosign signature | A holder of the trusted private key signed the covered bytes. | That the signer was authorized unless the verifier trusts the key/identity; or that the predicate is true. |
| in-toto Statement + DSSE | A signed, structured predicate binds one or more subject digests to claims. | The truth of custom Gate claims; those still require independent verification. |
| Fulcio certificate | A short-lived signing key is bound to an accepted OIDC identity. | Acceptance of an issuer that Fulcio does not configure. |
| Rekor transparency log | An append-only public record, inclusion evidence, and useful audit/timing material. | Correctness of the Gate predicate or authorization to publish. |
| Sigstore bundle | Verification material collected for later offline verification. | Durable storage; AGW still must retain it and bind it to the run. |
| Gate predicate | The semantic decision: scope, tests, evidence, limits, and verdict. | Cryptographic authenticity by itself. It must be signed and independently reconstructed/checked. |

### Comparison with the current AGW report

The current implementation already has a meaningful report contract:

- [`v3/internal/gate/report.go`](../../v3/internal/gate/report.go) builds a bounded,
  canonical `VerificationReport` only from a decision produced by the Gate evaluator;
- the report binds Run UID, resolved spec digest, base SHA, patch digest, Gate UID and
  generation, runtime/verifier/helper image digests, skill digests, commands, checks,
  evidence summaries, and verdict;
- it signs canonical JSON with Ed25519 and stores a `SignedReport` envelope;
- [`v3/cmd/agw-operator/report_signer.go`](../../v3/cmd/agw-operator/report_signer.go)
  loads a raw seed/private key from an immutable, exact-shape Kubernetes Secret;
- [`v3/internal/publishcontroller/driver.go`](../../v3/internal/publishcontroller/driver.go)
  checks the artifact digest, trusted public key, report identity bindings, Gate
  verdict, and patch manifest before calling the effect-ledger-backed publisher.

This is not in-toto/DSSE today. It is a custom canonical JSON envelope. That is a
format limitation, not evidence that the Gate contract is redundant.

The safe migration boundary is:

| Can eventually be replaced by cosign/in-toto | Must remain AGW-owned |
| --- | --- |
| `SignedReport`’s custom algorithm/public-key/signature JSON envelope | `BuildReport`’s rule that only an evaluated Gate can produce a report |
| Custom signature serialization and local verification wrapper | Canonical Gate checks: scope, diff limits, test strength, coverage, binary policy, critic evidence, and verdict |
| The report-signing key adapter, if cosign KMS/key-provider integration is proven | Independent verify-pod execution, clean checkout, evidence digests, input identity binding, and report-to-patch binding |
| Optional object-store report/bundle format | Trusted key/trust-root selection, rotation, revocation/expiry policy, and fail-closed verification |
| Optional OCI packaging/discovery | Effect ledger, `UnknownEffect`, and publish idempotency |

The canonical predicate should become an in-toto predicate with a versioned custom
predicate type, for example `https://agents.astatide.com/verification/v1alpha1`. Its
subject must bind the patch digest (and, where useful, the report/evidence digest); its
predicate must carry the same immutable run/Gate/input identities and every blocking
check. DSSE/cosign makes the evidence portable. It does not make an `Accepted` value
true if the verifier let a forged or incomplete report through.

### Kyverno and policy-controller: no magical workflow gate

Kyverno’s current [validate documentation](https://kyverno.io/docs/policy-types/cluster-policy/validate/)
describes admission-time validation of Kubernetes resources, with `Enforce` blocking
admission and `Audit` reporting results. Its [image verification documentation](https://kyverno.io/docs/policy-types/cluster-policy/verify-images/overview/)
is likewise an admission/image policy surface. Generate rules create or synchronize
Kubernetes resources; they do not turn an object-store JSON report into an Argo step
result.

Sigstore policy-controller’s [current policy documentation](https://docs.sigstore.dev/policy-controller/overview/)
is a Kubernetes admission controller for signed images and attestations. It can be a
good later guard for AGW runtime/helper images and can evaluate attestation predicates
when the subject is an OCI artifact. It does not pause an already-running Workflow,
read an arbitrary report from the AGW object store, evaluate the Gate’s clean-checkout
rules, or authorize the next publish step.

The real workflow design is therefore:

```text
verify/capture step -> independently verifies report/predicate -> emits a bounded
Argo result or resource status -> Argo condition chooses publish/no-publish
```

Kyverno/policy-controller may protect the images and the Kubernetes resources admitted
along that path; the AGW verifier still owns the Gate decision.

### ADR-027 correction and Phase 0 test

**Corrected ADR-027:** Adopt in-toto Statement + DSSE/cosign as a portable evidence
format, but do not make public Fulcio keyless signing a prerequisite. For self-managed
k3s, start with cosign self-managed/KMS key material and bundles in the existing
immutable object store. Keep the current report reader and signer until dual-format
parity is proven.

Phase 0:

1. Create a deterministic patch and Gate report fixture from the current v3 evaluator.
2. Produce a cosign v3.1.3 `attest-blob` bundle using the selected Kubernetes Secret or
   KMS key path, with Rekor upload disabled for the isolated test if necessary. Verify
   the bundle using the trusted public key/trust root and store/retrieve it through the
   existing artifact store.
3. Change the patch, predicate, verdict, and one identity field independently. Confirm
   the verifier rejects each mismatch and that it cannot accept a hand-written
   `Accepted` predicate without the Gate’s deterministic checks.
4. Attempt public Fulcio keyless signing with a projected token from a disposable
   self-managed k3s ServiceAccount. Record the issuer rejection; this is a falsification
   of the “arbitrary k3s SA works with public Fulcio” assumption, not a production
   configuration step.
5. In a disposable registry, upload a blob, attach its DSSE attestation, and discover
   it through OCI referrers. Separately attest a blob without uploading it and confirm
   that only the detached bundle/object-store path exists. This proves the packaging
   boundary.
6. Run old-report verification and new DSSE verification over the same corpus. Do not
   delete the old envelope until both readers agree on every blocking Gate field and
   key-rotation/rollback behavior is tested.

**STOP:** no signer/envelope deletion if the self-managed trust root, bundle retention,
identity binding, or dual-reader parity is unresolved. **GO:** cosign can replace the
envelope only after the semantic Gate verifier remains independent and the trust model
is explicit.

## C. Current v3 tree and deletion claims

### Measurement method

The count was run from the repository root with:

```text
find v3 -type f -name '*.go' ! -name '*_test.go' -print0 |
  xargs -0 awk 'FNR==1 {files++} {lines++} END {print files, lines}'
```

It counts physical lines in all non-test Go files and includes the generated
`v3/api/v1alpha1/zz_generated.deepcopy.go` file (1,415 lines). The count is a snapshot of
the current working tree, not a claim about the eventual v3 branch.

### Disjoint current counts by responsibility

This is the historical pre-expansion snapshot used by the original spike. Each
package was counted once and the rows sum to 47,021 lines; it is not the current
worktree total. The current 71,183-line measurement is recorded in the decision
summary above.

| Responsibility | Packages included | Files/packages | Lines |
| --- | --- | ---: | ---: |
| API/schema | `api/v1alpha1` | 10 files / 1 package | 2,294 |
| Lifecycle/orchestration | `cmd/agw-operator`; `internal/controller`, `capturecontroller`, `fsm`, `publishcontroller`, `runplan`, `runtimeevents`, `sandbox`, `status`, `verifycontroller`, `workload` | 22 files / 11 packages | 9,807 |
| Gate/verification/artifact contract | `internal/artifactauth`, `artifacts`, `canonical`, `capture`, `gate`, `objectstore`, `publish`, `verifier`, `verifyfetch`, `verifyworkload`; `pkg/artifactcatalog` | 30 files / 11 packages | 9,826 |
| External effects/GitHub publish | `internal/effects`, `githubapp`, `githubpublish` | 3 files / 3 packages | 2,461 |
| Broker/runtime/context/skills | `cmd/agw-broker`, runtime commands, `cmd/agw-skills`; `internal/broker`, `contextpack`; runtime/adapter/skills packages | 26 files / 13 packages | 12,091 |
| Security/config/retention | `internal/admission`, `preflight`, `resolved`, `retention`, `runsecret`, `shadowreview` | 14 files / 6 packages | 5,374 |
| Workflow executor binaries | `cmd/agw-capture`, `agw-clone`, `agw-preflight`, `agw-verifier`, `agw-verify-apply`, `agw-verify-fetch` | 9 files / 6 packages | 3,299 |
| CLI | `cmd/kubectl-agw` | 6 files / 1 package | 1,869 |
| **Total** |  | **118 files / 52 packages** | **47,021** |

### What Argo can genuinely delete

Only after a working Argo path is proven, the following are candidates for deletion or
substantial reduction:

- the FSM transition *implementation* if Argo plus the status mirror becomes the
  authoritative execution scheduler; the API phase enum and validation still remain;
- the reconcile branches in `internal/controller/agentrun.go` that only create, poll,
  and transition child lifecycle objects duplicated by the Workflow;
- portions of `runplan`, `sandbox`, and `workload` that only render and observe the
  old direct child lifecycle, if their security and fallback responsibilities have a
  tested replacement;
- orchestration-only wiring in `cmd/agw-operator` after the translator, status mirror,
  report signer, preflight, and retention responsibilities are separated and covered;
- duplicate retry/backoff/finalizer code whose exact replacement is a pinned Argo
  feature with equivalent failure semantics.

This is not a license to delete the whole packages. The current Sandbox backend can
remain the rollback path; the workload builder may still be needed by the Workflows;
capture/verify/publish drivers are domain operations, not orchestration; and status
helpers may be needed to project Argo outcomes safely.

### What does not become deletable by adopting Argo or cosign

The following are product contracts, not generic platform plumbing:

- CRD types, defaults, validation, resolved references, and `AgentRun.status`;
- Gate evaluation and report construction, including scope and test-strength rules;
- independent verify execution and evidence binding;
- capture/patch parsing and immutable artifact/object-store references;
- effect-ledger claim/outcome/tombstone logic and `UnknownEffect`;
- GitHub App/publisher behavior and the publish input identity checks;
- `runtimeproto`, Codex/Claude adapters, skill verification, broker/runtime policy;
- preflight isolation checks and the Job fallback;
- retention safety for PVCs, evidence, and ledger tombstones;
- the CLI contract, unless a replacement client is explicitly accepted.

The current code documents these boundaries directly. For example, the report is built
from an evaluated decision in [`v3/internal/gate/report.go`](../../v3/internal/gate/report.go),
the independent verifier waits for Sandbox `Finished` and evidence before gating in
[`v3/internal/verifycontroller/verifycontroller.go`](../../v3/internal/verifycontroller/verifycontroller.go),
and retention protects ledger replay fences in [`v3/internal/retention/`](../../v3/internal/retention/).

### Reconciliation of the 3,000–4,500 LOC claim

The estimate confused “the orchestration engine can be adopted” with “all code around
a run is orchestration.” The measured unique-core lower bound already exceeds the
estimate before many required production responsibilities are counted. Argo does not
replace those contracts, and Cosign replaces only the report envelope, not the Gate
predicate or publish verification.

A defensible planning statement is:

> Argo must remove duplicated lifecycle scheduling and agentgateway must remove
> duplicated transport/credential plumbing after live proof. The original 3,000–4,500
> estimate is falsified for the accepted scope; every retained responsibility and
> every deletion remains measurable and reviewable.

## Migration sequence that preserves verified work

1. **Freeze the current contract.** Keep the current direct AgentRun/Sandbox/Job path,
   FSM tests, Gate report tests, effect-ledger tests, retention tests, and publish
   identity tests. Record the current 71,183-line baseline and its category ledger.
   No v2/v3 deletion in this spike.
2. **Run Argo Phase 0 only.** Prove the condition-array wait, owner-reference behavior,
   PVC ownership, child workspace/artifact capture, retry behavior, and least-privilege
   RBAC in a disposable cluster.
3. **Add a side-by-side translator.** For one opt-in run, translate the already resolved
   `AgentRun` into a pinned Workflow. Keep direct v3 execution as the fallback and make
   the translator status-only until it can mirror every required condition/ref.
4. **Move safe compute first.** Use Argo for clone and bounded capture/verify pods or
   for a proven Sandbox bridge. Keep the existing Gate evaluator and current signed
   report format. Do not route publish through Argo retries yet.
5. **Move publication last.** Have one Argo step call the existing publish driver and
   effect ledger with deterministic run/effect keys. Test process termination before
   and after GitHub accepts the request. Any ambiguity must project `UnknownEffect`.
6. **Migrate the evidence envelope in parallel.** Emit current and DSSE/cosign formats,
   verify both, compare the canonical Gate predicate, and retain both artifacts during
   the compatibility window. Switch the reader only after parity and rollback tests.
7. **Defer Argo Events.** When webhook triggering becomes an actual requirement, add it
   as a separate integration with signed payload verification, replay/duplicate tests,
   deterministic AgentRun identity, and least-privilege Sensor RBAC.
8. **Delete in a separate reviewed change.** Remove only code proven redundant by the
   running Argo path. Retain a rollback release and the direct backend until shadow
   runs, retention tests, and publish ambiguity tests pass.

## Explicit stop/go gates

| Gate | GO condition | STOP condition |
| --- | --- | --- |
| G0: source pins | Argo `v4.1.0`, Agent Sandbox `v0.5.4`, cosign `v3.1.3`, and all other adopted components are digest-pinned and their release notes reviewed. | A plan relies on stale `v4.0.x` or calls `v0.5.3` the current release, or uses floating images/tags. |
| G1: Sandbox completion | A live test proves type-safe `Ready=True` plus `Finished=True`, or a minimal derived-status/wait bridge is independently tested. | Resource-template success can be triggered by one condition, depends on array order, or has no proof of actual `Finished`. |
| G2: evidence | Capture reads the known child workspace only after completion, persists immutable content and digest, and survives restart/retry without duplicate logical artifacts. | Resource-template status/log output is being treated as patch/report evidence. |
| G3: retry safety | Clone/capture/verify are idempotent or safely repeatable; publish is ledger-guarded; ambiguous external results become `UnknownEffect`. | Argo retry/resubmit can create a second PR, overwrite evidence, or turn an unknown result into a blind retry. |
| G4: RBAC | Workflow and Sensor ServiceAccounts are namespace-scoped and `kubectl auth can-i` proves no secret/node/cluster-resource escape. | A generic workflow author can submit arbitrary privileged resource manifests or the controller needs unexplained cluster-wide write access. |
| G5: report trust | Self-managed/KMS cosign verification, bundle retention, identity binding, DSSE parity, and key rotation pass; public Fulcio assumptions are removed. | A k3s SA is assumed to be accepted by public Fulcio, or signature verification is mistaken for Gate semantic validation. |
| G6: Events | A real webhook use case exists and duplicate/replay-to-run identity is defined. | Argo Events is installed only because it might be useful later. |
| G7: deletion | Side-by-side runs, rollback, shadow evidence, and release tests prove a specific old package/responsibility is redundant. | Deletion is justified only by an estimated LOC target. |

## Unknowns that must remain explicit

- The live v0.5.4 condition sequence and exact Argo behavior for any attempted array
  expression are unknown until the disposable Phase 0 run; source inspection alone is
  not enough to bless a production wait.
- The best artifact bridge depends on whether the Sandbox workspace is an AGW-owned PVC,
  an Agent Sandbox `volumeClaimTemplates` PVC, or an existing claim. The two controllers
  must not both own deletion.
- A private Fulcio/Rekor deployment may be technically straightforward but its trust
  root, upgrades, availability, and audit cost have not been justified for this
  single-owner cluster.
- The final translator size depends on whether status is mirrored from a derived bridge
  resource or from a custom observer; the 2,000–5,000 range is a planning bound, not a
  measured implementation.
- Argo Events webhook replay semantics and GitHub signature verification have not been
  tested because webhook triggering is intentionally deferred.

The immediate plan correction is therefore narrow: prove Argo’s Sandbox boundary first,
adopt Argo only as a hybrid orchestrator, use cosign as a portable evidence envelope
with self-managed trust, and leave Argo Events out of stable v3 until its trigger
contract is real.
