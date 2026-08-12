# Agents Gateway v3 operator chart

This is the Helm v3 deployment chart for the current `v3/cmd/agw-operator`
foundation. It installs the operator and its namespaced operating boundary; it
does not install the seven Agents Gateway CRDs, Agent Sandbox, a broker image, or
any runtime image.

`sandboxBackend` selects the execution child and defaults to `agent-sandbox`.
Use `sandboxBackend: job` for the built-in `batch/v1 Job` fallback; that mode
does not register a Sandbox watch and does not require the upstream
`agents.x-k8s.io/v1beta1` CRD/controller. The Job backend is intentionally
limited to the one-shot running shape emitted by the v3 workload planner.

## Placement boundary

Resolved runs may name a `RuntimeClass` or `StorageClass` only when the operator
deployment has explicitly listed that exact name in
`placement.allowedRuntimeClasses` or `placement.allowedStorageClasses`. Both
lists are empty by default, so caller-controlled placement is disabled until a
cluster operator reviews the class and its security/storage behavior. Wildcards
are not supported.

## Prerequisites

Install these before the chart:

1. A supported two-node k3s cluster with the Phase 0 user-namespace and
   network-airlock checks passing. The chart refuses to render the attestor
   until `preflight.airlockProven: true` is set from that real evidence.
2. When `sandboxBackend=agent-sandbox`, the upstream
   `kubernetes-sigs/agent-sandbox` **core** release v0.5.4. The chart
   deliberately does not fork, vendor, or install it. Install the pinned
   upstream asset from the v0.5.4 release, then verify the controller is ready.
3. The seven CRDs from this repository. They remain authoritative in
   `v3/config/crd`; this chart does not duplicate them because copied CRDs drift
   silently and Helm CRD upgrades have special lifecycle semantics.

From the repository root, the CRD install is:

```bash
kubectl apply -k v3/config/crd
```

For the primary backend, install Agent Sandbox from the upstream release
procedure and pin its asset digest in your deployment process. The expected API is
`agents.x-k8s.io/v1beta1`, release `v0.5.4`, core `sandbox.yaml`.

## Trusted phase supervisor status

The chart is deliberately fail-closed for a trusted Explore-to-Edit phase
supervisor. `phaseSupervisor.enabled` is fixed to `false` in the values schema;
attempting to set it to `true` fails Helm rendering. The current work Sandbox
keeps `shareProcessNamespace: false`, does not project a phase credential, and
does not claim process supervision. See
[`docs/v3/trusted-phase-supervisor-contract.md`](../../../docs/v3/trusted-phase-supervisor-contract.md)
for the prerequisites before this guard may be replaced.

## Work-pod agentgateway status

The top-level `agentGateway` value is an explicit seam for a future per-run
agentgateway sidecar and is disabled, with every nested value empty, by default.
Helm rejects `agentGateway.enabled: true` until the work-pod guard-to-agentgateway
adapter and its live capability evidence are proven. Do not add credentials,
host-network settings, or an arbitrary ConfigMap; when this seam eventually
opens, its image must be digest-pinned and its configuration must be an immutable,
credential-free per-run artifact. The operator flags are already wired through
`runplan.Config`, but the workload planner remains fail-closed and emits only the
agent and broker containers today.

The Argo backend is an operator-only submission path. The chart grants
`workflows.create` in `agw-runs` only to the operator ServiceAccount; do not
grant that verb to human users, run ServiceAccounts, or an Argo submitter. A
caller that can submit arbitrary Workflows can select the lifecycle template's
service accounts and parameters outside the `AgentRun` translator boundary.
If an external Argo Server or platform role adds Workflow submission, keep
`orchestrationBackend: direct` until an admission policy pins the expected
template digest, owner UID, service accounts, and parameter set.

## Optional Prometheus Operator metrics

The operator's existing webhook Service exposes its metrics on the named
`metrics` port (`/metrics`, HTTP). The chart can create a Prometheus Operator
`ServiceMonitor` for that endpoint, but it is disabled by default so the
default chart remains installable when the Prometheus Operator CRD is absent:

```yaml
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack # match the Prometheus serviceMonitorSelector
  interval: 30s
  scrapeTimeout: 10s
```

When enabled, the `ServiceMonitor` is created in `namespaces.system`, selects
only the chart's operator Service (`app.kubernetes.io/name`,
`app.kubernetes.io/instance`, and `app.kubernetes.io/component: controller`),
and targets only its `metrics` port at `/metrics`. The chart does not install
the `monitoring.coreos.com/v1` CRD; enable this option only after a compatible
Prometheus Operator is installed. `labels` are only for matching the
installed Prometheus resource's selector and do not alter the scrape target.

