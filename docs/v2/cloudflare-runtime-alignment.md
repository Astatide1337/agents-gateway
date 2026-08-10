# Cloudflare runtime alignment

Status: optional reference architecture and implementation guidance
Last reviewed: 2026-08-08

Agents Gateway borrows useful runtime boundaries demonstrated by Cloudflare
Agents and Sandbox without depending on Cloudflare services. This document
does not define the standalone installation baseline.

## Verified upstream pattern

Re-checked against Cloudflare's official documentation on 2026-08-09:

- Cloudflare runs each hostile sandbox in its own VM and enforces separate
  filesystems, processes, networks, and resource quotas. Agents Gateway's
  rootless Podman profile is intentionally weaker shared-kernel isolation;
  gVisor and future microVM profiles must remain distinguishable capabilities.
- Cloudflare supports running a coding harness such as Claude Code inside the
  sandbox image. That validates Agents Gateway's adapter/harness placement;
  it does not justify launching the coding CLI on the control-plane host.
- Cloudflare's recommended credential pattern gives the sandbox a short-lived
  scoped token, sends calls through an out-of-sandbox proxy, and injects the
  real upstream credential there. API clients are pointed at that proxy with
  a configurable base URL. This is the exact portable pattern required for
  Agents Gateway's model, MCP, Git, and artifact brokers.
- Cloudflare also documents direct environment-secret injection as a simpler
  but weaker option. Agents Gateway may expose that only as an explicit
  owner-operated compatibility mode whose credential exposure is recorded;
  it is not the secure default.

Primary references:

- <https://developers.cloudflare.com/sandbox/concepts/security/>
- <https://developers.cloudflare.com/sandbox/guides/proxy-requests/>
- <https://developers.cloudflare.com/sandbox/tutorials/claude-code/>
- <https://developers.cloudflare.com/agents/tools/sandbox/>

## Harness placement

Two execution modes are valid and intentionally distinct:

1. A durable control-plane harness may own conversation state, model turns,
   scheduling, and recovery while delegating untrusted code execution to a
   sandbox.
2. A coding harness such as Codex, Claude Code, or OpenCode runs completely
   inside a per-run sandbox. Its image entrypoint is an Agents Gateway adapter
   which translates between the harness protocol and `agw.runtime.v1`.

The second mode is the primary v1alpha1 coding-agent path. V2 must not launch
coding harnesses through host tmux sessions. The adapter, harness binary,
workspace, and any owner-bound ephemeral CLI credential bundle share the
sandbox security boundary. PostgreSQL, Temporal, authorization policy, MCP
credentials, provider credentials, audit records, and durable artifacts stay
outside it.

```text
API / CLI
   |
   v
agw-server -> selected run engine
                           |
                         mTLS
                           |
                           v
                      agw-runner
                           |
                 one sandbox per run
                 +------------------+
                 | runtime adapter  |
                 | coding harness   |
                 | workspace        |
                 | read-only skills |
                 +------------------+
                           |
                  deny-by-default egress
                           |
                 model / MCP / artifact brokers
```

## Isolation classes

- Rootless Podman is the owner-operated standalone baseline and reports
  standard/shared-kernel isolation.
- containerd with gVisor is an optional enhanced-isolation/shared-runner profile.
- A per-run Firecracker or Kata microVM is required before Agents Gateway may
  advertise hostile public multi-tenant execution comparable to Cloudflare's
  VM-backed sandbox boundary.

The control plane remains portable across all three classes. Isolation is a
runner capability and an immutable fact recorded for each run.

## Networking and credentials

`none` is the only executable network mode until the broker transport and
host-enforced firewall exist. `brokered` remains a declarative schema value,
but runners reject it rather than silently degrading it to another mode.

When brokered networking ships, the sandbox receives only a short-lived,
run-scoped identity. Host-side brokers validate organization, project, user,
run, agent revision, exact capability, resource constraints, effect class,
budget, and approval before injecting an upstream credential. A general MCP,
Skills Gateway, source-control, object-store, or model-provider credential is
never placed in a normal sandbox.

Subscription CLI credentials are the explicit exception: if a provider's
official CLI cannot use an out-of-process broker, an owner-bound encrypted
bundle may be materialized into only that owner's ephemeral sandbox. The run
records that weaker credential boundary and destroys the bundle during fenced
cleanup.

## Skills boundary

Skills are data, not authority. Selected skills are materialized before
sandbox start and mounted at exactly `/skills` read-only. Their metadata may
request tools but cannot grant them.

The current schema does not yet distinguish:

- an upstream Git commit or OCI manifest digest;
- the digest of a fetched package blob; and
- a canonical digest of the exported skill contents.

Production materialization stays fail-closed until that distinction is made
explicit. The first connected format should use a deterministic skill bundle
with a content digest, while separately recording its source repository,
source revision, and exported path. Arbitrary runner-side URL fetching is not
an acceptable substitute.

## Evidence required

The runtime is not complete until automated evidence proves:

- a real adapter invokes a real harness binary inside the sandbox;
- no host credential, runtime socket, process, or adjacent workspace is
  visible;
- DNS, metadata, direct-IP, and proxy-bypass egress fail;
- approved broker calls succeed without revealing upstream credentials;
- skills are digest-verified and read-only;
- output is persisted to an immutable artifact before sandbox destruction;
- success, failure, timeout, cancellation, and runner restart all clean up;
- a single trace correlates the control plane, workflow, runner, sandbox,
  model, tool, approval, verification, and artifact operations.
