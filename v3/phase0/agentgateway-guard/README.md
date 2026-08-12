# Phase 0: agent UID -> `agw-guard` -> agentgateway v1.4.1

This is a bounded, disposable composition probe. It is not a production
deployment and it does not modify the v3 controller, broker, APIs, Argo probe,
or any cluster.

The intended path is:

```text
agent UID 1000
    | loopback TCP/8081 only
    v
agw-guard UID 1337
    | loopback TCP/8082 only
    v
agentgateway v1.4.1 UID 1338
    | recording-only loopback TCP/9090
    v
recording MCP upstream UID 1339
```

The guard owns the exact JSON and ToolSet decision. `agentgateway` only routes
MCP and injects the fake backend canary from a file visible to UID 1338. The
recording server validates that the canary arrived, increments a safe counter,
and never returns the canary. The agent and guard have no canary mount or
canary environment variable.

## Important security boundaries

- The HTTP `Call` type has no approval field. A body containing
  `"approved":true` is rejected as an unknown field.
- Writes require a trusted `ApprovalVerifier` configured in Go. The verifier is
  called with the immutable effect key and request digest; the phase-0 unit
  fixture approves only a precomputed key. The pod process has no object-store
  ledger, so its write path fails closed with `effect_ledger_required`.
- The real `effects.Ledger` is used in tests. A downstream failure commits
  `OutcomeUnknown`, and replay does not call upstream again. An ambiguous claim
  never reaches downstream.
- IPv4 and IPv6 OUTPUT owner rules constrain origin UIDs. INPUT has only exact
  loopback NEW TCP allows for ports 8081, 8082, and 9090, plus established
  replies. There is no public-test mode in this probe.
- Agentgateway admin, stats, and readiness are Unix sockets mounted only in the
  gateway container. They are not TCP listeners and the agent has no socket
  mount.
- The source pod contains a zero-digest render sentinel for agentgateway. It
  must not be applied directly. A live run requires an exact
  `cr.agentgateway.dev/agentgateway@sha256:<64 hex>` reference resolved from
  the `v1.4.1` tag and reviewed by the operator.
- A pinned-binary config check loads the rendered `agentgateway.yaml` with a
  synthetic readable canary before fixture images are built. The live pod then
  tightens the file to UID 1338 and mode 0400.
- The pod has `hostUsers: false`, `automountServiceAccountToken: false`,
  RuntimeDefault seccomp, unique non-root UIDs, read-only roots, and dropped
  capabilities for every long-lived container. The short-lived credential
  fixture init gets only `CHOWN` and `DAC_OVERRIDE` so it can assign the
  canary and Unix-socket runtime directory to UID 1338. The namespaced
  ServiceAccount has an empty Role and no Secret access.

## Local checks

From `v3/`:

```sh
./phase0/agentgateway-guard/scripts/test.sh
RACE=1 ./phase0/agentgateway-guard/scripts/test.sh
CONTAINER_CHECK=1 ./phase0/agentgateway-guard/scripts/test.sh
```

The default suite runs the static verifier, Go tests, and `go vet`. `RACE=1`
adds bounded race tests. `CONTAINER_CHECK=1` builds the two scratch Go fixture
images and the disposable iptables helper, then starts only the recording
fixture to verify its canary behavior. It does not start agentgateway and does
not prove Kubernetes UID isolation.

The default Go suite now also runs a provider-free HTTP chain in
`phase0/agentgateway-guard/gateway`: `agw-guard` speaks MCP to a fixed gateway
backend contract, the fixture gateway injects a trusted canary, and the
recording MCP server verifies that the canary arrived without returning it.
The test exercises initialize → notification → tools/call, reuses the MCP
session boundary, rejects an unknown method, rejects non-exact guard arguments,
and proves an unapproved write never reaches the gateway. It uses ephemeral
test listeners while retaining the production ports in the constructor
validation; it is not evidence for the real agentgateway binary or Kubernetes
UID/iptables behavior.

Run only that seam test with:

```sh
GOTOOLCHAIN=go1.26.5 go test -count=1 ./phase0/agentgateway-guard/gateway
```

