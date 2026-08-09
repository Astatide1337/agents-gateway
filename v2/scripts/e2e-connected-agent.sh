#!/usr/bin/env bash
# Opt-in acceptance test for one owner-operated, deployed Agents Gateway chain.
#
# This script intentionally does not provision resources, create credentials, or
# infer missing deployment configuration. It applies disposable, pinned
# definitions to the supplied project, runs one real AgentRun, and removes the
# disposable GitHub branch with a separately supplied cleanup credential.
set -Eeuo pipefail
umask 077

fail() {
  printf 'e2e-connected-agent: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing prerequisite: $1"
}

require_value() {
  local name=$1 value=${!1-}
  [[ -n $value ]] || fail "$name is required"
  # Bash variables cannot contain NUL bytes: Bash strips them during command
  # substitution and cannot represent them in parameter expansion.  A
  # `$'\0'` pattern therefore becomes an empty pattern and matches every
  # value.  Reject the representable line-breaking controls here; downstream
  # validators constrain each value's remaining character set.
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || fail "$name contains a forbidden control character"
}

valid_name() {
  [[ $1 =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]
}

valid_uuid() {
  [[ $1 =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]
}

valid_identifier() {
  [[ $1 =~ ^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$ ]]
}

valid_model() {
  [[ $1 =~ ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$ ]]
}

valid_digest() {
  [[ $1 =~ ^sha256:[a-f0-9]{64}$ ]]
}

valid_image() {
  [[ $1 == *@sha256:* ]] || return 1
  local digest=${1##*@}
  valid_digest "$digest"
}

need_command curl
need_command jq
need_command mktemp
need_command date
need_command sed
need_command sleep
need_command awk
need_command grep
need_command head
need_command cp

require_value AGW_CONNECTED_API_URL
require_value AGW_CONNECTED_API_TOKEN
require_value AGW_CONNECTED_ORGANIZATION
require_value AGW_CONNECTED_PROJECT
require_value AGW_CONNECTED_RUNTIME_IMAGE
require_value AGW_CONNECTED_SKILL_REF
require_value AGW_CONNECTED_SKILL_DIGEST
require_value AGW_CONNECTED_SKILL_FILE_SHA256
require_value AGW_CONNECTED_MCP_URL
require_value AGW_CONNECTED_MCP_RESOURCE
require_value AGW_CONNECTED_GITHUB_CLEANUP_TOKEN

api_url=${AGW_CONNECTED_API_URL%/}
[[ $api_url == https://* || $api_url == http://* ]] || fail "AGW_CONNECTED_API_URL must be an absolute HTTP(S) URL"
[[ $api_url != *\?* && $api_url != *#* && $api_url != */ ]] || fail "AGW_CONNECTED_API_URL must not contain query, fragment, or trailing slash"
valid_uuid "$AGW_CONNECTED_ORGANIZATION" || fail "AGW_CONNECTED_ORGANIZATION must be a UUID"
valid_uuid "$AGW_CONNECTED_PROJECT" || fail "AGW_CONNECTED_PROJECT must be a UUID"
valid_image "$AGW_CONNECTED_RUNTIME_IMAGE" || fail "AGW_CONNECTED_RUNTIME_IMAGE must be image@sha256:<64 hex>"
valid_digest "$AGW_CONNECTED_SKILL_DIGEST" || fail "AGW_CONNECTED_SKILL_DIGEST must be sha256:<64 hex>"
valid_digest "$AGW_CONNECTED_SKILL_FILE_SHA256" || fail "AGW_CONNECTED_SKILL_FILE_SHA256 must be sha256:<64 hex>"
[[ $AGW_CONNECTED_MCP_URL == https://* ]] || fail "AGW_CONNECTED_MCP_URL must use HTTPS"
[[ $AGW_CONNECTED_MCP_URL == */mcp ]] || fail "AGW_CONNECTED_MCP_URL must end in /mcp"
[[ $AGW_CONNECTED_MCP_URL != *\?* && $AGW_CONNECTED_MCP_URL != *#* ]] || fail "AGW_CONNECTED_MCP_URL must not contain query or fragment"
case "$AGW_CONNECTED_MCP_URL" in *[[:space:]\"]*) fail "AGW_CONNECTED_MCP_URL contains unsafe characters" ;; esac
[[ $AGW_CONNECTED_SKILL_REF =~ ^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)+$ ]] || fail "AGW_CONNECTED_SKILL_REF must be a relative skill path"
[[ $AGW_CONNECTED_MCP_RESOURCE =~ ^github:repo:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "AGW_CONNECTED_MCP_RESOURCE must be github:repo:owner/repository"
mcp_repository=${AGW_CONNECTED_MCP_RESOURCE#github:repo:}
github_repository=${AGW_CONNECTED_GITHUB_REPOSITORY:-$mcp_repository}
[[ $github_repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "AGW_CONNECTED_GITHUB_REPOSITORY must be owner/repository"
[[ $github_repository == "$mcp_repository" ]] || fail "AGW_CONNECTED_GITHUB_REPOSITORY must match AGW_CONNECTED_MCP_RESOURCE"
github_owner=${github_repository%%/*}
github_repo=${github_repository#*/}

model=${AGW_CONNECTED_OPENROUTER_MODEL:-nvidia/nemotron-3-ultra-550b-a55b:free}
valid_model "$model" || fail "AGW_CONNECTED_OPENROUTER_MODEL is not a valid model identifier"

openrouter_credential_env=${AGW_CONNECTED_OPENROUTER_CREDENTIAL_ENV:-OPENROUTER_API_KEY}
mcp_credential_env=${AGW_CONNECTED_MCP_CREDENTIAL_ENV:-MCP_GATEWAY_AUTH_TOKEN}
for env_name in "$openrouter_credential_env" "$mcp_credential_env"; do
  [[ $env_name =~ ^[A-Z_][A-Z0-9_]{0,127}$ ]] || fail "credential environment names must be uppercase variable names"
done

organization=$AGW_CONNECTED_ORGANIZATION
project=$AGW_CONNECTED_PROJECT
scope="/api/v1alpha1/organizations/$organization/projects/$project"
timeout_seconds=${AGW_CONNECTED_TIMEOUT_SECONDS:-900}
poll_seconds=${AGW_CONNECTED_POLL_SECONDS:-3}
[[ $timeout_seconds =~ ^[1-9][0-9]*$ ]] || fail "AGW_CONNECTED_TIMEOUT_SECONDS must be a positive integer"
[[ $poll_seconds =~ ^[1-9][0-9]*$ ]] || fail "AGW_CONNECTED_POLL_SECONDS must be a positive integer"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-connected-agent.XXXXXX")
api_auth_header="$tmp_dir/api-auth.header"
github_auth_header="$tmp_dir/github-auth.header"
printf 'Authorization: Bearer %s\n' "$AGW_CONNECTED_API_TOKEN" >"$api_auth_header"
printf 'Authorization: Bearer %s\n' "$AGW_CONNECTED_GITHUB_CLEANUP_TOKEN" >"$github_auth_header"
chmod 0600 "$api_auth_header" "$github_auth_header"
response_file=
events_file=
audit_file=
artifacts_file=
run_id=
branch_name=
cleanup_done=0

cleanup_branch() {
  [[ $cleanup_done == 1 || -z ${branch_name:-} ]] && return 0
  cleanup_done=1
  local encoded status endpoint
  encoded=$(jq -rn --arg value "$branch_name" '$value|@uri')
  endpoint="https://api.github.com/repos/$github_repository/git/refs/heads/$encoded"
  status=$(curl -sS --connect-timeout 10 --max-time 45 --output /dev/null --write-out '%{http_code}' \
    --request GET \
    --header "@$github_auth_header" \
    --header 'Accept: application/vnd.github+json' \
    --header 'User-Agent: agents-gateway-connected-e2e' \
    "$endpoint" \
    2>/dev/null || true)
  [[ $status == 200 || $status == 404 ]] || {
    printf 'e2e-connected-agent: disposable branch lookup returned HTTP %s\n' "$status" >&2
    return 1
  }
  [[ $status == 200 ]] || return 0
  status=$(curl -sS --connect-timeout 10 --max-time 45 --output /dev/null --write-out '%{http_code}' \
    --request DELETE \
    --header "@$github_auth_header" \
    --header 'Accept: application/vnd.github+json' \
    --header 'User-Agent: agents-gateway-connected-e2e' \
    "$endpoint" \
    2>/dev/null || true)
  [[ $status == 204 ]] || {
    printf 'e2e-connected-agent: disposable branch cleanup returned HTTP %s\n' "$status" >&2
    return 1
  }
}

cleanup() {
  cleanup_branch || true
  [[ -z ${tmp_dir:-} || ! -d $tmp_dir ]] || rm -rf -- "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

api_request() {
  local method=$1 path=$2 body=${3-} status code
  response_file="$tmp_dir/response.$RANDOM.json"
  if [[ -n $body ]]; then
    status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$response_file" --write-out '%{http_code}' \
      --request "$method" --header "@$api_auth_header" \
      --header 'Accept: application/json' --header 'Content-Type: application/json' \
      --data-binary "@$body" "$api_url$path" 2>/dev/null) || fail "request failed: $method $path"
  else
    status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$response_file" --write-out '%{http_code}' \
      --request "$method" --header "@$api_auth_header" \
      --header 'Accept: application/json' "$api_url$path" 2>/dev/null) || fail "request failed: $method $path"
  fi
  [[ $status =~ ^2[0-9][0-9]$ ]] || {
    code=$(jq -r '.error.code // "http_error"' "$response_file" 2>/dev/null || printf 'http_error')
    fail "request failed: $method $path (HTTP $status, $code)"
  }
  jq -e . "$response_file" >/dev/null || fail "request returned invalid JSON: $method $path"
}

api_content() {
  local path=$1 status
  response_file="$tmp_dir/content.$RANDOM"
  status=$(curl -sS --connect-timeout 15 --max-time 90 --output "$response_file" --write-out '%{http_code}' \
    --request GET --header "@$api_auth_header" \
    --header 'Accept: */*' "$api_url$path" 2>/dev/null) || fail "request failed: GET $path"
  [[ $status =~ ^2[0-9][0-9]$ ]] || fail "request failed: GET $path (HTTP $status)"
}

apply_resource() {
  local kind=$1 name=$2 document=$3 digest
  api_request PUT "$scope/resources/$kind/$name" "$document"
  jq -e --arg kind "$kind" --arg name "$name" \
    '.data.kind == $kind and .data.name == $name and (.data.digest | test("^sha256:[a-f0-9]{64}$"))' \
    "$response_file" >/dev/null || fail "apply response was not a pinned $kind/$name"
  digest=$(jq -r '.data.digest' "$response_file")
  printf '%s\n' "$digest"
}

api_request GET /readyz
jq -e '.data.status == "ready"' "$response_file" >/dev/null || fail "deployed API is not ready"

stamp=$(date -u +%Y%m%d%H%M%S)-$$
sandbox_name="connected-sandbox-$stamp"
openrouter_credential_name="connected-openrouter-$stamp"
mcp_credential_name="connected-mcp-$stamp"
model_route_name="connected-openrouter-$stamp"
skill_set_name="connected-skills-$stamp"
tool_set_name="connected-tools-$stamp"
agent_name="connected-agent-$stamp"
branch_name="agw-e2e/$stamp"

for resource_name in "$sandbox_name" "$openrouter_credential_name" "$mcp_credential_name" "$model_route_name" "$skill_set_name" "$tool_set_name" "$agent_name"; do
  valid_name "$resource_name" || fail "generated resource name is invalid"
done

sandbox_file="$tmp_dir/sandbox.json"
credential_file="$tmp_dir/credential-openrouter.json"
mcp_credential_file="$tmp_dir/credential-mcp.json"
model_route_file="$tmp_dir/model-route.json"
skill_set_file="$tmp_dir/skill-set.json"
tool_set_file="$tmp_dir/tool-set.json"
agent_file="$tmp_dir/agent.json"

cat >"$sandbox_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"SandboxProfile",
  "metadata":{"name":"$sandbox_name"},
  "spec":{
    "backend":"podman",
    "image":"$AGW_CONNECTED_RUNTIME_IMAGE",
    "resources":{"cpu":"1","memory":"2Gi","disk":"1Gi","pids":128},
    "filesystem":{"root":"read-only","workspace":"writable"},
    "network":{"mode":"brokered","directInternet":false,"routes":[],"allowedHosts":[]}
  }
}
EOF

cat >"$credential_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"Credential",
  "metadata":{"name":"$openrouter_credential_name"},
  "spec":{"kind":"openrouter-api-key","owner":"owner-operated-e2e","secretRef":"env://$openrouter_credential_env"}
}
EOF

cat >"$mcp_credential_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"Credential",
  "metadata":{"name":"$mcp_credential_name"},
  "spec":{"kind":"mcp-bearer-token","owner":"owner-operated-e2e","secretRef":"env://$mcp_credential_env"}
}
EOF

cat >"$model_route_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"ModelRoute",
  "metadata":{"name":"$model_route_name"},
  "spec":{"providers":[{"name":"openrouter-free","kind":"openrouter-responses","model":"$model","credentialRef":"$openrouter_credential_name","priority":1}],"budget":{"maxCostUsd":0,"cooldown":"1m"}}
}
EOF

cat >"$skill_set_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"SkillSet",
  "metadata":{"name":"$skill_set_name"},
  "spec":{"skills":[{"name":"connected-acceptance-skill","ref":"$AGW_CONNECTED_SKILL_REF","digest":"$AGW_CONNECTED_SKILL_DIGEST"}]}
}
EOF

cat >"$tool_set_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"ToolSet",
  "metadata":{"name":"$tool_set_name"},
  "spec":{"servers":[{"name":"gateway","ref":"$AGW_CONNECTED_MCP_URL","credentialsRef":"$mcp_credential_name","tools":[
    {"name":"get_me","effect":"read","approval":"allow"},
    {"name":"create_branch","resources":["$AGW_CONNECTED_MCP_RESOURCE"],"effect":"write","approval":"allow"}
  ]}]}
}
EOF

cat >"$agent_file" <<EOF
{
  "apiVersion":"agents.astatide.com/v1alpha1",
  "kind":"Agent",
  "metadata":{"name":"$agent_name"},
  "spec":{
    "runtime":{"harness":"codex","image":"$AGW_CONNECTED_RUNTIME_IMAGE"},
    "instructions":{"inline":"You are running the Agents Gateway connected acceptance chain. Work only in the current workspace. First use the shell to compute sha256sum for the staged file /skills/*/SKILL.md and fail unless it exactly equals ${AGW_CONNECTED_SKILL_FILE_SHA256#sha256:}; this is required evidence that the immutable skill was mounted and read. Do not expose the skill contents. Then call the MCP tool get_me exactly once as a read. Then call create_branch exactly once as an approved write using owner=$github_owner, repo=$github_repo, branch=$branch_name, from_branch=main. Do not call any other MCP tool and do not retry either call. Create a Markdown file named connected-agent-report.md containing these exact markers: AGW_CONNECTED_ARTIFACT_MARKER_$stamp and AGW_CONNECTED_SKILL_PROOF_$AGW_CONNECTED_SKILL_FILE_SHA256. Create .agw/artifacts/connected-agent-report.json with schema=agents-gateway.artifact.v1, title=Connected agent acceptance report, description=Live connected-chain acceptance artifact, content_kind=document, media_type=text/markdown, source=connected-agent-report.md, and capabilities=[]. Finish with a concise message containing both exact markers and the MCP results."},
    "skillSetRef":"$skill_set_name",
    "toolSetRef":"$tool_set_name",
    "modelRouteRef":"$model_route_name",
    "sandboxProfileRef":"$sandbox_name",
    "limits":{"timeout":"12m","maxToolCalls":2,"maxCostUsd":0},
    "environment":[]
  }
}
EOF

sandbox_digest=$(apply_resource SandboxProfile "$sandbox_name" "$sandbox_file")
openrouter_credential_digest=$(apply_resource Credential "$openrouter_credential_name" "$credential_file")
mcp_credential_digest=$(apply_resource Credential "$mcp_credential_name" "$mcp_credential_file")
model_route_digest=$(apply_resource ModelRoute "$model_route_name" "$model_route_file")
skill_set_digest=$(apply_resource SkillSet "$skill_set_name" "$skill_set_file")
tool_set_digest=$(apply_resource ToolSet "$tool_set_name" "$tool_set_file")
agent_digest=$(apply_resource Agent "$agent_name" "$agent_file")

for digest in "$sandbox_digest" "$openrouter_credential_digest" "$mcp_credential_digest" "$model_route_digest" "$skill_set_digest" "$tool_set_digest" "$agent_digest"; do
  valid_digest "$digest" || fail "API returned an invalid resource digest"
done

# Read back the immutable revisions through the public envelope. This verifies
# the exact spec fields the factory will resolve, including the Podman profile,
# the pinned/read-only Skills selection, and the single runtime image.
api_request GET "$scope/resources/SandboxProfile/$sandbox_name"
jq -e --arg digest "$sandbox_digest" --arg image "$AGW_CONNECTED_RUNTIME_IMAGE" \
  '.data.digest == $digest and .data.document.kind == "SandboxProfile" and
   .data.document.spec.backend == "podman" and
   .data.document.spec.image == $image and
   .data.document.spec.filesystem.root == "read-only" and
   .data.document.spec.filesystem.workspace == "writable" and
   .data.document.spec.network.directInternet == false' \
  "$response_file" >/dev/null || fail "applied SandboxProfile did not match the immutable rootless Podman policy"

api_request GET "$scope/resources/SkillSet/$skill_set_name"
jq -e --arg digest "$skill_set_digest" --arg skill_ref "$AGW_CONNECTED_SKILL_REF" --arg skill_digest "$AGW_CONNECTED_SKILL_DIGEST" \
  '.data.digest == $digest and .data.document.kind == "SkillSet" and
   (.data.document.spec.skills | length == 1) and
   .data.document.spec.skills[0].ref == $skill_ref and
   .data.document.spec.skills[0].digest == $skill_digest' \
  "$response_file" >/dev/null || fail "applied SkillSet did not preserve the pinned Skills reference"

api_request GET "$scope/resources/Agent/$agent_name"
jq -e --arg digest "$agent_digest" --arg image "$AGW_CONNECTED_RUNTIME_IMAGE" \
  --arg skill_set "$skill_set_name" --arg tool_set "$tool_set_name" --arg route "$model_route_name" --arg sandbox "$sandbox_name" \
  '.data.digest == $digest and .data.document.kind == "Agent" and
   .data.document.spec.runtime.image == $image and
   .data.document.spec.skillSetRef == $skill_set and
   .data.document.spec.toolSetRef == $tool_set and
   .data.document.spec.modelRouteRef == $route and
   .data.document.spec.sandboxProfileRef == $sandbox' \
  "$response_file" >/dev/null || fail "applied Agent did not reference the selected immutable capabilities"

# A run request takes the applied Agent revision digest, not the resource body.
run_key="connected-agent-$stamp"
run_request="$tmp_dir/run.json"
cat >"$run_request" <<EOF
{
  "kind":"AgentRun",
  "definitionDigest":"$agent_digest",
  "idempotencyKey":"$run_key",
  "agentRef":"$agent_name"
}
EOF
api_request POST "$scope/runs" "$run_request"
jq -e --arg kind AgentRun --arg digest "$agent_digest" --arg status Pending \
  '.data.kind == $kind and .data.definitionDigest == $digest and (.data.id | length > 0) and (.data.status == $status or .data.status == "Running" or .data.status == "Succeeded")' \
  "$response_file" >/dev/null || fail "run creation response did not match the current AgentRun shape"
run_id=$(jq -r '.data.id' "$response_file")
valid_identifier "$run_id" || fail "run response contained an invalid run ID"

events_file="$tmp_dir/events.json"
audit_file="$tmp_dir/audit.json"
artifacts_file="$tmp_dir/artifacts.json"
deadline=$(( $(date +%s) + timeout_seconds ))
terminal_status=
while (( $(date +%s) < deadline )); do
  api_request GET "$scope/runs/$run_id"
  status=$(jq -r '.data.status // empty' "$response_file")
  case "$status" in
    Succeeded|Failed|Cancelled|Lost) terminal_status=$status; break ;;
    Pending|Running|WaitingApproval|WaitingCapacity|Starting|*) : ;;
  esac
  api_request GET "$scope/runs/$run_id/events?after=0"
  cp -- "$response_file" "$events_file"
  sleep "$poll_seconds"
done

[[ -n $terminal_status ]] || fail "run $run_id did not reach a terminal state within ${timeout_seconds}s"
api_request GET "$scope/runs/$run_id/events?after=0"
cp -- "$response_file" "$events_file"
api_request GET "$scope/audit?limit=500"
cp -- "$response_file" "$audit_file"
api_request GET "$scope/artifacts"
cp -- "$response_file" "$artifacts_file"

[[ $terminal_status == Succeeded ]] || fail "connected AgentRun ended in $terminal_status"
jq -e '.data.events | type == "array" and length > 0' "$events_file" >/dev/null || fail "run returned no durable events"

# These are the exact runtime protocol signals. The acceptance test is
# intentionally strict: a status-only implementation cannot claim this chain.
jq -e '[.data.events[] | select(.type == "run.started")] | length == 1' "$events_file" >/dev/null || fail "missing unique run.started event"
jq -e '[.data.events[] | select(.type == "run.completed")] | length == 1' "$events_file" >/dev/null || fail "missing unique run.completed event"
jq -e '[.data.events[] | select(.type == "run.started" and (.payload.agent_id | type == "string") and (.payload.sandbox_id | type == "string"))] | length == 1' \
  "$events_file" >/dev/null || fail "run.started event did not carry the runner sandbox identity"
jq -e '[.data.events[] | select(.type == "model.requested" and .payload.model == $model)] | length >= 1' --arg model "$model" "$events_file" >/dev/null || fail "missing requested OpenRouter model evidence"
jq -e '[.data.events[] | select(.type == "model.completed" and ((.payload.input_tokens // 0) > 0) and ((.payload.output_tokens // 0) > 0))] | length >= 1' "$events_file" >/dev/null || fail "missing real model output/usage evidence"
jq -e '[.data.events[] | select(.type == "assistant.message" and ((.payload.message // .payload.content // "") | length > 0))] | length >= 1' "$events_file" >/dev/null || fail "missing real assistant output evidence"

# A successful run after the read-back above proves the factory resolved the
# selected Agent, Podman SandboxProfile, ToolSet, ModelRoute, and pinned
# SkillSet before starting the runner. Skills staging is intentionally not
# inferred from a made-up event: when local host inspection is enabled, the
# optional check below only verifies cleanup of owner-run containers.

marker="AGW_CONNECTED_ARTIFACT_MARKER_$stamp"
skill_marker="AGW_CONNECTED_SKILL_PROOF_$AGW_CONNECTED_SKILL_FILE_SHA256"
jq -e --arg marker "$marker" \
  '[.data.events[] | select(.type == "assistant.message" and ((.payload.message // .payload.content // "") | contains($marker)))] | length >= 1' \
  "$events_file" >/dev/null || fail "model output did not contain the required artifact marker"
jq -e --arg marker "$skill_marker" \
  '[.data.events[] | select(.type == "assistant.message" and ((.payload.message // .payload.content // "") | contains($marker)))] | length >= 1' \
  "$events_file" >/dev/null || fail "model output did not contain the staged-skill proof marker"

artifact_id=$(jq -r '.data.events[] | select(.type == "artifact.created") | .payload.artifact.id // empty' "$events_file" | head -n 1)
[[ -n $artifact_id ]] || fail "run emitted no artifact.created event"
valid_identifier "$artifact_id" || fail "artifact event contained an invalid artifact ID"
jq -e --arg artifact_id "$artifact_id" \
  '.data | type == "array" and any(.[]; .artifact_id == $artifact_id and .version_id == $artifact_id)' \
  "$artifacts_file" >/dev/null || fail "artifact collection envelope did not contain the published artifact version"
artifact_version_id=$(jq -r --arg artifact_id "$artifact_id" \
  '.data[] | select(.artifact_id == $artifact_id) | .version_id' "$artifacts_file" | head -n 1)
[[ -n $artifact_version_id ]] || fail "artifact collection did not expose a version identifier"
valid_identifier "$artifact_version_id" || fail "artifact collection contained an invalid version identifier"
api_request GET "$scope/artifacts/$artifact_id/versions/$artifact_version_id"
jq -e --arg marker "$marker" \
  '.data.manifest.media_type == "text/markdown" and .data.manifest.content_kind == "document" and (.data.manifest.title == "Connected agent acceptance report")' \
  "$response_file" >/dev/null || fail "authored artifact catalog entry is missing or has the wrong manifest"
api_content "$scope/artifacts/$artifact_id/versions/$artifact_version_id/content"
grep -F "$marker" "$response_file" >/dev/null || fail "authored artifact content did not contain the required marker"
grep -F "$skill_marker" "$response_file" >/dev/null || fail "authored artifact content did not contain the staged-skill proof marker"

# Durable MCP audit metadata deliberately contains no arguments or response
# bodies. Verify both authorization and terminal outcomes for each required
# tool, tied to this run ID.
jq -e --arg run_id "$run_id" --arg tool get_me \
  '[.data.items[] | select(.action == "mcp.tool.authorization" and .metadata.run_id == $run_id and .metadata.tool == $tool and .decision == "allowed")] | length == 1' \
  "$audit_file" >/dev/null || fail "missing durable get_me authorization audit"
jq -e --arg run_id "$run_id" --arg tool get_me \
  '[.data.items[] | select(.action == "mcp.tool.outcome" and .metadata.run_id == $run_id and .metadata.tool == $tool and .metadata.phase == "outcome" and .metadata.outcome == "read")] | length == 1' \
  "$audit_file" >/dev/null || fail "missing durable get_me read outcome audit"
jq -e --arg run_id "$run_id" --arg tool create_branch \
  '[.data.items[] | select(.action == "mcp.tool.authorization" and .metadata.run_id == $run_id and .metadata.tool == $tool and .decision == "allowed")] | length == 1' \
  "$audit_file" >/dev/null || fail "missing durable create_branch authorization audit"
jq -e --arg run_id "$run_id" --arg tool create_branch \
  '[.data.items[] | select(.action == "mcp.tool.outcome" and .metadata.run_id == $run_id and .metadata.tool == $tool and .metadata.phase == "outcome" and .metadata.outcome == "succeeded")] | length == 1' \
  "$audit_file" >/dev/null || fail "missing durable create_branch success outcome audit"

# Optional local observation: when the acceptance script runs on the owner
# runner and Podman is available, no run-owned container may remain. Remote
# deployments remain covered by the durable run/artifact/audit checks above.
if [[ ${AGW_CONNECTED_CHECK_LOCAL_PODMAN:-0} == 1 ]]; then
  need_command podman
  [[ ${EUID:-1} -ne 0 ]] || fail "local Podman observation must run as the rootless runner user"
  if podman ps -aq --filter "label=io.agents-gateway.run=$run_id" | grep -q .; then
    fail "local Podman observation found a leftover run-owned container"
  fi
fi

cleanup_branch || fail "could not clean disposable GitHub branch"
printf 'CONNECTED_AGENT_E2E=PASS run=%s model=%s\n' "$run_id" "$model"
