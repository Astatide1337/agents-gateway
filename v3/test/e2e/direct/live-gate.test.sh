#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
gate="$script_dir/live-gate.sh"
test -f "$gate"
bash -n "$gate"

help_output=$(AGW_DIRECT_KUBECTL_BIN=/does/not/exist bash "$gate" --help)
grep -Fq -- '--keep' <<<"$help_output"
grep -Fq -- 'AGW_DIRECT_KEEP=1' <<<"$help_output"

plan_output=$(AGW_DIRECT_KUBECTL_BIN=/does/not/exist bash "$gate" --plan)
grep -Fq -- 'direct-live-gate: PLAN PASS' <<<"$plan_output"

run_invalid_prefix() {
  local prefix=$1 expected=$2 output status
  set +e
  output=$(
    AGW_DIRECT_KUBECTL_BIN=/does/not/exist \
      AGW_DIRECT_NAMESPACE_PREFIX="$prefix" \
      bash "$gate" --plan 2>&1
  )
  status=$?
  set -e
  test "$status" -eq 1
  grep -Fq -- "$expected" <<<"$output"
}

run_invalid_prefix 'agw-direct/unsafe' 'namespace prefix must be a lowercase DNS label'
run_invalid_prefix 'agw-directevil' 'namespace prefix must be agw-direct or start with agw-direct-'
run_invalid_prefix 'agw-direct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' 'generated success namespace is longer than 63 characters'

printf '%s\n' 'direct live-gate script tests: PASS'
