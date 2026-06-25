# Task Lifecycle Reference

Tasks represent individual units of work dispatched to agents. This document covers task states, transitions, the HTTP API, and how tasks produce events and artifacts.

## Task States

A task moves through a defined set of states during its lifecycle.

| State | Description |
|---|---|
| `created` | Task has been submitted but not yet queued for execution |
| `queued` | Task is waiting in the execution queue for a worker slot |
| `running` | An agent runtime is actively executing the task |
| `waiting` | The task is paused, waiting for an external event or callback |
| `completed` | The task finished successfully and produced an artifact |
| `failed` | The task terminated with an error |
| `cancelled` | The task was cancelled by the user before completion |

## Allowed Transitions

Not all state transitions are valid. The gateway enforces the following transition rules:

```
created  -> queued
queued   -> running
queued   -> cancelled
running  -> completed
running  -> failed
running  -> waiting
running  -> cancelled
waiting  -> running
waiting  -> cancelled
```

Any attempt to move a task to an invalid state results in a `409 Conflict` response.

Terminal states (`completed`, `failed`, `cancelled`) have no outgoing transitions. Once a task reaches a terminal state, its state cannot be changed.

## Task HTTP API

### Create a Task

```
POST /tasks
Content-Type: application/json

{
  "agent_id": "echo-agent",
  "input": "Hello, world"
}
```

Response (`201 Created`):

```json
{
  "task_id": "t_01HXYZABC",
  "agent_id": "echo-agent",
  "state": "created",
  "input": "Hello, world",
  "created_at": "2025-01-15T10:30:00Z"
}
```

### List Tasks

```
GET /tasks
```

Returns a list of all tasks, ordered by creation time descending. Supports optional query parameters for filtering:

| Parameter | Description |
|---|---|
| `state` | Filter by task state |
| `agent_id` | Filter by agent ID |

Response (`200 OK`):

```json
{
  "tasks": [
    {
      "task_id": "t_01HXYZABC",
      "agent_id": "echo-agent",
      "state": "completed",
      "created_at": "2025-01-15T10:30:00Z",
      "completed_at": "2025-01-15T10:30:02Z"
    }
  ]
}
```

### Get a Task

```
GET /tasks/{task_id}
```

Response (`200 OK`):

```json
{
  "task_id": "t_01HXYZABC",
  "agent_id": "echo-agent",
  "state": "completed",
  "input": "Hello, world",
  "output": {"text": "Hello, world"},
  "created_at": "2025-01-15T10:30:00Z",
  "completed_at": "2025-01-15T10:30:02Z"
}
```

Returns `404 Not Found` if the task does not exist.

### Cancel a Task

```
POST /tasks/{task_id}/cancel
```

Transitions a task to the `cancelled` state if the current state allows it (see allowed transitions above).

Response (`200 OK`):

```json
{
  "task_id": "t_01HXYZABC",
  "state": "cancelled"
}
```

Returns `409 Conflict` if the task is in a terminal state.

## Events

Tasks emit events as they progress through their lifecycle. Events are stored and can be retrieved for auditing or monitoring.

### List Task Events

```
GET /tasks/{task_id}/events
```

Response (`200 OK`):

```json
{
  "events": [
    {
      "event": "task.created",
      "timestamp": "2025-01-15T10:30:00Z",
      "data": {"agent_id": "echo-agent"}
    },
    {
      "event": "task.running",
      "timestamp": "2025-01-15T10:30:01Z",
      "data": {}
    },
    {
      "event": "task.completed",
      "timestamp": "2025-01-15T10:30:02Z",
      "data": {"artifact_id": "a_01HXYZDEF"}
    }
  ]
}
```

### Event Types

| Event | Description |
|---|---|
| `task.created` | Task was submitted |
| `task.queued` | Task entered the execution queue |
| `task.running` | Task execution began |
| `task.waiting` | Task is paused for an external event |
| `task.completed` | Task finished successfully |
| `task.failed` | Task failed with an error |
| `task.cancelled` | Task was cancelled |

## Artifacts

When a task completes, it may produce an artifact containing the output data. Artifacts are stored in the configured storage backend.

### Get Task Artifact

```
GET /tasks/{task_id}/artifact
```

Response (`200 OK`):

```json
{
  "artifact_id": "a_01HXYZDEF",
  "task_id": "t_01HXYZABC",
  "content_type": "application/json",
  "data": {"text": "Hello, world"},
  "created_at": "2025-01-15T10:30:02Z"
}
```

Returns `404 Not Found` if the task has no artifact or the task does not exist.
