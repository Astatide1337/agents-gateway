# Agents Gateway operator image

This is the single long-lived v3 control-plane binary used by the Helm chart.
It runs as UID/GID 65532 with a read-only root filesystem; the chart mounts a
bounded memory-backed `/tmp` for webhook serving certificates and Go runtime
scratch space. The image contains CA certificates and timezone data but no
shell-time configuration, credentials, runtime image, or Kubernetes manifest.
It also carries the immutable upstream cosign v3.1.3 binary used by the
optional verification-attestation lifecycle. The image digest pins that binary
alongside the operator; signing key material is never included in the image.

Build it from the repository root through the wrapper:

```bash
IMAGE=ghcr.io/astatide/agw-operator:build-<revision> \
  v3/images/operator/build.sh
```

Push the image, resolve its registry manifest digest, and configure
`image.repository` plus `image.digest` in the Helm values. A local tag or
floating registry tag is not a release reference.
