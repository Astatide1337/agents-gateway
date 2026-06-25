# Troubleshooting Guide

This document covers common issues and their resolutions when running Agent Gateway.

## Agents Not Found

**Symptom**: The `/inventory` endpoint returns an empty agents list, or you see `catalog.loaded agents=0` in logs.

**Possible causes**:

1. **Agent directories not configured.** Verify that `agents.dirs` in `agents-gateway.yaml` points to the correct directories. Default is `["./agents"]`.

2. **Agent manifests missing or misplaced.** Each agent requires an `agent.yaml` file inside a subdirectory of the agents directory. Ensure the file exists and is named exactly `agent.yaml`.

3. **Manifest validation failures.** Check the gateway logs for validation errors. Common issues include:
   - Missing required fields (`id`, `name`, `description`, `version`, `runtime.type`)
   - Invalid agent ID format (must be lowercase alphanumeric with hyphens, 2-64 characters)
   - Unknown `runtime.type` value
   - Duplicate agent IDs

4. **Volume mount issues in Docker.** If running in Docker, ensure the agents directory is mounted correctly. Verify with:
   ```bash
   docker exec <container> ls /app/agents
   ```

## Configuration Not Loading

**Symptom**: Gateway starts with unexpected settings or ignores your `agents-gateway.yaml`.

**Possible causes**:

1. **Wrong file path.** The gateway looks for `agents-gateway.yaml` in the current working directory. Use `--config /path/to/agents-gateway.yaml` to specify an alternative path.

2. **YAML syntax errors.** Malformed YAML prevents the file from being parsed. Validate the file with a YAML linter:
   ```bash
   python -c "import yaml; yaml.safe_load(open('agents-gateway.yaml'))"
   ```

3. **Environment variable override.** An `AGW_`-prefixed environment variable may be silently overriding your YAML values. Check for any `AGW_` variables in your environment:
   ```bash
   env | grep AGW_
   ```

4. **Profile not active.** If you defined values under a profile section, ensure the profile is activated via `AGW_PROFILE` or `--profile`.

## Port Already in Use

**Symptom**: Gateway fails to start with an error like `Address already in use` or `bind: address already in use`.

**Resolution**:

1. Find the process using the port:
   ```bash
   lsof -i :8902
   ```

2. Stop the conflicting process or change the gateway port:
   ```bash
   AGW_SERVICE__PORT=8903 agent-gateway
   ```

3. In Docker, remap the host port:
   ```yaml
   ports:
     - "8903:8902"
   ```

## SQLite Database Locked

**Symptom**: Requests fail with `database is locked` errors.

**Possible causes**:

1. **Concurrent writes from multiple processes.** SQLite supports one writer at a time. Ensure only one gateway instance is using the same database file.

2. **Stale lock file.** If the gateway was killed ungracefully, a `-wal` or `-shm` file may remain. Stop all gateway processes, then remove these files:
   ```bash
   rm -f /data/agent-gateway.db-wal /data/agent-gateway.db-shm
   ```

3. **NFS or network filesystem.** SQLite locking is unreliable over NFS. Use a local filesystem for the storage directory.

4. **Long-running transactions.** If a task is stuck in `running` state and holding a write lock, cancel the task or restart the gateway.

## Authentication Failures

**Symptom**: Requests return `401 Unauthorized` or `403 Forbidden`.

**Possible causes by mode**:

### cloudflare-access

- **Missing or invalid JWT.** Ensure the `Cf-Access-Jwt-Assertion` header is present and contains a valid token.
- **Expired token.** Tokens have a limited lifetime. Obtain a fresh token.
- **Wrong audience.** The `CF_ACCESS_APPLICATION_AUD` environment variable must match the audience configured in Cloudflare Access.
- **Wrong team domain.** The `CF_ACCESS_TEAM_DOMAIN` must match your Cloudflare Access team.

### internal-only

- **Source IP outside allowed ranges.** Only `127.0.0.0/8`, `::1`, and `172.16.0.0/12` are allowed. If the gateway is behind a reverse proxy, the proxy's IP may not be in the allowed range. Configure the proxy to pass the original client IP via `X-Forwarded-For` or `X-Real-IP` headers, or add the proxy IP to your network allowlist.

### dev-none

- `dev-none` should never deny requests. If you see auth failures in `dev-none` mode, check that the mode is actually active by inspecting `/ready` or startup logs.

## Docker Issues

### Container Exits Immediately

Check container logs:
```bash
docker compose logs agent-gateway
```

Common causes:
- Invalid configuration (YAML syntax error, unknown profile)
- Port conflict (address already in use on the host)
- Missing agents directory mount

### Cannot Reach API from Host

Ensure the port is published in your compose file:
```yaml
ports:
  - "8902:8902"
```

Not to be confused with `expose`, which only makes the port available to other containers.

### Healthcheck Failing

```bash
docker inspect --format='{{.State.Health}}' <container>
```

If the healthcheck continually fails, the gateway may not be starting. Check the logs and verify:
- The `/ready` endpoint is reachable from inside the container
- The startup is not blocked on configuration or manifest errors

### Permission Errors on Volumes

If you see permission denied errors writing to `/data`, ensure the volume is writable by the container process. For bind mounts:
```bash
chmod 777 ./data
```

For named volumes, Docker manages permissions automatically.

###agents Directory Not Found in Container

Verify the mount:
```bash
docker exec <container> ls -la /app/agents
```

Ensure the host path exists and contains `agent.yaml` files in subdirectories.
