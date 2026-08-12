# Local Kubernetes API smoke

`v3/scripts/local-api-smoke.sh` is the opt-in, disposable API/schema smoke for
Agents Gateway v3. It is intentionally separate from `validate-local.sh`, so
ordinary local validation never creates a cluster or contacts a Kubernetes API.

The complete mode:

- resolves local `kind`, `kubectl`, `helm`, and `python3` tools and fails with a
  direct message when one is missing;
- creates one uniquely named Kind cluster with a private temporary kubeconfig;
- validates the complete cluster name as a lowercase DNS label before using it;
- refuses an existing exact cluster-name collision; a deterministic
  `AGW_API_SMOKE_CLUSTER_NAME=agw-v3-api-*` override exists for local safety
  regression tests only;
- applies the seven AGW CRDs server-side and waits for `Established`;
- reads a local Argo Workflows v4.1.0 full-install or CRD-only manifest, extracts
  only its eight official CRD documents, applies them server-side, and waits for
  `Established`;
- establishes `agw-system` and `agw-runs`;
- server-side dry-runs the AGW samples and both direct and Argo Helm output;
- creates the samples in the disposable cluster and proves that an admitted
  `AgentRun` execution-spec mutation is rejected by the CRD validation;
- sets cancellation once and proves the API rejects clearing it again;
- deletes the exact generated cluster through an EXIT trap after a successful
  create, and removes the temporary kubeconfig on every exit. A failed create
  is not deleted because its ownership is ambiguous.

The script never accepts a remote Argo URL and never calls GitHub, `gh`, an image
registry, an Argo submission, Docker/Podman directly, or a prune command. Kind
may use the locally configured container engine internally to create its node;
the script does not inspect, alter, or clean that engine's other state.

## Prerequisites

Install or place these tools locally:

```text
kind
kubectl
helm
python3
```

The complete check also needs a locally stored, pinned Argo Workflows v4.1.0
manifest. It may be the CRD-only bundle or a larger official installation
manifest; the script applies only these eight CRDs:

```text
clusterworkflowtemplates.argoproj.io
cronworkflows.argoproj.io
workflowartifactgctasks.argoproj.io
workfloweventbindings.argoproj.io
workflows.argoproj.io
workflowtaskresults.argoproj.io
workflowtasksets.argoproj.io
workflowtemplates.argoproj.io
```

Keep that artifact under your own local/reviewed supply-chain process. The smoke
script deliberately does not download or verify a remote URL.

## Run

From the repository root, run the complete smoke with a local Argo bundle:

```bash
AGW_ARGO_CRDS_FILE=/path/to/argo-workflows-v4.1.0-crds.yaml \
  ./v3/scripts/local-api-smoke.sh
```

The same option can be passed explicitly:

```bash
./v3/scripts/local-api-smoke.sh \
  --argo-crds-file /path/to/argo-workflows-v4.1.0-crds.yaml
```

If the local tools are outside `PATH`, provide their exact paths:

```bash
AGW_KIND_BIN=/path/to/kind \
AGW_KUBECTL_BIN=/path/to/kubectl \
AGW_HELM_BIN=/path/to/helm \
AGW_ARGO_CRDS_FILE=/path/to/argo-workflows-v4.1.0-crds.yaml \
  ./v3/scripts/local-api-smoke.sh
```

For a reduced AGW-only smoke when an Argo bundle is intentionally unavailable:

```bash
./v3/scripts/local-api-smoke.sh --skip-argo
```

That mode does not prove the Argo CRD or Argo Helm contract. A successful run
ends with `local-api-smoke: PASS`; the final cleanup line names the exact Kind
cluster deleted by the trap.

The focused fake-tool regression suite is also local-only:

```bash
bash ./v3/scripts/local-api-smoke.test.sh
```

It proves collision refusal, invalid-name refusal before invoking Kind, and that
a failed create does not trigger deletion of a possibly pre-existing cluster.
It never creates a cluster.

## What this proves

This is an API/schema and admission smoke. It proves the local Kubernetes API
accepts the CRDs, sample resources, immutable-spec rule, and rendered chart
objects. It does not prove k3s node isolation, CNI enforcement, Agent Sandbox
controller behavior, a running operator, a Workflow execution, object storage,
provider access, or GitHub publishing. Those remain separate live Phase-0 and
integration gates.
