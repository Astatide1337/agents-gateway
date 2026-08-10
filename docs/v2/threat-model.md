# Agents Gateway v2 threat model

Status: implementation baseline for `agents.astatide.com/v1alpha1`.

## Who this product is for

Agents Gateway is software that an operator installs on infrastructure they
own or administer. The default standalone profile is not a public code
execution service and does not accept anonymous tenants. The operator decides
who can submit runs and which repositories, skills, tools, and model accounts
those runs may use.

That does **not** make the agent workload trusted. Model-generated commands,
checked-out repositories, dependency install scripts, skills, and tool output
may all be malicious or simply wrong. The primary default security boundary is
therefore:

```text
trusted operator/control plane -> runner -> untrusted per-run sandbox
```

The optional distributed profile adds remote runners and multiple teams, but
does not silently turn a self-hosted installation into a safe hostile public
multi-tenant compute service.

## Default standalone boundary

The default runner uses rootless Podman on an ordinary Linux host. Every run
gets a fresh rootless container, a read-only root filesystem, a non-root user,
all capabilities dropped, `no-new-privileges`, cgroup limits, PID limits,
network disabled by default, and only the documented per-run mounts. Neither
the control plane nor a sandbox receives a Docker, Podman, or containerd
socket. Upstream model and tool credentials stay outside the sandbox.

Rootless Podman is shared-kernel isolation. It is an appropriate default for
an owner-operated installation where the workload is untrusted but submitters
are trusted. It is not equivalent to a microVM and must be reported as
`standard/shared-kernel` isolation.

## Optional stronger boundaries

- gVisor (`containerd-runsc`) is an opt-in enhanced-isolation runner for
  operators whose host supports it.
- A future Firecracker/Kata backend may provide `strong/microvm` isolation.
- Remote runners use mTLS, leases, and fencing tokens and are an optional
  scaling/fault-domain feature.
- OIDC, organizations/projects, PostgreSQL RLS, quotas, and distinct-person
  approvals are optional team/enterprise controls. Local authentication and a
  single administrative scope are sufficient for the standalone profile.

Agents Gateway must not advertise hostile public multi-tenant execution until
a supported microVM boundary and the corresponding adversarial test suite are
available. Operators who intentionally expose run submission to untrusted
people must select and configure the stronger profile themselves.

## Assets

- Model, MCP, Git, OIDC, service-account, and subscription credentials.
- Definitions, prompts, skills, tools, policies, and private source code.
- Workflow state, approvals, artifacts, logs, traces, and audit records.
- External side effects such as commits, pull requests, deployments, messages,
  and database writes.
- Host availability and integrity.

## Threats and controls

| Threat | Baseline control | Optional stronger control |
|---|---|---|
| Workload reads the host or another run | Rootless user namespace, per-run workspace, strict mount allowlist, read-only root, no runtime socket | gVisor or microVM runner; dedicated runner host |
| Workload escapes through the network | Network namespace disabled by default; no direct internet | Firewall-enforced broker-only egress |
| Credential theft | No upstream credentials in sandbox; short-lived references; redaction | Envelope encryption and external secret manager |
| Tool permission bypass | Exact ToolSet policy at the broker; skills and MCP annotations never grant authority | Connector-specific resource policy and approvals |
| Duplicate external write | Idempotency/effect ledger; unknown effects require reconciliation | Provider-native idempotency and distinct-person approval |
| Malicious repository or skill | Digest-pinned inputs, read-only materialization, bounded extraction | Signatures, SBOM, and admission policy |
| Lost work after restart | Durable local queue/state in PostgreSQL | Temporal engine and remote runner failover |
| Unauthorized API use | Private-by-default endpoint and local token | OIDC, scoped service accounts, RLS, quotas |
| Compromised runner host | Documented infrastructure incident boundary | Dedicated host and microVM isolation |

## Explicit non-guarantees

No security claim is made against root compromise of the runner host or
control-plane database. Rootless containers reduce privileges; they do not
make a shared Linux kernel equivalent to a VM. The default profile is not a
safe way to sell arbitrary code execution to strangers.

## Release tests

The standalone release gate exercises environment and filesystem credential
discovery, `/proc` visibility, runtime-socket access, host-mount attempts,
cross-run access, symlink traversal, fork and memory exhaustion, oversized
output, cancellation, restart recovery, and duplicate-write handling.

The distributed profile additionally tests forged certificates, stale leases,
cross-project authorization, RLS, broker bypass, and runner reassignment. A
feature is documented as optional or unavailable until its enforcement and
failure-path tests pass.
