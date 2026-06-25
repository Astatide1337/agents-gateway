# Agent Manifest Reference

Each agent is defined by an `agent.yaml` manifest file placed in one of the directories listed under `agents.dirs` in the gateway configuration. The gateway scans these directories at startup and builds a catalog of available agents.

## File Format

The manifest is a YAML file named `agent.yaml` located in a subdirectory of an agents directory. The subdirectory name is typically derived from the agent `id`.

```
agents/
  my-agent/
    agent.yaml
```

## Required Fields

Every agent manifest must include the following fields. Missing required fields cause the agent to be rejected during catalog construction and an error is logged.

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique identifier for the agent. Must match the regex `^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`. Must be unique across the entire catalog. |
| `name` | string | Human-readable display name |
| `description` | string | Short description of the agent's purpose |
| `version` | string | Semantic version string (e.g. `1.0.0`) |
| `runtime.type` | string | Runtime adapter to use. Currently supported: `stub` |

### Minimal Example

```yaml
id: echo-agent
name: Echo Agent
description: Returns the input unchanged
version: 1.0.0
runtime:
  type: stub
```

## Recommended Fields

The following fields are not required but are strongly recommended for production agents.

| Field | Type | Description |
|---|---|---|
| `skills` | list of strings | Capabilities the agent provides (e.g. `["summarization", "translation"]`) |
| `tools` | list of strings | External tools or MCP servers the agent can invoke |
| `permissions` | list of strings | Permission scopes the agent requires (e.g. `["fs.read", "network.outbound"]`) |
| `risk_level` | string | One of `low`, `medium`, `high`. Indicates the potential impact of running this agent. Defaults to `medium` if unset. |
| `tags` | list of strings | Freeform tags for categorization and filtering |
| `author` | string | Contact or team that maintains the agent |

### Full Example

```yaml
id: summarizer
name: Summarizer Agent
description: Summarizes long-form text into concise bullet points
version: 1.2.0
runtime:
  type: stub

skills:
  - summarization
  - extraction

tools:
  - text-splitter

permissions:
  - fs.read

risk_level: low
tags:
  - nlp
  - text-processing
author: platform-team
```

## Validation Rules

The gateway validates each manifest at startup. The following rules are enforced:

1. **`id` format**: Must match `^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$`. Lowercase alphanumeric and hyphens only, 2-64 characters, cannot start or end with a hyphen.

2. **`id` uniqueness**: No two agents may share the same `id`. Duplicate IDs cause the duplicate to be rejected with a logged error.

3. **`version` format**: Should follow semver (`MAJOR.MINOR.PATCH`). Non-semver strings produce a warning but do not prevent loading.

4. **`runtime.type` known**: The runtime type must correspond to a registered adapter. Unknown types cause the agent to be rejected.

5. **`risk_level` values**: If provided, must be one of `low`, `medium`, `high`. Invalid values cause a validation error.

6. **YAML parse errors**: Malformed YAML files are skipped with a logged error and do not prevent other agents from loading.

7. **Empty manifest**: A manifest with all required fields but empty string values is rejected.

## Directory Structure Convention

```
agents/
  echo-agent/
    agent.yaml
  summarizer/
    agent.yaml
  translator/
    agent.yaml
```

Each agent lives in its own subdirectory. The directory name does not need to match the `id` field, but matching them is recommended for clarity.
