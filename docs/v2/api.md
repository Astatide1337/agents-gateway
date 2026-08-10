# Agents Gateway v2 HTTP API

Base path: `/api/v1alpha1`

Authentication: local owner token, OIDC/identity-aware proxy JWT, or short-lived service token
Media type: `application/json`

Every protected route currently carries organization/project scope and is
audited. Standalone installations use generated local scope names (for
example `local/default`); operators do not need to configure tenant identity
infrastructure. The distributed profile maps these scopes to real teams,
OIDC memberships, RLS, and quotas. Responses
contain `requestId`; callers should also send a stable `Idempotency-Key` for
run creation and control operations.

## Resources and runs

| Method | Path | Purpose |
| --- | --- | --- |
| `PUT` or `POST` | `/organizations/{org}/projects/{project}/resources/{kind}/{name}` | Validate and apply an immutable resource revision |
| `GET` | `/organizations/{org}/projects/{project}/resources/{kind}/{name}` | Read the current resource revision |
| `POST` | `/organizations/{org}/projects/{project}/runs` | Start an `AgentRun` or `WorkflowRun` |
| `GET` | `/organizations/{org}/projects/{project}/runs/{run}` | Read durable run status |
| `GET` | `/organizations/{org}/projects/{project}/runs/{run}/events` | Read events; request SSE with `Accept: text/event-stream` |

Run input is an immutable `inputRef`; inline input/prompt bytes are rejected.

## Run controls

All controls use `POST`. A production client should supply a stable
`Idempotency-Key` header. Reusing a key with a different command returns a
conflict; an identical accepted command is safe to replay. Concurrent delivery
uses a 30-second durable claim lease. A second caller receives
`signal_in_flight` plus `Retry-After` while the first owns that lease.

PostgreSQL claim state and the selected run-engine delivery cannot be committed atomically.
If a process stops after an engine accepts a command but before PostgreSQL
records acceptance, the same command may be delivered again after the lease.
Current cancel, approval-ID, and immutable-reply-reference handling is
semantically duplicate-safe, but operators should still treat an
`idempotency_unavailable` response as ambiguous and inspect run events/state.

### Cancel

`/organizations/{org}/projects/{project}/runs/{run}/cancel`

```json
{"reason":"operator requested stop"}
```

Cancellation targets the top-level workflow and is available to project
editors or organization administrators.

### Approval

`/organizations/{org}/projects/{project}/runs/{run}/approval`

```json
{
  "target": "workflow",
  "approvalId": "publish-approval",
  "stepId": "publish-approval",
  "decision": "approved"
}
```

Use `target: "agent"` plus the exact `stepId` for an approval requested by a
running agent child. Only approvers and organization administrators may submit
approval decisions. Decisions are exactly `approved` or `denied`.

### Immutable user reply

`/organizations/{org}/projects/{project}/runs/{run}/reply`

```json
{
  "target": "agent",
  "stepId": "implement",
  "taskId": "task-…",
  "replyRef": "s3://tenant-scoped-inputs/replies/reply-1.json"
}
```

The API accepts only a bounded immutable reference. Raw reply text, prompts,
credentials, and arbitrary payloads are not accepted or copied into Temporal
history.

## Health

- `/health` and `/healthz` report process liveness.
- `/ready` and `/readyz` check storage, authentication configuration, and the
  selected run engine with a bounded deadline.
