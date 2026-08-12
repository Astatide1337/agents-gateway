#!/usr/bin/env bash
# shellcheck shell=bash
#
# Phase-0 conformance probe for the object-store effect-ledger assumption.
#
# The production checks use the AWS CLI S3 API surface.  They never perform an
# unconditional write: every test write is If-None-Match: *.  The ambiguous
# transport check uses the local fixture under scripts/fixtures/objectstore so
# a fault is never injected into the configured production endpoint.

set -u -o pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/phase0-common.sh"

PHASE0_SCRIPT_NAME=phase0-objectstore
PHASE0_AWS_CLI_BIN="${PHASE0_AWS_CLI_BIN:-aws}"
PHASE0_OBJECTSTORE_ENDPOINT="${PHASE0_OBJECTSTORE_ENDPOINT:-}"
PHASE0_OBJECTSTORE_BUCKET="${PHASE0_OBJECTSTORE_BUCKET:-}"
PHASE0_OBJECTSTORE_PREFIX="${PHASE0_OBJECTSTORE_PREFIX:-}"
PHASE0_OBJECTSTORE_REGION="${PHASE0_OBJECTSTORE_REGION:-us-east-1}"
PHASE0_OBJECTSTORE_ADDRESSING_STYLE="${PHASE0_OBJECTSTORE_ADDRESSING_STYLE:-auto}"
PHASE0_OBJECTSTORE_WRITERS="${PHASE0_OBJECTSTORE_WRITERS:-8}"
PHASE0_OBJECTSTORE_CONNECT_TIMEOUT="${PHASE0_OBJECTSTORE_CONNECT_TIMEOUT:-5}"
PHASE0_OBJECTSTORE_READ_TIMEOUT="${PHASE0_OBJECTSTORE_READ_TIMEOUT:-15}"
PHASE0_OBJECTSTORE_MAX_ATTEMPTS="${PHASE0_OBJECTSTORE_MAX_ATTEMPTS:-1}"
PHASE0_OBJECTSTORE_CLEANUP="${PHASE0_OBJECTSTORE_CLEANUP:-true}"

OBJECTSTORE_ROOT=""
OBJECTSTORE_CONCURRENT_KEY=""
OBJECTSTORE_AMBIGUOUS_KEY=""
OBJECTSTORE_PAYLOAD_DIR=""
OBJECTSTORE_FIXTURE_PID=""
OBJECTSTORE_FIXTURE_ENDPOINT=""
OBJECTSTORE_AWS_CONFIG=""

usage() {
  cat <<'EOF'
Usage: phase0-objectstore.sh [common options]

Proves the S3-compatible object-store contract required by the v3 effect
ledger:

  * concurrent conditional create has exactly one winner;
  * the winner's bytes are retrieved exactly;
  * a later conditional conflict cannot overwrite the winner; and
  * an ambiguous transport failure is treated as unknown, never success.

The configured endpoint, bucket, and prefix are explicit inputs. They are read
from PHASE0_OBJECTSTORE_ENDPOINT, PHASE0_OBJECTSTORE_BUCKET, and
PHASE0_OBJECTSTORE_PREFIX. There is no production endpoint or bucket default.

Live --apply requires --yes and an AWS CLI with credentials supplied through
the normal AWS credential chain. It writes only unique probe objects under the
configured prefix and uses If-None-Match: * for every test write. Cleanup
deletes only objects whose run metadata matches this run ID, and refuses an
ownership mismatch. Set PHASE0_RUN_ID to the original run ID for a separate
--cleanup invocation.

Environment:
  PHASE0_OBJECTSTORE_ENDPOINT       HTTPS S3-compatible endpoint (required live)
  PHASE0_OBJECTSTORE_BUCKET         bucket name (required live)
  PHASE0_OBJECTSTORE_PREFIX         non-empty safety prefix (required live)
  PHASE0_OBJECTSTORE_REGION         signing/CLI region (default: us-east-1)
  PHASE0_OBJECTSTORE_ADDRESSING_STYLE auto|path|virtual (default: auto)
  PHASE0_OBJECTSTORE_WRITERS         concurrent writers, 2..32 (default: 8)
  PHASE0_OBJECTSTORE_CONNECT_TIMEOUT AWS CLI connect timeout seconds (default: 5)
  PHASE0_OBJECTSTORE_READ_TIMEOUT    AWS CLI read timeout seconds (default: 15)
  PHASE0_OBJECTSTORE_MAX_ATTEMPTS    AWS retry attempts (default: 1; fail closed)
  PHASE0_AWS_CLI_BIN                 AWS CLI executable (default: aws)
EOF
  phase0_usage_common
}

