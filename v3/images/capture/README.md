# agw-capture image

This image contains only the trusted `/agw/capture` executable, Git, and CA
certificates. It runs as UID/GID 1000 with a read-only root filesystem in the
capture Job; the work PVC is the only writable mount. One run produces the
exact `patch.diff`, canonical `patch-manifest.json`, canonical result JSON, and
one authenticated `stdout-frame-v2` carrying all three payloads.

Run the build from the repository root; the Dockerfile expects the root context
to contain the `v3/` module:

```bash
cd /home/ubuntu/Projects/agents-gateway
docker build --file v3/images/capture/Dockerfile --tag agw-capture:dev .
# or:
podman build --file v3/images/capture/Dockerfile --tag agw-capture:dev .
```

For a rootless Podman bind-mount smoke test, use `--userns=keep-id`; this maps
the host user's writable workspace to the image's UID/GID 1000. Docker does
not need that flag. Both engines should run the image with `--network=none`,
`--read-only`, `--cap-drop=ALL`, and `--security-opt=no-new-privileges`.

The operator must publish the resulting image by immutable manifest digest and
put that reference in the capture `Options.Image`. The build-time base image
tags are not runtime trust decisions; the Kubernetes contract rejects an image
reference that is not pinned to `@sha256:<digest>`.

The executable accepts the closed `AGW_CAPTURE_*` contract produced by
`internal/capture`, including the explicit `AGW_CAPTURE_RUN_UID`, manifest
staging path, and manifest byte bound. The run UID binds the result envelope to
the AgentRun and is never inferred from pod names, paths, or mutable metadata.

The process has no object-store credentials, service-account token, network
allowance, or shell entrypoint. Git commands use a temporary index and object
directory, `--no-filters`, `--no-ext-diff`, `--no-textconv`, no-renames, disabled
credential/prompt helpers, and `GIT_ALLOW_PROTOCOL=none`. Repository Git
metadata, submodules, changed symlinks, special files, binary content, and
nested Git metadata are rejected before evidence is trusted. Manifest bytes
are produced and round-tripped through the strict canonical publisher helpers.
