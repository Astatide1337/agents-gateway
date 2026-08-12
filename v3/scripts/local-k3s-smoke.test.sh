#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
smoke="$script_dir/local-k3s-smoke.sh"
test -f "$smoke"
bash -n "$smoke"

help_output=$(bash "$smoke" --help)
grep -Fq -- '--phase0' <<<"$help_output"
grep -Fq -- '--install-upstream' <<<"$help_output"
grep -Fq -- '--agentgateway-chain' <<<"$help_output"
grep -Fq -- 'default: disabled' <<<"$help_output"

plan_output=$(bash "$smoke" --plan)
grep -Fq -- 'phase0:      disabled' <<<"$plan_output"
grep -Fq -- 'phase0 upstream install: disabled' <<<"$plan_output"
grep -Fq -- 'agentgateway chain: disabled' <<<"$plan_output"

invalid_output=$(mktemp "${TMPDIR:-/tmp}/agw-local-k3s-smoke-test.XXXXXX")
trap 'rm -f -- "$invalid_output"' EXIT
set +e
bash "$smoke" --plan --install-upstream >"$invalid_output" 2>&1
status=$?
set -e
test "$status" -eq 2
grep -Fq -- '--install-upstream requires --phase0' "$invalid_output"

set +e
bash "$smoke" --apply --yes --phase0 --agentgateway-chain >"$invalid_output" 2>&1
status=$?
set -e
test "$status" -eq 2
grep -Fq -- '--agentgateway-chain cannot be combined with --phase0' "$invalid_output"

set +e
bash "$smoke" --apply --yes --install-upstream >"$invalid_output" 2>&1
status=$?
set -e
test "$status" -eq 2
grep -Fq -- '--install-upstream requires --phase0' "$invalid_output"

# Keep the forwarding contract visible to this script-level regression test.
grep -Fq -- 'phase0_args+=(--install-upstream)' "$smoke"
grep -Fq -- '"${phase0_args[@]}"' "$smoke"

printf '%s\n' 'local-k3s-smoke script tests: PASS'
