# Agents Gateway v2 deployment

This directory contains deployment contracts and no credentials.

## Product profiles

Agents Gateway has two supported architecture profiles:

- **Standalone (default target):** bundled PostgreSQL, local owner token,
  local durable run engine, local artifacts, and a host-installed rootless
  Podman runner connected by a Unix socket.
- **Distributed (optional):** managed or self-hosted PostgreSQL, Temporal,
  S3-compatible artifacts, OIDC, external OTLP, and remote mTLS runners.

The standalone code path includes local PostgreSQL orchestration, local
artifacts, per-run model/tool/skill brokers, the Codex adapter, and the Unix
runner transport. The separate `compose.yaml` remains the distributed
integration profile and uses the bundled Temporal development service when its
bundled profile is selected. The exact evidence boundary is tracked in
[`../../docs/v2/implementation-status.md`](../../docs/v2/implementation-status.md).

The owner-operated rootless-Podman E2E, enforced tmpfs quota, opaque secret
materialization, connected Skills Gateway test, connected MCP/audit test, and
fault-injection suite are verified. The combined real-provider local-engine
chain and live Coolify deployment remain pending.

## Current integration profile

```sh
cp v2/deploy/.env.example v2/deploy/.env
# Fill every blank secret. AGW_AUTH_TOKEN must be at least 32 random chars.
docker compose -f v2/deploy/compose.yaml --env-file v2/deploy/.env \
  --profile bundled config
docker compose -f v2/deploy/compose.yaml --env-file v2/deploy/.env \
  --profile bundled up -d
```

PostgreSQL and Temporal have no host ports. Expose only
`agw-console:8080` through Coolify or a loopback port. The console proxies
`/api/` to `agw-server`; never publish PostgreSQL, Temporal, a runtime socket,
or the OTLP collector.

The bundled Temporal service uses `temporal server start-dev`. It is only an
integration fixture. The optional distributed profile must use a supported
Temporal deployment or Temporal Cloud. The standalone Compose profile uses
the local engine and does not start Temporal.

Migrations are one-shot and checksummed. For a managed database, use the
`migrate` profile with a deployment-only `AGW_MIGRATION_DATABASE_URL`; never
pass that privileged URL to the application.

## Standalone runner

The runner is a host service because neither the web control plane nor an
agent sandbox may receive a Podman/Docker/containerd socket. This does not
mean the user must provision another VM. The ordinary standalone installation
runs the unprivileged runner on the same Linux machine.

Prerequisites:

- Linux with cgroups v2 and unprivileged user namespaces.
- Rootless Podman.
- A dedicated non-login `agw-runner` identity with subordinate UID/GID
  ranges.
- `/var/lib/agw-runner` owned by that identity and a private
  `/run/agw-runner` runtime directory.

Use [`runner.standalone.yaml.example`](runner.standalone.yaml.example) as
`/etc/agw/runner.yaml`, install the release binary at
`/usr/local/bin/agw-runner`, and install the supplied systemd unit. The unit
enables cgroup delegation and leaves namespace creation available to rootless
Podman while retaining non-root execution and filesystem protections. The
trusted runner itself must be able to invoke `newuidmap`/`newgidmap`; every
untrusted sandbox separately enforces `no-new-privileges` and drops all
capabilities.

The installer also enables systemd lingering for the dedicated account and
configures Podman's `systemd` cgroup manager. Its canonical
`/run/user/<uid>` runtime directory is used only for the rootless systemd
session; the application-facing runner socket remains at
`/run/agw-runner/runner.sock`. This is required for CPU, memory, and PID
limits to be enforced without an interactive login.

The supported installer is `v2/scripts/install-standalone.sh`. It generates the
runner config and unit, validates both before installation, and refuses to
overwrite an existing differing file by default. This protects a manually
customized host from an accidental installer run. An existing installation
that needs the generated `brokerRoot` or another managed-file update can opt in
explicitly:

```sh
sudo AGW_UPDATE_EXISTING=1 \
  AGW_UPDATE_BINARY=1 \
  AGW_RUNNER_BINARY=/usr/local/bin/agw-runner \
  v2/scripts/install-standalone.sh
```

Update mode still validates the generated runner config with `agw-runner
doctor` and the systemd unit with `systemd-analyze` before changing anything.
Each differing existing managed file is copied byte-for-byte to a private
same-directory `.backup.*` file before replacement. Re-running the command is
idempotent when the generated files already match. The standalone env file is
stored at `/etc/agw/standalone.env` by default and is never regenerated or
rewritten during an update, so existing owner and
database secrets remain unchanged. Review and remove backups only after the
new runner has been checked; they may contain the previous unit/config state.
`AGW_UPDATE_BINARY=1` similarly rebuilds from the checksummed module and
atomically replaces the binary only when its bytes differ.

