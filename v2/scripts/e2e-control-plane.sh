#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
v2_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
project=${AGW_E2E_PROJECT:-agw-v2-e2e}

case "$project" in
  *[!a-z0-9_-]*|[!a-z0-9]*|'')
    echo "AGW_E2E_PROJECT must be a lowercase Compose project name" >&2
    exit 2
    ;;
esac

compose_file="$v2_dir/deploy/compose.yaml"
server_image="$project-server:latest"
console_image="$project-console:latest"
cli_image="$project-cli:latest"

export POSTGRES_ADMIN_PASSWORD=e2e-admin-password
export AGW_DB_PASSWORD=e2e-app-password
export AGW_AUTH_DB_PASSWORD=e2e-auth-password
export AGW_ENVIRONMENT=development
export AGW_AUTH_MODE=local
export AGW_AUTH_TOKEN=e2e-control-token-0123456789abcdef
export AGW_AUTH_PRINCIPAL_ID=e2e-admin
export AGW_LOCAL_ORGANIZATION_ID=11111111-1111-4111-8111-111111111111
export AGW_LOCAL_PROJECT_ID=22222222-2222-4222-8222-222222222222
export AGW_TEMPORAL_INSECURE=true
export AGW_SERVER_IMAGE=$server_image
export AGW_CONSOLE_IMAGE=$console_image

compose() {
  docker compose -p "$project" -f "$compose_file" --profile bundled "$@"
}

cleanup() {
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  docker image rm "$server_image" "$console_image" "$cli_image" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
cleanup

compose pull postgres temporal otel-collector
compose up -d postgres temporal otel-collector

for service in postgres temporal; do
  attempt=0
  while :; do
    state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$project-$service-1" 2>/dev/null || true)
    [ "$state" = healthy ] && break
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
      compose logs --no-color "$service"
      exit 1
    fi
    sleep 2
  done
done

compose up -d postgres-init
[ "$(docker wait "$project-postgres-init-1")" = 0 ]
compose up -d agw-migrate
[ "$(docker wait "$project-agw-migrate-1")" = 0 ]

compose build --pull agw-server agw-console
compose up -d agw-server agw-console

for service in agw-server agw-console; do
  attempt=0
  while :; do
    state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$project-$service-1" 2>/dev/null || true)
    [ "$state" = healthy ] && break
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
      compose ps
      compose logs --no-color agw-migrate temporal agw-server agw-console
      exit 1
    fi
    sleep 2
  done
done

organization_id=$AGW_LOCAL_ORGANIZATION_ID
project_id=$AGW_LOCAL_PROJECT_ID

docker build --pull -f "$v2_dir/Dockerfile.cli" -t "$cli_image" "$v2_dir/.."
server_network="container:$project-agw-server-1"
apply_output=$(docker run --rm --network "$server_network" -e AGW_TOKEN="$AGW_AUTH_TOKEN" \
  -v "$v2_dir/examples/quickstart.yaml:/manifest.yaml:ro" "$cli_image" \
  apply -server http://127.0.0.1:8094 -organization "$organization_id" -project "$project_id" -f /manifest.yaml)
printf '%s\n' "$apply_output"
workflow_digest=$(printf '%s\n' "$apply_output" | sed -n 's/^applied Workflow\/fix-issue revision=//p')
[ -n "$workflow_digest" ]

run_output=$(docker run --rm --network "$server_network" -e AGW_TOKEN="$AGW_AUTH_TOKEN" "$cli_image" \
  run -server http://127.0.0.1:8094 -organization "$organization_id" -project "$project_id" \
  -kind WorkflowRun -ref fix-issue -revision "$workflow_digest" \
  -input-ref s3://e2e-inputs/issue-1.json -idempotency-key e2e-run-1)
printf '%s\n' "$run_output"
run_id=$(printf '%s\n' "$run_output" | awk '/^run / {print $2}')
[ -n "$run_id" ]

docker run --rm --network "$server_network" -e AGW_TOKEN="$AGW_AUTH_TOKEN" "$cli_image" \
  cancel -server http://127.0.0.1:8094 -organization "$organization_id" -project "$project_id" \
  -run-id "$run_id" -idempotency-key e2e-cancel-1 -reason 'disposable e2e complete'

event_types=$(compose exec -T postgres psql -U postgres -d agents_gateway --tuples-only --no-align \
  -c "SELECT event_type FROM run_events WHERE run_id='$run_id' ORDER BY sequence")
printf '%s\n' "$event_types"
printf '%s\n' "$event_types" | grep -qx run.signal_requested
printf '%s\n' "$event_types" | grep -qx run.signal_accepted

compose ps
echo LIVE_CONTROL_PLANE_E2E=PASS
