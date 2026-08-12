# Phase 0: Argo Workflows ↔ Agent Sandbox

This is a bounded, disposable experiment for the hybrid Agents Gateway v3 design.
It proves one compatibility boundary:

```text
Argo Workflow
  ├─ deterministic PVC + setup fixture
  ├─ resource-template apply → Agent Sandbox v0.5.4
  ├─ Go bridge: condition type Ready=True
  ├─ UID-bound Ready marker + Finished=True/PodSucceeded
  ├─ capture file + digest + Argo output artifact
  └─ idempotent onExit deletion of Sandbox and PVC
```

The bridge is deliberately separate from Argo's `successCondition`. Agent Sandbox
stores `Ready` and `Finished` in a condition array; the bridge resolves those entries
by `type`, requires a valid identity-bound UID marker, follows the upstream terminal
shape (`Ready=False/PodSucceeded` after success), and
fails closed on duplicate, unknown, malformed, or unsuccessful state.

This subtree is static/local experiment material. It does not modify the existing v3
operator, CRDs, charts, or any cluster. No Argo Events resources are included.

## Pinned inputs

The exact release, upstream commit, release URL, and downloaded manifest SHA-256 are
in [`versions.env`](versions.env). The experiment targets:

- Argo Workflows `v4.1.0` (`e5ed20d5cb54d4708d5aeb29148b3e49922f795c`)
- `kubernetes-sigs/agent-sandbox` `v0.5.4` (`6e2b7617310e3bf084b6d1a1cffbeb141a5e37fe`)
- Go `1.26.5` for the bridge build
- Alpine `3.22.1` by OCI index digest for fixture/setup/capture pods

Do not use these manifests against the existing production or home-lab cluster. The
live step requires a separate disposable Kubernetes context.

## Local prerequisites

Required for local verification:

- Go `1.26.5` or a compatible newer Go toolchain
- `bash`, `rg`, and `sha256sum`
- `jq` (required by the live evidence checker)

Optional offline schema verification uses the Argo Workflows `v4.1.0` CLI. Its
Linux-amd64 release archive SHA-256 is
`ad176b3bb64a7eb6666586a50001e4807ba0c98ab50bb4f2e87a465f07784ced`.

Required only for the optional live experiment:

- `kubectl` and `jq`, plus an explicit kubeconfig/context for a disposable cluster
- Argo Workflows `v4.1.0` installed from the pinned release manifest
- Agent Sandbox `v0.5.4` installed from the pinned release manifest
- a default `StorageClass` with at least 256 MiB available
- an Argo artifact repository (S3-compatible MinIO/R2 or equivalent) configured in
  the Argo controller; the output artifact cannot be durably stored without one
- a way to make the local `agw-sandbox-wait-bridge:phase0` image available to the
  disposable cluster (for example `kind load docker-image` or an immutable private
  registry digest)

The checked-in Workflow does not contain credentials or an artifact-repository
ConfigMap. That is intentional: credentials and the repository endpoint are cluster
inputs, not safe defaults. The captured file and SHA-256 are still written to the
workspace PVC before the Argo artifact upload is attempted.

## Run bounded local tests

From the repository's `v3` directory:

```sh
./phase0/argo-sandbox/scripts/test.sh
```

This runs the static verifier and only the bridge unit tests. It does not call
`kubectl`, create containers, contact a cluster, or claim that the hybrid design works
live.

If the pinned Argo CLI is in `PATH`, add its offline schema check:

```sh
ARGO_LINT=1 ./phase0/argo-sandbox/scripts/test.sh
```

To build the bridge image locally after the tests pass:

```sh
BUILD_BRIDGE_IMAGE=1 ./phase0/argo-sandbox/scripts/test.sh
```

The image is built from the pinned Go base and ends in a scratch filesystem with only
the bridge binary. The Workflow deliberately references the local tag with
`imagePullPolicy: Never`; a real deployment must replace that reference with a
reviewed immutable registry digest.

## Disposable-cluster procedure

Run these steps only after selecting a disposable context. `kubectl apply` is not run
by the repository test script.

1. Download the two pinned upstream manifests and verify their SHA-256 values against
   `versions.env`.
2. Install Argo Workflows `v4.1.0` using its namespace-scoped install manifest. Wait
   for the Argo controller and executor components to become ready.
3. Install Agent Sandbox `v0.5.4` using `sandbox.yaml`. Wait for its controller and
   CRD establishment.
4. Make `agw-sandbox-wait-bridge:phase0` available to the nodes.
5. Configure an Argo artifact repository for namespace `agw-runs` without committing
   credentials here.
6. Inspect the rendered resources, then apply this subtree:

   ```sh
   kubectl apply -k ./phase0/argo-sandbox
   ```

7. Observe the run:

   ```sh
   kubectl -n agw-runs get workflow phase0-argo-sandbox -w
   # Optional while the Workflow is still running, before onExit cleanup:
   workflow_uid="$(kubectl -n agw-runs get workflow phase0-argo-sandbox -o jsonpath='{.metadata.uid}')"
   kubectl -n agw-runs get sandbox "phase0-${workflow_uid}" -o yaml
   kubectl -n agw-runs get pvc "phase0-${workflow_uid}-workspace"
   ```

