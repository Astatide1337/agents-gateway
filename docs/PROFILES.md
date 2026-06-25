# Profile System Reference

Profiles allow you to define named sets of configuration overrides that can be activated at runtime. This is useful for maintaining different configurations for development, staging, and production within a single `agents-gateway.yaml` file.

## Defining Profiles

Profiles are defined under the top-level `profiles` key in `agents-gateway.yaml`. Each profile is a named object containing partial configuration that overrides the base configuration when that profile is active.

```yaml
profiles:
  staging:
    auth:
      mode: cloudflare-access
    service:
      port: 8903
    observability:
      log_level: debug

  production:
    auth:
      mode: cloudflare-access
    service:
      port: 80
    observability:
      log_level: warning
    storage:
      dir: /var/lib/agent-gateway/data
```

A profile can override any subset of configuration keys. Keys not specified in the profile inherit their values from the base configuration.

## Activating Profiles

A profile is activated through one of two mechanisms:

### Environment Variable

Set `AGW_PROFILE` to the name of the desired profile:

```bash
export AGW_PROFILE=staging
```

### CLI Flag

Pass the `--profile` flag when starting the gateway:

```bash
agent-gateway --profile production
```

The `--profile` flag takes precedence over `AGW_PROFILE` if both are set. See the full precedence chain in [CONFIG.md](./CONFIG.md).

## Behavior When No Profile Is Set

When neither `AGW_PROFILE` nor `--profile` is provided, the gateway runs with the base configuration only. No profile overrides are applied. This is the default and expected behavior for local development.

## Error on Unknown Profiles

If the active profile name does not match any profile defined in `agents-gateway.yaml`, the gateway will fail to start with an error message indicating the unknown profile name and the list of available profiles. This prevents silent misconfiguration.

Example error:

```
error: unknown profile "production", available profiles: staging, ci
```

## Profile Merge Behavior

When a profile is active, its values are shallow-merged into the base configuration at the section level. For example, if a profile overrides `auth.mode` but does not specify `service.port`, the base `service.port` value is used.

```yaml
service:
  port: 8902       # inherited from base
  host: "0.0.0.0"   # inherited from base

auth:
  mode: cloudflare-access  # overridden by profile
```

Deep merges are not performed. If a profile specifies a section, the entire section from the profile replaces the base section.

## Example Configuration with Profiles

```yaml
service:
  port: 8902
  host: "0.0.0.0"

auth:
  mode: dev-none

agents:
  dirs:
    - ./agents

storage:
  dir: ./data
  sqlite_url: "sqlite:///./data/agent-gateway.db"

observability:
  log_level: info
  log_format: json

profiles:
  dev:
    auth:
      mode: dev-none
    observability:
      log_level: debug

  staging:
    auth:
      mode: internal-only
    service:
      port: 8903
    observability:
      log_level: debug

  production:
    auth:
      mode: cloudflare-access
    observability:
      log_level: warning
    storage:
      dir: /var/lib/agent-gateway/data
      sqlite_url: "sqlite:////var/lib/agent-gateway/data/agent-gateway.db"
```

To run with the staging profile:

```bash
agent-gateway --profile staging
```
