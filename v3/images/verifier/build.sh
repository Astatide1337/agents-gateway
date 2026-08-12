#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../../.." && pwd)
image=${AGW_VERIFIER_IMAGE:-agw-verifier:dev}

exec docker build \
  --pull \
  --file "$repo_root/v3/images/verifier/Containerfile" \
  --tag "$image" \
  "$repo_root"