phase0_parse_common_args "$@" || exit 2
for arg in "${PHASE0_REMAINING_ARGS[@]}"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    *) phase0_die "unknown argument: $arg"; exit 2 ;;
  esac
done

if [[ "$PHASE0_MODE" == cleanup && -z "${PHASE0_RUN_ID:-}" ]]; then
  phase0_die "--cleanup requires PHASE0_RUN_ID from the original apply run"
  exit 2
fi

for pair in \
  "aws-cli=$PHASE0_AWS_CLI_BIN" \
  "endpoint=$PHASE0_OBJECTSTORE_ENDPOINT" \
  "bucket=$PHASE0_OBJECTSTORE_BUCKET" \
  "prefix=$PHASE0_OBJECTSTORE_PREFIX" \
  "region=$PHASE0_OBJECTSTORE_REGION" \
  "addressing-style=$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" \
  "writers=$PHASE0_OBJECTSTORE_WRITERS" \
  "connect-timeout=$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
  "read-timeout=$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
  "max-attempts=$PHASE0_OBJECTSTORE_MAX_ATTEMPTS"; do
  phase0_validate_single_line "${pair%%=*}" "${pair#*=}" || exit 2
done
if [[ -n "${AWS_PROFILE:-}" ]]; then
  phase0_validate_single_line AWS_PROFILE "$AWS_PROFILE" || exit 2
  if [[ ! "$AWS_PROFILE" =~ ^[A-Za-z0-9_.@-]+$ ]]; then
    phase0_die "AWS_PROFILE contains unsupported characters"
    exit 2
  fi
fi

if [[ "$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" != auto && \
      "$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" != path && \
      "$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" != virtual ]]; then
  phase0_die "PHASE0_OBJECTSTORE_ADDRESSING_STYLE must be auto, path, or virtual"
  exit 2
fi
if ! [[ "$PHASE0_OBJECTSTORE_WRITERS" =~ ^[2-9]$|^[1-2][0-9]$|^3[0-2]$ ]]; then
  phase0_die "PHASE0_OBJECTSTORE_WRITERS must be an integer from 2 through 32"
  exit 2
fi
for numeric in \
  "$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
  "$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
  "$PHASE0_OBJECTSTORE_MAX_ATTEMPTS"; do
  if ! [[ "$numeric" =~ ^[1-9][0-9]*$ ]]; then
    phase0_die "object-store timeout and retry settings must be positive integers"
    exit 2
  fi
done
if [[ "$PHASE0_OBJECTSTORE_CLEANUP" != true && "$PHASE0_OBJECTSTORE_CLEANUP" != false ]]; then
  phase0_die "PHASE0_OBJECTSTORE_CLEANUP must be true or false"
  exit 2
fi

