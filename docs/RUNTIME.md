# Runtime Adapter Reference

Runtime adapters define how agents execute tasks. Each agent manifest specifies a `runtime.type` that determines which adapter is used.

## Stub Runtime

The stub runtime is the default adapter. It is designed for testing and development only.

### How It Works

The stub runtime does not make any external calls. When a task is dispatched to a stub agent, the runtime:

1. Accepts the task input
2. Constructs a JSON artifact containing the input data along with metadata
3. Returns the artifact immediately

The output artifact produced by the stub runtime has the following structure:

```json
{
  "input": "<the original task input>",
  "agent_id": "<the agent id>",
  "stub": true,
  "timestamp": "2025-01-15T10:30:00Z"
}
```

No external processes, containers, or network calls are involved. The stub runtime executes synchronously within the gateway process.

### Stub Is for Testing Only

The stub runtime is intentionally minimal. It does not execute real agent logic, invoke tools, or communicate with any external system. It exists to:

- Validate the task lifecycle end-to-end without requiring a real agent runtime
- Support integration and smoke tests
- Allow development of the gateway infrastructure independently of agent runtimes

Do not use the stub runtime in production. It provides no isolation, no sandboxing, and no meaningful computation.

## Future Runtime Adapters

The runtime adapter system is extensible. The following adapters are planned for future releases:

### Docker Runtime

Will execute agent tasks inside Docker containers. Each agent defines a container image in its manifest. The gateway will create, run, and clean up containers for each task. Expected manifest additions:

```yaml
runtime:
  type: docker
  image: my-registry/echo-agent:1.0.0
  cpu_limit: "0.5"
  memory_limit: "512m"
```

### Process Runtime

Will execute agent tasks as local subprocesses. The agent manifest will specify a command to run. This is suitable for trusted, locally-deployed agents. Expected manifest additions:

```yaml
runtime:
  type: process
  command: ["python", "agent.py"]
  working_dir: /opt/agents/echo-agent
```

## Implementing a Custom Runtime

To implement a custom runtime adapter, extend the runtime adapter interface:

### 1. Create the Adapter Class

Implement the adapter protocol/interface defined by the gateway. At a minimum, the adapter must provide:

- **`run(task_input, agent_config) -> artifact`**: Execute the task and return an artifact
- **`validate_config(agent_config) -> list[str]`**: Validate the runtime-specific configuration from the manifest, returning a list of validation errors
- **`name -> str`**: Return the runtime type identifier (e.g. `"docker"`)

### 2. Register the Adapter

Register the adapter with the runtime registry so it can be discovered by `runtime.type` values in agent manifests. Registration typically occurs at application startup.

### 3. Add Runtime Configuration

Add any runtime-specific fields to the `runtime` section of agent manifests. Ensure validation rules are in place to catch misconfiguration early.

### 4. Handle Errors

The adapter should raise well-defined exceptions for:

- Startup failures (e.g. container image not found)
- Execution timeouts
- Resource exhaustion
- Unexpected process exit codes

These errors are caught by the gateway and result in the task transitioning to the `failed` state with an appropriate error message.

### 5. Clean Up Resources

The adapter is responsible for cleaning up any resources it creates (containers, processes, temporary files). The gateway assumes that after `run()` returns or raises, no lingering resources remain.
