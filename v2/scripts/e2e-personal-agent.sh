#!/usr/bin/env bash
# Personal AgentBundle acceptance path.
#
# Static mode validates the bundle compiler tests and probes `agw run -f`
# through compilation without contacting a real server. Live mode additionally
# applies the credential references, launches a real AgentBundle run, checks
# status/events/logs and primary/supporting artifacts, exercises cancellation,
# and verifies optional local sandbox cleanup.
#
# This script does not edit Go/TypeScript code, create secrets, print tokens,
# or change the existing low-level E2E scripts.
set -Eeuo pipefail
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
v2_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
repo_dir=$(CDPATH= cd -- "$v2_dir/.." && pwd)
example_file="$v2_dir/examples/personal-agent.yaml"

fail() {
  printf 'e2e-personal-agent: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing prerequisite: $1"
}

require_value() {
  local name=$1 value=${!1-}
  [[ -n $value ]] || fail "$name is required"
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || fail "$name contains a forbidden control character"
}

valid_identifier() {
  [[ $1 =~ ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$ ]]
}

valid_digest() {
  [[ $1 =~ ^sha256:[a-f0-9]{64}$ ]]
}

valid_image() {
  [[ $1 =~ ^[A-Za-z0-9._:/-]+@sha256:[a-f0-9]{64}$ ]] || return 1
  valid_digest "${1##*@}"
}

