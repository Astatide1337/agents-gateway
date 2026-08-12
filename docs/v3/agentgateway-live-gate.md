# Agentgateway composition live gate

Status: passed on 2026-08-12 in a disposable local provider-free gate. This is evidence for the
`agw-guard -> agentgateway v1.4.1 -> recording MCP` boundary only. It is not a
provider test, a production deployment, or evidence that agentgateway replaces
AGW exact-argument policy or the effect ledger.

## What was missing

The Go `phase0/agentgateway-guard/gateway` package is deliberately a data-plane
double. Its tests prove the MCP framing, canary contract, and guard behavior,
but they do not execute the agentgateway binary. The existing Kubernetes
checker also required an operator to prepare a cluster and load all images.

The bounded local runner now closes that testability gap:

1. It requires the reviewed `cr.agentgateway.dev/agentgateway@sha256:...`
   digest recorded in `versions.env`.
2. It builds the guard, recording, and airlock fixtures from their pinned
   `golang`/`alpine` bases using unique local tags. The disposable fixture
   build disables only Docker's optional provenance manifest so the k3s worker
   containerd can import one linux/amd64 image stream; production release
   builds retain their attestation policy.
3. It runs as a child of `v3/scripts/local-k3s-smoke.sh`, which creates one
   uniquely named disposable two-node k3s cluster and isolated kubeconfig.
4. It imports the real agentgateway digest, the pinned Alpine base, and all
   local fixtures into the k3s worker's `k8s.io` containerd namespace with an
   explicit `linux/amd64` platform.
   Unqualified fixture refs are asserted using containerd's normalized
   `docker.io/library/<name>:<tag>` spelling.
   The digest in `versions.env` is the reviewed multi-platform registry
   manifest/index digest. During the local import, containerd selects the
   immutable linux/amd64 child manifest and the runner creates a repository
   alias for that child digest; the live pod therefore uses the child digest,
   while the runner proves it came from the reviewed index digest. The two
   digests are expected to differ and are both immutable.
5. It invokes `scripts/live-check.sh`, which applies only the named Phase 0
   resources and exercises the real binary. Before applying, the child also
   runs `validate-agentgateway-config.sh` against the pinned binary with a
   readable synthetic canary, so file-backed auth syntax cannot silently drift.
   Evidence is read through a dedicated unprivileged probe with no canary mount;
   this avoids treating a nested-k3s `kubectl port-forward` limitation as a
   data-plane failure.
6. The child removes the exact four local fixture tags and named Phase-0
   namespace on exit; the parent removes the exact k3s containers, network, and
   temporary kubeconfig on every exit. There is no keep mode.

## Run it

Prerequisites are Docker, `kubectl`, and the parent two-node k3s smoke harness.
The explicit opt-in is intentional because the command builds local fixtures,
pulls the pinned test images, and mutates only that disposable cluster:

```sh
cd /home/ubuntu/Projects/agents-gateway
AGW_PHASE0_LOCAL_LIVE=1 \
  bash v3/scripts/local-k3s-smoke.sh --apply --yes --agentgateway-chain
```

Run the offline guard fixture suite separately:

```sh
cd /home/ubuntu/Projects/agents-gateway/v3
./phase0/agentgateway-guard/scripts/test.sh
```

The agentgateway image cannot be overridden by this runner: it must equal the
recorded v1.4.1 repository and digest. The parent harness owns the exact
cluster, kubeconfig, logs, and Docker resources and removes them after the run.

The passing run used the source revision in `versions.env`, its reviewed
multi-platform digest, the selected linux/amd64 child digest, a dynamically
named two-node k3s cluster, the worker label `agw.astatide.com/agents=true`,
and the worker `NoSchedule` taint. Its final output included `canaryValid=1`,
transport-level denied direct-destination checks, no canary in agent-visible
inputs or pod logs, and negative Secret/exec RBAC checks. The exact cluster,
network, kubeconfig, namespace, and fixture image tags were removed by their
ownership-scoped traps.

## What a passing run proves

The real pod must pass all of the existing live checks:

- UID 1000 can call only the fixed guard endpoint.
- The guard rejects an unknown tool, non-exact arguments, and an unapproved
  write before agentgateway or the recording server sees them.
- The one permitted read traverses the real agentgateway MCP listener and its
  one-time initialize/notification session into the recording server.
- The recording server observes exactly one valid backend canary and never
  returns the canary to the guard or agent.
- The agent cannot directly reach agentgateway, its Unix-socket management
  endpoints, the recording server, alternate loopback addresses, or the IPv6
  equivalents.
- The canary is absent from the agent/guard environment and mounted files.
- The run ServiceAccount cannot read Secrets or execute into pods.
- The rendered pod contains the immutable selected linux/amd64 child digest
  derived from the real reviewed image index rather than the source manifest's
  zero-digest sentinel.

The evidence endpoint is queried by a disposable UID-1338 probe that has no
canary volume or environment variable. This is an observation aid only; it is
not part of the production pod design.

The nested k3s CNI is not treated as proof of the target host's NetworkPolicy
implementation, kernel user namespaces, or the two-node production floor.
Those still require the separate target-node Phase 0 gate.

## Security boundary that remains authoritative

The guard still owns strict JSON decoding, exact ToolSet arguments, immutable
run/spec binding, effect classification, approvals, and ledger semantics. The
live fixture has no object-store ledger, so a write fails closed with
`effect_ledger_required`; this gate must never turn that into a fake successful
mutation. A downstream uncertainty is not retried by the HTTP transport.

The agentgateway container receives only the recording canary file. The agent
and guard do not receive it. Management listeners remain Unix sockets mounted
only into the gateway container. No provider, provider credential, real MCP
server, or hosted service is contacted by the chain.

## Evidence boundary

The local result is valid only for the exact rendered digest, source revision,
fixture image build, and disposable k3s context named in the run output. It
does not prove:

- a real provider/model request or provider credential exchange;
- a real GitHub/MCP side effect;
- production IAM, KMS, object storage, or GitHub App behavior;
- NetworkPolicy enforcement on the target bare-metal cluster; or
- that agentgateway can replace AGW's exact-argument/effect authority.

If any direct bypass succeeds, a denied request increments the recording
counter, a canary appears in an agent-visible output, the image is mutable, or
the agentgateway process fails closed incorrectly, stop adoption and retain the
current AGW broker boundary.
