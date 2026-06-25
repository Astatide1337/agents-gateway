# Security Reference

This document covers the security model of Agent Gateway, including authentication modes, logging safety, and production recommendations.

## Authentication Modes

The gateway supports three authentication modes. The chosen mode determines how requests are authorized before reaching agent logic.

| Mode | Safety | Intended Environment |
|---|---|---|
| `dev-none` | Unsafe -- no authentication | Local development only |
| `cloudflare-access` | Safe -- JWT validation | Production (internet-facing) |
| `internal-only` | Conditional -- network-level | Private Docker deployments |

See [AUTH.md](./AUTH.md) for full details on each mode.

## No Secrets in Logs

The gateway never writes secrets, tokens, passwords, or credentials to logs. This applies to:

- Authentication tokens (JWT, Bearer tokens)
- API keys
- Environment variables that may contain sensitive values
- Task input and output data that could contain credentials

Log events include identifiers (request IDs, task IDs, agent IDs) but never secret material. If a request contains an `Authorization` header, only a redacted form (e.g. `Bearer ***`) would appear in debug-level logs, if at all.

## dev-none Is Unsafe

The `dev-none` authentication mode disables all access control. Every request is accepted without credentials. This mode exists solely for local development convenience.

Risks of running `dev-none` in a non-local environment:

- Any network-reachable caller can invoke all API endpoints
- Tasks can be created, listed, and cancelled by anyone
- Agent catalogs and artifacts are fully accessible
- No audit trail of who made which request

The gateway logs a warning at startup when `dev-none` is active. Treat this warning seriously in any non-development context.

## Production Recommendations

1. **Use `cloudflare-access` for internet-facing deployments.** Deploy the gateway behind Cloudflare and enforce JWT-based access. This provides identity-aware authentication without managing custom token infrastructure.

2. **Use `internal-only` for sidecar or internal service patterns.** When the gateway runs within a Docker Compose stack and only other containers need access, restrict connectivity to internal networks. Ensure no ports are published to host interfaces unless absolutely required.

3. **Enable structured logging at `info` level or above.** This ensures authentication denial events are captured and auditable.

4. **Restrict network access at the infrastructure level.** Use Docker network isolation, firewall rules, or Cloudflare Access policies to limit which clients can reach the gateway, even when authentication is enabled.

5. **Do not store sensitive data in agent manifests.** Manifests are read from the filesystem and may appear in logs or API responses. Use environment variables or secret management tools for sensitive configuration.

6. **Keep the gateway updated.** Apply updates promptly to benefit from security fixes.

## Artifact Storage Security

Task artifacts are stored on disk in the configured `storage.dir` directory. Security considerations:

- **File permissions**: The storage directory should be readable and writable only by the gateway process. Set directory permissions to `0700` and file permissions to `0600`.

- **Volume isolation**: When running in Docker, use a named volume for storage rather than a host bind mount. This prevents other host processes from accessing artifact files.

- **Artifact content**: Artifacts may contain task output that includes sensitive data. Access controls on the `/tasks/{id}/artifact` endpoint are governed by the active auth mode. Ensure the auth mode is appropriately strict for the sensitivity of data your agents process.

- **SQLite database**: The gateway uses SQLite for task metadata. The database file inherits filesystem permissions from its parent directory. Ensure the database file is not world-readable.

- **Backup**: Regular backups of the storage directory should be encrypted at rest and protected by access controls equivalent to the live data.