8. After the Workflow reports `Succeeded`, run the guarded, read-only checks
   with an explicit disposable kubeconfig and context. The checker does not
   inherit the shell's ambient kubectl context:

   ```sh
   AGW_PHASE0_LIVE=1 RUN_LIVE=1 \
     AGW_PHASE0_KUBECONFIG=/tmp/agw-disposable.kubeconfig \
     AGW_PHASE0_CONTEXT=kind-agw-v3 \
     ./phase0/argo-sandbox/scripts/test.sh
   ```

The static `Workflow` has a fixed name and intentionally does not use `generateName`.
To repeat the experiment, wait for `onExit` cleanup and the TTL, or delete only the
named Phase 0 Workflow in the disposable namespace before applying it again. Never
delete a broad namespace or an existing AGW installation as part of this test.

## Expected evidence

Static evidence is limited to source/manifests and unit-test behavior. A successful
live run must additionally show all of the following:

- Argo nodes for setup, PVC apply, Sandbox apply, `wait-ready`, `wait-finished`,
  capture, and cleanup.
- an identity-bound marker proving `Ready=True` was observed, followed by the Agent
  Sandbox v0.5.4 terminal shape: `Finished=True/PodSucceeded` and
  `Ready=False/PodSucceeded`.
- `/workspace/.agw/ready.json` and `/workspace/.agw/finished.json` with the same
  Sandbox UID; a stale marker must block the finished step.
- `/workspace/artifact.json`, `/workspace/.agw/artifact-sha256`, and the capture
  parameter matching the artifact bytes.
- an Argo output artifact retrievable from the configured artifact repository.
- the Sandbox and workspace PVC absent after cleanup, or an explicitly explained
  Kubernetes finalization delay.
- `kubectl auth can-i` proving the workflow ServiceAccount cannot read Secrets or
  Nodes and cannot create namespaces or other cluster-scoped resources.

The live checker preserves the Ready and Finished marker bytes, artifact digest,
and Workflow JSON in a temporary evidence directory before exiting. It also
requires a Phase-0-labelled namespace and validates every lifecycle node is
`Succeeded`; this avoids treating a merely-created Workflow or a pre-cleanup
resource snapshot as proof of the contract.

The current repository has not performed those live checks. No live-cluster success
is claimed by the local test result.

## Idempotency and failure behavior

One logical Phase 0 run owns these deterministic names:

| Object | Name | Retry behavior |
| --- | --- | --- |
| Workflow | `phase0-argo-sandbox` | fixed; duplicate submission is visible and must be reconciled |
| WorkflowTemplate | `agw-phase0-sandbox-lifecycle` | fixed |
| PVC | `phase0-<workflow-uid>-workspace` | `apply`, bounded retry, isolated per Workflow identity |
| Sandbox | `phase0-<workflow-uid>` | `apply`, bounded retry, UID recorded in markers |

`apply` makes a replay after an executor/API transient convergent for the same
Workflow UID without allowing a later Workflow to adopt an earlier run's child.
The bridge never treats a same-name object as the same run unless its UID matches the
marker. Deletion uses Argo's `--ignore-not-found` behavior and is safe to repeat.
The Sandbox's absolute `shutdownTime` is generated by setup (`now + 10 minutes`) as
a safety bound; the Workflow itself has a 15-minute `activeDeadlineSeconds` and each
bridge wait has an 8-minute timeout. A production translator must render a fresh
shutdown time from its immutable run deadline rather than reuse an expired one.

There is no publish step and no external side effect in this experiment. The effect
ledger is therefore not exercised here; that is a separate Phase 0 gate before any
Argo backend can handle publication.

## Stop/go criteria

**Stop** the Argo migration if any of these occur:

- the bridge accepts a missing, duplicate, unknown, or `PodFailed` condition;
- `Finished=True` can pass without a prior, identity-matched `Ready=True` marker;
- a transient executor failure creates a second Sandbox or PVC for one Workflow;
- the output artifact cannot be read back with a matching digest;
- cleanup leaves resources without an explained controller/finalizer delay;
- the Workflow ServiceAccount can access Secrets, Nodes, namespaces, or other
  unapproved resources;
- the cluster cannot enforce the pinned user-namespace/egress assumptions needed by
  the later sandbox security phases.

**Go** only when all seven live evidence groups above are captured in the experiment
record, the direct v3 path remains available for rollback, and the remaining custom
translator/status/effect/Gate responsibilities have explicit owners. Passing this
experiment does not authorize deleting the existing operator.

## Rollback

For a disposable cluster, remove the named Phase 0 Workflow and namespace after
evidence capture, then uninstall the separately installed Argo and Agent Sandbox
releases using their pinned release procedures. This subtree has no production
rollback action and does not touch the existing `agw-operator` chart or code.
