# Argo/Agent Sandbox live gate

Status: disposable-k3s preflight addition

This gate is intentionally separate from `v3/phase0/argo-sandbox/scripts/live-check.sh`.
The existing checker validates the complete Phase-0 fixture, including the bridge,
markers, artifact output, and onExit cleanup. This addition proves the upstream
installation and the two lower-level boundaries first, without editing that
fixture, the chart, or Go code.

## What is pinned

The authoritative pins are in
[`v3/phase0/argo-sandbox/versions.env`](../../v3/phase0/argo-sandbox/versions.env):

- Argo Workflows `v4.1.0`, commit
  `e5ed20d5cb54d4708d5aeb29148b3e49922f795c`
- Agent Sandbox `v0.5.4`, commit
  `6e2b7617310e3bf084b6d1a1cffbeb141a5e37fe`
- SHA-256 checksums for both release manifests
- immutable Alpine fixture image used by the disposable Sandbox

The preflight downloads only those two GitHub release URLs and verifies the
recorded SHA-256 before applying anything. A floating tag, different host, or
checksum mismatch stops the run. On a shared disposable cluster it reuses the
already-installed controllers after readiness checks; it never removes them.

## Evidence boundaries

The output directory is mode `0700` and contains three intentionally distinct
evidence groups.

### API-only evidence

This is evidence that the selected disposable k3s API accepted the installation
and established the schemas. It includes:

- API server version and cluster-info
- node/runtime and StorageClass inventory
- all eight Argo CRDs plus `sandboxes.agents.x-k8s.io` at
  `Established=True`
- the checksum-verified upstream assets and their provenance

API-only evidence does not prove a controller reconciled anything or that a Pod
ran.

### Controller evidence

The preflight waits for and records:

- Argo `workflow-controller` and `argo-server` in the disposable namespace
- Agent Sandbox `agent-sandbox-controller`
- the Agent Sandbox webhook endpoints
- Argo controller `--namespaced` mode

Controller readiness does not prove a resource template created a workload.

### Workload evidence

Two small Workflows are created with deterministic names derived from one run ID.

1. The condition probe applies two ConfigMaps through Argo resource templates.
   The positive node must finish `Succeeded` from `successCondition`; the
   negative node must finish `Failed` from `failureCondition`, while
   `continueOn.failed` allows the parent Workflow to finish successfully. This
   proves the resource-template evaluator itself.
2. The Sandbox probe applies an actual
   `agents.x-k8s.io/v1beta1/Sandbox` through a resource template. The script
   waits for its generated Pod and for `Ready=True` and `Finished=True` in the
   Sandbox condition array, and records that the generated Pod preserved
   `hostUsers: false`.

This distinction matters: Argo's simple `successCondition` expression is not
the Agent Sandbox condition-array verifier. The existing Go bridge remains the
type-aware, identity-bound verifier for `Ready`/`Finished`; the live preflight
only proves that Argo can execute the resource-template boundary and that the
upstream Sandbox controller can complete a real fixture.

## Disposable k3s setup

The following uses a privileged k3s node inside one Docker container. It is a
disposable local control plane, not production evidence for a two-node host
isolation floor. Binding the API to loopback keeps it off the public interface.
Use a fresh suffix for every run.

```sh
set -euo pipefail
run_id="$(date -u +%Y%m%d%H%M%S)"
k3s_name="agw-v3-k3s-${run_id}"
net_name="agw-v3-k3s-net-${run_id}"
kubeconfig="/tmp/${k3s_name}.kubeconfig"

docker network create --driver bridge "$net_name"
docker run --detach --privileged \
  --name "$k3s_name" \
  --network "$net_name" \
  --publish 127.0.0.1:16443:6443 \
  --env K3S_TOKEN="agw-v3-disposable-${run_id}" \
  rancher/k3s:v1.35.0-k3s1 \
  server --disable=traefik --disable=servicelb --write-kubeconfig-mode=644 \
  --tls-san=127.0.0.1

until docker exec "$k3s_name" kubectl get node >/dev/null 2>&1; do sleep 2; done
docker cp "$k3s_name:/etc/rancher/k3s/k3s.yaml" "$kubeconfig"
chmod 600 "$kubeconfig"
kubectl --kubeconfig "$kubeconfig" config set-cluster default \
  --server=https://127.0.0.1:16443 >/dev/null
kubectl --kubeconfig "$kubeconfig" config rename-context default \
  "agw-v3-k3s-${run_id}" >/dev/null
```

The preflight requires the renamed context and loopback API endpoint. It will
refuse an ambient context, a non-loopback server, a namespace that already
exists, or a missing explicit confirmation token.

## Run the live gate

From the repository root:

```sh
AGW_ARGO_PREFLIGHT_LIVE=1 \
AGW_ARGO_PREFLIGHT_CONFIRM=AGW-V3-DISPOSABLE-K3S \
AGW_ARGO_PREFLIGHT_KUBECONFIG="$kubeconfig" \
AGW_ARGO_PREFLIGHT_CONTEXT="agw-v3-k3s-${run_id}" \
AGW_ARGO_PREFLIGHT_RUN_ID="${run_id}" \
  ./v3/scripts/argo-live-preflight.test.sh
```

The default live run removes only namespaced objects carrying the exact
`agw.astatide.com/preflight=<RUN_ID>` label. It does not delete CRDs,
controllers, namespaces, unlabeled events, or any cluster-level resource.
`AGW_ARGO_PREFLIGHT_KEEP=1` retains even the run-labelled probe objects for
inspection. The shared namespace and upstream installation remain for the main
test owner.

The preflight does not delete the k3s container or Docker network. After every
bounded test has completed and the evidence directory is reviewed, the main
agent may remove only the two exact disposable objects:

```sh
# Main-agent teardown only, after all shared-cluster tests:
docker rm --force "$k3s_name"
docker network rm "$net_name"
find /tmp -maxdepth 1 -type f -name "${k3s_name}.kubeconfig" -delete
```

Do not use `docker system prune`, `kubectl delete all`, `kubectl delete -A`, a
wildcard, a manifest delete, or a broad namespace selector. If the script stops
before cleanup, remove only the exact run-labelled names in its evidence
directory; do not touch the shared namespace, controllers, or CRDs, and do not
point the script at another cluster.

## Focused static checks

The test is offline unless `AGW_ARGO_PREFLIGHT_LIVE=1` is set:

```sh
./v3/scripts/argo-live-preflight.test.sh
bash -n ./v3/scripts/argo-live-preflight.test.sh
```

Static mode re-runs the existing Argo/Sandbox static verifier and bridge tests,
checks the concrete release pins and URLs, and checks that the live path has
checksum verification, explicit context/confirmation guards, and no broad
deletion command.

## Stop/go interpretation

This gate is a prerequisite, not a claim that the entire v3 system is complete.

Stop if any of the following occurs:

- a release checksum differs;
- a CRD is not established or a controller is not Ready;
- the positive/negative resource-template condition results are reversed;
- the Agent Sandbox resource is accepted but no Pod or condition evidence is
  produced;
- `hostUsers: false` is not preserved by the generated Pod;
- cleanup leaves an unexpected object or cannot prove the owned namespace is
  safe to remove.

Go to the existing full Argo Phase-0 fixture only after this gate passes. That
next gate still requires the local bridge image and an explicitly configured
artifact repository; it is the evidence for artifact digest/read-back and the
full marker/onExit contract, not something this preflight silently fakes.
