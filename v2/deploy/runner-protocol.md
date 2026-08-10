# Worker-to-runner protocol

`agw-worker` dispatches Temporal activities to the host-installed
`agw-runner serve` daemon. The worker never receives a containerd, Docker, or
Podman socket.

## Transport

- HTTPS with TLS 1.3 and mutual certificate authentication is mandatory.
- The runner CA should issue certificates only to the runner server and the
  worker identity. There is no bearer-token or plaintext fallback.
- Requests and responses are limited to 1 MiB, JSON decoding rejects unknown
  and trailing fields, redirects are forbidden, and transport errors do not
  expose response bodies.
- The verified client-certificate fingerprint is the task owner. Every status,
  cancel, and resume operation must match that owner and the task's exact
  organization, project, and run identity.

Implemented endpoints:

| Method and path | Contract | Result |
| --- | --- | --- |
| `GET /healthz` | none | process health |
| `GET /readyz` | none | host/backend readiness |
| `POST /v1/tasks/schedule` | `ScheduleRunnerTaskInput` | stable task ID/status |
| `POST /v1/tasks/status` | `StatusRunnerTaskInput` | current/terminal status and output reference |
| `POST /v1/tasks/cancel` | `CancelRunnerTaskInput` | empty 204 |
| `POST /v1/tasks/resume` | `ResumeRunnerTaskInput` | empty 204 |

All endpoints require mTLS, including health endpoints.

## Idempotency and state

Schedule is keyed by organization, project, and the activity idempotency key.
Reusing a key with different input or a different certificate owner is
rejected. Resume commands have a durable, payload-bound command ledger; an
ambiguous worker retry does not deliver a second approval or user reply.

Task metadata is atomically persisted under the configured runner workspace.
After restart, tasks that were active are reported as `lost`; scheduled tasks
are recovered. Terminal states are `succeeded`, `failed`, `cancelled`, or
`lost`. Success requires all of the following:

- a valid `run.completed` adapter event;
- a clean, unsignaled process exit with code zero;
- a valid immutable output artifact reference;
- successful fenced sandbox cleanup.

The current single-runner profile uses an owner-bound local fencing token. It
does not yet support control-plane-authoritative reassignment between runner
hosts; that capability remains a release blocker for a multi-runner HA mode.

## Schedule payload

The payload contains tenant/run identity, immutable input and dependency
references, a stable idempotency key, and a fully validated `SandboxSpec`.
Prompts, artifact bytes, provider credentials, and runtime sockets never cross
this boundary.

The runner starts the sandbox and immediately sends a bounded `run.start`
contract on stdin. The adapter emits the `agw.runtime.v1` stream on stdout.
See [the runtime protocol](../../docs/v2/runtime-protocol.md) for that exact
contract.

## Configuration

The worker requires:

- `AGW_RUNNER_ENDPOINT`
- `AGW_RUNNER_TLS_SERVER_NAME`
- `AGW_RUNNER_TLS_CA_FILE`
- `AGW_RUNNER_TLS_CERT_FILE`
- `AGW_RUNNER_TLS_KEY_FILE`
- `AGW_RUNNER_REQUEST_TIMEOUT`

All certificate variables are paths to mounted secret files. The runner is a
host service because it owns the local sandbox runtime; the control plane and
agent containers must never mount the runtime socket.