The resolver prints the reviewed immutable reference recorded in
`versions.env` (and can resolve the exact release tag when no digest has yet
been recorded):

```sh
AGENTGATEWAY_IMAGE="$(./phase0/agentgateway-guard/scripts/resolve-agentgateway-image.sh)"
printf '%s\n' "$AGENTGATEWAY_IMAGE"
```

Do not copy a tag into `AGENTGATEWAY_IMAGE`. The v1.4.1 registry manifest was
resolved on 2026-08-11 and is recorded as
`sha256:efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29`.
Upgrades repeat release discovery, source review, digest resolution, and the
entire Phase-0 matrix before changing that value.

## Optional disposable two-node k3s run

The supported local path is the parent two-node k3s harness. It supplies an
isolated kubeconfig and worker identity; the child runner does not create a
second cluster. The source manifests are rendered into a new temporary
directory; the tracked sentinel is never applied.

Plan only:

```sh
AGW_PHASE0_LOCAL_LIVE=1 \
  bash v3/scripts/local-k3s-smoke.sh --plan --agentgateway-chain
```

Apply is a separate explicit gate:

```sh
AGW_PHASE0_LOCAL_LIVE=1 \
  bash v3/scripts/local-k3s-smoke.sh --apply --yes --agentgateway-chain
```

The parent harness provides the explicit apply gate and passes the reviewed
digest from `versions.env`; no independent cluster context is accepted by the
child runner.

The checker refuses any context other than the exact value of
`AGW_PHASE0_CONTEXT`. It then performs these probes from the UID-1000 agent
container:

- allowed fixed call to guard `/call`;
- unknown tool and unapproved write rejection before upstream;
- direct IPv4 and IPv6 attempts to gateway 8082, admin 15000, stats 15020,
  readiness 15021, and recording 9090;
- recording evidence showing one call and one valid canary;
- pod-log canary absence; and
- `kubectl auth can-i` checks for Secret and `pods/exec` denial.

## Expected evidence

A live evidence bundle is acceptable only if it includes the rendered pod with
the exact image digest, the pod spec showing UIDs and security fields, the
airlock script/rules, the full guard allow/deny matrix, recording evidence
`calls=1` and `canaryValid=1`, direct-destination failures for both address
families, no canary in agent/guard output, and negative RBAC results.

Static, unit, race, vet, YAML, and container results are explicitly not live
k3s evidence. On 2026-08-12, the disposable two-node local k3s chain passed
with the v1.4.1 multi-platform digest reviewed in `versions.env` and its
selected immutable linux/amd64 child digest used by the pod. Those digests are
expected to differ during the containerd import; the result is recorded in
`docs/v3/agentgateway-live-gate.md`; it does not stand in for the target-node
kernel and NetworkPolicy gates.

## Hard stop/go

Stop adoption if any of these occur:

- the HTTP body can supply approval or a write reaches the verifier without an
  immutable effect key and digest;
- the agent reaches gateway, management, recording, alternate loopback, or
  IPv6 destinations directly;
- INPUT rules block the allowed guard path or allow an unlisted listener;
- a denied or unknown call increments the recording counter;
- the canary appears in agent/guard output, logs, or workspace;
- the gateway image is not resolved to the reviewed v1.4.1 digest;
- the ServiceAccount can read Secrets or execute into pods; or
- a ledger ambiguity causes an automatic second downstream call.

The provider-free composition gate is now passed on the exact disposable
cluster and image, with cleanup complete. Remaining unknowns are the actual
`hostUsers:false`/`iptables -m owner` behavior on the target kernel and the
target cluster's NetworkPolicy enforcement. Public downstream egress is
intentionally outside this recording-only probe.

## Cleanup

Cleanup is also plan-by-default:

```sh
AGW_PHASE0_CONTEXT='<disposable-context>' \
  ./phase0/agentgateway-guard/scripts/cleanup.sh
```

After reviewing the exact named targets, set `AGW_PHASE0_CLEANUP=1` to delete
only this experiment's Pod, ConfigMap, NetworkPolicy, RBAC objects,
ServiceAccount, and namespace on that exact context.
