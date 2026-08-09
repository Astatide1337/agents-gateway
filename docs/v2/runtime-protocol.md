# Agents Gateway runtime protocol

Protocol: `agw.runtime.v1`

Transport: newline-delimited JSON over sandbox stdin/stdout
Maximum frame: 1 MiB; maximum `data`: 768 KiB

This document describes the protocol implemented by `v2/proto` and
`v2/pkg/runtimeproto`. It is the compatibility contract between an agent
runtime adapter and `agw-runner`; proposed extensions are not part of v1.

## Envelope

Every frame is exactly one JSON object followed by a newline:

```json
{
  "protocol": "agw.runtime.v1",
  "kind": "event",
  "type": "run.started",
  "run_id": "run-01J...",
  "seq": 1,
  "terminal": false,
  "data": {
    "agent_id": "issue-fixer",
    "sandbox_id": "agw-01J..."
  }
}
```

Unknown or duplicate top-level fields, trailing JSON values, blank frames,
oversized frames, non-object `data`, and unsupported types are rejected. Event
sequence numbers start at one and increase by exactly one. The first event is
`run.started`; no event is accepted after a terminal event.

The two directions maintain independent sequence numbers:

- runner to adapter uses `kind: request` on stdin;
- adapter to runner uses `kind: event` on stdout.

## Runner requests

- `run.start`: the immutable, bounded, non-secret run contract. An adapter must
  wait for and validate this before doing work.
- `input`: an immutable external input reference.
- `tool.result`: a broker result tied to `data.request_id`.
- `approval.result`: an approval result with `data.approval_id` and
  `data.decision`.
- `run.cancel`: cancellation reason and deadline.
- `ping`: liveness request.

Request frames always have `terminal: false`. Credentials, provider API keys,
runtime sockets, and unrestricted host paths are never included in a contract.

## Adapter events

Supported non-terminal events are:

- `run.started` (`agent_id`, `sandbox_id` required)
- `heartbeat`
- `assistant.message`
- `model.requested` (`model` required)
- `model.completed`
- `tool.requested` (`request_id`, `tool` required)
- `tool.completed` (`request_id`, `status` required)
- `approval.requested` (`approval_id` required)
- `approval.resolved` (`approval_id`, `decision` required)
- `artifact.created` (`artifact_id` required); zero or more authored artifacts
  may be emitted before terminal completion

Supported terminal events are:

- `run.completed` (`result` required)
- `run.failed` (`error` required)
- `run.cancelled` (`reason` required)
- `run.unknown_effect` (`effect_id` required)

Exactly one terminal event is required. A zero process exit without one is a
protocol failure. A terminal event does not by itself make a run successful:
the runner also requires a clean process exit and a valid immutable output
artifact reference.

For `run.completed`, `data.result` is a required bounded result summary and
`data.output` is the immutable artifact reference:

```json
{
  "result": "completed",
  "output": {
    "id": "artifact-01J...",
    "uri": "s3://agw-artifacts/org/project/run/output.json",
    "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "size_bytes": 321,
    "media_type": "application/json"
  }
}
```

The runner validates and persists only the reference (plus the bounded status
summary); artifact bytes stay out of runner metadata and Temporal history.
The generic output reference is always required even when earlier
`artifact.created` events announce richer authored artifacts. The Codex
adapter discovers those optional artifacts through the bounded
`.agw/artifacts/*.json` convention documented in [artifacts.md](artifacts.md),
uploads them through the private loopback broker, and never places raw bytes in
protocol frames.

## Approval and effects

An adapter emits `approval.requested` and pauses. The control plane durably
records a decision, and the runner sends `approval.result`. A mutating tool
request must have a stable broker effect key. If the upstream write outcome is
ambiguous, the terminal state is `run.unknown_effect`; the operation is not
blindly retried.

## Error handling

Protocol violations fail the task and trigger fenced cleanup. Errors returned
to the worker use stable, non-secret codes; raw provider errors, credentials,
prompts, and tool arguments are not copied into task status. The runner checks
its owner-bound local lease before startup and cleanup. The current release is
single-runner: task reassignment and cross-runner fencing are not supported.

The current v1 transport is intentionally small. Timestamped event IDs,
content negotiation, checkpoint frames, and additional typed payload schemas
require a new backwards-compatible protocol revision rather than silently
changing this envelope.
