# Trusted phase supervisor deployment contract

Status: **fail-closed and not wired**

This document records the deployment-side boundary for authorizing the
`Explore -> Edit` ToolSet transition and for observing the harness process. It
does not change `v3/internal/broker`; the current release must not claim that a
trusted phase supervisor exists.

## Audit result

The current work Sandbox cannot safely host such a supervisor:

| Question | Current evidence | Result |
|---|---|---|
| Can a sidecar observe the agent process? | `workload.go` renders `shareProcessNamespace: false`. Each container has its own PID namespace. | **No** |
| Is the runtime event stream independent process evidence? | The agent posts `agw.runtime.v1` events to the loopback broker. Those events are harness-authored. | **No**; events are useful telemetry, not an independent exit observation |
| Can the lifecycle image authorize `Explore -> Edit`? | `agw-agent-run-lifecycle` only implements bounded Argo `prepare`, `stage`, `wait`, `handoff`, and `cleanup` modes. Its `wait` ServiceAccount observes an Agent Sandbox from a separate lifecycle pod. | **No** |
| Does the production broker have a deployment-ready phase authority? | The command rejects `AGW_TRUSTED_PHASE_SOCKET` and constructs no phase authority or private phase listener. The pure broker protocol seam remains unbound until an independently observing adapter is proven. | **No** |
| Can the work pod receive a phase credential today? | The work pod has `automountServiceAccountToken: false`, the agent has no Secret mounts, and no phase credential is projected. | **Must remain no** |

The existing broker-side phase transport is therefore not evidence of a working
deployment. The current operator/lifecycle observer can establish Sandbox
`Ready`/`Finished` state and, separately, can accept kubelet-reported termination
only after proving the exact child Pod identity, a server-assigned Pod UID,
`RestartPolicy=Never`, and an unambiguous `agent` container status. That is
independent exit evidence, but it is not a live harness-PID observer and it does
not authorize a tool-profile transition.

The broker command now deliberately rejects `AGW_TRUSTED_PHASE_SOCKET` with
`trusted_phase_supervisor_unsupported` and does not construct a phase authority
or private phase listener. The pure broker handler tests are protocol tests, not
deployment evidence. This removes the misleading optional mode until a real
supervisor adapter exists.

## Enforced deployment guardrails

The current implementation makes the boundary machine-checkable:

1. `PodSpec.shareProcessNamespace` is explicitly `false`.
2. The work Sandbox is annotated
   `agents.astatide.com/trusted-phase-supervisor: disabled`.
3. The Helm value `phaseSupervisor.enabled` is schema-constrained to `false`.
   The chart validation helper also rejects `true`, including when schema
   validation is bypassed.
4. The lifecycle binary has no `supervise` mode; an attempt fails with the
   usage error.
5. The broker command rejects `AGW_TRUSTED_PHASE_SOCKET`; no chart or workload
   manifest can opt into the unproven private listener.
6. Workload tests reject phase-credential environment variables and assert that
   only the `agent` and `broker` regular containers are rendered.

These are intentional negative contracts. They prevent a future deployment
manifest from silently turning on PID sharing or presenting an unbound token as
if it were a trusted authority.

## Contract required before implementation

Replacing the guard requires all of the following in one reviewed change:

### 1. A real authority binding

The broker target must be constructed with both halves of its existing host
contract: a `TrustedPhaseTransitioner` and a separate high-entropy phase token.
The token must not be the process-exit token. The broker must bind the target to
the immutable run identity (`run UID`, resolved-spec digest, and base SHA), and
must retain its own `PhaseTransitionAuthorizer` check. The protocol seam carries
all three identity fields and compares them exactly with the server-side
supervisor identity; a request containing only the operation, request ID, or a
bearer token is not sufficient authority.

The deployment surfaces in this audit do not provide this binding. A private
socket by itself is not a supervisor: no trusted process currently owns the
matching OS identity, observes the harness independently, or submits the
authorization request. The command therefore rejects the retired socket setting
instead of exposing a misleading partial adapter. Until a production adapter is
proven, this audit does not add a token or a sidecar.

### 2. An independent observation channel

The supervisor must observe the actual harness process termination and runtime
identity independently of model-authored events. It must have a bounded,
identity-bound report that the controller can reconcile with the runtime event
stream. A clean process exit without a valid terminal runtime event remains a
protocol failure; a terminal event without an independently observed exit never
becomes completion.

`shareProcessNamespace: true` is not itself an implementation. If that design is
chosen, Phase 0 must prove on the target kernel/containerd that the supervisor
can read the intended process state while the agent cannot read its credentials,
inspect sensitive arguments/environments, signal it, or alter its observation.
Giving the supervisor `CAP_SYS_PTRACE`, a host PID namespace, host IPC/network,
or a broad Kubernetes ServiceAccount is not an acceptable shortcut.

### 3. Credential and network isolation

The phase token may be mounted only into the supervisor, through a fixed file
projection with a reviewed UID/mode. It must be absent from the agent's
environment, argv, mounts, workspace, `/proc`, logs, runtime events, and
artifacts. The agent must not be able to use a generic proxy path to submit the
phase request. The loopback/network rule must identify the exact supervisor
operation; a broad same-port allowance is not proof of caller identity.

The supervisor must not receive the GitHub, skills, model, artifact, or broker
provider credentials. It needs only the minimum phase capability and the
identity/observation inputs required by the contract.

### 4. Fail-closed lifecycle behavior

Missing token, missing authority, stale identity, invalid process state,
ambiguous request outcome, broker restart, supervisor restart, or a failed
observation must leave the broker in `Explore` and the run in a non-success
state. The operation is one-way and idempotent only for the same immutable
request identity. No agent-authored event, file, marker, or environment value
may authorize the transition.

## Required acceptance tests for a future change

The following tests must pass locally and on the target two-node k3s cluster
before removing the guard:

- Rendered work pod has the supervisor container, exact immutable image digest,
  explicit PID-namespace setting, and no agent credential projection.
- The agent cannot reach the phase operation without the supervisor-only
  capability; malformed, replayed, stale, and cross-run requests fail closed.
- The supervisor can authorize exactly one `Explore -> Edit` transition, while a
  missing/denying authority leaves the phase at `Explore`.
- The supervisor observes the actual harness PID/exit for success, non-zero
  exit, signal termination, timeout, and supervisor/broker restart cases.
- The agent cannot read the phase token from environment, argv, mounts,
  workspace, `/proc`, logs, events, or artifacts, and cannot signal or tamper
  with the supervisor.
- Runtime completion still requires both a valid terminal event and the
  independent process observation; the operator reconciles the result after
  restart without duplicating effects.
- The test runs against the exact user-namespace, seccomp, CNI, and container
  runtime versions used by the release. A Kind API/schema smoke is not enough.

Until these tests and the broker binding exist, the correct operational action
is to leave the chart default unchanged. Agents remain in `Explore`; any
edit-phase tools must be treated as unavailable.
