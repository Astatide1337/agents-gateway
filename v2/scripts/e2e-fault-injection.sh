#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/.." && pwd)
cd "$repo_root"

echo "Running gate 4 process-crash, control-plane, MCP ambiguity, and artifact compensation tests"
runner_status=0
go test -count=1 ./cmd/agw-runner || runner_status=$?
go test -count=1 ./pkg/brokerdispatch ./pkg/toolbroker ./pkg/artifact

echo "Running PostgreSQL fault-injection tests (set AGW_TEST_DATABASE_URL for the live database proof)"
go test -count=1 ./pkg/store -run '^TestPostgreSQLFaultInjection'
go test -count=1 ./pkg/localengine -run '^TestPostgreSQLFaultInjection'

if (( runner_status != 0 )); then
	printf '%s\n' "Gate 4 runner process-crash test is blocked by the existing cmd/agw-runner compile failure; see the output above." >&2
	exit "$runner_status"
fi

echo "Gate 4 fault-injection tests passed"
