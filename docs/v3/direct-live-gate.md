# Direct lifecycle live gate

Status: disposable, provider-free e2e contract test

This gate is intentionally separate from the v3 Go implementation. It adds only
fixtures and a test harness under `v3/test/e2e/direct/`; it does not edit the
controller, backends, image build scripts, CRDs, or production manifests.

## What was inspected

The fixture is shaped from the current direct path:

- `internal/controller/agentrun.go` owns the domain lifecycle and projects
  `Pending`, `Working`, `Capturing`, `Verifying`, and terminal status.
- `internal/runplan` resolves the per-run plan and its owner-bound projections.
- `internal/workload` builds the hardened work topology and direct airlock.
- `internal/sandbox` contains the Agent Sandbox backend and the Job/PVC fallback.
- Existing `v3/test/e2e/phase0/` fixtures cover node-floor and user-namespace
  assumptions; they are not a complete AgentRun lifecycle test.

The direct gate therefore checks the boundaries that can be proven without
provider credentials: API admission, status-subresource persistence, work to
capture handoff, fresh verification input, exact failure evidence, and
owner-reference cleanup.

## Files

- `v3/test/e2e/direct/agent-run-fixture.yaml` — the schema fixture. It uses
  mock, non-runnable image digests and an offline model/tool route, so applying it
  cannot call a model provider.
- `v3/test/e2e/direct/contract-resources.yaml` — ordinary Jobs, PVCs, and a
  ConfigMap that model the work, capture, and verify boundaries with BusyBox.
- `v3/test/e2e/direct/failure-child.yaml` — a deterministic verifier failure
  returning exit code 42 and one exact JSON evidence line.
- `v3/test/e2e/direct/live-gate.sh` — plan parser and bounded live runner.

## What the live run proves

The success case creates a unique `agw-direct-success-*` namespace and:

1. Applies the Agent, Gate, ToolSet, ModelRoute, Policy, ContextStrategy, and
   AgentRun fixture; reads the API-assigned UID.
2. Patches only the AgentRun status subresource to prove admission and status
   storage without pretending that a mock image completed a real run.
3. Runs a work Job that writes a clean base marker and a change marker to the
   work PVC.
4. Runs capture against that work PVC, writes an immutable patch marker, and
   copies only the captured patch into a separate verify PVC.
5. Deletes the exact work Job and work PVC after capture, then confirms they are
   gone before verification proceeds.
6. Confirms the verify Job mounts `direct-verify-workspace` and does not mount
   `direct-workspace`, then checks the accepted report includes the run UID,
   spec digest, base SHA, and `fresh-verify-volume` evidence marker.
7. Projects `Succeeded` with patch, gate, verifier, and condition references.
8. Deletes only the exact AgentRun in the generated namespace and waits for its
   owner-referenced ConfigMap, PVC, and Jobs to be garbage-collected.

The failure case creates a separate `agw-direct-failure-*` namespace and checks
that the verifier emits exactly:

```json
{"code":"VerificationCommandFailed","message":"offline verifier rejected the exact patch","retryable":false,"phase":"Verifying","runUid":"<admitted UID>","specDigest":"<failure digest>","baseSHA":"<base SHA>"}
```

It then persists `Failed` status with the same code, message, retryability,
spec digest, and base SHA before deleting only that exact AgentRun and checking
owner-reference garbage collection.

Every child has both the `agents.astatide.com/e2e=direct-contract` label and an
owner reference to the exact AgentRun UID. No label-wide or broad cluster-wide
delete is used. After the exact child checks, the harness deletes only the two
generated namespaces by default. Set `AGW_DIRECT_KEEP=1` to retain those exact
namespaces for inspection. It never deletes CRDs, Argo resources, the Agent
Sandbox controller, or any resource outside the generated namespaces.

The same inspection opt-in is available as `--keep`. The namespace prefix is
validated before any fixture interpolation: it must be `agw-direct` or begin
with `agw-direct-`, and both generated success/failure names must satisfy the
Kubernetes lowercase DNS-label rule and stay within 63 characters.

## Run it safely on the disposable k3s cluster

The live mode requires an already-installed, Established set of v3 CRDs. It
does not install, update, or delete cluster-scoped resources. It also refuses a
non-loopback API endpoint and validates the complete generated namespace names
as lowercase DNS labels before any fixture interpolation.

Provide the kubeconfig and context for a currently running disposable k3s API;
do not reuse a production context or a stale container name. For example:

```bash
ctx=<disposable-loopback-context>
kubeconfig=/secure/path/disposable-k3s.kubeconfig
KUBECONFIG="$kubeconfig" PATH="/home/ubuntu/.local/bin:$PATH" \
  bash v3/test/e2e/direct/live-gate.sh \
    --live --context "$ctx" --kubeconfig "$kubeconfig" --timeout 90

# Optional: retain and inspect only the exact namespaces by setting this first.
# AGW_DIRECT_KEEP=1 bash v3/test/e2e/direct/live-gate.sh \
#   --live --context "$ctx" --kubeconfig "$kubeconfig"
```

The repository's `v3/scripts/local-k3s-smoke.sh` is the supported disposable
two-node harness for Phase-0 probes. It tears the cluster down before returning;
run this direct gate inside an equivalent explicitly managed disposable cluster
when live direct-gate evidence is required.

The script deletes the generated namespaces on success or failure. Cluster-level
teardown, including k3s container cleanup, belongs to the main lifecycle owner.
When `AGW_DIRECT_KEEP=1` is used, inspect only the exact reported names, for
example:

```bash
kubectl --kubeconfig /path/to/k3s-config --context default \
  get jobs,pvc,configmap,agentruns -n <reported-namespace> \
  -l agents.astatide.com/e2e=direct-contract
```

Do not turn the inspection command into a broad `delete`, `prune`, or CRD
cleanup command.

## Why this is not a full operator workload

The current direct production path has no supported offline/mock execution
mode. A real run needs the digest-pinned runtime and verifier images, clone and
skills/context images, the direct broker/airlock topology, preflight-approved
node capabilities, artifact storage credentials, and the configured model and
MCP provider seams. The clone plan also expects a real source repository and
GitHub App-backed credentials. Building or pushing those images, or injecting
provider credentials into this test, would make the test less safe and would
turn it into a deployment rather than a bounded e2e check.

The BusyBox workload is therefore a precise live fallback: it proves Kubernetes
resource and evidence contracts, not model quality, sandbox isolation, GitHub
publishing, or the actual harness adapter. The provider-free production-builder
contract now lives in `v3/test/e2e/full-workload/`; it exercises the real
builders/controllers in memory but still does not execute production images.

## Focused checks

These checks are local and do not invoke CI, push images, call providers, or
touch production:

```bash
bash -n v3/test/e2e/direct/live-gate.sh
bash v3/test/e2e/direct/live-gate.sh --plan
git diff --check -- v3/test/e2e/direct docs/v3/direct-live-gate.md
```

The live result should end with `LIVE PASS (provider-free contract only)`. With
`AGW_DIRECT_KEEP=1`, it also reports the retained namespaces for inspection.
