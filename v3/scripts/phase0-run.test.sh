#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
runner="$script_dir/phase0-run.sh"
test -f "$runner"
bash -n "$runner"

help_output=$(bash "$runner" --help)
grep -Fq -- '--apply --yes' <<<"$help_output"
grep -Fq -- '--cleanup --yes' <<<"$help_output"
grep -Fq -- 'deletes a namespace' <<<"$help_output"
grep -Fq -- '--install-upstream' <<<"$help_output"
grep -Fq -- '--require-digests' <<<"$help_output"
grep -Fq -- '--server-side' "$script_dir/phase0-sandbox.sh"
grep -Fq -- '--field-manager=' "$script_dir/phase0-sandbox.sh"
grep -Fq -- 'upstream-crd.txt' "$script_dir/phase0-sandbox.sh"

output_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-phase0-run-test.XXXXXX")
trap 'find -- "$output_dir" -depth -delete' EXIT
run_id='phase0-run-test-123'
bash "$runner" --plan --run-id "$run_id" --namespace agw-phase0-test --output-dir "$output_dir" >"$output_dir/stdout" 2>"$output_dir/stderr"

test -f "$output_dir/$run_id/orchestrator.lock"
test -f "$output_dir/$run_id/summary.tsv"
test "$(wc -l <"$output_dir/$run_id/summary.tsv" | tr -d ' ')" -eq 6
for phase in inventory userns airlock credentials sandbox objectstore; do
  grep -Fq "${phase}"$'\t0\t' "$output_dir/$run_id/summary.tsv"
done

set +e
bash "$runner" --cleanup --yes --context never-used --run-id "$run_id" --namespace wrong --output-dir "$output_dir" >"$output_dir/cleanup.stdout" 2>"$output_dir/cleanup.stderr"
status=$?
set -e
test "$status" -eq 2
grep -Fq 'cleanup namespace does not match its marker' "$output_dir/cleanup.stderr"

set +e
bash "$runner" --cleanup --yes --context wrong-context --run-id "$run_id" --namespace agw-phase0-test --output-dir "$output_dir" >"$output_dir/context.stdout" 2>"$output_dir/context.stderr"
status=$?
set -e
test "$status" -eq 2
grep -Fq 'cleanup context does not match its marker' "$output_dir/context.stderr"

set +e
bash "$runner" --plan --install-upstream --run-id "$run_id" --namespace agw-phase0-test --output-dir "$output_dir" >"$output_dir/install.stdout" 2>"$output_dir/install.stderr"
status=$?
set -e
test "$status" -eq 2
grep -Fq -- '--install-upstream requires --apply --yes' "$output_dir/install.stderr"

set +e
bash "$runner" --plan --require-digests --run-id phase0-digest-test-123 --namespace agw-phase0-test --output-dir "$output_dir" >"$output_dir/digests.stdout" 2>"$output_dir/digests.stderr"
status=$?
set -e
test "$status" -ne 0
grep -Fq 'must be an immutable image@sha256' "$output_dir/digests.stderr"

printf '%s\n' 'phase0-run script tests: PASS'
