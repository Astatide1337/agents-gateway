#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
engine=${CONTAINER_ENGINE:-docker}
image=${IMAGE:-${AGW_VERIFIER_IMAGE:-agw-verifier:dev}}

exec "$engine" build \
  --pull \
  --file "$repo_root/v3/images/verifier/Containerfile" \
  --tag "$image" \
  "$repo_root"
