# `agw-verify-fetch`

This is the credentialed verify init image. Its only responsibilities are:

1. fetch the exact resolved GitHub commit with a short-lived, read-only
   installation token;
2. create `/verify/workspace/base` from `git archive` without executing files
   from the repository; and
3. retrieve one exact S3-compatible object by its `s3://bucket/key` location,
   enforcing the declared byte count and SHA-256 digest.

The image never accepts HTTPS artifact URLs or presigned URLs. It uses explicit
access-key/secret/session credentials from fixed files, an HTTPS origin, a
no-redirect HTTP client, and a resolver-aware dialer that rejects private,
loopback, link-local, multicast, unspecified, and metadata addresses. It does
not print Git or provider diagnostics; the entrypoint emits only a stable
failure message on stderr.

The Secret volume is mounted only into this init container. The following
fixed files are expected:

| File | Required |
| --- | --- |
| `/run/agw/fetch/clone-token` | yes |
| `/run/agw/fetch/artifact-access-key-id` | yes |
| `/run/agw/fetch/artifact-secret-access-key` | yes |
| `/run/agw/fetch/artifact-session-token` | yes |

The controller-side Secret materializer must populate all three short-lived
`artifactauth` keys. Requiring the session token prevents this path from
silently accepting long-lived static credentials. The legacy v2
`artifact-token` projection is intentionally ignored by this image and is not
a valid compatibility path.

The fetch init runs as namespace UID 0 with all capabilities dropped. It keeps
the mutable clone writable for the UID 1000 apply/verifier steps through
explicit mode bits; it does not require `CAP_CHOWN`. The independent base tree
is created separately, remains namespace-root-owned, and is made read-only.

Artifact credentials are short-lived and scoped read-only to the requesting
run's exact object prefix. Static R2/S3 credentials are not accepted on the
verification path. No credential is copied into the workspace or passed to
the apply/verifier containers.

## Object-size contract

General runtime artifacts, effects, and event objects use the shared
`objectstore.GeneralMaxObjectBytes` limit (64 MiB). Verify-fetch is a separate
read-only path for a controller-authored patch: it defaults to the same 64 MiB
limit, but permits an explicit `verifyfetch.PatchFetchMaxObjectBytes` ceiling
of 1 GiB when a Gate needs it. The patch size, fetch contract, and S3 client
must all carry the same selected value; this larger patch-fetch ceiling is not
accepted for general broker or lifecycle objects.

## Build

Run from the repository root and provide digest-pinned build inputs in CI. The
build context must contain `v3/`:

```bash
cd /home/ubuntu/Projects/agents-gateway
docker build --file v3/images/verify-fetch/Containerfile \
  --build-arg GO_IMAGE=docker.io/library/golang:1.26.5@sha256:<recorded-digest> \
  --build-arg RUNTIME_IMAGE=docker.io/library/alpine:3.22.5@sha256:7c8cb692ae09657cbc4a3f3cbd0e8d5a2690ba38386aaaf252dbb060bf5eb2e6 \
  --tag ghcr.io/astatide/agw-verify-fetch:build .
```

Resolve the pushed OCI manifest digest and use only
`ghcr.io/astatide/agw-verify-fetch@sha256:...` in the operator configuration.
The `verifyworkload` builder rejects a mutable runtime reference.

## Verification

```bash
go test ./internal/verifyfetch ./cmd/agw-verify-fetch
go test -race ./internal/verifyfetch ./cmd/agw-verify-fetch
go vet ./internal/verifyfetch ./cmd/agw-verify-fetch
```

The live airlock, user namespace, DNS/IP policy, and S3/GitHub permissions are
Phase-0 cluster evidence. Local unit tests do not claim to replace those
checks.