## Required production values

The schema requires all of the following, and the operator exits if the
fingerprints or object-store bucket are empty:

```yaml
image:
  repository: ghcr.io/astatide/agw-operator
  digest: sha256:<64 lowercase hexadecimal characters>

runtimeImages:
  clone:    { repository: ghcr.io/astatide/agw-clone,    digest: sha256:<64 hex> }
  skills:   { repository: ghcr.io/astatide/agw-skills,   digest: sha256:<64 hex> }
  context:  { repository: ghcr.io/astatide/agw-context,  digest: sha256:<64 hex> }
  lockdown: { repository: ghcr.io/astatide/agw-lockdown, digest: sha256:<64 hex> }
  broker:   { repository: ghcr.io/astatide/agw-broker,   digest: sha256:<64 hex> }
  capture:  { repository: ghcr.io/astatide/agw-capture,  digest: sha256:<64 hex> }
  verifyFetch: { repository: ghcr.io/astatide/agw-verify-fetch, digest: sha256:<64 hex> }
  verifyApply: { repository: ghcr.io/astatide/agw-verify-apply, digest: sha256:<64 hex> }
  preflight: { repository: ghcr.io/astatide/agw-preflight, digest: sha256:<64 hex> }

githubApp:
  existingSecret: gh-app-astatide

reportSigning:
  existingSecret: agw-report-signer

verificationAttestation:
  enabled: true
  cosignPath: /usr/local/bin/cosign
  keyRef: awskms://arn:aws:kms:us-east-1:<account-id>:key/<key-id>
  verifyKeyRef: "" # optional; use a public-key path for file-backed signing
  timeout: 3m

# Optional. A Gate that requests a critic fails closed while this is false.
critic:
  enabled: true
  image:
    repository: ghcr.io/astatide/agw-critic
    digest: sha256:<reviewed critic image digest>
  agentGateway:
    image:
      repository: cr.agentgateway.dev/agentgateway
      digest: sha256:efd79355b89094a8225a9db465d9a01dc656b377f0bab458761b935a13231d29
    capabilityVerified: true
    evidenceDigest: sha256:<local native-Messages/no-retry probe evidence>
  gateway:
    endpoint: http://agentgateway.agw-system.svc.cluster.local:8080
    routeRef: critic-anthropic
    serviceAccountName: agw-critic
    tokenExpirationSeconds: 900
    jwt:
      issuer: https://kubernetes.default.svc
      audience: agents-gateway-critic
      policyRef: critic-jwt
      verified: true
      evidenceDigest: sha256:<central route JWT rejection probe evidence>
    podSelector: { app.kubernetes.io/name: agentgateway }
  objectStoreEgressCIDRs: [<exact stable object-store CIDR>/32]
  timeout: 10m
  maxOutputBytes: 1048576

objectStore:
  bucket: <immutable-artifact-bucket>
  prefix: agents-gateway/v3
  region: us-east-1

# Required only when orchestrationBackend: argo. These are the exact stable
# destination CIDRs for the object-store endpoint; a public /0 is rejected.
argo:
  workflowTemplateName: agw-agent-run-lifecycle
  workflowTemplateUID: <live WorkflowTemplate UID>
  workflowTemplateDigest: sha256:<canonical spec digest>
  objectStoreEgressCIDRs: [<exact stable object-store CIDR>/32]

artifactSTS:
  roleARN: arn:aws:iam::<account-id>:role/agw-v3-artifacts
  credentialTTL: 1h

preflight:
  enabled: true
  airlockProven: true                 # only after live Phase-0 review
  nodeName: <selected agent node>
  nodeFingerprint: <sha256 fingerprint emitted by attestor>
  runtimeFingerprint: <sha256 fingerprint emitted by attestor>
  schedule: "*/2 * * * *"
  apiServerURL: https://kubernetes.default.svc:443
  apiServerCIDR: <kubernetes service IP>/32
  nodeSelector: { agw.astatide.com/agents: "true" }
  toleration: { key: agw.astatide.com/agents, operator: Equal, value: "true", effect: NoSchedule }
  hostPaths:
    kubelet: /var/lib/rancher/k3s/agent/kubelet
    runc: /var/lib/rancher/k3s/data/current/bin/runc
    containerd: /var/lib/rancher/k3s/data/current/bin/containerd
    k3s: /usr/local/bin/k3s
```

