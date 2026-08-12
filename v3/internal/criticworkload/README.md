# Critic workload boundary

`criticworkload` is the independent execution-free critic boundary for v3.

## Production composition

`Build` and default `NewRunner` now emit/observe a real bounded two-container
Job, but admission still requires an explicit `AgentGatewayCapability` result
for the exact digest-pinned v1.4.1 image. Missing or mismatched evidence fails
closed; this is a capability-probe gate, not a permanent integration scaffold.

The ordered workload is:

1. `input-materializer` fetches only the patch and ContextPack from their
   canonical `s3://` references, checks exact lengths/digests, and writes
   regular `0444` files.
2. `gateway-config` renders the v1.4.1 standalone agentgateway config from
   the resolved critic model. It contains the native Anthropic `/v1/messages`
   route, a file-backed Bearer credential at the fixed projected-token path,
   and `retry.attempts: 1` with an empty retry-code set. That Bearer token is
   the run ServiceAccount's short-lived JWT; it is not a provider credential.
3. `airlock` reuses `internal/airlock.Render`: UID 1000 can initiate only
   loopback TCP to 8082, while UID 1338 is the only process with downstream
   egress.
4. `critic` mounts only the verified input read-only and has no Secret or
   token mount. `agentgateway` alone mounts the projected ServiceAccount token
   read-only. The Pod keeps `automountServiceAccountToken: false` so the token
   is not ambiently available to either container.

The provider Secret remains in the long-lived `agw-system` agentgateway data
plane. The critic Job does not copy it into `agw-runs`, and the local sidecar
does not accept a provider-key environment variable. Production admission is
fail-closed until a capability probe has verified the central gateway's JWT
issuer, JWKS validation, audience, and route policy for this ServiceAccount.
The probe's policy reference and evidence digest are recorded on the Job. The
configured JWT evidence digest must be generated with
`GatewayJWTCapabilityEvidenceDigest` from the exact endpoint, route,
ServiceAccount, token TTL, issuer, audience, policy, and gateway Pod selector;
changing any of those values invalidates the digest. This binds the reviewed
evidence to its security-relevant configuration but does not replace the live
probe required for `Verified: true`.

The Runner also owns a per-run NetworkPolicy. It denies ingress, permits DNS,
permits the exact selected `agw-system` gateway Pods and port, and permits only
the operator-resolved object-store CIDRs and HTTPS port. The UID airlock still
restricts critic UID 1000 to the loopback listener; the gateway UID is the only
container allowed to use the other egress paths. NetworkPolicy is created and
read-back validated idempotently before the Job.

The route is deliberately single-provider and Anthropic-native for this first
production adapter. There is no fallback route, no retry, and no silent
`agw-broker` path. Operator/chart wiring remains outside this package change.

## Contract that is ready for that integration

The immutable contract binds the resolved `AgentRun` UID/generation,
spec/base/patch/ContextPack digests, Gate revision, all worker/critic provider
families, and the deterministic critic provider (including its logical
credential reference, never the credential value). The Job is non-retrying
(`backoffLimit: 0`), deadline-bounded, `hostUsers: false`, has ambient
ServiceAccount-token access disabled with one explicit, audience-scoped
projection mounted only into agentgateway, uses read-only-rootfs, and uses a
loopback-only agentgateway sidecar. Patch and ContextPack bytes are
materialized and then mounted read-only before the critic starts.

The local [`agw-critic`](../../cmd/agw-critic) executable and
[`critic` image contract](../../images/critic/README.md) exercise the bounded
file/digest/model-family/output behavior against a loopback HTTP fixture. It
emits exactly one `AGW_CRITIC_INPUT_V1` frame containing canonical
`findingcorroboration.CorroborationInput`; verdicts, scores, Gate results, and
unknown fields are rejected. It never reads or sends provider credentials and
performs no retry.

## Authenticated evidence

The package still provides the runner/source seam for the future integrated
Job. After an authenticated Job and single successful Pod are observed, the
trusted Runner writes the raw canonical input to a create-if-absent object-store
key and writes a separate controller-authored output record. The record binds
Job UID, Pod UID, input contract, artifact URI/digest/size, and logical key. It
is signed by an operator-held Ed25519 key. The Source verifies Kubernetes
ownership, signature, immutable object identity, and content digest before
strict-decoding the record or critic input.

The package never derives `CorroborationResult`, scores findings, or makes a
Gate decision. `VerifyAdapter` exposes only canonical critic input and binding
metadata to the existing verifier.
