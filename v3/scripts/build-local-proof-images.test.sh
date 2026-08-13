#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/build-local-proof-images.sh"

[[ -x "$SCRIPT" ]] || { echo "script is not executable: $SCRIPT" >&2; exit 1; }
bash -n "$SCRIPT"

list=$("$SCRIPT" --list)
expected=$'operator\nclone\nskills\ncontext\nlockdown\nbroker\ncapture\nverify-fetch\nverify-apply\npreflight\nruntime-codex\nverifier'
[[ "$list" == "$expected" ]] || {
  printf 'unexpected direct-codex image profile:\n%s\n' "$list" >&2
  exit 1
}

help=$($SCRIPT --help)
grep -F -- '--image NAME' <<<"$help" >/dev/null
grep -F -- 'does not push, scan, sign, attest, or prune' <<<"$help" >/dev/null

if "$SCRIPT" --image runtime-claude >/dev/null 2>&1; then
  echo 'runtime-claude unexpectedly accepted by direct-codex profile' >&2
  exit 1
fi

echo 'build-local-proof-images: static contract: PASS'
