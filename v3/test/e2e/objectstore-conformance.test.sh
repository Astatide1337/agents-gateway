#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/../../scripts/objectstore-conformance.sh"
test -f "$RUNNER"
bash -n "$RUNNER"

help_output=$(bash "$RUNNER" --help)
grep -Fq -- 'provider-free object-store release gate' <<<"$help_output"
grep -Fq -- '--race' <<<"$help_output"

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-objectstore-conformance-test.XXXXXX")
trap 'rm -rf -- "$temporary_dir"' EXIT
set +e
bash "$RUNNER" --unexpected-option >"$temporary_dir/stdout" 2>"$temporary_dir/stderr"
status=$?
set -e
test "$status" -eq 2
grep -Fq -- 'unknown argument' "$temporary_dir/stderr"

printf '%s\n' 'objectstore-conformance script tests: PASS'
