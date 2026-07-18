# Security Policy

Agents Gateway is designed as an execution substrate for autonomous
long-horizon agent work. It deliberately handles untrusted environments
(LLM-generated code, external integrations, real harness subprocesses)
explicitly rather than implicitly. This document is the source of truth
for who-gets-what permissions and how secrets are kept out of harm's
way.

## Threat model

| Threat | Mitigation |
|--------|------------|
| Gateway secrets (CF JWKS client config, internal token, MCP tokens) leak into harness subprocesses | Every subprocess spawned by the runtime — verification commands AND any helper scripts — receives only `_safe_env()` (see below). Anything else is filtered out. |
| LLM-generated code reads `~/.aws/credentials` from the gateway user | Mitigation today is host-isolation: the harness runs as the gateway user, so technically visible files are visible. **Roadmap** is per-task container isolation (`ContainerDriver`) with no host bind mounts. |
| Tokens appear in HTML review reports / log artifacts | `harness/reports.py:_REDACT_PATTERNS` replaces Bearer tokens, `ghp_/gho_/ghs_/ghu_/ghr_/gho_` GitHub tokens, and URL credentials (`https://user:pass@...`) with `[REDACTED]` markers before the report is written. |
| Man-in-the-middle on CF Access edge | The origin verifies the `Cf-Access-Jwt-Assertion` header signature using CF Access JWKS in `cloudflare-access` mode. `alg: none` is rejected; expired / wrong-aud / wrong-iss JWTs are rejected. |
| Replay / brute-force internal token | Compared with `secrets.compare_digest` (constant-time). Failed attempts are logged but never echoed. |
| Rate-limit abuse from the public net | Per-IP rate-limiting middleware is opt-in via `AGW_SERVICE__RATE_LIMITING__ENABLED=true`. Edge-level rate-limiting (Cloudflare) is the recommended front line. |
| A malicious/injured harness session runs forever | Three safety nets without auto-fail: `session_stall_seconds`, `relay_max_time_seconds`, `max_verify_iterations`. When any trips, the session becomes `stalled` (NOT `failed`) and Composer decides how to proceed. Composer (or an operator) can always `POST /sessions/{id}/stop` to force-terminate. |
| Multiple agent runs edit the same directory | Per-task git worktrees (`worktree.create_worktree`); enforced by `branch: agent/<task_id>-<slug>` per task. No two tasks share a worktree. |
| Composer replays a reply across tasks | Interaction ids are UUIDs; interaction status transitions are gated (`pending -> answered`) — replaying the same id ends in HTTP 409. |

## The `_safe_env` boundary

This is the most enforced boundary in the codebase. The
`VerificationRunner._safe_env()` method returns ONLY the following
environment variables to verification subprocesses:

```
PATH
HOME
LANG
LC_ALL
LC_CTYPE
TERM
SHELL
USER
USERNAME
PYTEST_DISABLE_PLUGIN_AUTOLOAD
UV_CACHE_DIR
PYTHONPATH
VIRTUAL_ENV
```

Anything not in this allow-list is silently dropped. This includes:

