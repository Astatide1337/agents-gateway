# Agents Gateway v3

Agents Gateway v3 is a Kubernetes-native controller for bounded coding-agent
runs. An `AgentRun` is complete only after a patch is captured, independently
verified from a clean checkout, evaluated by a `Gate`, and—when configured—
published by the operator through an idempotent GitHub App effect.

The complete architecture, trust model, rollout gates, and current checkpoint
are in [`docs/v3/kubernetes-native-implementation-plan.md`](../docs/v3/kubernetes-native-implementation-plan.md).
The quality and build-vs-adopt decisions are recorded in
[`quality-architecture-addendum.md`](../docs/v3/quality-architecture-addendum.md)
and
[`build-vs-adopt-addendum.md`](../docs/v3/build-vs-adopt-addendum.md).

## API

The operator owns exactly seven namespaced CRDs in
`agents.astatide.com/v1alpha1`:

- `AgentRun`
- `Agent`
- `Gate`
- `ToolSet`
- `ModelRoute`
- `Policy`
- `ContextStrategy`

The operator defaults to the upstream `agents.x-k8s.io/v1beta1` `Sandbox` CRD;
`--sandbox-backend=job` selects the built-in Job/PVC fallback and starts without
that CRD or controller. It does not create a separate API server, queue,
database, or web console. Argo Workflows is being evaluated as an optional
durable sequencing backend behind the same `AgentRun` contract; the verified
direct backend remains available until the Argo Phase-0 gates pass.

## Trust boundary

- The agent is UID 1000, receives no Secret or service-account token, and has
  no direct egress after the network airlock runs.
- The loopback guard/broker is UID 1337 and receives only explicitly
  projected, phase/run-scoped credentials. agentgateway is a conditional
  downstream routing/credential adapter, never the exact-policy or effect
  authority.
- Capture is networkless and credential-free.
- Verification starts from a fresh exact-SHA clone, applies the immutable
  patch offline, and never mounts the work PVC.
- The operator signs the Gate report and owns GitHub publication. The agent
  never receives publish credentials.
- An ambiguous external effect is terminal `UnknownEffect` and is never
  retried blindly.

## Local validation

```bash
./scripts/validate-local.sh
```

The local entrypoint runs the unit, serialized race, vet, vulnerability,
manifest, chart, image-contract, Phase-0 plan, CRD-drift, and command-build
checks without creating a cluster, invoking GitHub, pushing images, or pruning
container state. Image builds are opt-in; see `scripts/README.md`.

`.github/workflows/v3-ci.yml` is manual-only. Its hosted image matrix and Kind
smoke test are separate opt-in inputs so ordinary pushes do not consume GitHub
Actions minutes. Use one deliberate hosted confirmation only after the local
suite is green.

## Deployment status

The code is a local implementation candidate, not a production release. Do
not deploy or delete v1/v2 until the real two-node k3s Phase-0 suite proves
user namespaces, the UID airlock, CNI policy, Agent Sandbox/PVC lifecycle, and
object-store semantics. Production also requires digest-pushed images, a
scoped GitHub App, a tested cosign self-managed-or-KMS trust root during the
dual-format evidence migration, and an exact-prefix STS role. See the plan
checkpoint for the remaining live gates.
