#!/bin/sh
set -eu

IFS= read -r start_line || true
run_id=$(printf '%s' "$start_line" | sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p')
[ -n "$run_id" ] || exit 64

printf '%s\n' "{\"protocol\":\"agw.runtime.v1\",\"kind\":\"event\",\"type\":\"run.started\",\"run_id\":\"$run_id\",\"seq\":1,\"terminal\":false,\"data\":{\"agent_id\":\"runtime-fixture\",\"sandbox_id\":\"runtime-fixture\"}}"

fail_check() {
  message=$1
  printf '%s\n' "{\"protocol\":\"agw.runtime.v1\",\"kind\":\"event\",\"type\":\"run.failed\",\"run_id\":\"$run_id\",\"seq\":2,\"terminal\":true,\"data\":{\"error\":{\"code\":\"sandbox_boundary_violation\",\"message\":\"$message\"}}}"
  exit 1
}

assert_absent() {
  path=$1
  [ ! -e "$path" ] || fail_check "unexpected path is visible: $path"
}

assert_network_denied() {
  if nslookup example.com >/dev/null 2>&1; then
    fail_check "DNS lookup unexpectedly succeeded"
  fi
  if wget -T 2 -q -O /dev/null http://1.1.1.1/ >/dev/null 2>&1; then
    fail_check "direct public-IP access unexpectedly succeeded"
  fi
  if wget -T 2 -q -O /dev/null http://169.254.169.254/latest/meta-data/ >/dev/null 2>&1; then
    fail_check "cloud metadata access unexpectedly succeeded"
  fi
  if http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 all_proxy=http://127.0.0.1:9 \
    wget -T 2 -q -O /dev/null http://1.1.1.1/ >/dev/null 2>&1; then
    fail_check "proxy-mediated network access unexpectedly succeeded"
  fi
}

assert_hardening() {
  host_run_file="/tmp/agw-host-file-$run_id"
  host_socket="/tmp/agw-host-socket-$run_id"
  host_credential="/tmp/agw-host-credential-$run_id"
  assert_absent "$host_run_file"
  assert_absent "$host_socket"
  assert_absent "$host_credential"
  assert_absent "/run/secrets/agw-host-credential-$run_id"

  if env | grep -Eq '^(AWS_ACCESS_KEY_ID|OPENAI_API_KEY|GITHUB_TOKEN|CLOUDFLARE_API_TOKEN)='; then
    fail_check "host credential environment leaked into sandbox"
  fi
  if [ "${AGW_E2E_SECRET:-}" != 'standalone secret="materialization"#proof\with=equals' ]; then
    fail_check "opaque runner-managed secret was not materialized into the sandbox"
  fi
  if id -u | grep -qx '0'; then
    fail_check "sandbox process is root"
  fi
  if ! awk '$1 == "NoNewPrivs:" && $2 == "1" { found=1 } END { exit(found ? 0 : 1) }' /proc/self/status; then
    fail_check "no-new-privileges is not enabled"
  fi
  if ! awk '$1 == "CapEff:" && $2 == "0000000000000000" { found=1 } END { exit(found ? 0 : 1) }' /proc/self/status; then
    fail_check "effective capabilities were not dropped"
  fi
  if ! awk '$2 == "/" && $4 ~ /(^|,)ro(,|$)/ { found=1 } END { exit(found ? 0 : 1) }' /proc/mounts; then
    fail_check "container root filesystem is not read-only"
  fi
  if touch "/etc/agw-readonly-$run_id" 2>/dev/null; then
    fail_check "write to the read-only root filesystem succeeded"
  fi
  assert_network_denied
}

case "$run_id" in
  concurrent-a)
    assert_hardening
    assert_absent /workspace/concurrent-b-marker
    printf '%s\n' a-only >/workspace/concurrent-a-marker || fail_check "workspace-a write failed"
    sleep 4
    ;;
  concurrent-b)
    assert_hardening
    assert_absent /workspace/concurrent-a-marker
    printf '%s\n' b-only >/workspace/concurrent-b-marker || fail_check "workspace-b write failed"
    sleep 4
    ;;
esac

case "$run_id" in
  cancel-*)
    seq=2
    while :; do
      printf '%s\n' "{\"protocol\":\"agw.runtime.v1\",\"kind\":\"event\",\"type\":\"heartbeat\",\"run_id\":\"$run_id\",\"seq\":$seq,\"terminal\":false,\"data\":{}}"
      seq=$((seq + 1))
      sleep 1
    done
    ;;
  disk-*)
    if dd if=/dev/zero of=/workspace/quota-check bs=1048576 count=2 2>/dev/null; then
      printf '%s\n' "{\"protocol\":\"agw.runtime.v1\",\"kind\":\"event\",\"type\":\"run.failed\",\"run_id\":\"$run_id\",\"seq\":2,\"terminal\":true,\"data\":{\"error\":{\"code\":\"disk_limit_not_enforced\",\"message\":\"workspace accepted bytes beyond the declared limit\"}}}"
      exit 1
    fi
    ;;
esac

printf '%s\n' "{\"protocol\":\"agw.runtime.v1\",\"kind\":\"event\",\"type\":\"run.completed\",\"run_id\":\"$run_id\",\"seq\":2,\"terminal\":true,\"data\":{\"result\":\"completed\",\"output\":{\"id\":\"fixture-output-$run_id-sandbox-acceptance-pass\",\"uri\":\"artifact://runtime-fixture/$run_id\",\"digest\":\"sha256:0000000000000000000000000000000000000000000000000000000000000000\",\"size_bytes\":0,\"media_type\":\"application/json\"}}}"