- `CONDUCTOR_*_GATEWAY_INTERNAL_TOKEN` (the gateway's own internal auth secret)
- `AGW_AUTH__INTERNAL_SECRET`
- `AGW_AUTH__CLOUDFLARE_AUD` / AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN
- Any other `AGW_*` env vars configured on the gateway process
- Any MCP / Skills Gateway auth tokens configured at the gateway level

If a verification command needs a secret (e.g. `GITHUB_TOKEN` for live
E2E), the user / Composer must declare it in the task spec's
`verification.live_e2e.env_required` array. Agents Gateway then:

1. Looks for the variable in `_safe_env()` (where it shouldn't be)
2. If missing, marks the command `blocked=True` with reason
   `missing_credentials: VAR1, VAR2`
3. Creates a `needs_credentials` Composer interaction listing
   `missing_env=[VAR1, ...]`
4. Stops the verification run; transitions the session to
   `blocked_external`

Agents Gateway NEVER auto-injects its own secrets into a subprocess.
The user has to explicitly configure the deployment (e.g. via a
per-task secrets broker, or by re-deploying the gateway with the
required env vars exported to the harness runtimes' allow-list).

## HTML review report redaction

`harness/reports.py:_REDACT_PATTERNS` is a regex list applied to every
chunk of text that goes into the HTML review report (timeline events,
verification outputs, log excerpts, etc.). The replacements use
`[REDACTED]` markers (NOT `<redacted>` — angle brackets get
HTML-escaped by `_esc()` and would render incorrectly).

Currently covered:

- `Authorization` headers in any format (HTTP-form
  `Authorization: Bearer xyz` AND JSON-encoded `"Authorization": "Bearer xyz"`)
- `X-Auth-Internal-Token` headers
- `Cf-Access-Jwt-Assertion` headers
- GitHub `_ghp_/_gho_/_ghs_/_ghu_/_ghr_/_gho_` prefixed tokens with 16+
  alphanumeric characters after the prefix
- Generic `token=` / `secret=` assignments with 8+ alphanumeric chars
  (catches common mis-formed secrets)
- URL credentials: `https://user:pass@host` → `https://<host>: [REDACTED]@host`

If you spot a token leaking into a report, file an issue — the regex
list is a moving target and evolves with the secret formats you see in
production.

## Authentication modes

The gateway supports three auth modes; consult the README + docs/architecture.md
for the full matrix. In summary:

- `dev-none` — open. Refused in production (`AGW_ENV=production`) per
  `auth.py:_assert_production_safe`.
- `internal-only` — `X-Auth-Internal-Token` header compared with
  `secrets.compare_digest`. Safe for service-to-service behind a
  reverse proxy / cloudflared.
- `cloudflare-access` — `Cf-Access-Jwt-Assertion` header verified via
  JWKS. RS256 only; audience + issuer + expiration enforced. Defense
  in depth.

## Production deployment checklist

- [ ] `AGW_ENV=production`
- [ ] `AGW_AUTH__MODE=cloudflare-access`
- [ ] `AGW_AUTH__CLOUDFLARE_TEAM_DOMAIN` set
- [ ] `AGW_AUTH__CLOUDFLARE_AUD` set
- [ ] Cloudflare Access application created at the edge
- [ ] `AGW_HARNESS__ARTIFACTS_ROOT` on a persistent volume (artifacts are large)
- [ ] `AGW_HARNESS__WORKTREE_ROOT` on a writable volume
- [ ] `tmux` installed and accessible as `tmux` on PATH
- [ ] `git` installed (≥ 2.20 for `-C` flag, branch args)
- [ ] `gh` CLI installed if `AGW_HARNESS__AUTO_PR=true`
- [ ] `AGW_HARNESS__USE_FAKE_TMUX=false` (default)
- [ ] `AGW_AUTH__INTERNAL_SECRET` rotated (if using internal-only)
- [ ] `AGW_HARNESS__RELAY_MAX_TIME_SECONDS` reviewed (default 3600 = 1 hour)
- [ ] `AGW_HARNESS__MAX_VERIFY_ITERATIONS` reviewed (default 50)
- [ ] `AGW_SERVICE__RATE_LIMITING__ENABLED=true` (or rely on edge)
- [ ] No LLM API keys configured as plain `AGW_*` env vars (use a per-task secrets broker instead — see `_safe_env`)

## Submitting security-sensitive issues

File a private disclosure rather than an open issue if you've found
something that escapes the `_safe_env` filter or appears in an HTML
report. The current security boundary is regex-based and brittle.

## What this milestone does NOT claim to defend

- The harness process running on the host has full read access to
  anything the gateway user can read. Containerized harness sessions
  are next, not yet.
- The harness's own LLM API calls bypass Agents Gateway entirely —
  they're the harness process's own network egress. Egress controls
  (network policy, firewall, sidecar) are operator responsibility.
- Multi-tenant deployments are out of scope for this build; treat
  the gateway itself as trusted infrastructure managed by a single
  operator.
- The `harness_session` runtime is now a first-class
  `RuntimeRegistry` entry (`HarnessSessionRuntimeAdapter`) and its
  profiles are registered in `AgentCatalog`. All the safety checks
  that apply to legacy runtimes (risk-level gating, manifest
  validation, registry dispatch) now apply to harness tasks too.
  Composer still owns intent (which profile + repo + skills to
  invoke) but the gateway is no longer bypassed.
