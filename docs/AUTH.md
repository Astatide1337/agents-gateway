# Auth Modes Reference

Agent Gateway supports multiple authentication modes to control access to its HTTP API. The active mode is set via `auth.mode` in `agents-gateway.yaml`, the `AGW_AUTH__MODE` environment variable, or the `--auth-mode` CLI flag.

## dev-none

**Not safe for production use.**

Disables all authentication. Every request is allowed without any credentials. This mode is intended exclusively for local development and testing.

```yaml
auth:
  mode: dev-none
```

When `dev-none` is active:

- No token validation is performed
- No authorization headers are required
- All endpoints are accessible to any caller
- A warning is logged at startup

Do not expose a gateway running in `dev-none` mode to any network that is not fully trusted.

## cloudflare-access

Validates requests using Cloudflare Access JSON Web Tokens (JWT) and Bearer tokens issued by Cloudflare.

```yaml
auth:
  mode: cloudflare-access
```

When `cloudflare-access` is active:

- The gateway validates the `Cf-Access-Jwt-Assertion` header or the `Authorization: Bearer <token>` header
- Tokens are verified against the Cloudflare Access team domain and application audience
- Expired, malformed, or invalid tokens result in a `401 Unauthorized` response
- Token claims may be used to identify the caller

Required environment variables for this mode:

| Variable | Description |
|---|---|
| `CF_ACCESS_TEAM_DOMAIN` | The Cloudflare Access team domain (e.g. `https://my-team.cloudflareaccess.com`) |
| `CF_ACCESS_APPLICATION_AUD` | The application audience tag configured in Cloudflare Access |

## internal-only

Restricts access to connections from Docker-internal networks and localhost only.

```yaml
auth:
  mode: internal-only
```

When `internal-only` is active:

- Requests from `127.0.0.0/8`, `::1`, and Docker bridge networks (`172.16.0.0/12`) are allowed
- All other source addresses receive a `403 Forbidden` response
- No token or credential validation is performed for allowed addresses
- This mode is suitable when the gateway runs as a sidecar or internal service within a Docker Compose deployment

## Auth in API Responses

### /inventory

The `/inventory` endpoint reflects the current auth mode in its response:

```json
{
  "auth_mode": "cloudflare-access",
  "agents": [...]
}
```

This allows clients to determine which auth mode the gateway expects.

### /ready

The `/ready` health endpoint includes auth status:

```json
{
  "status": "ok",
  "auth_mode": "cloudflare-access"
}
```

When running in `dev-none` mode, the `auth_mode` field is present but explicitly set to `"dev-none"` to make the unsafe state visible.

## Production Safety

Follow these recommendations for production deployments:

1. **Never use `dev-none` in production.** It provides zero access control.

2. **Use `cloudflare-access` for internet-facing deployments.** Deploy behind Cloudflare and let Cloudflare Access handle identity and token issuance.

3. **Use `internal-only` for private deployments** where the gateway is only reachable from trusted containers or localhost. Ensure no ports are published to the host network in Docker Compose unless explicitly needed.

4. **Rotate secrets regularly.** If using Cloudflare Access, rotate application audience keys when recommended by your security policy.

5. **Audit access.** Enable structured logging at `info` level or above so that authentication failures are recorded and can be reviewed.