validate_live_inputs() {
  local scheme host path query fragment authority

  if [[ -z "$PHASE0_OBJECTSTORE_ENDPOINT" || \
        -z "$PHASE0_OBJECTSTORE_BUCKET" || \
        -z "$PHASE0_OBJECTSTORE_PREFIX" ]]; then
    phase0_result SKIP objectstore-configuration \
      "endpoint, bucket, and prefix environment inputs are required for live mode"
    return 1
  fi

  if [[ "$PHASE0_OBJECTSTORE_ENDPOINT" =~ [[:space:]] || \
        "$PHASE0_OBJECTSTORE_ENDPOINT" == *$'\n'* || \
        "$PHASE0_OBJECTSTORE_ENDPOINT" == *$'\r'* ]]; then
    phase0_result FAIL objectstore-endpoint "endpoint contains whitespace or a control character"
    return 1
  fi
  if [[ "$PHASE0_OBJECTSTORE_ENDPOINT" =~ ^([a-zA-Z][a-zA-Z0-9+.-]*)://([^/?#@]+)(/[^?#]*)?([?][^#]*)?(#.*)?$ ]]; then
    scheme="${BASH_REMATCH[1],,}"
    authority="${BASH_REMATCH[2]}"
    path="${BASH_REMATCH[3]:-}"
    query="${BASH_REMATCH[4]:-}"
    fragment="${BASH_REMATCH[5]:-}"
    host="$authority"
    if [[ "$host" == *:* && "$host" != \[*\]:* ]]; then
      host="${host%%:*}"
    elif [[ "$host" == \[*\]:* ]]; then
      host="${host#\[}"
      host="${host%%\]:*}"
    fi
  else
    phase0_result FAIL objectstore-endpoint "endpoint must be a URL with an explicit scheme and host"
    return 1
  fi
  if [[ "$scheme" != https ]]; then
    phase0_result FAIL objectstore-endpoint "live object-store endpoint must use HTTPS"
    return 1
  fi
  if [[ -z "$host" || "$authority" == *'@'* || -n "$query" || -n "$fragment" ]]; then
    phase0_result FAIL objectstore-endpoint "endpoint must not contain credentials, query parameters, or fragments"
    return 1
  fi
  if [[ -n "$path" && "$path" != / ]]; then
    phase0_result FAIL objectstore-endpoint "endpoint path is not supported; use the service endpoint only"
    return 1
  fi
  if [[ ! "$host" =~ ^[A-Za-z0-9.-]+$ && ! "$host" =~ ^\[[0-9A-Fa-f:]+\]$ ]]; then
    phase0_result FAIL objectstore-endpoint "endpoint host contains unsupported characters"
    return 1
  fi

  if [[ ! "$PHASE0_OBJECTSTORE_BUCKET" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]]; then
    phase0_result FAIL objectstore-bucket "bucket must be a DNS-compatible S3 bucket name"
    return 1
  fi
  if [[ "$PHASE0_OBJECTSTORE_PREFIX" == /* || \
        "$PHASE0_OBJECTSTORE_PREFIX" == *$'\n'* || \
        "$PHASE0_OBJECTSTORE_PREFIX" == *$'\r'* || \
        "$PHASE0_OBJECTSTORE_PREFIX" == *'//' || \
        "$PHASE0_OBJECTSTORE_PREFIX" == *'..'* || \
        ! "$PHASE0_OBJECTSTORE_PREFIX" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]]; then
    phase0_result FAIL objectstore-prefix "prefix must be a relative, traversal-free object-key prefix"
    return 1
  fi
  if (( ${#PHASE0_OBJECTSTORE_PREFIX} > 700 )); then
    phase0_result FAIL objectstore-prefix "prefix is too long"
    return 1
  fi
  if [[ ! "$PHASE0_OBJECTSTORE_REGION" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]]; then
    phase0_result FAIL objectstore-region "region contains unsupported characters"
    return 1
  fi
  return 0
}

phase0_init_evidence || exit 2
if [[ "$PHASE0_RUN_ID" == *'..'* ]]; then
  phase0_die "run ID must not contain consecutive dots"
  exit 2
fi

phase0_objectstore_finish() {
  local rc
  if [[ "$PHASE0_MODE" == cleanup && "$PHASE0_SKIP_COUNT" -gt 0 && \
        "$PHASE0_FAIL_COUNT" -eq 0 ]]; then
    PHASE0_MODE=apply
    phase0_finish
    rc=$?
    PHASE0_MODE=cleanup
    return "$rc"
  fi
  phase0_finish
}

if [[ -n "$PHASE0_OBJECTSTORE_ENDPOINT" ]]; then
  printf 'objectstore_endpoint=%s\n' "$PHASE0_OBJECTSTORE_ENDPOINT" >>"$PHASE0_RUN_DIR/metadata.txt"
fi
if [[ -n "$PHASE0_OBJECTSTORE_BUCKET" ]]; then
  printf 'objectstore_bucket=%s\n' "$PHASE0_OBJECTSTORE_BUCKET" >>"$PHASE0_RUN_DIR/metadata.txt"
fi
if [[ -n "$PHASE0_OBJECTSTORE_PREFIX" ]]; then
  printf 'objectstore_prefix=%s\n' "$PHASE0_OBJECTSTORE_PREFIX" >>"$PHASE0_RUN_DIR/metadata.txt"
fi

if [[ "$PHASE0_MODE" == plan || "$PHASE0_MODE" == dry-run ]]; then
  if [[ -n "$PHASE0_OBJECTSTORE_ENDPOINT" || \
        -n "$PHASE0_OBJECTSTORE_BUCKET" || \
        -n "$PHASE0_OBJECTSTORE_PREFIX" ]]; then
    validate_live_inputs || true
  else
    phase0_result SKIP objectstore-configuration \
      "live endpoint, bucket, and prefix were not supplied; plan remains non-destructive"
  fi
  {
    printf 'operation=conditional-create-if-absent\n'
    printf 'operation=concurrent-writers count=%s\n' "$PHASE0_OBJECTSTORE_WRITERS"
    printf 'operation=exact-byte-retrieval\n'
    printf 'operation=conflict-no-overwrite\n'
    printf 'operation=ambiguous-transport local-fault-fixture-only\n'
    printf 'write_header=If-None-Match: *\n'
    printf 'production_unconditional_writes=false\n'
    printf 'cleanup_scope=exact-run-owned-keys-only\n'
  } >"$PHASE0_RUN_DIR/plan.txt"
  phase0_result PASS objectstore-plan "planned S3 API conformance checks without contacting an endpoint"
  phase0_objectstore_finish
  exit $?
fi

if ! validate_live_inputs; then
  phase0_objectstore_finish
  exit $?
fi

if ! phase0_require_cmd "$PHASE0_AWS_CLI_BIN" || ! phase0_require_cmd sha256sum || \
   ! phase0_require_cmd cmp || ! phase0_require_cmd python3; then
  phase0_objectstore_finish
  exit $?
fi

OBJECTSTORE_ROOT="${PHASE0_OBJECTSTORE_PREFIX%/}/phase0/${PHASE0_RUN_ID}"
OBJECTSTORE_CONCURRENT_KEY="$OBJECTSTORE_ROOT/concurrent/claim.json"
OBJECTSTORE_AMBIGUOUS_KEY="$OBJECTSTORE_ROOT/ambiguous/claim.json"
OBJECTSTORE_PAYLOAD_DIR="$PHASE0_RUN_DIR/payloads"
mkdir -p "$OBJECTSTORE_PAYLOAD_DIR" || {
  phase0_result FAIL evidence "could not create payload evidence directory"
  phase0_objectstore_finish
  exit $?
}

aws_s3api() {
  if [[ -n "$OBJECTSTORE_AWS_CONFIG" ]]; then
    AWS_CONFIG_FILE="$OBJECTSTORE_AWS_CONFIG" \
      AWS_EC2_METADATA_DISABLED=true AWS_PAGER="" \
      AWS_MAX_ATTEMPTS="$PHASE0_OBJECTSTORE_MAX_ATTEMPTS" AWS_RETRY_MODE=standard \
      "$PHASE0_AWS_CLI_BIN" s3api \
      --endpoint-url "$PHASE0_OBJECTSTORE_ENDPOINT" \
      --region "$PHASE0_OBJECTSTORE_REGION" \
      --cli-connect-timeout "$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
      --cli-read-timeout "$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
      "$@"
  else
    AWS_EC2_METADATA_DISABLED=true AWS_PAGER="" \
      AWS_MAX_ATTEMPTS="$PHASE0_OBJECTSTORE_MAX_ATTEMPTS" AWS_RETRY_MODE=standard \
      "$PHASE0_AWS_CLI_BIN" s3api \
      --endpoint-url "$PHASE0_OBJECTSTORE_ENDPOINT" \
      --region "$PHASE0_OBJECTSTORE_REGION" \
      --cli-connect-timeout "$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
      --cli-read-timeout "$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
      "$@"
  fi
}

prepare_aws_config() {
  local source_config="${AWS_CONFIG_FILE:-}"
  local setting="default.s3.addressing_style"
  if [[ "$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" == auto ]]; then
    return 0
  fi
  if [[ -z "$source_config" && -n "${HOME:-}" ]]; then
    source_config="$HOME/.aws/config"
  fi
  OBJECTSTORE_AWS_CONFIG="$(mktemp "${TMPDIR:-/tmp}/agw-phase0-aws-config.XXXXXX")" || return 1
  if [[ -n "$source_config" && -f "$source_config" ]]; then
    if ! cp -- "$source_config" "$OBJECTSTORE_AWS_CONFIG"; then
      return 1
    fi
  fi
  if [[ -n "${AWS_PROFILE:-}" ]]; then
    setting="profile.${AWS_PROFILE}.s3.addressing_style"
  fi
  if ! AWS_CONFIG_FILE="$OBJECTSTORE_AWS_CONFIG" "$PHASE0_AWS_CLI_BIN" configure set \
      "$setting" "$PHASE0_OBJECTSTORE_ADDRESSING_STYLE" \
      >"$PHASE0_RUN_DIR/addressing-config.stdout" 2>"$PHASE0_RUN_DIR/addressing-config.stderr"; then
    sanitize_evidence "$PHASE0_RUN_DIR/addressing-config.stdout" || true
    sanitize_evidence "$PHASE0_RUN_DIR/addressing-config.stderr" || true
    phase0_result FAIL objectstore-addressing-style "could not prepare an isolated AWS CLI addressing-style config"
    return 1
  fi
  return 0
}

sanitize_evidence() {
  local file="$1"
  if [[ ! -f "$file" ]]; then
    phase0_result FAIL evidence "expected evidence file was not created"
    return 1
  fi
  if ! phase0_sanitize_file "$file"; then
    phase0_result FAIL evidence "evidence sanitization failed"
    return 1
  fi
  return 0
}

aws_call() {
  local label="$1"
  shift
  local stdout="$PHASE0_RUN_DIR/${label}.stdout"
  local stderr="$PHASE0_RUN_DIR/${label}.stderr"
  local rc
  if aws_s3api "$@" >"$stdout" 2>"$stderr"; then
    rc=0
  else
    rc=$?
  fi
  sanitize_evidence "$stdout" || return 125
  sanitize_evidence "$stderr" || return 125
  return "$rc"
}

aws_error_contains() {
  local label="$1" pattern="$2"
  grep -Eiq -- "$pattern" \
    "$PHASE0_RUN_DIR/${label}.stdout" "$PHASE0_RUN_DIR/${label}.stderr" 2>/dev/null
}

is_expected_conflict() {
	local label="$1"
	# Only HTTP 412 proves that another immutable object already won. S3's
	# ConditionalRequestConflict/409 is not treated as an existing claim: the
	# caller cannot safely infer whether it should execute an external effect.
	aws_error_contains "$label" 'PreconditionFailed|412|precondition' \
		&& ! aws_error_contains "$label" 'ConditionalRequestConflict|409' \
		&& ! aws_error_contains "$label" 'Unable to locate credentials|NoCredentials|ExpiredToken|AccessDenied|AccessDeniedException|Could not connect|EndpointConnectionError|timed out|timeout|Connection reset|Connection refused|Network is unreachable|certificate verify failed|UnknownEndpoint'
}

trap 'if [[ -n "${OBJECTSTORE_AWS_CONFIG:-}" ]]; then rm -f -- "$OBJECTSTORE_AWS_CONFIG"; fi' EXIT

delete_owned_object() {
  local label="$1" key="$2" metadata etag
  local head_label="${label}-head"
  if aws_call "$head_label" head-object \
      --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$key" \
      --query 'Metadata."agw-phase0-run"' --output text; then
    :
  else
    local head_rc=$?
    if (( head_rc != 125 )) && aws_error_contains "$head_label" 'NoSuchKey|NotFound|404|Not Found'; then
      phase0_result PASS "$label" "owned probe object was already absent"
      return 0
    fi
    phase0_result FAIL "$label" "could not inspect probe object ownership before cleanup"
    return 1
  fi
  metadata="$(tr -d '\r\n' <"$PHASE0_RUN_DIR/${head_label}.stdout")"
  if [[ "$metadata" != "$PHASE0_RUN_ID" ]]; then
    phase0_result FAIL "$label" "ownership metadata did not match this run; refusing deletion"
    return 1
  fi
  local etag_label="${label}-etag"
  if ! aws_call "$etag_label" head-object \
      --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$key" \
      --query ETag --output text; then
    phase0_result FAIL "$label" "could not obtain an ETag before cleanup"
    return 1
  fi
  etag="$(tr -d '\r\n' <"$PHASE0_RUN_DIR/${etag_label}.stdout")"
  if [[ -z "$etag" || "$etag" == None ]]; then
    phase0_result FAIL "$label" "object store did not return an ETag; refusing unguarded deletion"
    return 1
  fi
  if aws_call "$label-delete" delete-object --bucket "$PHASE0_OBJECTSTORE_BUCKET" \
       --key "$key" --if-match "$etag"; then
    phase0_result PASS "$label" "owned probe object deleted with an ETag guard"
    return 0
  fi
  phase0_result FAIL "$label" "owned probe object could not be deleted"
  return 1
}

if [[ "$PHASE0_MODE" == cleanup ]]; then
  if ! prepare_aws_config; then
    phase0_objectstore_finish
    exit $?
  fi
  delete_owned_object cleanup-concurrent "$OBJECTSTORE_CONCURRENT_KEY"
  delete_owned_object cleanup-ambiguous "$OBJECTSTORE_AMBIGUOUS_KEY"
  phase0_objectstore_finish
  exit $?
fi

if ! prepare_aws_config; then
  phase0_objectstore_finish
  exit $?
fi

write_probe_payloads() {
  local i
  for ((i = 1; i <= PHASE0_OBJECTSTORE_WRITERS; i++)); do
    printf 'agents-gateway phase0 object-store conformance\nrun=%s\nwriter=%02d\nbytes=deterministic\n' \
      "$PHASE0_RUN_ID" "$i" >"$OBJECTSTORE_PAYLOAD_DIR/writer-${i}.bin" || return 1
  done
  printf 'agents-gateway phase0 conflict payload\nrun=%s\nbytes=must-not-overwrite\n' \
    "$PHASE0_RUN_ID" >"$OBJECTSTORE_PAYLOAD_DIR/conflict.bin" || return 1
  printf 'agents-gateway phase0 ambiguous transport payload\nrun=%s\nbytes=unknown-effect-fixture\n' \
    "$PHASE0_RUN_ID" >"$OBJECTSTORE_PAYLOAD_DIR/ambiguous.bin" || return 1
}

payload_digest() {
  sha256sum "$1" | awk '{print $1}'
}

if ! write_probe_payloads; then
  phase0_result FAIL payloads "could not create deterministic probe payloads"
  phase0_objectstore_finish
  exit $?
fi

printf 'concurrent_key=%s\nambiguous_key=%s\n' \
  "$OBJECTSTORE_CONCURRENT_KEY" "$OBJECTSTORE_AMBIGUOUS_KEY" \
  >"$PHASE0_RUN_DIR/object-keys.txt"

# This is an access probe as well as an early credential/endpoint classifier.
# A 404/NoSuchKey is expected for a fresh unique key; auth and transport
# failures are recorded as INCONCLUSIVE prerequisites, while other failures
# are real conformance failures.
probe_label="access-probe"
if aws_call "$probe_label" head-object --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$OBJECTSTORE_CONCURRENT_KEY"; then
  phase0_result FAIL objectstore-access "probe key already exists; refusing to test or overwrite it"
  phase0_objectstore_finish
  exit $?
else
  probe_rc=$?
  if (( probe_rc == 125 )); then
    phase0_objectstore_finish
    exit $?
  fi
  if aws_error_contains "$probe_label" 'NoSuchKey|NotFound|404|Not Found'; then
    phase0_result PASS objectstore-access "endpoint and bucket accepted an authenticated absence probe"
  elif aws_error_contains "$probe_label" 'Unable to locate credentials|NoCredentials|ExpiredToken|Could not connect|EndpointConnectionError|timed out|timeout|Connection reset|Connection refused|Network is unreachable|certificate verify failed|UnknownEndpoint'; then
    phase0_result SKIP objectstore-access "credentials or live endpoint transport is unavailable"
    phase0_objectstore_finish
    exit $?
  elif aws_error_contains "$probe_label" 'AccessDenied|AccessDeniedException|NoSuchBucket|InvalidAccessKeyId|SignatureDoesNotMatch|PermanentRedirect'; then
    phase0_result FAIL objectstore-access "endpoint, bucket, or credential permissions rejected the access probe"
    phase0_objectstore_finish
    exit $?
  else
    phase0_result FAIL objectstore-access "access probe failed without an expected S3 absence response"
    phase0_objectstore_finish
    exit $?
  fi
fi

run_conditional_put() {
  local label="$1" key="$2" body="$3"
  aws_call "$label" put-object \
    --bucket "$PHASE0_OBJECTSTORE_BUCKET" \
    --key "$key" \
    --body "$body" \
    --content-type application/octet-stream \
    --metadata "agw-phase0-run=$PHASE0_RUN_ID" \
    --if-none-match '*'
}

writer_pids=()
writer_labels=()
writer_index=1
while (( writer_index <= PHASE0_OBJECTSTORE_WRITERS )); do
  label="writer-${writer_index}"
  run_conditional_put "$label" "$OBJECTSTORE_CONCURRENT_KEY" \
    "$OBJECTSTORE_PAYLOAD_DIR/writer-${writer_index}.bin" &
  writer_pids+=("$!")
  writer_labels+=("$label")
  ((writer_index += 1))
done

writer_successes=0
writer_winner=""
writer_unexpected=0
for index in "${!writer_pids[@]}"; do
  if wait "${writer_pids[$index]}"; then
    ((writer_successes += 1))
    if [[ -n "$writer_winner" ]]; then
      writer_unexpected=1
    else
      writer_winner="${writer_labels[$index]}"
    fi
  else
    writer_rc=$?
    if (( writer_rc == 125 )); then
      writer_unexpected=1
    elif ! is_expected_conflict "${writer_labels[$index]}"; then
      writer_unexpected=1
    fi
  fi
done

if (( writer_successes == 1 && writer_unexpected == 0 )); then
  phase0_result PASS conditional-concurrency \
    "exactly one of ${PHASE0_OBJECTSTORE_WRITERS} If-None-Match writers created the object"
else
  phase0_result FAIL conditional-concurrency \
    "expected exactly one winner and only conditional conflicts; successes=${writer_successes}"
fi

winner_payload=""
if [[ -n "$writer_winner" ]]; then
  winner_payload="$OBJECTSTORE_PAYLOAD_DIR/${writer_winner}.bin"
  winner_digest="$(payload_digest "$winner_payload")"
  retrieved="$PHASE0_RUN_DIR/concurrent-retrieved.bin"
  if aws_call concurrent-get --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$OBJECTSTORE_CONCURRENT_KEY" "$retrieved" && \
     cmp --silent "$winner_payload" "$retrieved"; then
    printf 'expected_sha256=%s\nretrieved_sha256=%s\n' "$winner_digest" "$(payload_digest "$retrieved")" \
      >"$PHASE0_RUN_DIR/exact-retrieval.txt"
    phase0_result PASS exact-byte-retrieval "retrieved object bytes exactly match the sole winning writer"
  else
    phase0_result FAIL exact-byte-retrieval "retrieved bytes did not exactly match the winning writer"
  fi
else
  phase0_result FAIL exact-byte-retrieval "no winning writer was available for retrieval"
fi

if [[ -n "$winner_payload" ]]; then
  if run_conditional_put conflict-attempt "$OBJECTSTORE_CONCURRENT_KEY" \
      "$OBJECTSTORE_PAYLOAD_DIR/conflict.bin"; then
    phase0_result FAIL conflict-no-overwrite "a later conditional write unexpectedly succeeded"
  else
    conflict_rc=$?
    if (( conflict_rc != 125 )) && is_expected_conflict conflict-attempt; then
      after_conflict="$PHASE0_RUN_DIR/after-conflict.bin"
      if aws_call conflict-get --bucket "$PHASE0_OBJECTSTORE_BUCKET" \
           --key "$OBJECTSTORE_CONCURRENT_KEY" "$after_conflict" && \
         cmp --silent "$winner_payload" "$after_conflict"; then
        phase0_result PASS conflict-no-overwrite \
          "conditional conflict was rejected and the original bytes remained unchanged"
      else
        phase0_result FAIL conflict-no-overwrite \
          "conditional conflict response was observed but original bytes changed or could not be retrieved"
      fi
    else
      phase0_result FAIL conflict-no-overwrite "later conditional write failed without a recognized conflict response"
    fi
  fi
fi

start_fault_fixture() {
  local fixture_dir="$PHASE0_RUN_DIR/objectstore-fixture"
  local ready_file="$fixture_dir/ready.txt"
  mkdir -p "$fixture_dir" || return 1
  python3 "$SCRIPT_DIR/fixtures/objectstore/ambiguous_s3.py" \
    --host 127.0.0.1 \
    --port 0 \
    --bucket "$PHASE0_OBJECTSTORE_BUCKET" \
    --fail-key "$OBJECTSTORE_AMBIGUOUS_KEY" \
    --ready-file "$ready_file" \
    >"$fixture_dir/stdout.txt" 2>"$fixture_dir/stderr.txt" &
  OBJECTSTORE_FIXTURE_PID="$!"
  local attempt endpoint
  for ((attempt = 1; attempt <= 40; attempt++)); do
    if [[ -s "$ready_file" ]]; then
      endpoint="$(sed -n 's/^endpoint=//p' "$ready_file" | head -n 1)"
      if [[ "$endpoint" =~ ^http://127\.0\.0\.1:[0-9]+$ ]]; then
        OBJECTSTORE_FIXTURE_ENDPOINT="${endpoint/127.0.0.1/localhost}"
        return 0
      fi
      return 1
    fi
    if ! kill -0 "$OBJECTSTORE_FIXTURE_PID" 2>/dev/null; then
      return 1
    fi
    sleep 0.1
  done
  return 1
}

stop_fault_fixture() {
  if [[ -n "$OBJECTSTORE_FIXTURE_PID" ]]; then
    kill "$OBJECTSTORE_FIXTURE_PID" 2>/dev/null || true
    wait "$OBJECTSTORE_FIXTURE_PID" 2>/dev/null || true
    OBJECTSTORE_FIXTURE_PID=""
  fi
  if [[ -d "$PHASE0_RUN_DIR/objectstore-fixture" ]]; then
    sanitize_evidence "$PHASE0_RUN_DIR/objectstore-fixture/stdout.txt" || true
    sanitize_evidence "$PHASE0_RUN_DIR/objectstore-fixture/stderr.txt" || true
  fi
  if [[ -n "$OBJECTSTORE_AWS_CONFIG" ]]; then
    rm -f -- "$OBJECTSTORE_AWS_CONFIG"
    OBJECTSTORE_AWS_CONFIG=""
  fi
}
trap stop_fault_fixture EXIT

if start_fault_fixture; then
  fixture_put_label="fixture-ambiguous-put"
  fixture_stdout="$PHASE0_RUN_DIR/${fixture_put_label}.stdout"
  fixture_stderr="$PHASE0_RUN_DIR/${fixture_put_label}.stderr"
  if AWS_EC2_METADATA_DISABLED=true AWS_PAGER="" AWS_ACCESS_KEY_ID=phase0-fixture \
       AWS_SECRET_ACCESS_KEY=phase0-fixture-secret AWS_DEFAULT_REGION="$PHASE0_OBJECTSTORE_REGION" \
       AWS_MAX_ATTEMPTS="$PHASE0_OBJECTSTORE_MAX_ATTEMPTS" AWS_RETRY_MODE=standard \
       "$PHASE0_AWS_CLI_BIN" s3api \
       --endpoint-url "$OBJECTSTORE_FIXTURE_ENDPOINT" \
       --region "$PHASE0_OBJECTSTORE_REGION" \
       --cli-connect-timeout "$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
       --cli-read-timeout "$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
       put-object --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$OBJECTSTORE_AMBIGUOUS_KEY" \
       --body "$OBJECTSTORE_PAYLOAD_DIR/ambiguous.bin" --content-type application/octet-stream \
       --metadata "agw-phase0-run=$PHASE0_RUN_ID" --if-none-match '*' \
       >"$fixture_stdout" 2>"$fixture_stderr"; then
    fixture_rc=0
  else
    fixture_rc=$?
  fi
  sanitize_evidence "$fixture_stdout" || true
  sanitize_evidence "$fixture_stderr" || true
  if (( fixture_rc != 0 )); then
    fixture_state="$PHASE0_RUN_DIR/fixture-ambiguous-state.bin"
    if AWS_EC2_METADATA_DISABLED=true AWS_PAGER="" AWS_ACCESS_KEY_ID=phase0-fixture \
         AWS_SECRET_ACCESS_KEY=phase0-fixture-secret AWS_DEFAULT_REGION="$PHASE0_OBJECTSTORE_REGION" \
         "$PHASE0_AWS_CLI_BIN" s3api \
         --endpoint-url "$OBJECTSTORE_FIXTURE_ENDPOINT" \
         --region "$PHASE0_OBJECTSTORE_REGION" \
         --cli-connect-timeout "$PHASE0_OBJECTSTORE_CONNECT_TIMEOUT" \
         --cli-read-timeout "$PHASE0_OBJECTSTORE_READ_TIMEOUT" \
         get-object --bucket "$PHASE0_OBJECTSTORE_BUCKET" --key "$OBJECTSTORE_AMBIGUOUS_KEY" \
         "$fixture_state" >"$PHASE0_RUN_DIR/fixture-state.stdout" 2>"$PHASE0_RUN_DIR/fixture-state.stderr"; then
      if cmp --silent "$OBJECTSTORE_PAYLOAD_DIR/ambiguous.bin" "$fixture_state"; then
        fixture_observation=present-and-exact
      else
        fixture_observation=present-but-different
      fi
    else
      fixture_observation=unknown-or-absent
    fi
    sanitize_evidence "$PHASE0_RUN_DIR/fixture-state.stdout" || true
    sanitize_evidence "$PHASE0_RUN_DIR/fixture-state.stderr" || true
    printf 'client_result=nonzero\nobserved_state=%s\nclassification=unknown-effect\n' \
      "$fixture_observation" >"$PHASE0_RUN_DIR/ambiguous-transport.txt"
    if [[ "$fixture_observation" == present-but-different ]]; then
      phase0_result FAIL ambiguous-transport "fault fixture returned different bytes after an ambiguous write"
    else
      phase0_result PASS ambiguous-transport \
        "fault-injected transport failure returned non-zero and was classified as unknown, never success"
    fi
  else
    phase0_result FAIL ambiguous-transport \
      "fault-injected write returned success; an ambiguous effect was not failed closed"
  fi
else
  sanitize_evidence "$PHASE0_RUN_DIR/objectstore-fixture/stdout.txt" 2>/dev/null || true
  sanitize_evidence "$PHASE0_RUN_DIR/objectstore-fixture/stderr.txt" 2>/dev/null || true
  phase0_result SKIP ambiguous-transport "python fault fixture could not be started"
fi

if [[ "$PHASE0_MODE" == cleanup || \
      ( "$PHASE0_MODE" == apply && "$PHASE0_OBJECTSTORE_CLEANUP" == true && "$PHASE0_KEEP" != 1 ) ]]; then
  delete_owned_object cleanup-concurrent "$OBJECTSTORE_CONCURRENT_KEY"
  delete_owned_object cleanup-ambiguous "$OBJECTSTORE_AMBIGUOUS_KEY"
fi

phase0_objectstore_finish