valid_url() {
  [[ $1 == https://* || $1 == http://localhost* || $1 == http://127.0.0.1* ]] || return 1
  [[ $1 != *\?* && $1 != *#* && $1 != */ ]] || return 1
  case "$1" in
    *[[:space:]\"\'\|\&]*) return 1 ;;
  esac
}

need_command awk
need_command cmp
need_command cp
need_command curl
need_command date
need_command grep
need_command jq
need_command mktemp
need_command sed
need_command sleep

[[ -f $example_file ]] || fail "personal example is missing: $example_file"

default_runtime_image=ghcr.io/astatide/agents-gateway-runtime-codex@sha256:0000000000000000000000000000000000000000000000000000000000000000
default_skill_ref=agent-manager/matt-pocock/codebase-design
default_skill_digest=sha256:d77fbf5456e10c0d323312b65ca54d462f975ab2e5867b9958ebed6b607a9af0
default_mcp_url=https://dockermcp.astatide.com/mcp
default_model=cohere/north-mini-code:free

runtime_image=${AGW_PERSONAL_RUNTIME_IMAGE:-$default_runtime_image}
skill_ref=${AGW_PERSONAL_SKILL_REF:-$default_skill_ref}
skill_digest=${AGW_PERSONAL_SKILL_DIGEST:-$default_skill_digest}
skill_file_sha256=${AGW_PERSONAL_SKILL_FILE_SHA256:-sha256:0000000000000000000000000000000000000000000000000000000000000000}
mcp_url=${AGW_PERSONAL_MCP_URL:-$default_mcp_url}
model=${AGW_PERSONAL_MODEL:-$default_model}
run_marker="personal-$(date -u +%Y%m%d%H%M%S)-$$"

valid_image "$runtime_image" || fail "AGW_PERSONAL_RUNTIME_IMAGE must be image@sha256:<64 hex>"
valid_digest "$skill_digest" || fail "AGW_PERSONAL_SKILL_DIGEST must be sha256:<64 hex>"
valid_digest "$skill_file_sha256" || fail "AGW_PERSONAL_SKILL_FILE_SHA256 must be sha256:<64 hex>"
[[ $skill_ref =~ ^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)+$ ]] || fail "AGW_PERSONAL_SKILL_REF must be a relative skill path"
valid_url "$mcp_url" || fail "AGW_PERSONAL_MCP_URL must be an HTTP(S) origin-safe URL"
[[ $model =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$ ]] || fail "AGW_PERSONAL_MODEL is invalid"

live=0
for value in AGW_PERSONAL_API_URL AGW_PERSONAL_API_TOKEN AGW_PERSONAL_ORGANIZATION AGW_PERSONAL_PROJECT; do
  [[ -z ${!value-} ]] || live=1
done

api_url=${AGW_PERSONAL_API_URL:-}
api_token=${AGW_PERSONAL_API_TOKEN:-}
organization=${AGW_PERSONAL_ORGANIZATION:-}
project=${AGW_PERSONAL_PROJECT:-}
if (( live )); then
  require_value AGW_PERSONAL_API_URL
  require_value AGW_PERSONAL_API_TOKEN
  require_value AGW_PERSONAL_ORGANIZATION
  require_value AGW_PERSONAL_PROJECT
  valid_url "$api_url" || fail "AGW_PERSONAL_API_URL must be an HTTP(S) origin URL without query, fragment, or trailing slash"
  valid_identifier "$organization" || fail "AGW_PERSONAL_ORGANIZATION is invalid"
  valid_identifier "$project" || fail "AGW_PERSONAL_PROJECT is invalid"
  require_value AGW_PERSONAL_SKILL_FILE_SHA256
fi

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-personal-agent.XXXXXX")
manifest_file="$tmp_dir/personal-agent.yaml"
api_auth_header="$tmp_dir/api-auth.header"
response_file="$tmp_dir/response.json"
events_file="$tmp_dir/events.json"
run_artifacts_file="$tmp_dir/run-artifacts.json"
artifact_content_file="$tmp_dir/artifact-content"

cleanup() {
  [[ -z ${tmp_dir:-} || ! -d $tmp_dir ]] || rm -rf -- "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

cp -- "$example_file" "$manifest_file"
# Keep the checked-in example valid with safe defaults. Overrides affect only
# the private copy and never write a secret into the bundle.
sed -i \
  -e "s|$default_runtime_image|$runtime_image|g" \
  -e "s|$default_skill_ref|$skill_ref|g" \
  -e "s|$default_skill_digest|$skill_digest|g" \
  -e "s|$default_mcp_url|$mcp_url|g" \
  -e "s|$default_model|$model|g" \
  -e "s|__AGW_PERSONAL_SKILL_FILE_SHA256__|$skill_file_sha256|g" \
  -e "s|__AGW_PERSONAL_RUN_MARKER__|$run_marker|g" \
  "$manifest_file"

if [[ -n ${AGW_PERSONAL_CLI:-} ]]; then
  cli=("$AGW_PERSONAL_CLI")
  cli_dir="$repo_dir"
elif command -v agw >/dev/null 2>&1; then
  cli=("$(command -v agw)")
  cli_dir="$repo_dir"
else
  need_command go
  cli=(go run ./cmd/agw)
  cli_dir="$v2_dir"
fi

run_cli_checked() {
  local label=$1 output=$2
  shift 2
  if ! (cd "$cli_dir" && AGW_TOKEN="$api_token" "${cli[@]}" "$@" >"$output" 2>&1); then
    fail "$label failed"
  fi
}

# Existing bundle tests cover strict decoding, prompt-file containment,
# deterministic compilation, digest pinning, and secret exclusion. Run them
# as part of the local acceptance without changing implementation files.
if [[ -f "$v2_dir/pkg/spec/bundle_test.go" ]]; then
  need_command go
  (cd "$v2_dir" && go test -count=1 ./pkg/spec -run 'TestAgentBundle') >/dev/null \
    || fail "AgentBundle compiler tests failed"
fi

# `agw run -f` compiles before it creates an API client request. A loopback
# refusal is an intentional static probe: it proves the example reached the
# launch boundary without requiring a server or a credential.
run_cli_probe="$tmp_dir/run-probe.txt"
if (cd "$cli_dir" && AGW_TOKEN=static-probe-token "${cli[@]}" run -f "$manifest_file" \
    --server http://127.0.0.1:1 --organization probe-org --project probe-project \
    --idempotency-key personal-static-probe --follow=false >"$run_cli_probe" 2>&1); then
  fail "static bundle probe unexpectedly reached an API"
fi
grep -Eq 'apply AgentBundle: .*request failed|server returned an invalid' "$run_cli_probe" \
  || fail "static bundle probe failed before the API launch boundary"

if [[ ${AGW_PERSONAL_RUN_SANDBOX_E2E:-0} == 1 ]]; then
  [[ $live == 0 ]] || fail "AGW_PERSONAL_RUN_SANDBOX_E2E is a local fixture check and cannot be combined with live mode"
  "$script_dir/e2e-standalone.sh" >/dev/null
  sandbox_result=real-fixture-pass
else
  sandbox_result=not-run
fi

if (( ! live )); then
  printf 'PERSONAL_AGENT_ACCEPTANCE=STATIC_PASS bundle=compiled sandbox=%s\n' "$sandbox_result"
  exit 0
fi

printf 'Authorization: Bearer %s\n' "$api_token" >"$api_auth_header"
chmod 0600 "$api_auth_header"
scope="/api/v1alpha1/organizations/$organization/projects/$project"

api_request() {
  local method=$1 path=$2 body=${3-} status
  if [[ -n $body ]]; then
    status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$response_file" --write-out '%{http_code}' \
      --request "$method" --header "@$api_auth_header" --header 'Accept: application/json' \
      --header 'Content-Type: application/json' --data-binary "@$body" "$api_url$path" 2>/dev/null) \
      || fail "API request failed: $method $path"
  else
    status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$response_file" --write-out '%{http_code}' \
      --request "$method" --header "@$api_auth_header" --header 'Accept: application/json' \
      "$api_url$path" 2>/dev/null) || fail "API request failed: $method $path"
  fi
  [[ $status =~ ^2[0-9][0-9]$ ]] || fail "API request failed: $method $path (HTTP $status)"
  jq -e . "$response_file" >/dev/null || fail "API returned invalid JSON: $method $path"
}

api_content() {
  local path=$1 status
  status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$artifact_content_file" --write-out '%{http_code}' \
    --request GET --header "@$api_auth_header" --header 'Accept: */*' "$api_url$path" 2>/dev/null) \
    || fail "artifact content request failed"
  [[ $status =~ ^2[0-9][0-9]$ ]] || fail "artifact content request failed (HTTP $status)"
}

api_request GET /readyz
jq -e '.data.status == "ready"' "$response_file" >/dev/null || fail "API is not ready"

# AgentBundle compilation intentionally never creates Credential resources.
# Apply only the two reference definitions; each contains a secret reference,
# never a secret value.
credential_openrouter="$tmp_dir/credential-openrouter.yaml"
credential_mcp="$tmp_dir/credential-mcp.yaml"
cat >"$credential_openrouter" <<'EOF'
apiVersion: agents.astatide.com/v1alpha1
kind: Credential
metadata:
  name: personal-openrouter
spec:
  kind: openrouter-api-key
  owner: owner-operated
  secretRef: env://OPENROUTER_API_KEY
EOF
cat >"$credential_mcp" <<'EOF'
apiVersion: agents.astatide.com/v1alpha1
kind: Credential
metadata:
  name: personal-mcp
spec:
  kind: mcp-bearer-token
  owner: owner-operated
  secretRef: env://MCP_GATEWAY_AUTH_TOKEN
EOF
run_cli_checked credentials "$tmp_dir/credentials.txt" apply --server "$api_url" --organization "$organization" --project "$project" \
  -f "$credential_openrouter" -f "$credential_mcp"

run_key="personal-agent-$run_marker"
run_cli_checked launch "$tmp_dir/launch.txt" run -f "$manifest_file" --server "$api_url" \
  --organization "$organization" --project "$project" --idempotency-key "$run_key" --follow=false
run_id=$(awk '/^run / {print $2; exit}' "$tmp_dir/launch.txt")
valid_identifier "$run_id" || fail "bundle launch did not return a valid run ID"

timeout_seconds=${AGW_PERSONAL_TIMEOUT_SECONDS:-900}
poll_seconds=${AGW_PERSONAL_POLL_SECONDS:-3}
[[ $timeout_seconds =~ ^[1-9][0-9]*$ && $poll_seconds =~ ^[1-9][0-9]*$ ]] || fail "personal timeout/poll settings must be positive integers"
deadline=$(( $(date +%s) + timeout_seconds ))
terminal_status=
while (( $(date +%s) < deadline )); do
  api_request GET "$scope/runs/$run_id"
  status=$(jq -r '.data.status // empty' "$response_file")
  case "$status" in
    Succeeded|Failed|Cancelled|Lost) terminal_status=$status; break ;;
    *) sleep "$poll_seconds" ;;
  esac
done
[[ -n $terminal_status ]] || fail "run did not reach a terminal state within ${timeout_seconds}s"
[[ $terminal_status == Succeeded ]] || fail "personal AgentRun ended in $terminal_status"

# Durable event history is the minimal troubleshooting log. Only bounded,
# non-sensitive assertions are made; payloads stay in the private temp dir.
api_request GET "$scope/runs/$run_id/events?after=0"
cp -- "$response_file" "$events_file"
jq -e '.data.events | type == "array" and length > 0' "$events_file" >/dev/null || fail "run returned no durable events"
jq -e '[.data.events[] | select(.type == "run.started")] | length == 1' "$events_file" >/dev/null || fail "missing run.started event"
jq -e '[.data.events[] | select(.type == "run.completed")] | length == 1' "$events_file" >/dev/null || fail "missing run.completed event"
jq -e --arg marker "$run_marker" \
  '[.data.events[] | select(.type == "assistant.message" and ((.payload.message // .payload.content // "") | contains($marker)))] | length >= 1' \
  "$events_file" >/dev/null || fail "assistant output did not contain the run marker"
jq -e --arg marker "AGW_PERSONAL_SKILL_PROOF_$skill_file_sha256" \
  '[.data.events[] | select(.type == "assistant.message" and ((.payload.message // .payload.content // "") | contains($marker)))] | length >= 1' \
  "$events_file" >/dev/null || fail "assistant output did not contain the skill proof marker"

api_request GET "$scope/runs/$run_id/artifacts"
cp -- "$response_file" "$run_artifacts_file"
jq -e '
  .data.primary.manifest.title == "Personal agent primary report" and
  ([.data.supporting[] | select(.manifest.title == "Personal agent supporting verification")] | length == 1)
' "$run_artifacts_file" >/dev/null || fail "run did not expose one primary and one supporting artifact"

primary_id=$(jq -r '.data.primary.artifact_id' "$run_artifacts_file")
primary_version=$(jq -r '.data.primary.version_id' "$run_artifacts_file")
supporting_id=$(jq -r '[.data.supporting[] | select(.manifest.title == "Personal agent supporting verification")][0].artifact_id' "$run_artifacts_file")
supporting_version=$(jq -r '[.data.supporting[] | select(.manifest.title == "Personal agent supporting verification")][0].version_id' "$run_artifacts_file")
valid_identifier "$primary_id" && valid_identifier "$primary_version" || fail "primary artifact identity was invalid"
valid_identifier "$supporting_id" && valid_identifier "$supporting_version" || fail "supporting artifact identity was invalid"

api_content "$scope/artifacts/$primary_id/versions/$primary_version/content"
grep -F "$run_marker" "$artifact_content_file" >/dev/null || fail "primary artifact did not contain the run marker"
api_content "$scope/artifacts/$supporting_id/versions/$supporting_version/content"
grep -F "$run_marker" "$artifact_content_file" >/dev/null || fail "supporting artifact did not contain the run marker"

api_request GET "$scope/audit?limit=500"
jq -e --arg run_id "$run_id" \
  '[.data.items[] | select(.action == "mcp.tool.authorization" and .metadata.run_id == $run_id and .metadata.tool == "get_me" and .decision == "allowed")] | length == 1' \
  "$response_file" >/dev/null || fail "missing scoped get_me authorization audit"
jq -e --arg run_id "$run_id" \
  '[.data.items[] | select(.action == "mcp.tool.outcome" and .metadata.run_id == $run_id and .metadata.tool == "get_me" and .metadata.outcome == "read")] | length == 1' \
  "$response_file" >/dev/null || fail "missing get_me read outcome audit"

if [[ ${AGW_PERSONAL_SKIP_STOP_TEST:-0} != 1 ]]; then
  run_cli_checked stop-launch "$tmp_dir/stop-launch.txt" run -f "$manifest_file" --server "$api_url" \
    --organization "$organization" --project "$project" --idempotency-key "${run_key}-stop" --follow=false
  stop_run_id=$(awk '/^run / {print $2; exit}' "$tmp_dir/stop-launch.txt")
  valid_identifier "$stop_run_id" || fail "stop test did not return a valid run ID"
  run_cli_checked stop "$tmp_dir/stop.txt" cancel --server "$api_url" --organization "$organization" --project "$project" \
    --run-id "$stop_run_id" --idempotency-key "${run_key}-cancel" --reason "personal acceptance stop test"
  stop_deadline=$(( $(date +%s) + 120 ))
  stop_status=
  while (( $(date +%s) < stop_deadline )); do
    api_request GET "$scope/runs/$stop_run_id"
    stop_status=$(jq -r '.data.status // empty' "$response_file")
    [[ $stop_status == Cancelled ]] && break
    [[ $stop_status == Succeeded || $stop_status == Failed || $stop_status == Lost ]] && break
    sleep 2
  done
  [[ $stop_status == Cancelled ]] || fail "stop test did not reach Cancelled (got $stop_status)"
else
  stop_status=skipped
fi

if [[ ${AGW_PERSONAL_CHECK_LOCAL_PODMAN:-0} == 1 ]]; then
  need_command podman
  [[ ${EUID:-1} -ne 0 ]] || fail "local Podman observation must run as the rootless runner user"
  if podman ps -aq --filter "label=io.agents-gateway.run=$run_id" | grep -q .; then
    fail "local Podman observation found a leftover primary-run container"
  fi
fi

printf 'PERSONAL_AGENT_ACCEPTANCE=PASS run=%s status=%s stop=%s sandbox=%s\n' \
  "$run_id" "$terminal_status" "$stop_status" "$sandbox_result"
