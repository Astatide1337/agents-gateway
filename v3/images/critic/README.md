# Agents Gateway critic image

`agw-critic` is a bounded, credential-free critic executable. It reads:

- `/workspace/input/patch.diff`
- `/workspace/input/context-pack.json`

It verifies both SHA-256 digests supplied by the Job contract, sends one
Anthropic-compatible request to an explicitly loopback-only model listener,
and emits exactly one `AGW_CRITIC_INPUT_V1` frame containing canonical
`findingcorroboration.CorroborationInput` JSON.

The executable never reads provider credentials and never sends an
Authorization header. It rejects same-family worker/critic routes, non-loopback
gateway URLs, oversized or mutable inputs, noncanonical output, and authority
fields such as verdicts or scores. It performs no retry.

## Production composition

`criticworkload.Build` composes this image into three ordered init steps and
two long-lived containers:

1. `materialize-input` reads only the per-run object-store Secret, fetches the
   exact `s3://` patch and ContextPack, verifies length and digest, and leaves
   regular mode `0444` files in the input volume.
2. `render-agentgateway` writes a deterministic v1.4.1 standalone config with
   native `/v1/messages`, file-backed Bearer auth from the projected
   ServiceAccount token, and `retry.attempts: 1` with no retry codes.
3. `airlock` installs the existing owner-based policy: UID 1000 can connect
   only to loopback port 8082; UID 1338 owns provider egress.
4. `critic` sees only read-only patch/ContextPack files and has no Secret or
   token mount. `agentgateway` alone receives a bounded, audience-specific
   projected ServiceAccount token at
   `/var/run/secrets/agw-gateway/token`. `automountServiceAccountToken` remains
   false.

The local sidecar forwards that token to the long-lived `agw-system`
agentgateway Service. The provider credential stays in the central gateway's
   `AgentgatewayBackend`/Secret and is injected there; no provider Secret is
   materialized in `agw-runs`. `Build` rejects production configuration unless
   an explicit capability probe proves the central issuer/JWKS/audience/route
   policy. The per-run object-store Secret is separate: it is mounted only by
   the trusted input-materializer init container to fetch the two verified
   artifacts.

The Runner creates and validates an owned least-privilege NetworkPolicy for
each Job: default-deny ingress, DNS, the selected central gateway Pod/port,
and only the resolved object-store CIDRs/HTTPS port. A hostname alone is not
accepted because Kubernetes NetworkPolicy cannot enforce hostname destinations.

The production builder requires explicit capability evidence for the exact
digest-pinned agentgateway v1.4.1 image before it emits a Job. It does not
route through `agw-broker`, add a fallback provider, or rely on an unproven
listener. No operator/chart wiring is included in this image change.

## Local checks

```bash
go test ./internal/criticworkload ./cmd/agw-critic
go test -race ./internal/criticworkload ./cmd/agw-critic
```

The command tests use a loopback HTTP fixture, never a paid provider.
