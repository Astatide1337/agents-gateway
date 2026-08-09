#!/usr/bin/env bash
# Real rootless-Podman runner E2E. It intentionally does not fake the runtime,
# invoke Docker, pull images, or pretend that the current Temporal server is a
# local engine. The fixture base image must already exist locally.
set -Eeuo pipefail
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
v2_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
fixture_dir="$v2_dir/testdata/runtime-fixture"

runner_binary=${AGW_RUNNER_BINARY:-/usr/local/bin/agw-runner}
fixture_base=${AGW_FIXTURE_BASE_IMAGE:-busybox:latest}
fixture_image=${AGW_FIXTURE_IMAGE:-localhost/agw-runtime-fixture:e2e}
run_timeout=${AGW_E2E_TIMEOUT_SECONDS:-45}
real_podman_binary=$(command -v podman 2>/dev/null || true)
runner_podman_binary=${AGW_RUNNER_PODMAN_BINARY:-$real_podman_binary}

fail() {
  printf 'e2e-standalone: %s\n' "$*" >&2
  exit 1
}

need_command() {
  command -v "$1" >/dev/null 2>&1 || fail "missing prerequisite: $1"
}

[[ $(uname -s) == Linux ]] || fail "rootless Podman E2E requires Linux"
[[ ${EUID} -ne 0 ]] || fail "run this E2E as the dedicated unprivileged runner user, not root"
need_command podman
need_command mktemp
need_command sed
need_command sha256sum
need_command stat
need_command awk
need_command timeout
need_command grep
need_command comm
need_command sort
need_command python3
[[ -x $runner_binary && ! -d $runner_binary ]] || fail "runner binary is not executable: $runner_binary"
[[ -d $fixture_dir && -f $fixture_dir/Containerfile && -x $fixture_dir/runtime.sh ]] || fail "runtime fixture assets are incomplete"
[[ -x $runner_podman_binary && ! -d $runner_podman_binary ]] || fail "runner Podman binary is not executable: $runner_podman_binary"
[[ -x $real_podman_binary && ! -d $real_podman_binary ]] || fail "real Podman binary is not executable: $real_podman_binary"
[[ $run_timeout =~ ^[1-9][0-9]*$ ]] || fail "AGW_E2E_TIMEOUT_SECONDS must be a positive integer"
export AGW_REAL_PODMAN_BINARY="$real_podman_binary"

