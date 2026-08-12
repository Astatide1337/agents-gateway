#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
smoke="$script_dir/local-api-smoke.sh"
test -f "$smoke"
bash -n "$smoke"

help_output=$(bash "$smoke" --help)
grep -Fq -- '--skip-argo' <<<"$help_output"
grep -Fq -- 'AGW_API_SMOKE_CLUSTER_NAME' <<<"$help_output"

test_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-local-api-smoke-test.XXXXXX")
trap 'rm -rf -- "$test_dir"' EXIT
kind_log="$test_dir/kind.log"
output="$test_dir/output.log"
fake_kind="$test_dir/fake-kind"
fake_kubectl="$test_dir/fake-kubectl"
fake_helm="$test_dir/fake-helm"

cat >"$fake_kind" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"${FAKE_KIND_LOG:?}"
case "${1:-}:${2:-}" in
  version:*) printf '%s\n' 'kind v0.0.0 fake' ;;
  get:clusters)
    if [[ "${FAKE_KIND_MODE:-collision}" == collision ]]; then
      printf '%s\n' "${AGW_API_SMOKE_CLUSTER_NAME:?}"
    fi
    ;;
  create:cluster)
    if [[ "${FAKE_KIND_MODE:-collision}" == fail-create ]]; then
      exit 42
    fi
    exit 99
    ;;
  delete:cluster) ;;
  *) exit 99 ;;
esac
EOF
cat >"$fake_kubectl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == version ]]; then
  printf '%s\n' 'clientVersion: fake'
  exit 0
fi
exit 99
EOF
cat >"$fake_helm" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == version ]]; then
  printf '%s\n' 'v3.0.0+fake'
  exit 0
fi
exit 99
EOF
chmod +x "$fake_kind" "$fake_kubectl" "$fake_helm"

run_smoke() {
  local mode=$1 name=$2
  : >"$kind_log"
  set +e
  env \
    AGW_KIND_BIN="$fake_kind" \
    AGW_KUBECTL_BIN="$fake_kubectl" \
    AGW_HELM_BIN="$fake_helm" \
    AGW_API_SMOKE_CLUSTER_NAME="$name" \
    FAKE_KIND_LOG="$kind_log" \
    FAKE_KIND_MODE="$mode" \
    bash "$smoke" --skip-argo >"$output" 2>&1
  smoke_status=$?
  set -e
}

collision_name=agw-v3-api-collision-test
run_smoke collision "$collision_name"
test "$smoke_status" -eq 2
grep -Fq -- "refusing to reuse existing disposable cluster name: $collision_name" "$output"
! grep -Fq -- 'create cluster' "$kind_log"
! grep -Fq -- 'delete cluster' "$kind_log"

invalid_name=agw-v3-api-unsafe/name
run_smoke none "$invalid_name"
test "$smoke_status" -eq 2
grep -Fq -- 'cluster name must be a lowercase DNS label' "$output"
! test -s "$kind_log"

failed_name=agw-v3-api-create-failure-test
run_smoke fail-create "$failed_name"
test "$smoke_status" -eq 42
grep -Fq -- 'FAILED during create disposable Kind cluster' "$output"
! grep -Fq -- 'delete cluster' "$kind_log"

printf '%s\n' 'local-api-smoke script tests: PASS'
