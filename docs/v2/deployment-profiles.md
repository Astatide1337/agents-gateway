# Deployment profiles

Agents Gateway has one execution kernel and several deployment profiles. The
standalone profile is the product default. Additional profiles add operational
capabilities without changing agent manifests or making hosted infrastructure
mandatory.

## Standalone: the default

Standalone is for a person or team running its own agents on infrastructure it
owns. It is not a public code-execution service and does not place third-party
workloads on an Astatide-operated machine.

Required components:

- one ordinary Linux host;
- rootless Podman;
- the bundled PostgreSQL and control-plane containers; and
- a generated local-owner token.

The API and console run in Compose. A small systemd runner owns rootless Podman
because the API container must never receive a container-runtime socket. Agent
containers use a read-only root filesystem, a quota-bounded ephemeral
workspace, no Linux capabilities, no privileges, and no direct network. Model
and MCP access is available only through an explicitly configured per-run
broker. Provider and upstream MCP credentials stay outside the sandbox.

### Coolify Git-backed deployment

Coolify's Git-backed Docker Compose resource is an **Application**, not a
Coolify Service. In the existing `Services` project and `production`
environment, create a Docker Compose Application with:

- repository: `Astatide1337/agents-gateway`;
- base directory: `/`;
- compose file: `v2/deploy/compose.coolify.yaml`; and
- the only assigned domain, on `agw-console`: `https://agents.astatide.com:8080`
  (the `:8080` identifies the container port; Coolify's proxy publishes the
  normal HTTPS hostname).

This is a committed deployment profile, not a claim that the live Coolify
resource has already been created or deployed. Live Coolify deployment and
verification remain pending.

The Coolify profile intentionally has no `ports:` mappings. `agw-console`
uses `expose: 8080`, so Coolify's proxy is the only public path; PostgreSQL,
the API, migration job, and host runner socket remain private. The Compose
file also leaves network creation to Coolify so the proxy can attach to the
stack's managed network.

The host-side prerequisites must be completed before the first Coolify
deployment:

1. Install rootless Podman and run
   `v2/scripts/install-standalone.sh` on the Coolify localhost server.
2. Keep the generated `agw-runner` systemd unit active and set
   `AGW_RUNNER_UID` and `AGW_SERVER_UID` to its actual UID, plus
   `AGW_RUNNER_GID` to its actual group ID. The Coolify application needs
   read-only access to `/run/agw-runner`, write access to `/run/agw-broker`,
   and write access to the artifact directory.
3. Set the required database and owner-token variables in Coolify. Keep
   provider tokens runtime-only; never mark them as build variables.
4. Configure the domain on `agw-console` only and verify the other services
   have no domain or host-port mapping.

Migrations are baked into `v2/deploy/Dockerfile.migrate`; Coolify does not
need a source-checkout bind mount at runtime. `postgres-init` and
`agw-migrate` are intentional one-shot services and have no health checks or
restart policy. A successful migration container is expected to be exited;
the API depends on its completion, not on it staying running.

The repository profile stays valid with ordinary Docker Compose. Coolify also
documents an `exclude_from_hc: true` service extension for one-shot jobs, but
that key is not part of the Compose specification and strict Compose parsers
reject it. If the installed Coolify release still includes exited one-shot
containers in its aggregate status, configure that exclusion in Coolify's
resource settings (or upgrade Coolify) before deploying; this review does not
change the Coolify instance.

The Coolify profile is still the owner-operated profile. Coolify's Docker
daemon is not the sandbox boundary and must not receive a Podman socket. The
only runtime bridge to the host runner is the narrowly scoped Unix socket and
broker/artifact directories above. Do not assign a public domain to
`agw-server`, PostgreSQL, or any one-shot service.

No Cloudflare account, Kubernetes cluster, Temporal server, object store, OIDC
provider, Grafana account, dedicated VM, or runner fleet is required.

## Team profile: optional

The team profile adds human identity and shared governance to the same
installation:

- OIDC and scoped service accounts;
- organization/project roles and PostgreSQL row-level security;
- approval policies, quotas, retention, and audit export; and
- optional stronger gVisor isolation for mutually untrusted submitters.

Selecting team features makes their dependencies mandatory and readiness fails
closed if they are incomplete. Merely installing standalone does not start or
configure them.

## Distributed profile: optional

The distributed profile is for operators that need independent scaling or
failure domains:

- Temporal instead of the embedded PostgreSQL run engine;
- remote mTLS runners with authoritative lease fencing;
- S3-compatible artifacts;
- external OTLP telemetry; and
- gVisor or microVM execution workers.

Kubernetes can host these components but is an adapter, not the product API or
the default installation. A deployment remains supportable on ordinary VMs.

## Hosted multi-tenant profile: future and explicit

Running code for unrelated or hostile customers is a different product and
threat model. Rootless Podman is not advertised as that security boundary. A
hosted profile requires per-run microVM isolation, hardened broker egress,
tenant quotas, abuse controls, authoritative cross-runner fencing, and a much
larger adversarial test suite. None of those requirements are imposed on a
self-hosting owner.

## Capability rule

Every optional capability follows the same rule:

1. the default is absent and has no operational cost;
2. selecting it is explicit configuration;
3. its dependencies become readiness requirements;
4. partial configuration fails closed; and
5. the public manifest and runtime protocol remain portable.

This lets a new user start with the smallest useful system while preserving a
credible path to enterprise deployment when the need is real.