`skillsGateway.endpoint` is an operator-wide, credential-free HTTPS `/mcp`
URL. It may remain empty when no Agent uses skills; a run that declares skills
fails closed unless the endpoint is configured. Authentication is selected
separately through `skillsCredential` and projected only into the skills
initContainer.

Use a values file stored outside this repository for real fingerprints and
deployment-specific settings. The chart's `tests/values-ci.yaml` contains only
synthetic values and is not a production configuration.

The GitHub App Secret must be immutable and Opaque with exactly `app-id`,
`installation-id`, and `private-key.pem`. The operator reads it by exact name
through the uncached API reader. Its private key is never copied into the runs
namespace; the clone init container receives only a repository-scoped,
contents-read installation token.

The report-signing Secret must be an existing immutable Opaque Secret containing
exactly one data key, `private-key`, whose raw value is a 32-byte Ed25519 seed
or a 64-byte Ed25519 private key. Do not put the key in Helm values, a chart
fixture, or a command-line override; the chart only passes the Secret name.

The critic path sends no provider credential to `agw-runs`. Its input
materializer receives only the run's read-only object-store lease, and its local
agentgateway sidecar receives only a short-lived, audience-specific projected
ServiceAccount JWT. The long-lived central gateway in `agw-system` owns provider
credentials and must validate the configured issuer, audience, ServiceAccount,
and route before injecting them. `capabilityVerified` and `jwt.verified` are
explicit production attestations, not discovery flags: keep the critic disabled
until the corresponding evidence digests have been reviewed. Because Kubernetes
NetworkPolicy cannot select DNS names, `objectStoreEgressCIDRs` must list the
exact stable addresses of the configured HTTPS object-store endpoint.

`verificationAttestation` is a second, portable envelope around that existing
authenticated Gate report. Enabling it makes Gate completion fail closed until
cosign has created and immediately verified an in-toto bundle, after which the
statement and bundle are persisted as immutable run artifacts. `keyRef` must be
an explicit provider/KMS URI or an absolute externally managed path; private key
bytes never belong in Helm values. Relative paths, `env://`, and HTTP(S) key
sources are rejected. `verifyKeyRef` defaults to the signing reference for
KMS-backed keys and must be set to a separate absolute public-key path for
file-backed signing. The chart does not mount or create that path, so the
operator image/runtime must supply it through an independently reviewed
read-only mechanism. The CLI equivalent is `--verify-key-ref`.
Public Fulcio/keyless issuance is not assumed for a
self-managed k3s ServiceAccount issuer.

The artifact STS role is assumed by the operator to mint short-lived,
per-run, prefix-scoped object-store credentials. `artifactSTS.roleARN` is
required, `credentialTTL` must be between 15 minutes and 1 hour, and
`artifactSTS.endpoint`—when set—must be an HTTPS endpoint. Configure the role
trust and identity policies outside Helm. `externalID` is optional and is
passed only when configured.

If static AWS credentials are needed, create an existing Secret in the system
namespace and set:

```yaml
objectStore:
  credentials:
    existingSecret: agw-objectstore-credentials
```

The chart references the Secret keys but never creates or commits credential
material. The default keys are `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
and `AWS_SESSION_TOKEN`. An external-secrets controller or an equivalent
workload-identity mechanism can provision the Secret before the Deployment is
started.

## Retention safety configuration

The chart installs a bounded retention inventory/applier in the operator. It
is enabled as a read-only dry-run by default:

```yaml
retention:
  enabled: true
  dryRun: true
  enforce: false
  lifecycleAttested: false
  interval: 15m
  artifactRetentionDays: 14
  workspaceRetentionDays: 7
  # Terminal effect pairs are never retired before 30 days.
  ledgerRetentionDays: 30
  ledgerPrefix: publish-effects
  maxActions: 256
  maxRuns: 256
  maxResources: 1024
  maxObjects: 4096
  maxLedgerBodyBytes: 65536
  maxEvents: 32
