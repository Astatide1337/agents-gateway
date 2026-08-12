# Agents Gateway v3 GHCR image release

`.github/workflows/v3-release.yml` is the release boundary for the fifteen v3
container images. It is deliberately separate from CI and is currently
`workflow_dispatch`-only, so ordinary commits and `v3.*` tags cannot consume
GitHub Actions minutes or publish images accidentally. CI proves source and
image contracts; this workflow publishes versioned images and the evidence
needed to deploy them by digest after an explicit release decision.

## What a release does

The workflow accepts only `workflow_dispatch` with an explicit, required
`version` input matching `v3.<major>.<minor>.<patch>` with an optional
prerelease/build suffix, such as `v3.1.0` or `v3.1.0-rc.1`. There is currently
no automatic tag trigger, and the workflow does not create a Git tag or a
GitHub Release.

The version is validated before it is used by Docker or `jq`. It is never
interpolated into shell source, and `latest` is rejected. Each run gets a unique
quarantine tag containing the version and GitHub run identity. The final version
tags do not exist until every image has passed scanning, signing, and
attestation verification. The successful workflow retains the final manifest
as an Actions artifact; creating a GitHub Release is a separate future decision.

Release identities are single-use. Existing GitHub Releases and manually
selected Git tags are rejected, and the promotion job checks all final GHCR
tags before changing any of them. A replay or partial prior promotion fails
instead of overwriting an image or release.

The workflow uses the repository `GITHUB_TOKEN` for GHCR authentication. The
build jobs have only `contents:read`, `packages:write`, and `id-token:write`.
Release jobs are serialized per manual version and are never cancelled once
started.

## Image matrix

Every entry below uses the repository root (`.`) as its Docker build context and
the Dockerfile used by the v3 CI image matrix. The release workflow does not
silently remove a platform: the declared platform set is checked against the
published index and recorded in the release manifest.

| Component | Image name | Dockerfile | Published platforms | Chart value (when chart-owned) |
| --- | --- | --- | --- | --- |
| operator | `agw-operator` | `v3/images/operator/Containerfile` | `linux/amd64` | `image` |
| broker | `agw-broker` | `v3/images/broker/Containerfile` | `linux/amd64` | `runtimeImages.broker` |
| critic | `agw-critic` | `v3/images/critic/Containerfile` | `linux/amd64`, `linux/arm64` | — |
| capture | `agw-capture` | `v3/images/capture/Dockerfile` | `linux/amd64`, `linux/arm64` | `runtimeImages.capture` |
| clone | `agw-clone` | `v3/images/clone/Containerfile` | `linux/amd64`, `linux/arm64` | `runtimeImages.clone` |
| context | `agw-context` | `v3/images/context/Containerfile` | `linux/amd64`, `linux/arm64` | `runtimeImages.context` |
| lockdown | `agw-lockdown` | `v3/images/lockdown/Containerfile` | `linux/amd64`, `linux/arm64` | `runtimeImages.lockdown` |
| preflight | `agw-preflight` | `v3/images/preflight/Containerfile` | `linux/amd64` | `runtimeImages.preflight` |
| runtime-claude | `agw-runtime-claude` | `v3/images/runtime-claude/Containerfile` | `linux/amd64`, `linux/arm64` | Agent runtime reference |
| runtime-codex | `agw-runtime-codex` | `v3/images/runtime-codex/Containerfile` | `linux/amd64`, `linux/arm64` | Agent runtime reference |
| skills | `agw-skills` | `v3/images/skills/Containerfile` | `linux/amd64`, `linux/arm64` | `runtimeImages.skills` |
| agent-run-lifecycle | `agw-agent-run-lifecycle` | `v3/images/agent-run-lifecycle/Containerfile` | `linux/amd64` | — |
| verifier | `agw-verifier` | `v3/images/verifier/Containerfile` | `linux/amd64` | Gate runtime reference |
| verify-apply | `agw-verify-apply` | `v3/images/verify-apply/Containerfile` | `linux/amd64` | `runtimeImages.verifyApply` |
| verify-fetch | `agw-verify-fetch` | `v3/images/verify-fetch/Containerfile` | `linux/amd64` | `runtimeImages.verifyFetch` |

The seven amd64-only entries are an intentional, documented consequence of their
current Dockerfiles containing `GOARCH=amd64`: operator, broker, preflight,
agent-run-lifecycle, verifier, verify-apply, and verify-fetch. The workflow
records this evidence and fails if the registry index differs from the declared
set. Enabling arm64 for any of these images requires making its Dockerfile
consume BuildKit's target architecture and then changing the matrix and
evidence together. An arm64 build failure for a currently dual-platform entry
is release-fatal.

The workflow uses QEMU for arm64 BuildKit stages, BuildKit GHA cache scopes per
component, `provenance: mode=max`, and BuildKit SBOM attestation support. The
runtime images resolve the current npm CLI package version during the build and
pass that exact version to the existing Dockerfile, so the package label and
installed package agree.

