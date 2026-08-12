#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
validator="$script_dir/validate-local.sh"

test -x "$validator"
bash -n "$validator"
contract_validator="$script_dir/validate-phase-supervisor-contract.sh"
test -x "$contract_validator"
bash -n "$contract_validator"

help_output="$($validator --help)"
for expected in \
  '--build-images' \
  '--build-image NAME' \
  '--list-commands' \
  '--skip-vulnerability-scan' \
  '--skip-phase0' \
  '--skip-crd-drift' \
  '--skip-binary-build'; do
  grep -Fq -- "$expected" <<<"$help_output"
done

image_output="$($validator --list-images)"
grep -Fxq critic <<<"$image_output"
grep -Fxq agent-run-lifecycle <<<"$image_output"
grep -Fxq runtime-codex <<<"$image_output"
grep -Fxq verify-fetch <<<"$image_output"

command_output="$($validator --list-commands)"
grep -Fxq agw-critic <<<"$command_output"
grep -Fxq agw-agent-run-lifecycle <<<"$command_output"
grep -Fxq kubectl-agw <<<"$command_output"

invalid_output=$(mktemp "${TMPDIR:-/tmp}/agw-validate-local-invalid.XXXXXX")
trap 'rm -f -- "$invalid_output"' EXIT
set +e
"$validator" --not-a-real-option >"$invalid_output" 2>&1
status=$?
set -e
test "$status" -eq 2
grep -Fq "unknown option: --not-a-real-option" "$invalid_output"

# Evidence sanitization must not turn a YAML/JSON key separator into invalid
# syntax. Phase-0 evidence is intentionally replayable for inspection only,
# but malformed evidence can hide the original live-probe result.
source "$script_dir/phase0-common.sh"
sanitized_yaml=$(printf '%s\n' 'clone-token: super-secret-value' | phase0_sanitize_stream)
test "$sanitized_yaml" = 'clone-token: "[REDACTED]"'
preserved_field=$(printf '%s\n' 'automountServiceAccountToken: false' | phase0_sanitize_stream)
test "$preserved_field" = 'automountServiceAccountToken: false'
sanitized_json=$(printf '%s\n' '{"api_key":"super-secret-value"}' | phase0_sanitize_stream)
test "$sanitized_json" = '{"api_key":"[REDACTED]"}'

printf '%s\n' 'validate-local script interface tests: PASS'
