# Agents Gateway v3 — agentgateway v1.4.1 composition spike

**Status:** source-backed architecture spike with a provider-free seam fixture; no production or deployment changes made
**Date:** 2026-08-12
**Scope:** ADR-026, with the current v3 broker, run-secret, run-plan, workload, and airlock implementation as the composition boundary

## Decision in one paragraph

Adopt agentgateway v1.4.1 as a narrowly-scoped downstream routing and credential-injection component, but keep a per-run `agw-guard` as the authority for exact JSON arguments, ToolSet/effect policy, the effect ledger, and `agw.runtime.v1` supervision. Start with a per-run sidecar. Do not make a centralized gateway the default, and do not claim that agentgateway is a generic CB4A vault/STS or that it can host the effect ledger through an official plugin API.

The v1.4.0 strict-argument finding remains valid in v1.4.1: MCP RBAC receives tool identity, not the call's arguments. v1.4.1 adds MCP Tasks and task context, but that is a different resource. The original broad-loopback risk has been removed from the current direct workload and preflight renderer; the future three-container composition still requires a live proof that its separate UID-and-destination rules hold under the reviewed gateway image. Phase 0 must prove that assumption before adoption.

## Evidence labels

- **Fact** — directly observed in the v1.4.1 tag/release, official documentation, or the current v3 source.
- **Inference** — an architecture conclusion derived from those facts.
- **Unknown** — requires a live test or an upstream answer; it is not treated as a security guarantee.

## Provider-free executable seam

The repository now contains a deliberately small data-plane double at
`v3/phase0/agentgateway-guard/gateway/`. It is not an implementation of
agentgateway. It exists to make the boundary executable without a provider,
registry pull, or paid request:

```text
guard HTTP handler
    -> fixed MCP gateway contract (trusted canary injection)
    -> recording MCP server
```

`gateway/gateway_test.go` runs the full HTTP chain through MCP initialize,
`notifications/initialized`, and `tools/call`; it proves the second call does
not reopen the handshake, the recorder observes the injected canary, and the
canary never appears in the guard result. Unknown MCP methods are rejected by
the gateway double, while unknown tools, non-exact arguments, and an
unapproved write are rejected by the guard before the gateway or recorder is
contacted. The transport regression caught by this fixture is fixed in
`guard/guard.go`: JSON-RPC notifications no longer carry an `id`, and a failed
initialization notification is no longer ignored.

This is stronger than a counting mock but deliberately weaker than the real
capability gate. It does not prove agentgateway v1.4.1 behavior, Kubernetes
`hostUsers:false`, UID-owner iptables enforcement, live Secret projection, or
provider-side credential handling. The production workload therefore remains
agentgateway-disabled by default and `workload.ValidateAgentGatewaySidecar`
continues to reject enablement until the exact image/configuration has live
evidence.

Focused command:

```sh
GOTOOLCHAIN=go1.26.5 go test -count=1 ./phase0/agentgateway-guard/gateway
```

## Release and revision under review

The GitHub release page identifies `v1.4.1` as released on 2026-07-29. The tag resolves to:

```text
agentgateway v1.4.1
commit 163ea2146acb7b82082acea30ed691b29079095f
image  cr.agentgateway.dev/agentgateway:v1.4.1
       cr.agentgateway.dev/controller:v1.4.1
```

The prior comparison revision was `v1.4.0` at `83c952731ee79b4372e3a031382c4ff419ddfee1`.

