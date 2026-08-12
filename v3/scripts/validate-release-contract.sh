#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

release_workflow=".github/workflows/v3-release.yml"
ci_workflow=".github/workflows/v3-ci.yml"
release_docs="docs/v3/image-release.md"

test -f "$release_workflow" || { echo "missing v3 release workflow" >&2; exit 1; }
test -f "$ci_workflow" || { echo "missing v3 CI workflow" >&2; exit 1; }

# Keep hosted execution opt-in. This guard is intentionally local and does not
# require actionlint or zizmor: every v3 workflow action must name an immutable
# commit and carry a same-line human-readable version comment.
grep -Eq '^  workflow_dispatch:' "$release_workflow" || {
  echo "v3 release workflow must expose workflow_dispatch" >&2
  exit 1
}
if grep -Eq '^  push:' "$release_workflow"; then
  echo "v3 release workflow must not have an automatic push trigger" >&2
  exit 1
fi
sed -n '/^  workflow_dispatch:/,/^permissions:/p' "$release_workflow" | grep -Fq '        required: true' || {
  echo "v3 release version input must remain required" >&2
  exit 1
}

action_pattern='^[[:space:]]+uses:[[:space:]]+[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-fA-F]{40}[[:space:]]+#[[:space:]]+v[0-9][0-9A-Za-z._-]*[[:space:]]*$'
mapfile -t v3_workflows < <(
  rg --files .github/workflows | rg '(^|/)v3[^/]*\.ya?ml$' | sort || true
)
test "${#v3_workflows[@]}" -gt 0 || { echo "no v3 workflows found" >&2; exit 1; }
for workflow in "${v3_workflows[@]}"; do
  # v3 hosted execution is deliberately opt-in. Keep this guard broad enough
  # to catch a future trigger added under a different GitHub event name, while
  # still allowing the single supported manual entrypoint.
  grep -Eq '^  workflow_dispatch:' "$workflow" || {
    echo "v3 workflow must expose workflow_dispatch: $workflow" >&2
    exit 1
  }
  if rg -n '^  (push|pull_request|pull_request_target|schedule|repository_dispatch|workflow_run|workflow_call|workflow_dispatches):' "$workflow" | rg -v '^.*workflow_dispatch:'; then
    echo "v3 workflow contains an automatic or externally callable trigger: $workflow" >&2
    exit 1
  fi
  mapfile -t action_lines < <(rg '^[[:space:]]+uses:' "$workflow" || true)
  test "${#action_lines[@]}" -gt 0 || {
    echo "no GitHub Action references found in $workflow" >&2
    exit 1
  }
  for action_line in "${action_lines[@]}"; do
    # Local composite actions are not remote GitHub Action references. Any
    # remote owner/repository reference must satisfy the immutable pattern.
    [[ "$action_line" == *'uses: ./'* ]] && continue
    if [[ ! "$action_line" =~ $action_pattern ]]; then
      echo "un-pinned GitHub Action or missing adjacent version comment in $workflow:" >&2
      echo "  $action_line" >&2
      exit 1
    fi
  done
done

mapfile -t release_paths < <(
  sed -n '/^  build:/,/^  manifest:/ { s/^            dockerfile: //p; }' "$release_workflow"
)
mapfile -t ci_paths < <(
  sed -n '/^  images:/,/^  kubernetes-api:/ { s/^            dockerfile: //p; }' "$ci_workflow"
)
mapfile -t release_components < <(
  sed -n '/^  build:/,/^  manifest:/ { s/^          - component: //p; }' "$release_workflow"
)
mapfile -t doc_components < <(
  sed -n 's/^| \([a-z0-9-]*\) | `agw-[a-z0-9-]*` |.*/\1/p' "$release_docs"
)

test "${#release_paths[@]}" -eq 15 || { echo "release matrix must contain exactly 15 images" >&2; exit 1; }
test "${#ci_paths[@]}" -eq 15 || { echo "CI matrix must contain exactly 15 images" >&2; exit 1; }
test "${#release_components[@]}" -eq 15 || { echo "release components must contain exactly 15 images" >&2; exit 1; }
test "${#doc_components[@]}" -eq 15 || { echo "release docs must list exactly 15 images" >&2; exit 1; }

if ! diff -u <(printf '%s\n' "${release_paths[@]}" | sort) <(printf '%s\n' "${ci_paths[@]}" | sort); then
  echo "release and CI image inventories differ" >&2
  exit 1
fi
if ! diff -u <(printf '%s\n' "${release_components[@]}" | sort) <(printf '%s\n' "${doc_components[@]}" | sort); then
  echo "release workflow and release docs component inventories differ" >&2
  exit 1
fi

for dockerfile in "${release_paths[@]}"; do
  test -f "$dockerfile" || { echo "missing release Dockerfile: $dockerfile" >&2; exit 1; }
done

grep -Fq 'tags: ${{ env.IMAGE_NAMESPACE }}/${{ matrix.image }}:${{ env.QUARANTINE_TAG }}' "$release_workflow" || {
  echo "release build does not publish a quarantine tag" >&2
  exit 1
}
if grep -Fq 'tags: ${{ env.IMAGE_NAMESPACE }}/${{ matrix.image }}:${{ env.VERSION }}' "$release_workflow"; then
  echo "release build still publishes the final version tag" >&2
  exit 1
fi
if grep -Fq -- '--clobber' "$release_workflow" || grep -Fq 'gh release upload' "$release_workflow"; then
  echo "release workflow permits release-asset overwrites" >&2
  exit 1
fi
grep -Fq $'  promote:\n' "$release_workflow" || { echo "promotion job is missing" >&2; exit 1; }
grep -Fq 'group: agw-v3-release-${{ inputs.version }}' "$release_workflow" || { echo "release concurrency is not keyed by version" >&2; exit 1; }
grep -Fq 'needs: [prepare, manifest]' "$release_workflow" || { echo "promotion dependency is missing" >&2; exit 1; }
grep -Fq 'name: agw-v3-release-manifest-${{ needs.prepare.outputs.version }}' "$release_workflow" || {
  echo "final manifest artifact is missing from promotion" >&2
  exit 1
}
if grep -Eq '\.\[\$row\.chartValues\.repositoryKey\] = \$row\.image($|[^A-Za-z])' "$release_workflow"; then
  echo "chart repository generation still uses a tagged image" >&2
  exit 1
fi
grep -Fq '.[$row.chartValues.repositoryKey] = $row.imageRepository' "$release_workflow" || {
  echo "chart repository generation must use imageRepository" >&2
  exit 1
}

repository="ghcr.io/astatide1337/agw-operator"
repository_pattern="$(jq -r '.properties.image.properties.repository.pattern' v3/charts/agw-operator/values.schema.json)"
[[ "$repository" != *:* ]] || { echo "chart repository sample contains a tag" >&2; exit 1; }
[[ "$repository" =~ $repository_pattern ]] || { echo "chart repository sample violates the Helm schema" >&2; exit 1; }
grep -Fq 'quarantine' "$release_docs" || { echo "release docs do not describe quarantine promotion" >&2; exit 1; }

echo "release contract: 15-image CI/release inventory, quarantine promotion, immutable releases, and schema-safe repositories: OK"