rootless=$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null) || fail "Podman is not ready for this user"
[[ $rootless == true ]] || fail "Podman is not rootless for this user"
podman image exists "$fixture_base" || fail "fixture base image $fixture_base is not present locally; preload it explicitly (the E2E never pulls)"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-standalone-e2e.XXXXXX")
serve_pid=
socket_pids=()
probe_paths=()
owned_filter='label=io.agents-gateway.owner=agw-runner'
baseline_containers="$tmp_dir/baseline-containers"
podman ps -aq --filter "$owned_filter" | sort -u >"$baseline_containers"
cleanup() {
  for socket_pid in "${socket_pids[@]}"; do
    if [[ -n $socket_pid ]] && kill -0 "$socket_pid" 2>/dev/null; then
      kill -TERM "$socket_pid" 2>/dev/null || true
      wait "$socket_pid" 2>/dev/null || true
    fi
  done
  for probe_path in "${probe_paths[@]}"; do
    rm -f -- "$probe_path"
  done
  if [[ -n ${serve_pid:-} ]] && kill -0 "$serve_pid" 2>/dev/null; then
    kill -TERM "$serve_pid" 2>/dev/null || true
    wait "$serve_pid" 2>/dev/null || true
  fi
  current_containers="$tmp_dir/current-containers"
  podman ps -aq --filter "$owned_filter" 2>/dev/null | sort -u >"$current_containers" || true
  comm -13 "$baseline_containers" "$current_containers" 2>/dev/null | while IFS= read -r container_id; do
    [[ -n $container_id ]] && podman rm -f "$container_id" >/dev/null 2>&1 || true
  done
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

fixture_id_file="$tmp_dir/fixture.id"
podman build --pull=never --format oci --build-arg "BASE_IMAGE=$fixture_base" \
  --iidfile "$fixture_id_file" -t "$fixture_image" "$fixture_dir" >/dev/null \
  || fail "could not build the local runtime fixture from the preloaded base image"
fixture_id=$(tr -d '[:space:]' <"$fixture_id_file")
[[ $fixture_id =~ ^sha256:[0-9a-f]{64}$ ]] || fail "Podman did not produce a usable fixture image ID: $fixture_id"
fixture_digest=$(podman image inspect "$fixture_id" --format '{{.Digest}}' 2>/dev/null | tr -d '[:space:]')
[[ $fixture_digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail "Podman did not produce a usable fixture manifest digest: $fixture_digest"
podman image inspect "$fixture_image@$fixture_digest" >/dev/null 2>&1 \
  || fail "Podman cannot resolve the fixture by its immutable digest: $fixture_image@$fixture_digest"

e2e_config="$tmp_dir/runner.yaml"
e2e_workspace="$tmp_dir/workspaces"
e2e_socket_dir="$tmp_dir/socket"
e2e_socket="$e2e_socket_dir/runner.sock"
e2e_secret_source="$tmp_dir/secret-source"
e2e_secret_materialized="$tmp_dir/secret-materialized"
e2e_secret_value='standalone secret="materialization"#proof\with=equals'
install -d -m 0700 "$e2e_workspace" "$e2e_socket_dir" "$e2e_secret_source" "$e2e_secret_materialized"
printf '%s' "$e2e_secret_value" >"$e2e_secret_source/e2e-provider"
chmod 0600 "$e2e_secret_source/e2e-provider"
printf '%s\n' \
  'backend: podman' \
  "workspaceRoot: $e2e_workspace" \
  'secrets:' \
  "  sourceRoot: $e2e_secret_source" \
  "  materializationRoot: $e2e_secret_materialized" \
  'podman:' \
  "  binary: $runner_podman_binary" \
  'server:' \
  '  transport: unix' \
  "  socketPath: $e2e_socket" >"$e2e_config"
chmod 0600 "$e2e_config"

doctor_output=$($runner_binary doctor --config "$e2e_config") || fail "agw-runner doctor failed: $doctor_output"
printf '%s\n' "$doctor_output"
printf '%s\n' "$doctor_output" | awk '/"ready":true/ { found=1 } END { exit(found ? 0 : 1) }' \
  || fail "agw-runner doctor did not report ready=true"

start_server() {
  "$runner_binary" serve --config "$e2e_config" >"$tmp_dir/serve.log" 2>"$tmp_dir/serve.err" &
  serve_pid=$!
  for attempt in {1..20}; do
    [[ -S $e2e_socket ]] && return 0
    kill -0 "$serve_pid" 2>/dev/null || break
    sleep 1
  done
  cat "$tmp_dir/serve.err" >&2 || true
  fail "runner serve did not create the Unix socket"
}

stop_server() {
  if [[ -n ${serve_pid:-} ]] && kill -0 "$serve_pid" 2>/dev/null; then
    kill -TERM "$serve_pid"
    wait "$serve_pid" 2>/dev/null || true
  fi
  serve_pid=
  [[ ! -e $e2e_socket ]] || fail "runner serve left its Unix socket after shutdown"
}

start_server
stop_server
start_server

future=$(date -u -d '+10 minutes' '+%Y-%m-%dT%H:%M:%SZ')
run_request() {
  local run_id=$1
  local request_file="$tmp_dir/$run_id.json"
  local output_file="$tmp_dir/$run_id.out"
  local error_file="$tmp_dir/$run_id.err"
  local uid gid
  uid=$(id -u)
  gid=$(id -g)
  cat >"$request_file" <<EOF
{
  "run_id": "$run_id",
  "lease": {"run_id":"$run_id","lease_id":"e2e-lease","owner":"e2e-runner","fencing_token":1,"expires_at":"$future"},
  "current_lease": {"run_id":"$run_id","lease_id":"e2e-lease","owner":"e2e-runner","fencing_token":1,"expires_at":"$future"},
  "spec": {
    "backend":"podman", "isolation_grade":"standard/shared-kernel",
    "image":"$fixture_image", "image_digest":"$fixture_digest", "run_as_user":"$uid:$gid",
    "read_only_root_fs":true, "no_new_privileges":true, "drop_all_capabilities":true,
    "network":"none", "direct_internet":false, "privileged":false,
    "resources":{"cpus":0.25,"memory_bytes":67108864,"disk_bytes":1048576,"pids":64,"timeout":30000000000},
    "mounts":[{"kind":"workspace","destination":"/workspace","read_only":false}]
  },
  "contract": {
    "organization_id":"e2e-org", "project_id":"e2e-project", "run_id":"$run_id",
    "workflow_name":"standalone-e2e", "step_id":"fixture", "agent_ref":"fixture-agent",
    "execution": {
      "agent":{"kind":"Agent","name":"fixture-agent","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"},
      "sandbox_profile":{"kind":"SandboxProfile","name":"podman","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"},
      "instructions_ref":"artifact://e2e/instructions", "instructions":"emit the deterministic fixture result",
      "environment":[{"name":"AGW_E2E_SECRET","ref":"secret://e2e-provider"}]
    }
  }
}
EOF
  if [[ $run_id == cancel-* ]]; then
    timeout --foreground "$run_timeout" "$runner_binary" run --config "$e2e_config" --request "$request_file" >"$output_file" 2>"$error_file" &
    run_pid=$!
    for attempt in {1..20}; do
      grep -Eq 'run\.started' "$output_file" 2>/dev/null && break
      kill -0 "$run_pid" 2>/dev/null || fail "cancel fixture exited before cancellation"
      sleep 1
    done
    kill -TERM "$run_pid" 2>/dev/null || true
    wait "$run_pid" 2>/dev/null && fail "cancel fixture unexpectedly succeeded"
    [[ -f $error_file ]]
  else
    timeout --foreground "$run_timeout" "$runner_binary" run --config "$e2e_config" --request "$request_file" >"$output_file" 2>"$error_file" \
      || { cat "$error_file" >&2; cat "$output_file" >&2; fail "success fixture failed"; }
    grep -Eq 'run\.completed' "$output_file" || fail "success fixture did not emit run.completed"
  fi
  if find "$e2e_workspace" -mindepth 1 -maxdepth 1 ! -name tasks -print -quit | grep -q .; then
    find "$e2e_workspace" -mindepth 1 -maxdepth 1 ! -name tasks -print >&2
    fail "runner left a workspace after $run_id"
  fi
  if find "$e2e_secret_materialized" -mindepth 1 -print -quit | grep -q .; then
    find "$e2e_secret_materialized" -mindepth 1 -print >&2
    fail "runner left materialized secret state after $run_id"
  fi
  if grep -Fq "$e2e_secret_value" "$request_file" "$output_file" "$error_file" "$tmp_dir/serve.log" "$tmp_dir/serve.err"; then
    fail "materialized secret value appeared in a request, event, or runner log after $run_id"
  fi
  for attempt in {1..15}; do
    podman ps -aq --filter "$owned_filter" | sort -u >"$tmp_dir/after-$run_id-containers"
    if ! comm -13 "$baseline_containers" "$tmp_dir/after-$run_id-containers" | grep -q .; then
      break
    fi
    sleep 1
  done
  if comm -13 "$baseline_containers" "$tmp_dir/after-$run_id-containers" | grep -q .; then
    podman ps -a --filter "$owned_filter" >&2
    fail "runner left an agw container after $run_id"
  fi
}

run_request success-e2e
run_request disk-e2e
run_request cancel-e2e

prepare_host_probes() {
  local run_id=$1
  local host_file="/tmp/agw-host-file-$run_id"
  local host_socket="/tmp/agw-host-socket-$run_id"
  local host_credential="/tmp/agw-host-credential-$run_id"
  probe_paths+=("$host_file" "$host_socket" "$host_credential")
  printf 'host-only-%s\n' "$run_id" >"$host_file"
  printf 'credential-must-not-cross-boundary\n' >"$host_credential"
  python3 - "$host_socket" <<'PY' &
import socket
import sys

path = sys.argv[1]
server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
server.bind(path)
server.listen(1)
try:
    while True:
        connection, _ = server.accept()
        connection.close()
finally:
    server.close()
PY
  socket_pids+=("$!")
  for attempt in {1..20}; do
    [[ -S $host_socket ]] && return 0
    kill -0 "${socket_pids[${#socket_pids[@]}-1]}" 2>/dev/null || break
    sleep 0.1
  done
  fail "host probe socket did not become ready: $host_socket"
}

run_concurrent_request() {
  local run_id=$1
  local request_file="$tmp_dir/$run_id.json"
  local output_file="$tmp_dir/$run_id.out"
  local error_file="$tmp_dir/$run_id.err"
  local uid gid
  uid=$(id -u)
  gid=$(id -g)
  cat >"$request_file" <<EOF
{
  "run_id": "$run_id",
  "lease": {"run_id":"$run_id","lease_id":"e2e-lease-$run_id","owner":"e2e-runner","fencing_token":1,"expires_at":"$future"},
  "current_lease": {"run_id":"$run_id","lease_id":"e2e-lease-$run_id","owner":"e2e-runner","fencing_token":1,"expires_at":"$future"},
  "spec": {
    "backend":"podman", "isolation_grade":"standard/shared-kernel",
    "image":"$fixture_image", "image_digest":"$fixture_digest", "run_as_user":"$uid:$gid",
    "read_only_root_fs":true, "no_new_privileges":true, "drop_all_capabilities":true,
    "network":"none", "direct_internet":false, "privileged":false,
    "resources":{"cpus":0.25,"memory_bytes":67108864,"disk_bytes":1048576,"pids":64,"timeout":30000000000},
    "mounts":[{"kind":"workspace","destination":"/workspace","read_only":false}]
  },
  "contract": {
    "organization_id":"e2e-org", "project_id":"e2e-project", "run_id":"$run_id",
    "workflow_name":"standalone-e2e", "step_id":"fixture", "agent_ref":"fixture-agent",
    "execution": {
      "agent":{"kind":"Agent","name":"fixture-agent","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"},
      "sandbox_profile":{"kind":"SandboxProfile","name":"podman","digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"},
      "instructions_ref":"artifact://e2e/instructions", "instructions":"perform bounded rootless Podman isolation acceptance",
      "environment":[{"name":"AGW_E2E_SECRET","ref":"secret://e2e-provider"}]
    }
  }
}
EOF
  timeout --foreground "$run_timeout" "$runner_binary" run --config "$e2e_config" --request "$request_file" >"$output_file" 2>"$error_file" &
  concurrent_pid=$!
}

prepare_host_probes concurrent-a
prepare_host_probes concurrent-b
run_concurrent_request concurrent-a
concurrent_a_pid=$concurrent_pid
run_concurrent_request concurrent-b
concurrent_b_pid=$concurrent_pid
for attempt in {1..30}; do
  if grep -Eq 'run\.started' "$tmp_dir/concurrent-a.out" 2>/dev/null && grep -Eq 'run\.started' "$tmp_dir/concurrent-b.out" 2>/dev/null; then
    break
  fi
  kill -0 "$concurrent_a_pid" 2>/dev/null || fail "concurrent-a exited before run.started"
  kill -0 "$concurrent_b_pid" 2>/dev/null || fail "concurrent-b exited before run.started"
  sleep 0.2
done
running_concurrent=$(podman ps -q --filter "$owned_filter" | awk 'NF { count++ } END { print count + 0 }')
[[ $running_concurrent -ge 2 ]] || fail "expected two concurrent labeled sandboxes, found $running_concurrent"
printf 'concurrent sandboxes observed: %s\n' "$running_concurrent"
while IFS= read -r sandbox_id; do
  [[ -n $sandbox_id ]] || continue
  declared_limits=$(podman inspect --format '{{.HostConfig.Memory}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}} {{index .HostConfig.Tmpfs "/workspace"}}' "$sandbox_id")
  read -r declared_memory declared_nano_cpus declared_pids declared_tmpfs <<EOF
$declared_limits
EOF
  [[ $declared_memory == 67108864 && $declared_nano_cpus == 250000000 && $declared_pids == 64 ]] \
    || fail "sandbox $sandbox_id does not carry the exact CPU/memory/PID limits: $declared_limits"
  case ",$declared_tmpfs," in
    *,rw,*size=1048576,*|*,size=1048576,*rw,*) ;;
    *) fail "sandbox $sandbox_id does not carry the exact 1 MiB writable workspace limit: $declared_tmpfs" ;;
  esac
  kernel_limits=$(podman exec "$sandbox_id" sh -c 'printf "%s|%s|%s\n" "$(cat /sys/fs/cgroup/memory.max)" "$(cat /sys/fs/cgroup/cpu.max)" "$(cat /sys/fs/cgroup/pids.max)"')
  [[ $kernel_limits == '67108864|25000 100000|64' ]] \
    || fail "sandbox $sandbox_id kernel cgroup limits do not match the contract: $kernel_limits"
done < <(podman ps -q --filter "$owned_filter")
set +e
wait "$concurrent_a_pid"
concurrent_a_status=$?
wait "$concurrent_b_pid"
concurrent_b_status=$?
set -e
[[ $concurrent_a_status -eq 0 ]] || { cat "$tmp_dir/concurrent-a.err" >&2; fail "concurrent-a failed"; }
[[ $concurrent_b_status -eq 0 ]] || { cat "$tmp_dir/concurrent-b.err" >&2; fail "concurrent-b failed"; }
grep -Eq 'run\.completed' "$tmp_dir/concurrent-a.out" || fail "concurrent-a did not complete"
grep -Eq 'run\.completed' "$tmp_dir/concurrent-b.out" || fail "concurrent-b did not complete"
grep -Eq 'sandbox-acceptance-pass' "$tmp_dir/concurrent-a.out" || fail "concurrent-a acceptance marker missing"
grep -Eq 'sandbox-acceptance-pass' "$tmp_dir/concurrent-b.out" || fail "concurrent-b acceptance marker missing"
for run_id in concurrent-a concurrent-b; do
  rm -f -- "/tmp/agw-host-file-$run_id" "/tmp/agw-host-credential-$run_id" "/tmp/agw-host-socket-$run_id"
done
for run_id in concurrent-a concurrent-b; do
  if [[ -e "/tmp/agw-host-file-$run_id" || -e "/tmp/agw-host-credential-$run_id" || -S "/tmp/agw-host-socket-$run_id" ]]; then
    fail "host probe paths were unexpectedly modified or visible for $run_id"
  fi
done
if find "$e2e_workspace" -mindepth 1 -maxdepth 1 ! -name tasks -print -quit | grep -q .; then
  fail "runner left a workspace after concurrent acceptance"
fi
if find "$e2e_secret_materialized" -mindepth 1 -print -quit | grep -q .; then
  fail "runner left materialized secret state after concurrent acceptance"
fi
while IFS= read -r -d '' candidate; do
  if grep -Fq "$e2e_secret_value" "$candidate"; then
    fail "materialized secret value appeared outside the private source file"
  fi
done < <(find "$tmp_dir" -type f ! -path "$e2e_secret_source/*" -print0)
podman ps -aq --filter "$owned_filter" | sort -u >"$tmp_dir/after-concurrent-containers"
if comm -13 "$baseline_containers" "$tmp_dir/after-concurrent-containers" | grep -q .; then
  podman ps -a --filter "$owned_filter" >&2
  fail "runner left an agw container after concurrent acceptance"
fi
stop_server

printf 'STANDALONE_PODMAN_E2E=PASS\n'