The live GitHub release API returned `v1.4.1`, published at `2026-07-29T21:48:46Z`, during this spike: [GitHub releases/latest API](https://api.github.com/repos/agentgateway/agentgateway/releases/latest).

Primary release record: [agentgateway v1.4.1 release](https://github.com/agentgateway/agentgateway/releases/tag/v1.4.1).

Pinned source tree: [agentgateway `v1.4.1`](https://github.com/agentgateway/agentgateway/tree/v1.4.1).
Prior source tree: [agentgateway `v1.4.0`](https://github.com/agentgateway/agentgateway/tree/v1.4.0).

The release correction changes the revision and adds the deltas below; it does not reverse the v1.4.0 strict-argument conclusion.

## v1.4.1 delta from the documented v1.4.0 finding

| Area | v1.4.1 result | Decision impact |
|---|---|---|
| MCP exact arguments in RBAC | **No change.** The regression test is still named `test_rbac_mcp_context_is_identity_only`; it asserts that `mcp.tool.arguments`, `result`, and `error` are absent in the RBAC context. | Keep `strictjson` and ToolSet exact-argument matching in `agw-guard`. |
| MCP call path | The session captures arguments for logging/telemetry, then authorizes a `ResourceType::Tool` containing only target/name. | Observability is not authorization. Captured arguments cannot be used as proof that the request was admitted under an exact rule. |
| MCP Tasks | Adds `ResourceType::Task`, task IDs, task operations, task context, and task RBAC. | Useful for task isolation, but it does not add tool-argument matching. |
| OAuth token exchange | Missing or empty configured subject-token source now fails closed. The v1.4.0 fallback to validated JWT claims was removed; using that token now requires an explicit CEL source such as `jwt.rawToken.unredacted()`. | Positive security change. Configuration migration must be explicit and tested. |
| External auth/processing responses | v1.4.1 removes an artificial response-body size limit for external authorization/processing services. | More useful for diagnostics, but it does not make request-body inspection MCP-aware or provide an effect hook. |
| MCP fail-open fan-out | When all upstreams fail, fail-open now returns an error rather than an empty success. | Better failure semantics; security paths should still use fail-closed. |
| MCP session logging | Synthetic internal session identifiers are no longer logged as MCP session IDs. | Reduces misleading telemetry; it is not cross-run session isolation. |
| Standalone config importer | Adds an initial extensible importer, currently a source-to-standalone configuration adapter. | Configuration import is not a request-lifecycle plugin and cannot host the effect ledger. |

The exact-argument evidence is in the tagged source: [authorization test](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/authorization_tests.rs#L288-L299), [RBAC validator](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/mcp/rbac.rs#L51-L59), [MCP call authorization](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/mcp/session.rs#L584-L600), and [MCP context model](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/mcp/mod.rs#L339-L465).

The auth delta is recorded in the release notes at [v1.4.1 authentication and security](https://github.com/agentgateway/agentgateway/releases/tag/v1.4.1#authentication-and-security) and in the tagged tests showing the explicit raw-token expression: [OAuth subject-token tests](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/tests.rs#L714-L753).

## 1. Credential-injection reality

### Static credentials

**Fact.** Standalone backend `key` auth accepts either an inline value or a file. The tagged source models this as `FileOrInline`; it is not a typed `envRef` field. v1.4.1 does expand environment variables before configuration parsing, so an environment-expanded value can enter an inline configuration, but that is still configuration interpolation rather than a separate secret-provider boundary.

**Fact.** The Kubernetes controller supports `secretRef`. Its credential resolver reads the referenced Kubernetes `Secret`, extracts the selected key, and translates it into the generated backend-auth policy. The data-plane gateway then inserts the configured value into the outgoing request. In other words, the controller and gateway are trusted credential holders for the request path.

**Fact.** The runtime applies static backend auth to the outgoing request. The agent receives neither the backend key nor the generated authorization header unless the surrounding configuration accidentally exposes it.

Sources: [backend auth types and application](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/mod.rs#L34-L53), [static key application](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/mod.rs#L157-L212), [file-or-inline loader](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/core/src/serdes.rs#L357-L435), [Kubernetes Secret resolver](https://github.com/agentgateway/agentgateway/blob/v1.4.1/controller/pkg/utils/kubeutils/secrets.go#L22-L126), and [controller backend-auth translation](https://github.com/agentgateway/agentgateway/blob/v1.4.1/controller/pkg/agentgateway/plugins/backend_policies.go#L854-L995).

### OAuth token exchange and Cross App Access

**Fact.** v1.4.1 supports RFC 8693 token exchange and RFC 7523 JWT bearer exchange. The gateway extracts a subject token from the configured request location, calls a configured token endpoint, and inserts the resulting access token into the backend request. It can use client-secret or private-key JWT client authentication, additional parameters, actor tokens, chained exchange, and an in-memory token cache.

**Fact.** The v1.4.1 implementation refuses to build an exchange request when the configured subject-token source is absent or empty. The previous implicit fallback to validated JWT claims is gone. To intentionally use the validated raw token, the config must select it explicitly with CEL.

**Fact.** Cross App Access is implemented as a configured two-leg exchange: the gateway talks to an identity provider and then a resource authorization server. Client registrations and their credentials remain configuration inputs to the gateway/controller.

Sources: [OAuth exchange request construction](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/oauth/mod.rs#L348-L381), [subject-token extraction](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/oauth/mod.rs#L762-L770), [in-memory token cache](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/oauth/mod.rs#L863-L900), and [OAuth client-key loading/rotation note](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/oauth/client_auth.rs#L283-L296). Official conceptual docs: [OAuth token exchange](https://agentgateway.dev/docs/standalone/latest/configuration/security/backend-authn/oauth-token-exchange/) and [Cross App Access](https://agentgateway.dev/docs/standalone/latest/configuration/security/backend-authn/cross-app-access/).

### Is there an actual controller, STS, or vault?

**Fact.** There is a Kubernetes controller that resolves Kubernetes Secrets and emits gateway policy/configuration. There is a configured OAuth token endpoint, and provider-specific cloud authentication paths exist. There is no generic agentgateway-owned credential vault or generic CB4A controller/STS in the v1.4.1 source examined. The standalone importer is a configuration adapter, not a secret broker.

**Inference.** The CB4A-style statement “the agent never receives the downstream credential” is a valid deployment property when the gateway is the only process with access to the static key or exchanged token. The stronger statement “agentgateway eliminates credential storage” is false. With `secretRef`, it moves which controller/data-plane process reads and holds the credential. With OAuth, it can reduce long-lived backend-token exposure by exchanging at a token endpoint, but the gateway still needs client authentication and holds the resulting token at least in process memory/cache.

**Unknown.** Whether a future deployment can delegate all client-key handling to an external STS/HSM is an integration choice outside the v1.4.1 generic gateway contract. It must not be assumed from the CB4A proposal.

## 2. Deployment-shape comparison

The three shapes are evaluated against the v3 requirement that a run has a resolved `ToolSet`, `ModelRoute`, credential projection, effect policy, and independent session boundary.

| Criterion | A. Per-run sidecar | B. Central `agw-system` Service | C. Optional external gateway |
|---|---|---|---|
| Provider credential exposure | Best. Only that run's gateway container mounts its projected credentials/config. | Broadest. A central process normally holds credentials for many runs; a mistake can cross run boundaries. | Depends on the provider. Credentials leave the cluster or are held by another operator. |
| Blast radius | One run/pod and its configured backends. | Gateway outage or policy error affects many runs. | External outage, policy drift, or account compromise affects every dependent run. |
| HA | Per-run availability is naturally tied to the run; no gateway HA needed. | Requires replicas, rollout discipline, health checks, and state/session strategy. | Usually delegated to the external service, but availability and rate limits are no longer under AGW control. |
| Config isolation | Strong. Generate one immutable config from one resolved snapshot. | Harder. Requires per-request identity, dynamic config/xDS, or a trusted tenant router. | Depends on whether the service offers hard tenant/config isolation. |
| Per-run ToolSet/ModelRoute | Direct fit. The sidecar gets only the run projection. | Requires proving that route/tool policy cannot be selected or confused by a caller. | Depends on external policy granularity and API. |
| Session isolation | Process and config boundary per run; easiest to test. | Shared process/session stores need explicit run identity, keying, and cleanup. | Must trust external session semantics and data retention. |
| Network policy | Pod-local owner rules can constrain agent → guard → gateway ports; namespace policy constrains egress. | Run pods need access to a Service; central gateway needs its own restricted egress and ingress policy. | Requires egress TLS, endpoint allowlisting, and protection against proxy misuse. |
| Maintenance | More containers/images, but no shared state and simple failure domain. | Fewer gateway processes, but config distribution, HA, and isolation become the maintenance work. | Least self-hosting maintenance, highest external dependency. |
| Recommended use | **Default Phase 0/1 shape.** | Later optimization for genuinely shared, read-only traffic after isolation tests. | Optional adapter for a provider or organization that already operates a compliant gateway. |

**Inference.** Shape A is the smallest secure composition for v3. It also preserves the current v3 design’s owner-referenced, immutable per-run Secret model. Shape B is not automatically “more production grade”; it changes the primary risk from process count to multi-run policy/session isolation. Shape C is an integration boundary, not a replacement for the local completion/effect contract.

## 3. Proposed secure composition

The initial composition should be:

```text
agent UID 1000
    │  loopback port 8081 only
    ▼
agw-guard UID 1337
    │  loopback port 8082 only
    ▼
agentgateway v1.4.1 UID 1338
    │  approved public HTTPS providers/MCP only
    ▼
provider or MCP backend
```

`agw-guard` owns:

- strict JSON parsing and exact argument matching;
- resolved ToolSet membership and effect classification;
- effect reservation/commit and `unknown_effect` handling;
- run UID, spec digest, base SHA, and request identity binding;
- runtime protocol supervision and completion semantics;
- artifact descriptors/upload authorization;
- the small request/response contract exposed to the harness.

agentgateway owns:

- downstream backend selection and model/provider protocol translation;
- static backend credential injection or OAuth exchange;
- coarse backend/MCP identity policy where useful;
- provider-facing connection management, retries, and telemetry;
- model catalogs and provider-specific request shaping, subject to the budget caveat below.

The agent must never be given an agentgateway credential, an agentgateway admin token, a gateway client certificate, or a Kubernetes service-account token. The sandbox pod keeps `automountServiceAccountToken: false`.

### What the current airlock proves — and does not prove

**Fact.** The current v3 workload sets `hostUsers: false`, uses per-container UIDs, drops capabilities for normal containers, gives `NET_ADMIN` only to the lockdown init container, and sets `automountServiceAccountToken: false`. The current network policy denies private/link-local egress at the pod level and allows public HTTPS for the broker-labeled pod.

**Fact.** The original spike found an unconditional `-o lo` allow and rejected it
as a bypass. The production direct-mode renderer now permits UID 1000 only to
`127.0.0.1:8081` and `[::1]:8081`; UID 1337 retains the direct broker's broad
provider egress. The relevant sources are
[`v3/internal/airlock/policy.go`](../../v3/internal/airlock/policy.go) and
[`v3/internal/workload/workload.go`](../../v3/internal/workload/workload.go).
Local contract tests reject a broad loopback rule.

**Fact.** A separate Phase-0 composition fixture implements the three-container
chain (UID 1000 agent → UID 1337 guard → UID 1338 agentgateway) with exact
IPv4/IPv6 loopback ports. It is not selected by the production workload and is
not live proof.

**Inference.** The static bypass is closed in source, but `-m owner` under the
target user namespace, conntrack behavior, CNI interaction, and actual container
UID ownership remain assumptions until the disposable k3s probe passes.

The intended rule shape is destination-specific, for both IPv4 and IPv6:

```text
default OUTPUT DROP
allow UID 1000 -> loopback TCP/8081       # guard only
deny  UID 1000 -> all other loopback ports
allow UID 1337 -> loopback TCP/8082       # agentgateway only
allow UID 1337 -> approved guard support endpoints
allow UID 1338 -> approved provider/MCP egress
deny  every run UID -> Kubernetes API, node metadata, RFC1918/link-local/private ranges
```

The exact rule order, conntrack behavior, DNS behavior, and user-namespace semantics must be tested rather than inferred from the manifest.

### Loopback, Unix sockets, mTLS, and bearer tokens

**Fact.** v1.4.1 supports Unix-domain sockets for management listeners (`admin`, `stats`, and `readiness`) and supports Unix-socket backend targets. The tagged `Address` type describes itself as a management-listener address. The front-door local proxy binds are represented as TCP socket addresses in the local gateway bind path; no official v1.4.1 source evidence was found for a Unix-domain front-door listener that the agent could use for MCP/LLM traffic.

**Unknown.** A front-door Unix socket may be possible through a lower-level configuration path, but it is not adopted here without a v1.4.1 live test. If supported, a guard-owned socket with mode `0600` or a dedicated group is preferable to loopback TCP.

**Inference.** mTLS is defense in depth, not the primary bypass control. If the agent can connect to the gateway port and is given the client key, it can use the gateway directly. A bearer token in an agent-visible environment variable has the same problem. If TCP remains, use destination-specific owner rules first; keep any gateway client credential in a mount visible only to the guard or in a guard-to-gateway channel that the agent cannot read.

### Bypass invariant

The composition is admissible only if all of the following are true in a live pod:

1. UID 1000 can reach the guard’s exact port but cannot connect to the agentgateway port, admin port, stats port, readiness port, alternate loopback ports, `127.0.0.1`, `::1`, pod IP, or a Service address that reaches the gateway.
2. The guard exposes a fixed operation surface, not a generic URL/proxy method. A malicious agent cannot turn an allowed guard call into an arbitrary gateway request.
3. The guard rejects a request whose run UID, spec digest, ToolSet, or effect metadata does not match its immutable run snapshot.
4. UID 1338 cannot reach the Kubernetes API, node metadata, private ranges, or any run-local credential source beyond its intended projected files.
5. The agent sees no Kubernetes service-account token: `automountServiceAccountToken=false` remains explicit and `/var/run/secrets/kubernetes.io/serviceaccount` is absent.

Until this invariant is demonstrated, the architecture is a design candidate, not a security claim.

## 4. External authorization and processing: extension or second proxy?

### What v1.4.1 actually exposes

**Fact.** `ExtAuthz` can call an HTTP or gRPC authorization service, include selected headers, and optionally buffer request bytes. The default body limit is 8 KiB; if a request exceeds the configured limit, the service receives a partial body only when `allowPartialMessage` is enabled. Its failure mode defaults to deny, but `allow` is also configurable.

**Fact.** `ExtProc` can send request/response headers and bodies in none, buffered, partial, or full-duplex-streamed modes. Its default failure mode is fail-closed, but fail-open is an explicit supported option.

**Fact.** The external service receives generic HTTP authorization/process messages and raw/buffered body data. It does not receive a typed `mcp.tool.arguments` field from the agentgateway RBAC context. MCP argument awareness must therefore be implemented by parsing the raw JSON body in the external service.

**Fact.** v1.4.1’s larger external-service response allowance does not change the request body limit, the partial-body option, or the failure-mode choices.

Sources: [ExtAuthz body and failure configuration](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_authz.rs#L51-L89), [ExtAuthz request buffering](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_authz.rs#L157-L179), [ExtAuthz buffering/failure path](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_authz.rs#L251-L296), and [ExtProc modes](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_proc/processing.rs#L14-L37).

### Evaluation for AGW

**Inference.** An external authorization service can be used as a pre-forwarding strict-argument check if it is configured with complete-body buffering, no partial-body acceptance, a complete cache key or no cache, and fail-closed behavior. That could move part of `agw-guard`’s admission check into the gateway’s external-auth path.

**Inference.** It cannot replace the full guard contract:

- ExtAuthz runs before the upstream effect and has no authoritative post-effect response to commit in the ledger.
- ExtProc can observe request and response streams, but it is still an external serial hop and does not supply effect idempotency, `unknown_effect`, run-level completion semantics, or durable artifact authorization.
- Failure-open configuration is incompatible with the v3 security path. It must be prohibited by AGW policy even though agentgateway supports it.
- Raw body visibility is not the same as exact JSON semantics. The guard still needs duplicate-key, number, depth, size, canonicalization, and ToolSet policy decisions.

**Verdict.** No: external authorization/processing is not an official in-process extension point that turns AGW strict policy and effects into a zero-cost agentgateway plugin. It can be a carefully constrained pre-check, but the secure composition remains a serial guard boundary. Keep `agw-guard` as the authority until an upstream extension contract exposes both the request and the completed downstream effect with fail-closed semantics.

## 5. Effect-ledger extension point

**Fact.** The v1.4.1 source has an internal controller plugin registry with contributions for policies, backends, resource extensions, listeners, routes, and status. It is a controller composition mechanism, not a data-plane request/response hook.

**Fact.** The controller `pluginsdk` exposes a `GatewayControllerExtension` lifecycle for controller behavior. It does not define a request effect, idempotency, or post-backend outcome interface.

**Fact.** The new standalone `ConfigImporter` trait imports source configuration into an `ImportPlan` and emits a standalone config. v1.4.1 currently registers the LiteLLM importer. It runs at configuration import time, not per request.

**Fact.** ExtAuthz and ExtProc are network protocol integrations. They are useful extension surfaces, but they are not an official agentgateway plugin ABI and do not provide the effect-ledger semantics required by AGW.

Sources: [controller plugin registry](https://github.com/agentgateway/agentgateway/blob/v1.4.1/controller/pkg/agentgateway/plugins/registry.go#L9-L50), [controller extension SDK](https://github.com/agentgateway/agentgateway/blob/v1.4.1/controller/pkg/pluginsdk/types.go#L19-L31), [standalone configuration importer](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/import.rs#L1-L123), and [import command](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway-app/src/lib.rs#L97-L121).

**Verdict.** No official v1.4.1 extension point was found where AGW can insert the effect ledger inside agentgateway. Keep the ledger in `agw-guard` or in a separate AGW-owned component. Do not build against internal controller plugin types as if they were a stable runtime plugin API.

## 6. What can actually be deleted from the current v3 broker

### Measured current non-test Go LOC

These are physical non-test Go lines in the current worktree, excluding tests, generated/vendor code, and the temporary agentgateway clone:

| Area | LOC | Composition result |
|---|---:|---|
| `v3/internal/broker` | 3,249 | Partially replaceable; guard-critical pieces remain. |
| `v3/cmd/agw-broker` | 1,235 | Reduce after the HTTP surface is narrowed to guard/runtime/artifact functions. |
| `v3/internal/runsecret` | 822 | Remains; it still creates the per-run credential projection, now consumed by gateway/guard containers. |
| `v3/internal/runplan` | 727 | Remains; it still creates work/verify plans and controller-owned clone credentials. |
| `v3/internal/workload` | 957 | Remains; it must add the gateway sidecar and fix the airlock. |
| `v3/internal/effects` | 418 | Remains; this is the product-specific ledger. |
| `v3/pkg/strictjson` | 702 | Remains; agentgateway v1.4.1 does not replace exact matching. |
| `v3/pkg/toolpolicy` | 124 | Remains or is absorbed into guard code; it is not replaced by gateway RBAC alone. |
| `v3/pkg/runtimeproto` | 884 | Remains. |
| `v3/pkg/codexadapter` | 1,454 | Remains. |
| `v3/internal/artifactauth` | 1,218 | Remains for scoped artifact access. |
| `v3/internal/sandbox` | 1,561 | Remains for Sandbox planning/adaptation. |

### Precise replacement candidates

| Current code | Candidate action | Boundary that must remain |
|---|---|---|
| `v3/internal/broker/policy.go:72`, `compileProviders` | Replace model-route-to-provider compilation with agentgateway backend/model configuration. | `compileToolSet`, exact argument encoding, effect classification, run identity, and limits remain local. |
| `v3/internal/broker/model.go:47-306`, including `InvokeModel`, `providerForModel`, wire selection, and provider request functions | Candidate for deletion/rewrite once OpenAI/Anthropic routing and provider translation are proven through v1.4.1. | Request admission, run identity, effect rules, and completion semantics remain in the guard. |
| `v3/internal/broker/model.go:430-598`, usage extraction and provider-specific usage parsing | Candidate for deletion if v1.4.1’s model catalog/usage/accounting can provide trustworthy per-run usage. | Keep a local budget fail-safe until usage, streaming, missing-usage, and provider error behavior are tested. |
| `v3/internal/broker/transport.go:105-173`, provider endpoint validation | Candidate duplicate after gateway owns all upstream connections. | Do not remove SSRF/private-range protection until equivalent gateway/network-policy behavior is demonstrated. |
| `v3/internal/broker/mcp.go:22-283`, direct MCP transport and response decoding | Candidate for replacement by agentgateway MCP routing, credentials, and session transport. | The guard still validates the incoming operation, exact arguments, ToolSet, effect, and result contract. |
| `v3/internal/broker/calls.go:157-176`, `resolveCredential` | Replace direct provider credential lookup with a gateway-only credential projection/config. | `CallTool`, effect reservation/commit, and unknown-effect handling remain. |
| `v3/internal/broker/broker.go`, `CostEstimator`/`PricingTable` and model budget plumbing | Candidate for reduction after a real accounting test. | `EffectLedger`, `ArtifactStore`, runtime state, and conservative per-run limits remain. |
| `v3/internal/broker/mcphandler.go` | Keep or reduce; it is the natural guard-facing MCP endpoint and contains strict local RPC validation. | Exact JSON and non-bypassable ToolSet policy remain. |
| `v3/internal/broker/runtime.go` and `runtimehandler.go` | Keep. | `agw.runtime.v1` is not supplied by agentgateway. |
| `v3/internal/broker/artifact.go` | Keep. | Artifact descriptors, scoped upload, and completion evidence remain AGW-owned. |

**Inference.** A conservative first deletion range is approximately **800–1,100 non-test lines** from the broker/command after model routing and selected provider translation have passed live tests. A broader hybrid cleanup may reach **1,100–1,400 lines** if agentgateway also takes direct MCP transport and the local guard keeps only admission/effects/runtime/artifacts. It is not realistic to delete the entire 3,249-line broker: the exact policy, effect ledger, runtime supervision, artifacts, identity binding, and fail-closed budget boundaries are precisely the parts agentgateway does not provide.

**Unknown.** v1.4.1’s model catalog and token accounting may not expose the exact per-run budget semantics AGW needs. Until it proves reliable usage for streaming, provider-specific response shapes, missing usage, retries, and cost limits, `CostEstimator` is a safety control, not dead code.

## 7. Adopt / hybrid / reject verdict

| Option | Verdict | Reason |
|---|---|---|
| Adopt agentgateway as the complete v3 broker and policy authority | **Reject** | It does not provide exact MCP argument authorization, an effect ledger, AGW runtime completion, or a stable runtime plugin hook for those features. |
| Adopt agentgateway behind a per-run `agw-guard` sidecar | **Adopt as the Phase 0 target** | Preserves v3’s strongest contracts while deleting solved provider credential/routing work after proof. |
| Centralize agentgateway in `agw-system` immediately | **Defer** | Increases cross-run credential, config, session, and blast-radius risk before the per-run composition is proven. |
| Support an external gateway as an optional backend | **Conditional adopt** | Useful for organizations that already operate a compliant gateway; never treat it as the local completion/effect authority. |

The resulting ADR-026 wording should be: **agentgateway is the downstream credential/routing adapter; AGW remains the outer loop and trust/effect boundary.**

## 8. Exact Phase 0 live-test plan

These tests were not executed by this documentation-only spike. They are the admission gate before changing the v3 workload or deleting broker code. Use immutable image digests after resolving `cr.agentgateway.dev/agentgateway:v1.4.1`; do not promote a mutable tag.

### P0-A — release and configuration smoke

1. Resolve and record the OCI digest for the v1.4.1 image and controller/chart artifacts.
2. Start a disposable per-run pod with agent UID 1000, guard UID 1337, gateway UID 1338, `hostUsers: false`, `automountServiceAccountToken: false`, no host network/PID/IPC, RuntimeDefault seccomp, and read-only root filesystems for normal containers.
3. Verify that the gateway binds only its intended front-door port. Set admin/stats/readiness to Unix sockets or `off` if the chosen deployment does not need them; do not expose management ports to the agent.
4. Confirm configuration errors, absent credentials, malformed OAuth subject tokens, and missing backends fail the pod closed.

**Pass:** immutable revision recorded, readiness works, no service-account token exists, and no management listener is reachable from UID 1000.

### P0-B — real GitHub MCP read

1. Use a disposable read-only GitHub credential/App installation scoped to a test repository or the existing read-only GitHub MCP backend.
2. Configure one allowed read tool, such as the actual server’s `get_me` or `list_issues`, through `agw-guard -> agentgateway v1.4.1 -> GitHub MCP`.
3. Record the gateway access log, guard ledger event, upstream request metadata, and returned MCP result.
4. Confirm the agent sees the tool result but never sees the GitHub credential, gateway static key, OAuth client secret, exchanged token, or credential canary.

**Pass:** the real read reaches GitHub once, the expected credential is injected only downstream, the result is correctly typed, and the guard records the run/effect classification.

### P0-C — denied write and exact-argument matrix

1. Configure a mutating GitHub MCP tool, such as issue/PR creation, as excluded or denied in the run ToolSet.
2. Ask the agent to call it. The guard must reject before agentgateway makes an upstream request. Verify the GitHub-side request counter remains zero.
3. Against a disposable recording MCP fixture, exercise an allowed exact object and then each of: missing field, explicit `null`, extra field, nested mismatch, reordered array, duplicate key, altered number representation, excessive depth, and oversized body.
4. Verify the v3 strict JSON result for every case. Do not substitute agentgateway RBAC for this matrix.

**Pass:** only the exact permitted JSON reaches the gateway/upstream; denied write and every malformed/mismatched case is rejected by the guard; no retry turns a denied write into an upstream call.

### P0-D — credential canaries and exposure audit

Use unique canary values in each credential source: a static backend key, an OAuth client secret, an exchanged token response, and an unrelated Secret key.

Check the agent container’s environment, argv, mounted files, `/proc` visibility, workspace, runtime transcript, guard response, gateway logs, and object-store artifacts. Check both static `secretRef` and OAuth exchange. Rotate the source Secret and verify the documented restart/reload behavior; v1.4.1’s private-key path states that file-loaded keys are read at config load and Kubernetes remounts need a restart.

**Pass:** only the gateway/controller path sees the canaries; unrelated keys are not materialized; no canary appears in agent output or artifacts; rotation is explicit and auditable.

### P0-E — bypass attempts

From UID 1000, attempt all of the following:

- `127.0.0.1:8082`, `::1:8082`, alternate loopback ports, and the gateway’s admin/stats/readiness ports;
- the pod IP and any Service DNS name that resolves to the gateway;
- direct provider/MCP HTTPS endpoints;
- Kubernetes API service, node metadata/link-local addresses, RFC1918/private ranges, and DNS rebinding targets;
- a forged run UID, spec digest, base SHA, authorization header, session ID, or effect key;
- a request that asks the guard to proxy an arbitrary URL or arbitrary MCP method.

From UID 1337, verify it can reach only the configured gateway port and approved backends. From UID 1338, verify it cannot read the guard’s credential/state mounts and cannot access the Kubernetes API or private ranges.

Run the same matrix over IPv4 and IPv6. Specifically verify that the current broad `-o lo -j ACCEPT` rule has been replaced by destination-specific rules; a successful direct connection is a hard failure, not a warning.

**Pass:** the only successful path for agent-originated tool/model traffic is the fixed guard endpoint.

### P0-F — sessions and tasks

Run two simultaneous per-run pods and, separately, two logical sessions through a central test gateway. Attempt to reuse MCP session IDs, task IDs, cookies, authorization headers, and connection state across runs. Exercise task creation/retrieval/update/cancellation if the upstream supports MCP Tasks.

**Pass:** no cross-run task/session/result access; task identity is bound to the run; synthetic gateway IDs are not treated as user session identity; central shape is rejected unless it passes the same isolation test.

### P0-G — restart, outage, and effect semantics

1. Restart agentgateway during a read. The guard may retry only a read under an explicit safe policy.
2. Restart agentgateway during a mutating call. If the outcome is ambiguous, the guard records `unknown_effect` and never blindly retries.
3. Crash/restart the guard after effect reservation but before recording the upstream result. Verify the ledger and reconciliation path.
4. Stop agentgateway, the token endpoint, the ext-auth service, and the ext-proc service independently. Verify security-path calls fail closed.
5. Exercise gateway fail-open MCP fan-out with one failing backend and with all backends failing. The all-failed case must be an error, never an empty success accepted by the guard.

**Pass:** no duplicate external mutation is caused by a gateway/guard restart, and every security dependency outage produces a deny/unavailable result.

### P0-H — model routing and accounting

Run OpenAI-compatible and Anthropic-compatible requests through the guard and v1.4.1 gateway with streaming, tool-call history, provider errors, retries, missing usage, and multiple model routes. Compare gateway usage/cost data with provider records. Exceed the configured per-run budget deliberately.

**Pass:** model routing and translation are equivalent to the current broker, usage is trustworthy for every tested response form, and budget exhaustion is fail-closed. Only then may the candidate model/usage functions be removed.

### P0-I — ext-auth/proc safety configuration

Send MCP request bodies below, at, and above the ExtAuthz limit; include duplicate keys and malformed JSON. Test complete buffering, partial-body rejection, cache disabled/complete keying, service timeout, service error, and process restart. Verify ExtProc request and response body modes and fail-closed behavior.

**Pass:** no partial body is authorized, no cache returns a decision for a different body, and no external-service outage allows a tool/model/effect request.

## 9. Implementation boundary after Phase 0

If every Phase 0 gate passes, the next change should be deliberately small:

1. Add agentgateway v1.4.1 as a per-run sidecar with an immutable generated config and only the projected credentials it requires.
2. Keep the current direct-mode exact airlock unchanged; enable the future composition only with its separately tested UID-and-destination rules for guard and gateway ports.
3. Keep `agw-guard`’s existing exact-argument, ToolSet, effect, runtime, artifact, and identity code.
4. Route one real read-only GitHub MCP tool through the composition.
5. Migrate one model route only after P0-H passes.
6. Delete only the measured provider/model code proven redundant; do not remove `runsecret`, `runplan`, `runtimeproto`, `effects`, or artifact authorization based on architectural optimism.

If any bypass, credential leak, ambiguous mutation retry, partial-body authorization, or fail-open path is observed, reject the composition and keep the current broker boundary until the specific invariant is repaired.

## Primary source index

- [v1.4.1 release notes and upgrade/security deltas](https://github.com/agentgateway/agentgateway/releases/tag/v1.4.1)
- [v1.4.1 tagged source](https://github.com/agentgateway/agentgateway/tree/v1.4.1)
- [Backend authentication documentation](https://agentgateway.dev/docs/standalone/latest/configuration/security/backend-authn/)
- [OAuth token exchange documentation](https://agentgateway.dev/docs/standalone/latest/configuration/security/backend-authn/oauth-token-exchange/)
- [Cross App Access documentation](https://agentgateway.dev/docs/standalone/latest/configuration/security/backend-authn/cross-app-access/)
- [External authorization documentation](https://agentgateway.dev/docs/standalone/latest/configuration/security/external-authz/)
- [Kubernetes API reference](https://agentgateway.dev/docs/kubernetes/latest/reference/api/)
- [v1.4.1 MCP RBAC test](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/authorization_tests.rs#L288-L299)
- [v1.4.1 MCP RBAC implementation](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/mcp/rbac.rs#L51-L59)
- [v1.4.1 OAuth implementation](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/auth/oauth/mod.rs#L348-L381)
- [v1.4.1 external authz implementation](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_authz.rs#L51-L89)
- [v1.4.1 external processing implementation](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/http/ext_proc/processing.rs#L14-L37)
- [v1.4.1 controller plugin registry](https://github.com/agentgateway/agentgateway/blob/v1.4.1/controller/pkg/agentgateway/plugins/registry.go#L9-L50)
- [v1.4.1 standalone config importer](https://github.com/agentgateway/agentgateway/blob/v1.4.1/crates/agentgateway/src/import.rs#L1-L123)
