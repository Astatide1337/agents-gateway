#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$root_dir/versions.env"

if [[ "${AGENTGATEWAY_IMAGE_DIGEST:-}" =~ ^[0-9a-f]{64}$ ]]; then
  printf '%s@sha256:%s\n' "$AGENTGATEWAY_IMAGE_REPOSITORY" "$AGENTGATEWAY_IMAGE_DIGEST"
  exit 0
fi

if command -v skopeo >/dev/null 2>&1; then
  digest="$(skopeo inspect --raw "docker://$AGENTGATEWAY_IMAGE_TAG" | sha256sum | awk '{print $1}')"
  printf '%s@sha256:%s\n' "$AGENTGATEWAY_IMAGE_REPOSITORY" "$digest"
  printf 'Resolved from the registry manifest; record and review this digest before use.\n' >&2
  exit 0
fi

if command -v docker >/dev/null 2>&1; then
  digest="$(docker buildx imagetools inspect "$AGENTGATEWAY_IMAGE_TAG" --format '{{json .Manifest.Digest}}' 2>/dev/null | tr -d '"' | tail -n 1)"
  if [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf '%s@%s\n' "$AGENTGATEWAY_IMAGE_REPOSITORY" "$digest"
    printf 'Resolved from the registry manifest; record and review this digest before use.\n' >&2
    exit 0
  fi
fi

cat >&2 <<'EOF'
Cannot resolve an immutable agentgateway image reference.
Install skopeo or Docker buildx, then inspect the exact v1.4.1 tag. Do not
replace the placeholder in pod.yaml with the floating v1.4.1 tag.
EOF
exit 2
