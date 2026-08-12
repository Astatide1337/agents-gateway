# Agents Gateway v3 Phase-0 runbook

`v3/scripts/phase0-run.sh` is a safe wrapper around the six individual Phase-0
probes. It is intended for a separately provisioned, disposable two-node k3s
context. It is not a cluster provisioner and it does not install k3s, Argo,
Agent Sandbox, or an object store.

The wrapper is plan-by-default. It never changes the caller's kubeconfig
context, accepts no ambient context for a live action, and never deletes a
namespace. Every live resource is labelled with one explicit run ID; cleanup
delegates to the existing probes' exact-label deletion paths.

## Before applying anything

Use a dedicated kubeconfig and verify the exact context out of band:

```bash
kubectl --kubeconfig /path/to/disposable.kubeconfig config get-contexts
kubectl --kubeconfig /path/to/disposable.kubeconfig --context disposable-k3s \
  get nodes -o wide
```

The target should have one control-plane node and one tainted/labelled agent
node. The inventory probe records the kernel, container runtime, kubelet root,
filesystem, and node labels. Do not use a production or home-lab context.

The six probes have different prerequisites:

- inventory: read-only host-root probe and the pinned inventory image;
- user namespaces: `hostUsers: false` support and a compatible runtime;
- airlock: CNI NetworkPolicy enforcement, namespaced `NET_ADMIN`, and the
  pinned network/lockdown images;
- credentials: a disposable pod and synthetic canaries only;
- Sandbox: Agent Sandbox v0.5.4, a StorageClass, and enough disposable disk;
- object store: explicit `PHASE0_OBJECTSTORE_*` inputs and test-only S3
  permissions. The probe never invents a bucket or reads a production secret.

## Plan first

```bash
cd /home/ubuntu/Projects/agents-gateway
run_id="phase0-$(date -u +%Y%m%d%H%M%S)"
bash v3/scripts/phase0-run.sh \
  --plan \
  --run-id "$run_id" \
  --namespace agw-phase0 \
  --output-dir /secure/path/agw-phase0-evidence
```

Inspect the rendered manifests and the per-probe evidence directories before
using a cluster. The evidence root is mode 0700 and must not be committed.

## Live disposable run

Use a unique run ID and an explicit disposable context. `--apply --yes` is the
only mode that creates resources:

```bash
export KUBECONFIG=/path/to/disposable.kubeconfig
export PHASE0_OBJECTSTORE_ENDPOINT=https://s3.example.invalid
export PHASE0_OBJECTSTORE_BUCKET=agw-phase0-only
export PHASE0_OBJECTSTORE_PREFIX=phase0/$(date -u +%Y%m%d)/my-run

bash v3/scripts/phase0-run.sh \
  --apply --yes \
  --context disposable-k3s \
  --run-id "$run_id" \
  --namespace agw-phase0 \
  --output-dir /secure/path/agw-phase0-evidence
```

The object-store values above are placeholders. Replace them only with a
dedicated test prefix and least-privilege credentials. Never paste credentials
into a command argument, manifest, or evidence directory.

The wrapper continues through every probe so the evidence records all stop/go
signals. Exit codes are:

- `0`: all executed probes passed or were non-destructive skips;
- `3`: the run is inconclusive because a probe skipped a required live check;
- `1`: at least one probe failed.

`sandbox --install-upstream` is not implied. If used, it still requires the
script's pinned local manifest and SHA-256 inputs; do not substitute a floating
URL or tag.

## Cleanup

Review the exact run ID and namespace marker first. Cleanup requires the same
namespace and evidence root used by the apply run:

```bash
bash v3/scripts/phase0-run.sh \
  --cleanup --yes \
  --context disposable-k3s \
  --run-id "$run_id" \
  --namespace agw-phase0 \
  --output-dir /secure/path/agw-phase0-evidence
```

The wrapper refuses a namespace mismatch, missing marker, missing prior
evidence directory, or absent explicit context. It does not remove the
namespace, unrelated objects, the Agent Sandbox installation, or the object
store bucket. Object-store cleanup remains subject to that probe's ownership
and exact-prefix checks.

## Evidence and stop/go

The wrapper records `orchestrator.lock`, `summary.tsv`, and a separate child
directory per probe. Individual probe evidence is sanitized before it is
written where the probe supports sanitization. Review `results.tsv` and
`metadata.txt` for each child; a plan or nested Kind result is not live
isolation evidence.

Do not proceed to production adoption unless the real target k3s run proves,
on the exact kernel/runtime/CNI combination:

- user namespaces and distinct UID/GID maps;
- the UID-owned airlock for IPv4, IPv6, API, metadata, DNS, Service, and pod
  bypasses;
- setup-only and broker-only credential visibility;
- Agent Sandbox readiness, suspend/resume, shutdown, PVC lifecycle, and
  cleanup; and
- object-store conditional-write, read-back, ambiguity, and retention behavior.

The disposable Kind probe in `docs/v3/completion-audit.md` proves API/schema
contracts only. Its nested runtime failure is explicitly not evidence for any
of the isolation properties above.