```

Every cycle requires a complete, bounded inventory. It only considers AGW
resources fenced by owner/UID/labels/spec digest and objects under the exact
per-run prefix. Effect-ledger retirement is separate: the operator reads
bounded canonical claim/outcome bodies, requires a terminal successful/failed
outcome, proves run/effect/request identity, and plans the claim and outcome as
one pair. A permanent tombstone is conditionally created before
outcome-then-claim deletion, so a partial delete cannot reopen an external
effect. `UnknownEffect`, pending/incomplete/malformed/mismatched records, and
ambiguous writes remain protected. Kubernetes deletes use UID preconditions;
object-store deletes use ETags; missing targets are idempotent and finalizers
are never modified.

Enforcement is intentionally explicit and requires all three settings:

```yaml
retention:
  dryRun: false
  enforce: true
  lifecycleAttested: true
```

Do not enable that combination until the real Phase-0 Sandbox/PVC lifecycle
probe and object-store conditional-delete proof have been recorded. The
operator includes a disabled-until-attested guard, so an unproven lifecycle
cannot silently turn a chart upgrade into deletion.

## Install and upgrade

The chart can create both namespaces. The release namespace is normally the
system namespace:

```bash
helm upgrade --install agw-operator ./v3/charts/agw-operator \
  --namespace agw-system --create-namespace \
  --values /secure/path/agw-operator-values.yaml
