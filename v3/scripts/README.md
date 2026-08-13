# Local v3 validation

Run the local CI-equivalent entrypoint from the `v3/` directory:

```bash
./scripts/validate-local.sh
```

The default run is intentionally local and serialized where Go work is
expensive. It performs:

- `gofmt` and whitespace checks, including untracked local v3 files;
- unit tests, race tests with Go package parallelism set to `1`, and `go vet`;
- the repository's reachable-dependency `govulncheck` scan;
- Bash syntax checks;
- the Helm chart's static contracts (plus `helm lint/template` when Helm is
  installed);
- the fail-closed trusted phase-supervisor deployment contract;
- every `v3/images/*/validate.sh` contract check;
- both Phase-0 static guard suites and all six Phase-0 scripts in `--plan`
  mode, plus the safe `phase0-run.sh` orchestration-wrapper interface test;
- controller-gen CRD output compared from a temporary directory, without
  rewriting `v3/config/crd` (the hand-owned `kustomization.yaml` index is
  excluded from the generated-file comparison); and
- every production Go command from the Makefile plus command entrypoints
  referenced by image build inputs into a temporary directory.

The default run does not call a Kubernetes API, create a Kind cluster, submit
an Argo workflow, invoke GitHub Actions or a GitHub API, push an image, or run
Docker/Podman cleanup. Phase-0 is explicitly forced into plan mode. Temporary
outputs are removed on exit.

The phase-supervisor check is intentionally negative: it proves that the work
pod keeps a private PID namespace, receives no phase credential, the chart
rejects enablement, and the lifecycle image has no unbound supervisor mode.
See [`docs/v3/trusted-phase-supervisor-contract.md`](../../docs/v3/trusted-phase-supervisor-contract.md)
for the live prerequisites before this guard may be changed.

For target-node Phase-0 evidence, add `--require-digests` to the shared
`phase0-run.sh` invocation. The wrapper forwards it to every probe and rejects
the default mutable probe tags before creating or dry-running resources. This
strict mode is opt-in so local plan-mode smoke remains lightweight; a target
run should also provide the actual cluster CIDRs and an absent disposable
namespace as described in the completion audit.

## Optional image builds

Image builds are opt-in:

```bash
./scripts/validate-local.sh --build-image operator
./scripts/validate-local.sh --build-image runtime-codex --build-image runtime-claude
./scripts/validate-local.sh --build-images
```

List the supported names with:

```bash
./scripts/validate-local.sh --list-images
```

Inspect the production command set with:

```bash
./scripts/validate-local.sh --list-commands
```

The selected engine is `docker` by default and can be changed with
`CONTAINER_ENGINE=podman`. Local tags use the
`agents-gateway-v3-local-<name>:validation` shape. The entrypoint deliberately
does not pass `--pull`, does not push, and does not prune; the runtime helper
scripts may resolve the current Codex/Claude package from npm when building.

## Fast local image proof

The first direct Codex AgentRun must not wait for the release workflow. Build
only the images that the direct backend actually consumes:

```bash
./scripts/build-local-proof-images.sh \
  --output /tmp/agw-v3-proof-images.env
```

This builds 12 amd64 images: the operator, direct-backend helpers, preflight,
the Codex runtime, and the verifier. It deliberately does not build Claude,
the optional critic, or the Argo lifecycle image. It performs only each image's
contract validator; it does not invoke GitHub Actions, QEMU, a registry push,
Trivy, SBOM generation, Cosign, or cleanup/prune commands.

The generated map contains local tags and engine image IDs for a disposable
proof cluster. Those values are not production references: the chart and
admission path still require registry-published `@sha256:` image references for
any real deployment.

Use `--image NAME` one or more times to build a smaller subset while iterating,
and `--list` to show the direct-Codex profile. The full fifteen-image,
multi-architecture, signed release remains an explicit operation in
`.github/workflows/v3-release.yml`; it is not an end-to-end test prerequisite.

## Explicit reductions for constrained shells

The required checks run by default. If a local machine is offline or resource
constrained, reductions must be visible in the command line:

```bash
./scripts/validate-local.sh \
  --skip-vulnerability-scan \
  --skip-binary-build
```

The interface test is a small shell test and does not run the full validation:

```bash
./scripts/validate-local.test.sh
```

Image validators are discovered from the tree at runtime. Production commands
start with the Makefile's existing build contract and automatically include
commands referenced by image build inputs, so a new image-backed command such
as `agw-critic` is covered without maintaining a second frozen count. The
entrypoint does not hard-code the current image/command totals.

## Disposable API-server smoke

When local Kind, kubectl, Helm, and a pinned Argo CRD bundle are available, run
the separate API-server check explicitly:

```bash
AGW_ARGO_CRDS_FILE=/path/to/argo-workflows-v4.1.0-crds.yaml \
  ./scripts/local-api-smoke.sh
```

Use `./scripts/local-api-smoke.sh --skip-argo` for the reduced AGW-only check.
It always deletes its uniquely named Kind cluster and does not run the operator
or any workload. See [`docs/v3/local-api-smoke.md`](../../docs/v3/local-api-smoke.md)
for its evidence boundary.