```sh
sudo loginctl enable-linger agw-runner
printf 'XDG_RUNTIME_DIR=/run/user/%s\n' "$(id -u agw-runner)" \
  | sudo tee /etc/agw/runner-service.env >/dev/null
sudo chmod 0644 /etc/agw/runner-service.env
sudo install -o root -g root -m 0644 \
  v2/deploy/systemd/agw-runner.service \
  /etc/systemd/system/agw-runner.service
sudo systemctl daemon-reload
sudo systemctl enable --now agw-runner.service
sudo -u agw-runner /usr/local/bin/agw-runner doctor \
  --config /etc/agw/runner.yaml
```

The standalone worker connects only to
`/run/agw-runner/runner.sock`. The directory must be `0750` or stricter and
the socket is created as `0660`. The client has no TCP fallback. A read-only
bind mount of that directory gives the worker access without exposing the
Podman socket.

Rootless Podman is an owner-operated production profile with the explicit
isolation grade `standard/shared-kernel`. It protects the host from ordinary
untrusted generated code, but it is not a microVM and is not advertised as a
hostile public multi-tenant execution boundary.

The installer and live standalone script require Linux, cgroups v2,
user-namespace support, `newuidmap`/`newgidmap`, and rootless Podman. The live
standalone E2E has passed under the dedicated service identity with concurrent
sandboxes, adversarial host/network checks, enforced CPU/memory/PID and tmpfs
disk limits, opaque secret materialization, and cleanup. A clean-host repeat is
still recommended before a general release.

When an agent selects a model route, tool set, or skill set, the local engine
creates a short-lived broker session. The sandbox receives only scoped session
files and exact read-only skill staging; provider and Skills Gateway
credentials remain outside the sandbox. An environment entry must use an
opaque `secret://<id>` reference. The standalone installer creates the
runner-owned `0700` source directory `/var/lib/agw-runner/secrets`; each ID is
the filename of one `0600` file in that directory. At run time the trusted
runner resolves that file and creates a private per-run `0600` env file below
`/run/agw-runner/materialized`. The env file is passed to the sandbox and
removed on every terminal path. Secret values are not put in manifests,
events, audit records, errors, or logs.

The model boundary supports explicit `openai`/`openai-responses` and
`openrouter`/`openrouter-responses` providers. The default endpoints are
`https://api.openai.com/v1/responses` and
`https://openrouter.ai/api/v1/responses`. `OPENAI_API_KEY` and
`OPENROUTER_API_KEY` are resolved at the host-side broker boundary; they are
not available inside the sandbox. The combined local-engine run against a
real provider is still pending.

The console is live by default. Its owner connection screen accepts the local
bearer token through an in-memory runtime provider; it does not persist the
token in browser storage, URLs, cookies, or build output. Demo fixtures require
explicit `VITE_AGW_MODE=demo`.

## Optional distributed runner

Set `AGW_RUNNER_TRANSPORT=mtls`, then provide:

- `AGW_RUNNER_ENDPOINT`
- `AGW_RUNNER_TLS_SERVER_NAME`
- `AGW_RUNNER_TLS_CA_FILE`
- `AGW_RUNNER_TLS_CERT_FILE`
- `AGW_RUNNER_TLS_KEY_FILE`

The endpoint is HTTPS-only, TLS 1.3, and client-certificate authenticated.
Certificate variables are mounted file paths, never certificate contents.
Select containerd+gVisor if the operator needs enhanced shared-runner
isolation. A future microVM backend is required before making hostile public
multi-tenant claims.

## Authentication

`AGW_AUTH_MODE=local` is the standalone default. It accepts one randomly
generated bearer token of at least 32 characters and grants that local owner
instance administration. Keep the API private or behind TLS and store the
token only in deployment secrets. The web console holds the token only in
memory for the current browser session and sends it through its runtime API
client.

`AGW_AUTH_MODE=oidc` is optional. It validates issuer, audience, signature,
algorithm, expiry, and subject. Cloudflare Access can be used by configuring
its issuer, application AUD, and `Cf-Access-Jwt-Assertion` header. OIDC mode
also enables membership lookup and short-lived service-account exchange; it
requires the separate narrow authorization database role and an Ed25519
service-token signing key.

## Optional observability and artifacts

The control plane emits structured logs without an OTLP dependency. Set
`AGW_TELEMETRY_DISABLED=true` for the minimal local install. To export OTLP,
start the collector profile or point directly at a compatible managed
endpoint. Grafana Cloud is one option, not a product dependency.

The standalone target uses a bounded local artifact directory and the local
run-output and agent-authored upload paths are wired through the per-run broker.
Published versions are stored in the tenant-scoped catalog and available in the
authenticated console workspace. S3/R2 remains optional. MinIO is a
disconnected development profile and is not evidence of an S3-compatible
production path.

## Image policy

Control-plane images follow the repository policy of current stable releases;
promotion therefore requires CI/E2E and retention of the previous image digest
for rollback. Agent runtime images are different: manifests must pin an exact
`sha256` digest because those images execute inside the untrusted sandbox.
