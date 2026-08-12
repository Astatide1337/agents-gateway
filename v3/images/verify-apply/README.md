# `agw-verify-apply`

This is the credential-free, offline verify init image. It receives only the
workspace and a closed JSON contract. It:

- checks the fresh checkout is exactly the recorded base SHA;
- reads the bounded patch and verifies its exact SHA-256 digest and byte size;
- runs `git apply --check -- <patch>`; and
- runs `git apply --whitespace=error -- <patch>` and verifies the base SHA again.

It has no Secret mount, no service-account token, no broker, no network
credential, and an environment that disables Git configuration, credential
helpers, prompts, and protocols. The `lockdown` init container runs after this
image and before the repository's verifier starts.

## Build

Run from the repository root with digest-pinned build inputs in CI. The build
context must contain `v3/`:

```bash
cd /home/ubuntu/Projects/agents-gateway
docker build --file v3/images/verify-apply/Containerfile \
  --build-arg GO_IMAGE=docker.io/library/golang:1.26.5@sha256:<recorded-digest> \
  --build-arg RUNTIME_IMAGE=docker.io/library/alpine:3.22.1@sha256:<recorded-digest> \
  --tag ghcr.io/astatide/agw-verify-apply:build .
```

Publish the image, resolve its OCI manifest digest, and configure only the
digest-pinned reference. Tags are not accepted by `verifyworkload`.

## Verification

```bash
go test ./cmd/agw-verify-apply
go test -race ./cmd/agw-verify-apply
go vet ./cmd/agw-verify-apply
```

Cluster-level offline enforcement remains a Phase-0 acceptance check: prove
that the network is denied after `fetch -> apply -> lockdown` and that the
verifier cannot modify the root-owned base tree.