## Security and evidence gates

The workflow is digest-first:

1. each build pushes only a unique quarantine tag, never the final version tag;
2. the quarantine reference is normalized to an image index and its digest and
   platform set are recorded;
3. the digest is scanned with the pinned Trivy action and pinned Trivy CLI;
4. the scan fails on fixable `HIGH` or `CRITICAL` OS/library vulnerabilities;
5. SPDX JSON and CycloneDX JSON SBOMs are generated for that digest;
6. Cosign signs the digest and attaches both SBOMs as keyless attestations;
7. the workflow verifies the signature and both attestations against the GitHub
   Actions OIDC issuer and this repository's `v3-release.yml` identity; and
8. only after all fifteen matrix jobs and the quarantine manifest succeed does
   the single promotion job create the final version tags from those exact
   digests.

Promotion checks that every destination tag is absent before changing any tag,
then verifies that each promoted reference resolves to the expected digest and
repeats signature/attestation verification on the final reference. A failed
gate can leave a quarantine tag, but it cannot create a deployable version tag.
The workflow never creates `latest` and never overwrites an existing image tag.

## Release manifest and chart values

The final artifact is one JSON document:

```json
{
  "kind": "AgentsGatewayV3ImageRelease",
  "version": "v3.1.0",
  "components": {
    "operator": {
      "image": "ghcr.io/astatide1337/agw-operator:v3.1.0",
      "imageRepository": "ghcr.io/astatide1337/agw-operator",
      "quarantineImage": "ghcr.io/astatide1337/agw-operator:v3.1.0-quarantine-123456789-1",
      "imageIndexDigest": "sha256:<64 hex characters>",
      "platforms": ["linux/amd64"]
    }
  },
  "chartValues": {
    "image.repository": "ghcr.io/astatide1337/agw-operator",
    "image.digest": "sha256:<64 hex characters>"
  }
}
```

The final document contains all fifteen components and the complete evidence
fields. `chartValues` is a convenience map; it does not mutate the Helm chart.
Every `*.repository` value is the repository only, without `:tag`; every
`*.digest` value is the immutable `sha256:` digest. Copy those pairs into
`v3/charts/agw-operator/values.yaml` or an environment-specific values file and
keep the digest unchanged. The critic, agent-run-lifecycle, and two agent
runtime images are present in `components` even when they are not chart-owned
helper values.

Download the final workflow artifact and treat it as the release record. It
contains the exact image-index digests that were scanned, signed, attested, and
promoted.

## Running and operating it

Before the first release, enable Actions to create/write packages for the
repository and make sure the organization permits the repository's
`GITHUB_TOKEN` to write GHCR packages. No GHCR PAT is required by this workflow.
The workflow itself rejects existing releases, manually selected Git tags, and
existing final package tags as a second line of defense. If a future reviewed
change adds a tag trigger, protect `v3.*` tags before enabling it.

The release workflow does not deploy the Helm chart. A deployment operator must
review the manifest, fill chart values with its digest pairs, and run the chart's
existing preflight and cluster checks separately.

## Local static validation

These commands validate the workflow without publishing, signing, or creating a
release. The release-contract script includes the local static action-safety
check, so this gate does not require `actionlint`, `zizmor`, or a hosted runner.

```bash
cd /home/ubuntu/Projects/agents-gateway

# YAML syntax (Ruby is present on GitHub-hosted Ubuntu runners).
ruby -e 'require "yaml"; YAML.safe_load(File.read(".github/workflows/v3-release.yml"), aliases: true); puts "YAML OK"'

# Confirm the local release contract, including the 15-image inventory, CI /
# release matrix equality, manual-only trigger, immutable action pins,
# quarantine promotion, immutable release behavior, and schema-safe chart
# repositories. This scans every action reference in both v3 workflows.
bash v3/scripts/validate-release-contract.sh

# Confirm the release workflow has fifteen Dockerfile paths.
test "$(rg -c '^            dockerfile:' .github/workflows/v3-release.yml)" -eq 15
while read -r dockerfile; do test -f "$dockerfile"; done < <(sed -n 's/^            dockerfile: //p' .github/workflows/v3-release.yml)
```

The workflow's own `validate` job repeats the important checks on GitHub before
any image build starts. Do not run the build or Cosign steps as a local
substitute for a deliberate release dispatch: those steps intentionally require
GHCR and OIDC permissions.

## Known scope caveat

This change intentionally touches only the release workflow, this document, and
the local release-contract checker.
The current v3 image Containerfiles use reviewed digest-pinned base manifests.
The retained v1/v2 Dockerfiles still include mutable base-image tags; they are
outside this v3 release workflow and should be hardened separately before any
legacy image is promoted. Runtime CLI version resolution is explicit in release
build arguments and the resulting version is recorded in image metadata. The
current arm64 matrix decisions above are also evidence to revisit when those
legacy Dockerfiles are made target-aware.