helm test agw-operator --namespace agw-system
```

`namespaces.system` and `namespaces.runs` are independent values. They must be
different. The operator watches only the configured runs namespace and uses the
system namespace for leader election, the preflight ConfigMap, the operator
Secret references, and the webhook Service.

The chart passes `namespaces.system` to the operator for named preflight and
credential reads, so custom system namespaces retain fail-closed admission
semantics.

## Production preflight attestor

When enabled, the chart creates the fixed `agw-preflight` ConfigMap, a
dedicated ServiceAccount/Role/RoleBinding, a recurring CronJob, and a
post-install/post-upgrade startup Job. Both Jobs run on the selected agent
node and have a 120-second Kubernetes deadline, no retries, a 16Mi in-memory
evidence volume, and `concurrencyPolicy: Forbid` for the CronJob.

The pod uses `hostUsers: false` and one init container with only namespaced
`NET_ADMIN` to install the same IPv4/IPv6 owner rules used by work Sandboxes.
`lockdown` is the sole init container; the UID-1337 attestor and UID-1000
credential-free agent probe are regular containers and start concurrently after
the airlock is installed. The shared in-memory evidence volume is bounded to
16Mi and writable through `fsGroup: 1337` plus the pod supplemental group,
while the API token volume is mounted only into the attestor.
The UID-1337 writer receives one explicitly projected, 10-minute Kubernetes
token solely to update the named ConfigMap. The UID-1000 probe receives no
ServiceAccount token, Secret, host mount, or ambient credential. It must fail
DNS and TCP access to the API service while UID 1337 resolves the service and
the writer successfully updates the ConfigMap. The writer also checks the
non-host UID/GID maps, host namespace identity, kubelet filesystem family,
kernel, and the selected k3s/containerd/runc version fingerprints.

The attestor first writes a non-passing `result.json`. It publishes a passing
result only after every check passes and the final ConfigMap update succeeds.
Missing host binaries, missing maps, a missing lockdown marker, an unavailable
API, a missing agent report, a fingerprint mismatch, or any failed probe leave
admission fail-closed. The attestor has no permission to list Secrets or
ConfigMaps and cannot recreate the fixed ConfigMap if it is deliberately
deleted; Helm owns that object so tamper or deletion fails closed until repair.

The ConfigMap is refreshed on chart install/upgrade and on the CronJob schedule.
The schedule is the node-change convergence mechanism for a selected node; it
does not claim to attest every agent node. If more than one node can run
AgentRuns, select and fingerprint a node pool only after proving that every
eligible node has the same runtime contract, or extend the design to a
per-node evidence set before enabling that topology.

The hostPath mounts are intentionally retained for node/runtime checks. The
host `/proc` mount is checked separately as `procfs` because it is used for
namespace identity and kernel inspection; it is not sent through the volume
filesystem-family allowlist. The kubelet root and runtime-binary hostPaths
receive a conservative ext4/xfs/btrfs/overlayfs family precheck and fail
closed for an unknown family. This filesystem-type check does not prove that
an idmapped mount will work; kubelet can still reject the Pod if the actual
mount cannot apply an idmap. `preflight.hostPathIdmapRequired` is a
non-disableable chart safety gate. See the primary
[Kubernetes user namespace filesystem documentation](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/#filesystem-support)
and prove this on the target kernel/runtime before enabling the chart.

Live-only assertions remain intentionally unresolved in this repository:
`hostUsers:false` behavior, namespaced `NET_ADMIN`, xt_owner enforcement,
hostPath readability and idmapped-mount support, k3s service CIDR, DNS labels,
and the exact runtime binary paths must be proven on the real
kernel/containerd/k3s cluster. Synthetic Helm values and local image
inspection do not constitute that proof.

The chart creates namespaced Roles and RoleBindings equivalent to
`v3/config/rbac/role.yaml`. It intentionally creates no ClusterRole, no
Ingress, and no public Service type. The webhook is reachable only through its
ClusterIP Service from the Kubernetes API server.

When `orchestrationBackend: argo` is enabled, the lifecycle prepare step can
only read the admitted AgentRun, the bound Workflow, and the operator-owned
Sandbox. It cannot create a Sandbox or access run-scoped Secrets. The system
Secret Role is an explicit `resourceNames` allowlist. The later stage and
handoff pods receive only mounted, per-run object-store credentials; cleanup
retains only the namespace-scoped Secret get/delete capability required to
remove that deterministic work credential after validating its owner, run UID,
and spec digest. Kubernetes RBAC cannot express a wildcard resource name for a
generated run Secret, so the cleanup binary's identity and UID-precondition
checks remain part of the security boundary.

## Webhook certificate lifecycle

On the first install, Helm generates a self-signed CA and serving certificate
with `genCA`/`genSignedCert`. The generated Secret contains `ca.crt`, `tls.crt`,
and `tls.key`; the Deployment mounts the latter two at controller-runtime's
default serving path:

`/tmp/k8s-webhook-server/serving-certs`

The `ValidatingWebhookConfiguration` receives the real base64-encoded `ca.crt`
from the same material. On upgrades, `lookup` reads the existing Secret and
reuses it, so a normal upgrade does not rotate the trust anchor. The Secret is
kept on uninstall by default to avoid accidental CA rotation during a
reinstall. Treat it as private cluster state; it is never rendered into a
committed file.

Client-side `helm template` cannot query a cluster, so it generates ephemeral
certificate material for that render. Use `--dry-run=server` when testing
upgrade reuse against a live cluster. A pre-existing TLS Secret must contain all
three required data keys; otherwise rendering fails closed.

The webhook uses `failurePolicy: Fail` and is scoped by the automatic
`kubernetes.io/metadata.name` label to `namespaces.runs`.

## Isolation and capacity defaults

The chart enables the runs namespace baseline from `v3/config/networkpolicy`:
default-deny ingress/egress, DNS only to kube-dns, and public TCP/443 only for
pods labelled `agents.astatide.com/egress: broker-public` or
`agents.astatide.com/egress: verify-fetch-public`. The latter is used only by
the ordered fetch init container; verify execution begins after its lockdown
init container has installed an unconditional egress drop. The pod-local UID
airlock remains the responsibility of each Sandbox workload. The operator
namespace deliberately has no default-deny policy because a policy that cannot
identify the k3s API-server source can break webhook admission.

The Argo lifecycle is narrower. `prepare`, `wait`, and `cleanup` are allowed
only to the configured Kubernetes API CIDR and port. `stage` and `handoff` may
use TCP/443 only to `argo.objectStoreEgressCIDRs`; the chart fails closed when
Argo is enabled without at least one CIDR and rejects an unrestricted `/0`.
Keep this list to the stable addresses of the selected object-store endpoint,
or use a private endpoint/egress gateway if the provider's addresses are
dynamic. The chart never falls back to a broad public rule for these phases.

ResourceQuotas and LimitRanges are installed in both namespaces. A single
operator replica and leader election are the defaults. The default PDB requires
that one operator remain available, which intentionally blocks voluntary node
drain of the only replica; increase replicas only after the control-plane and
webhook behavior has been tested with the chosen image.

The process timezone is UTC so machine timestamps remain UTC. The
`AGW_HUMAN_TIMEZONE` setting is `America/New_York` for human-facing formatting;
it does not change Kubernetes status timestamps or artifact timestamps.

## Validation

Run the chart-local validator:

```bash
bash v3/charts/agw-operator/tests/validate.sh
```

When Helm is installed it runs `helm lint` and `helm template` using synthetic
values and checks the rendered webhook, certificate, RBAC, quota, and network
policy objects. Without Helm it performs dependency-free static checks and
validates the JSON schema with Python's standard library. The health Helm test
uses a configurable curl helper image; pin its digest in a production values
file if the test is enabled in a restricted registry.
