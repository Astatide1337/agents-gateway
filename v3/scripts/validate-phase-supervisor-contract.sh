#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Static, local-only guard for the trusted phase-supervisor deployment boundary.
# This script deliberately does not render, apply, or contact a cluster. It
# prevents a future manifest change from silently turning a non-supervised work
# pod into a claimed phase-supervised one.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
V3_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
REPO_ROOT=$(CDPATH= cd -- "$V3_DIR/.." && pwd)

WORKLOAD="$V3_DIR/internal/workload/workload.go"
WORKLOAD_TEST="$V3_DIR/internal/workload/workload_test.go"
CHART_VALUES="$V3_DIR/charts/agw-operator/values.yaml"
CHART_SCHEMA="$V3_DIR/charts/agw-operator/values.schema.json"
CHART_HELPERS="$V3_DIR/charts/agw-operator/templates/_helpers.tpl"
LIFECYCLE="$V3_DIR/cmd/agw-agent-run-lifecycle/main.go"
CONTRACT="$REPO_ROOT/docs/v3/trusted-phase-supervisor-contract.md"

fail() {
	printf 'trusted phase supervisor contract failed: %s\n' "$1" >&2
	exit 1
}

grep -Eq 'ShareProcessNamespace:[[:space:]]+boolPtr\(false\)' "$WORKLOAD" || fail 'workload does not explicitly keep PID namespaces private'
grep -Fq 'TrustedPhaseSupervisorAnnotationKey' "$WORKLOAD" || fail 'workload supervisor status annotation is missing'
grep -Fq 'TrustedPhaseSupervisorDisabled' "$WORKLOAD" || fail 'workload supervisor disabled value is missing'
grep -Fq 'shareProcessNamespace must be explicitly false' "$WORKLOAD_TEST" || fail 'workload test does not enforce private PID namespaces'
grep -Fq 'phaseSupervisor:' "$CHART_VALUES" || fail 'chart phaseSupervisor values contract is missing'
grep -Fq 'enabled: false' "$CHART_VALUES" || fail 'chart phase supervisor is not disabled by default'
grep -Fq 'phaseSupervisor.enabled' "$CHART_HELPERS" || fail 'chart render-time fail-closed guard is missing'
grep -Fq 'no trusted phase supervisor is wired' "$CHART_HELPERS" || fail 'chart guard does not explain the missing binding'
grep -Fq '"phaseSupervisor"' "$CHART_SCHEMA" || fail 'chart schema does not define phaseSupervisor'
grep -Fq '"const": false' "$CHART_SCHEMA" || fail 'chart schema does not constrain enablement to false'
grep -Fq 'There is intentionally no supervise mode' "$LIFECYCLE" || fail 'lifecycle command does not document the absent supervisor mode'
grep -Fq '"supervise"' "$V3_DIR/cmd/agw-agent-run-lifecycle/main_test.go" || fail 'lifecycle test does not reject an unbound supervise mode'
test -s "$CONTRACT" || fail 'trusted phase supervisor runbook is missing'

if rg -n 'ShareProcessNamespace:\s*boolPtr\(true\)|AGW_(PHASE|RUNTIME_PHASE|PROCESS_EXIT)_TOKEN' "$WORKLOAD"; then
	fail 'workload contains an unsafe PID-sharing or phase/process token projection'
fi

python3 - "$CHART_SCHEMA" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    schema = json.load(handle)

phase = schema["properties"]["phaseSupervisor"]
assert phase["properties"]["enabled"]["const"] is False
assert "phaseSupervisor" in schema["required"]
PY

printf '%s\n' 'trusted phase supervisor deployment contract: fail-closed guard is intact'
