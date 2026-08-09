#!/usr/bin/env bash
# Install the owner-operated rootless runner and validate the standalone
# Compose contract. This script never installs OS packages and never removes
# existing state. On a source checkout it builds a missing runner from the
# checksummed Go module; that build may fetch modules through the Go toolchain.
set -Eeuo pipefail
umask 077

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
v2_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
runner_user=${AGW_RUNNER_USER:-agw-runner}
runner_binary=${AGW_RUNNER_BINARY:-/usr/local/bin/agw-runner}
podman_binary=$(command -v podman 2>/dev/null || true)
env_file=${AGW_STANDALONE_ENV_FILE:-$v2_dir/deploy/.env.standalone}
config_file=${AGW_RUNNER_CONFIG:-/etc/agw/runner.yaml}
service_file=${AGW_RUNNER_SERVICE_FILE:-/etc/systemd/system/agw-runner.service}
workspace_root=${AGW_RUNNER_WORKSPACE_ROOT:-/var/lib/agw-runner/workspaces}
secret_source_root=${AGW_RUNNER_SECRET_SOURCE_ROOT:-/var/lib/agw-runner/secrets}
secret_materialization_root=${AGW_RUNNER_SECRET_MATERIALIZATION_ROOT:-/run/agw-runner/materialized}
runtime_dir=${AGW_RUNNER_RUNTIME_DIR:-/run/agw-runner}
broker_root=${AGW_BROKER_ROOT:-/run/agw-broker}
artifact_host_root=${AGW_ARTIFACT_HOST_ROOT:-/var/lib/agents-gateway/artifacts}
compose_project=${AGW_COMPOSE_PROJECT:-agents-gateway-v2-standalone}
update_existing=${AGW_UPDATE_EXISTING:-0}
update_binary=${AGW_UPDATE_BINARY:-0}
managed_files_updated=0
runner_binary_updated=0

die() {
  printf 'install-standalone: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat >&2 <<'EOF'
Usage: install-standalone.sh

Environment overrides:
  AGW_RUNNER_BINARY       absolute agw-runner destination
  AGW_RUNNER_USER         dedicated non-root user (default: agw-runner)
  AGW_STANDALONE_ENV_FILE generated Compose env file
  AGW_UPDATE_EXISTING=1
                          explicitly update differing installer-managed runner
                          config and systemd files; each existing file is
                          backed up first. The default is to refuse changes.
  AGW_UPDATE_BINARY=1    rebuild and atomically replace the installed runner
                          binary from this checkout, preserving a private
                          same-directory backup when bytes differ
  AGW_NO_START=1          prepare and validate without starting Compose
  AGW_RUN_E2E=1           run e2e-standalone.sh after installation

The installer requires root, Linux, systemd, Podman, cgroups v2, user
namespaces, and valid subordinate UID/GID ranges. It does not install missing
OS packages. If the runner binary is absent, Go is required to build it.
EOF
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  exit 0
fi
[[ $# -eq 0 ]] || { usage; exit 2; }

case $update_existing in
  0|1) ;;
  *) die "AGW_UPDATE_EXISTING must be 0 or 1" ;;
esac
case $update_binary in
  0|1) ;;
  *) die "AGW_UPDATE_BINARY must be 0 or 1" ;;
esac

[[ ${EUID} -eq 0 ]] || die "run as root; the runner is still unprivileged, but systemd and /etc require root"
[[ $(uname -s) == Linux ]] || die "standalone runner requires Linux"
[[ -r /sys/fs/cgroup/cgroup.controllers ]] || die "cgroups v2 is required (/sys/fs/cgroup/cgroup.controllers is missing)"

max_user_namespaces=$(cat /proc/sys/user/max_user_namespaces 2>/dev/null || printf '0')
[[ $max_user_namespaces =~ ^[0-9]+$ && $max_user_namespaces -gt 0 ]] || die "unprivileged user namespaces are disabled (max_user_namespaces=$max_user_namespaces)"
if [[ -r /proc/sys/kernel/unprivileged_userns_clone ]]; then
  userns_clone=$(cat /proc/sys/kernel/unprivileged_userns_clone)
  [[ $userns_clone == 1 ]] || die "unprivileged user namespaces are disabled (unprivileged_userns_clone=$userns_clone)"
fi

for command_name in awk basename chmod chown cmp cp dirname getent id install loginctl mktemp mv rm runuser stat systemctl systemd-run useradd usermod; do
  command -v "$command_name" >/dev/null 2>&1 || die "required command is missing: $command_name"
done
command -v newuidmap >/dev/null 2>&1 || die "newuidmap is required for rootless Podman"
command -v newgidmap >/dev/null 2>&1 || die "newgidmap is required for rootless Podman"
[[ -n $podman_binary && -x $podman_binary ]] || die "Podman is not installed; install it separately, then rerun this script"

[[ $runner_user =~ ^[a-z_][a-z0-9_-]{0,30}\$?$ ]] || die "invalid AGW_RUNNER_USER"
[[ $runner_binary == /* && $runner_binary != *$'\n'* && $runner_binary != *$'\r'* && $runner_binary != *[[:space:]]* ]] || die "AGW_RUNNER_BINARY must be an absolute path without whitespace"
[[ $workspace_root == /* && $workspace_root != *$'\n'* && $workspace_root != *$'\r'* && $workspace_root != *[[:space:]]* ]] || die "AGW_RUNNER_WORKSPACE_ROOT must be an absolute path without whitespace"
[[ $secret_source_root == /var/lib/agw-runner/* && $secret_source_root != *$'\n'* && $secret_source_root != *$'\r'* && $secret_source_root != *[[:space:]]* ]] || die "AGW_RUNNER_SECRET_SOURCE_ROOT must stay below /var/lib/agw-runner"
[[ $secret_materialization_root == /run/agw-runner/* && $secret_materialization_root != *$'\n'* && $secret_materialization_root != *$'\r'* && $secret_materialization_root != *[[:space:]]* ]] || die "AGW_RUNNER_SECRET_MATERIALIZATION_ROOT must stay below /run/agw-runner"
[[ $runtime_dir == /run/agw-runner ]] || die "AGW_RUNNER_RUNTIME_DIR must remain /run/agw-runner so the systemd and Compose contracts stay aligned"
[[ $broker_root == /run/agw-broker ]] || die "AGW_BROKER_ROOT must remain /run/agw-broker so the runner and Compose contracts stay aligned"
[[ $artifact_host_root == /* && $artifact_host_root != / && $artifact_host_root != *$'\n'* && $artifact_host_root != *$'\r'* && $artifact_host_root != *[[:space:]]* ]] || die "AGW_ARTIFACT_HOST_ROOT must be a non-root absolute path without whitespace"
[[ $config_file == /* && $config_file != *$'\n'* && $config_file != *$'\r'* && $config_file != *[[:space:]]* ]] || die "AGW_RUNNER_CONFIG must be an absolute path without whitespace"
[[ $service_file == /* && $service_file != *$'\n'* && $service_file != *$'\r'* && $service_file != *[[:space:]]* ]] || die "AGW_RUNNER_SERVICE_FILE must be an absolute path without whitespace"
[[ $compose_project =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || die "AGW_COMPOSE_PROJECT must be a lowercase Compose project name"
[[ -f $v2_dir/deploy/compose.standalone.yaml ]] || die "standalone Compose file is missing"

if [[ ! -e $runner_binary || $update_binary == 1 ]]; then
  command -v go >/dev/null 2>&1 || die "runner binary is absent and Go is unavailable: $runner_binary"
  runner_build=$(mktemp)
  trap 'rm -f -- "$runner_build"' EXIT
  (cd "$v2_dir" && go build -trimpath -o "$runner_build" ./cmd/agw-runner) \
    || die "could not build agw-runner from the source checkout"
  if [[ -e $runner_binary || -L $runner_binary ]]; then
    [[ -f $runner_binary && ! -L $runner_binary && -x $runner_binary ]] \
      || die "refusing to replace a non-regular runner binary: $runner_binary"
    if ! cmp -s "$runner_build" "$runner_binary"; then
      runner_binary_backup=$(mktemp "$(dirname -- "$runner_binary")/.agw-runner.backup.XXXXXX")
      cp -p -- "$runner_binary" "$runner_binary_backup"
      cmp -s -- "$runner_binary" "$runner_binary_backup" \
        || die "could not verify the runner binary backup"
      install -o root -g root -m 0755 "$runner_build" "$runner_binary"
      runner_binary_updated=1
      printf 'updated runner binary; backup: %s\n' "$runner_binary_backup"
    fi
  else
    install -o root -g root -m 0755 "$runner_build" "$runner_binary"
    runner_binary_updated=1
  fi
  rm -f -- "$runner_build"
  trap - EXIT
fi
[[ -x $runner_binary && ! -d $runner_binary ]] || die "runner binary is not an executable file: $runner_binary"

if ! systemctl show-environment >/dev/null 2>&1; then
  die "systemd is not available; install the runner unit on a systemd host"
fi

if getent passwd "$runner_user" >/dev/null; then
  runner_uid=$(id -u "$runner_user")
  runner_gid=$(id -g "$runner_user")
  runner_group=$(id -gn "$runner_user")
  [[ $runner_uid -ne 0 ]] || die "refusing to use root as the runner user"
else
  shell=/usr/sbin/nologin
  [[ -x $shell ]] || shell=/bin/false
  useradd --system --user-group --create-home --home-dir /var/lib/agw-runner --shell "$shell" "$runner_user"
  runner_uid=$(id -u "$runner_user")
  runner_gid=$(id -g "$runner_user")
  runner_group=$(id -gn "$runner_user")
fi

# The dedicated user cannot and should not traverse an operator's source
# checkout under /home. Enter its private state directory before dropping
# privileges so Podman and runner probes never inherit an inaccessible cwd.
run_as_runner() {
  (cd /var/lib/agw-runner && runuser -u "$runner_user" -- "$@")
}

# Rootless Podman's systemd cgroup manager needs the dedicated user's systemd
# manager and its canonical runtime directory. Lingering makes both available
# at boot without requiring an interactive login.
loginctl enable-linger "$runner_user"
runner_xdg_runtime_dir="/run/user/$runner_uid"
for attempt in {1..20}; do
  [[ -S $runner_xdg_runtime_dir/bus ]] && break
  sleep 1
done
[[ -S $runner_xdg_runtime_dir/bus ]] \
  || die "the lingering systemd user manager did not create $runner_xdg_runtime_dir/bus"

[[ ! -e /etc/subuid || -f /etc/subuid ]] || die "/etc/subuid is not a regular file"
[[ ! -e /etc/subgid || -f /etc/subgid ]] || die "/etc/subgid is not a regular file"
[[ -e /etc/subuid ]] || install -o root -g root -m 0644 /dev/null /etc/subuid
[[ -e /etc/subgid ]] || install -o root -g root -m 0644 /dev/null /etc/subgid

has_subid_range() {
  local file=$1
  awk -F: -v user="$runner_user" '$1 == user && $2 ~ /^[0-9]+$/ && $3 ~ /^[0-9]+$/ && $3 >= 65536 { found=1 } END { exit(found ? 0 : 1) }' "$file"
}

range_is_free() {
  local file=$1
  local start=$2
  local end=$3
  awk -F: -v lo="$start" -v hi="$end" 'BEGIN { free=1 } ($2+0) <= hi && (($2+0)+($3+0)-1) >= lo { free=0 } END { exit(free ? 0 : 1) }' "$file"
}

if ! has_subid_range /etc/subuid || ! has_subid_range /etc/subgid; then
  sub_start=100000
  while ! range_is_free /etc/subuid "$sub_start" "$((sub_start + 65535))" || ! range_is_free /etc/subgid "$sub_start" "$((sub_start + 65535))"; do
    sub_start=$((sub_start + 65536))
    [[ $sub_start -lt 4000000000 ]] || die "could not find a free subordinate UID/GID range"
  done
  sub_end=$((sub_start + 65535))
  has_subid_range /etc/subuid || usermod --add-subuids "${sub_start}-${sub_end}" "$runner_user"
  has_subid_range /etc/subgid || usermod --add-subgids "${sub_start}-${sub_end}" "$runner_user"
fi
has_subid_range /etc/subuid || die "runner has no valid subordinate UID range"
has_subid_range /etc/subgid || die "runner has no valid subordinate GID range"

if [[ -e /var/lib/agw-runner && ! -d /var/lib/agw-runner ]]; then
  die "/var/lib/agw-runner exists and is not a directory"
fi
install -d -o "$runner_user" -g "$runner_group" -m 0700 /var/lib/agw-runner
containers_config_dir=/var/lib/agw-runner/.config/containers
containers_config_file=$containers_config_dir/containers.conf
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$containers_config_dir"
containers_config_tmp=$(mktemp)
printf '%s\n' \
  '[engine]' \
  'cgroup_manager = "systemd"' \
  'events_logger = "file"' >"$containers_config_tmp"
if [[ -e $containers_config_file || -L $containers_config_file ]]; then
  [[ -f $containers_config_file && ! -L $containers_config_file ]] || die "refusing non-regular runner containers config: $containers_config_file"
  if ! cmp -s "$containers_config_tmp" "$containers_config_file"; then
    [[ $update_existing == 1 ]] || die "runner containers config differs; set AGW_UPDATE_EXISTING=1 to update: $containers_config_file"
    containers_backup=$(mktemp "$containers_config_dir/.containers.conf.backup.XXXXXX")
    cp -p -- "$containers_config_file" "$containers_backup"
    install -o "$runner_user" -g "$runner_group" -m 0600 "$containers_config_tmp" "$containers_config_file"
  fi
else
  install -o "$runner_user" -g "$runner_group" -m 0600 "$containers_config_tmp" "$containers_config_file"
fi
rm -f -- "$containers_config_tmp"
if [[ -e $workspace_root && -L $workspace_root ]]; then
  die "workspace root must not be a symlink: $workspace_root"
fi
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$workspace_root"
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$secret_source_root"

podman_probe_error=$(mktemp)
podman_info=$(run_as_runner env HOME=/var/lib/agw-runner XDG_RUNTIME_DIR="$runner_xdg_runtime_dir" PATH="$PATH" podman info --format '{{.Host.Security.Rootless}} {{.Host.CgroupManager}} {{.Host.CgroupsVersion}}' 2>"$podman_probe_error") || {
  probe_detail=$(tail -n 20 "$podman_probe_error")
  rm -f -- "$podman_probe_error"
  die "rootless Podman readiness failed: $probe_detail"
}
rm -f -- "$podman_probe_error"
[[ $podman_info == 'true systemd v2' ]] \
  || die "Podman must report rootless=true, cgroupManager=systemd, cgroupsVersion=v2 (got: $podman_info)"

config_parent=$(dirname -- "$config_file")
install -d -o root -g "$runner_group" -m 0750 "$config_parent"
install -d -o "$runner_user" -g "$runner_group" -m 0750 "$runtime_dir"
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$secret_materialization_root"
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$broker_root"
if [[ -e $artifact_host_root && -L $artifact_host_root ]]; then
  die "artifact root must not be a symlink: $artifact_host_root"
fi
install -d -o "$runner_user" -g "$runner_group" -m 0700 "$artifact_host_root"

atomic_install() {
  local source=$1
  local target=$2
  local mode=$3
  local owner=$4
  local group=$5
  local target_dir
  local target_tmp

  target_dir=$(dirname -- "$target")
  target_tmp=$(mktemp "$target_dir/.agw-install.XXXXXX")
  if ! install -o "$owner" -g "$group" -m "$mode" "$source" "$target_tmp"; then
    rm -f -- "$target_tmp"
    return 1
  fi
  if ! mv -f -- "$target_tmp" "$target"; then
    rm -f -- "$target_tmp"
    return 1
  fi
}

backup_existing() {
  local target=$1
  local target_dir
  local target_name
  local backup

  target_dir=$(dirname -- "$target")
  target_name=$(basename -- "$target")
  backup=$(mktemp "$target_dir/.${target_name}.backup.XXXXXX")
  if ! cp -p -- "$target" "$backup" || ! cmp -s -- "$target" "$backup"; then
    rm -f -- "$backup"
    die "could not create an exact backup before updating: $target"
  fi
  printf '%s\n' "$backup"
}

restore_backup() {
  local backup=$1
  local target=$2
  local mode=$3
  local owner=$4
  local group=$5
  local restore_tmp
  local target_dir

  target_dir=$(dirname -- "$target")
  restore_tmp=$(mktemp "$target_dir/.agw-restore.XXXXXX")
  if install -o "$owner" -g "$group" -m "$mode" "$backup" "$restore_tmp" && mv -f -- "$restore_tmp" "$target"; then
    return 0
  fi
  rm -f -- "$restore_tmp"
  return 1
}

validate_staged_files() {
  local config_source=$1
  local service_source=$2
  local config_parent=$3
  local config_validation
  local doctor_output

  command -v systemd-analyze >/dev/null 2>&1 \
    || die "systemd-analyze is required to validate the runner unit before installation"
  systemd-analyze verify "$service_source" >/dev/null \
    || die "generated runner systemd unit failed validation; no managed files were changed"

  config_validation=$(mktemp "$config_parent/.runner.yaml.validate.XXXXXX")
  doctor_output=$(mktemp)
  if ! install -o "$runner_user" -g "$runner_group" -m 0640 "$config_source" "$config_validation"; then
    rm -f -- "$config_validation" "$doctor_output"
    die "could not stage the generated runner config for validation"
  fi
  if ! run_as_runner "$runner_binary" doctor --config "$config_validation" >"$doctor_output" 2>/dev/null; then
    rm -f -- "$config_validation" "$doctor_output"
    die "generated runner config failed validation; no managed files were changed"
  fi
  rm -f -- "$config_validation" "$doctor_output"
}

install_managed_files() {
  local config_source=$1
  local service_source=$2
  local config_diff=0
  local service_diff=0
  local config_backup=''
  local service_backup=''
  local config_present=0
  local service_present=0

  if [[ -e $config_file || -L $config_file ]]; then
    [[ -f $config_file && ! -L $config_file ]] || die "refusing to replace non-regular path: $config_file"
    config_present=1
    cmp -s -- "$config_source" "$config_file" || config_diff=1
  fi
  if [[ -e $service_file || -L $service_file ]]; then
    [[ -f $service_file && ! -L $service_file ]] || die "refusing to replace non-regular path: $service_file"
    service_present=1
    cmp -s -- "$service_source" "$service_file" || service_diff=1
  fi

  if [[ $update_existing != 1 && ( $config_diff -eq 1 || $service_diff -eq 1 ) ]]; then
    if [[ $config_diff -eq 1 ]]; then
      die "existing runner config differs; refusing to overwrite: $config_file (set AGW_UPDATE_EXISTING=1 to opt in)"
    fi
    die "existing runner systemd unit differs; refusing to overwrite: $service_file (set AGW_UPDATE_EXISTING=1 to opt in)"
  fi

  if [[ $config_diff -eq 1 ]]; then
    config_backup=$(backup_existing "$config_file")
  fi
  if [[ $service_diff -eq 1 ]]; then
    service_backup=$(backup_existing "$service_file")
  fi

  if [[ $config_diff -eq 1 || $config_present -eq 0 ]]; then
    if ! atomic_install "$config_source" "$config_file" 0640 root "$runner_group"; then
      [[ -z $config_backup ]] || restore_backup "$config_backup" "$config_file" 0640 root "$runner_group" || true
      [[ -z $service_backup ]] || restore_backup "$service_backup" "$service_file" 0644 root root || true
      die "could not install runner config; managed files were restored where possible"
    fi
  else
    chmod 0640 "$config_file"
    chown root:"$runner_group" "$config_file"
  fi

  if [[ $service_diff -eq 1 || $service_present -eq 0 ]]; then
    if ! atomic_install "$service_source" "$service_file" 0644 root root; then
      if [[ -n $config_backup ]]; then
        restore_backup "$config_backup" "$config_file" 0640 root "$runner_group" || true
      elif [[ $config_present -eq 0 ]]; then
        rm -f -- "$config_file"
      fi
      if [[ -n $service_backup ]]; then
        restore_backup "$service_backup" "$service_file" 0644 root root || true
      fi
      die "could not install runner systemd unit; managed files were restored where possible"
    fi
  else
    chmod 0644 "$service_file"
    chown root:root "$service_file"
  fi

  if [[ -n $config_backup || -n $service_backup ]]; then
    managed_files_updated=1
    printf 'updated managed standalone files; backups were created beside the replaced files\n'
  fi
  if [[ $config_present -eq 0 || $service_present -eq 0 ]]; then
    managed_files_updated=1
  fi
}

config_tmp=$(mktemp)
service_tmp=$(mktemp --suffix=.service)
trap 'rm -f -- "$config_tmp" "$service_tmp"' EXIT
printf '%s\n' \
  'backend: podman' \
  "workspaceRoot: $workspace_root" \
  'secrets:' \
  "  sourceRoot: $secret_source_root" \
  "  materializationRoot: $secret_materialization_root" \
  'podman:' \
  "  binary: $podman_binary" \
  "  brokerRoot: $broker_root" \
  'server:' \
  '  transport: unix' \
  "  socketPath: $runtime_dir/runner.sock" >"$config_tmp"

printf '%s\n' \
  '[Unit]' \
  'Description=Agents Gateway standalone rootless sandbox runner' \
  'Wants=network-online.target' \
  "Wants=user@${runner_uid}.service" \
  "After=network-online.target user@${runner_uid}.service" \
  '' \
  '[Service]' \
  'Type=simple' \
  "User=$runner_user" \
  "Group=$runner_group" \
  'WorkingDirectory=/var/lib/agw-runner' \
  'Environment=HOME=/var/lib/agw-runner' \
  "Environment=XDG_RUNTIME_DIR=$runner_xdg_runtime_dir" \
  "ExecStart=$runner_binary serve --config $config_file" \
  'Restart=on-failure' \
  'RestartSec=5s' \
  'UMask=0077' \
  '# The trusted runner must invoke newuidmap/newgidmap setuid helpers for' \
  '# rootless Podman. Every untrusted sandbox still gets no-new-privileges.' \
  'PrivateTmp=yes' \
  'ProtectSystem=strict' \
  '# ProtectHome=yes hides /run/user as well as home directories. Podman needs' \
  '# the dedicated user manager there for its rootless systemd cgroups.' \
  'ProtectHome=read-only' \
  'ProtectKernelTunables=yes' \
  'ProtectKernelModules=yes' \
  'Delegate=yes' \
  'RestrictSUIDSGID=yes' \
  'LockPersonality=yes' \
  'MemoryDenyWriteExecute=yes' \
  'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6' \
  'RuntimeDirectory=agw-runner agw-broker' \
  'RuntimeDirectoryMode=0700' \
  'StateDirectory=agw-runner' \
  'StateDirectoryMode=0700' \
  "ReadWritePaths=/var/lib/agw-runner /run/agw-runner /run/agw-broker $runner_xdg_runtime_dir" \
  '' \
  '[Install]' \
  'WantedBy=multi-user.target' >"$service_tmp"

validate_staged_files "$config_tmp" "$service_tmp" "$config_parent"

random_hex() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

if [[ -e $env_file || -L $env_file ]]; then
  [[ -f $env_file && ! -L $env_file ]] || die "refusing to use non-regular standalone env file: $env_file"
  chmod 600 "$env_file"
else
  env_parent=$(dirname -- "$env_file")
  if [[ $env_parent == "$config_parent" ]]; then
    # The runner needs traverse access to read runner.yaml; the root-only
    # standalone env file itself remains 0600 and is not readable by the
    # runner group.
    install -d -o root -g "$runner_group" -m 0750 "$env_parent"
  else
    install -d -o root -g root -m 0700 "$env_parent"
  fi
  env_tmp=$(mktemp)
  generated_auth_token=$(random_hex)
  trap 'rm -f -- "$config_tmp" "$service_tmp" "$env_tmp"' EXIT
  printf '%s\n' \
    'AGW_ENVIRONMENT=production' \
    'AGW_AUTH_MODE=local' \
    "AGW_AUTH_TOKEN=$generated_auth_token" \
    "AGW_TOKEN=$generated_auth_token" \
    'AGW_AUTH_PRINCIPAL_ID=local-owner' \
    'AGW_LOCAL_ORGANIZATION_ID=00000000-0000-4000-8000-000000000001' \
    'AGW_LOCAL_PROJECT_ID=00000000-0000-4000-8000-000000000002' \
    'AGW_ORGANIZATION_ID=00000000-0000-4000-8000-000000000001' \
    'AGW_PROJECT_ID=00000000-0000-4000-8000-000000000002' \
    'AGW_ORCHESTRATION_MODE=local' \
    'AGW_EXECUTION_MODE=standalone' \
    'POSTGRES_ADMIN_USER=postgres' \
    "POSTGRES_ADMIN_PASSWORD=$(random_hex)" \
    'AGW_DB_NAME=agents_gateway' \
    'AGW_DB_USER=agw' \
    "AGW_DB_PASSWORD=$(random_hex)" \
    'AGW_POSTGRES_IMAGE=postgres:latest' \
    'AGW_SERVER_IMAGE=agents-gateway-v2-server:local' \
    'AGW_CONSOLE_IMAGE=agents-gateway-v2-console:local' \
    'AGW_CONSOLE_PORT=8080' \
    'AGW_SERVER_URL=http://127.0.0.1:8080' \
    "AGW_RUNNER_UID=$runner_uid" \
    "AGW_SERVER_UID=$runner_uid" \
    "AGW_RUNNER_GID=$runner_gid" \
    "AGW_RUNNER_SOCKET_DIR=$runtime_dir" \
    "AGW_RUNNER_SOCKET=$runtime_dir/runner.sock" \
    "AGW_BROKER_ROOT=$broker_root" \
    "AGW_ARTIFACT_HOST_ROOT=$artifact_host_root" \
    'AGW_SKILLS_GATEWAY_URL=' \
    'SKILLS_GATEWAY_AUTH_TOKEN=' \
    'MCP_GATEWAY_AUTH_TOKEN=' \
    'OPENAI_API_KEY=' \
    'AGW_OPENAI_RESPONSES_URL=' \
    'OPENROUTER_API_KEY=' \
    'AGW_OPENROUTER_RESPONSES_URL=' >"$env_tmp"
  install -o root -g root -m 0600 "$env_tmp" "$env_file"
fi

env_value() {
  local key=$1
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$env_file"
}
auth_token=$(env_value AGW_AUTH_TOKEN)
[[ ${#auth_token} -ge 32 ]] || die "AGW_AUTH_TOKEN in $env_file must contain at least 32 characters"
cli_token=$(env_value AGW_TOKEN)
[[ -z $cli_token || $cli_token == "$auth_token" ]] || die "AGW_TOKEN in $env_file must match AGW_AUTH_TOKEN"
cli_organization=$(env_value AGW_ORGANIZATION_ID)
[[ -z $cli_organization || $cli_organization == "$(env_value AGW_LOCAL_ORGANIZATION_ID)" ]] \
  || die "AGW_ORGANIZATION_ID must match AGW_LOCAL_ORGANIZATION_ID"
cli_project=$(env_value AGW_PROJECT_ID)
[[ -z $cli_project || $cli_project == "$(env_value AGW_LOCAL_PROJECT_ID)" ]] \
  || die "AGW_PROJECT_ID must match AGW_LOCAL_PROJECT_ID"
for secret_key in POSTGRES_ADMIN_PASSWORD AGW_DB_PASSWORD; do
  secret_value=$(env_value "$secret_key")
  [[ -n $secret_value && $secret_value != *$'\n'* && $secret_value != *$'\r'* ]] || die "$secret_key in $env_file must be non-empty"
done
[[ $(env_value AGW_RUNNER_UID) == "$runner_uid" ]] || die "AGW_RUNNER_UID in $env_file does not match the installed runner user"
[[ $(env_value AGW_SERVER_UID) == "$runner_uid" ]] || die "AGW_SERVER_UID in $env_file does not match the installed runner user"
[[ $(env_value AGW_RUNNER_GID) == "$runner_gid" ]] || die "AGW_RUNNER_GID in $env_file does not match the installed runner group"
[[ $(env_value AGW_RUNNER_SOCKET_DIR) == "$runtime_dir" ]] || die "AGW_RUNNER_SOCKET_DIR in $env_file must be $runtime_dir"
configured_broker_root=$(env_value AGW_BROKER_ROOT)
[[ -z $configured_broker_root || $configured_broker_root == "$broker_root" ]] || die "AGW_BROKER_ROOT in $env_file must be $broker_root"
configured_artifact_root=$(env_value AGW_ARTIFACT_HOST_ROOT)
[[ -z $configured_artifact_root || $configured_artifact_root == "$artifact_host_root" ]] || die "AGW_ARTIFACT_HOST_ROOT in $env_file must be $artifact_host_root"

service_unit=$(basename -- "$service_file")
[[ $service_unit == *.service && $service_unit != *[[:space:]]* ]] || die "AGW_RUNNER_SERVICE_FILE must name a .service unit without whitespace"

install_managed_files "$config_tmp" "$service_tmp"
systemctl daemon-reload
doctor_tmp=$(mktemp)
run_as_runner "$runner_binary" doctor --config "$config_file" >"$doctor_tmp" || {
  doctor_status=$?
  cat "$doctor_tmp" >&2 || true
  rm -f -- "$doctor_tmp"
  die "agw-runner doctor failed with status $doctor_status"
}
cat "$doctor_tmp"
rm -f -- "$doctor_tmp"

systemctl enable "$service_unit"
if [[ $managed_files_updated -eq 1 || $runner_binary_updated -eq 1 ]]; then
  systemctl restart "$service_unit"
else
  systemctl start "$service_unit"
fi
systemctl is-active --quiet "$service_unit" || die "$service_unit did not become active"
for attempt in {1..20}; do
  [[ -S $runtime_dir/runner.sock ]] && break
  sleep 1
done
[[ -S $runtime_dir/runner.sock ]] || die "runner service is active but Unix socket was not created"

if [[ ${AGW_RUN_E2E:-0} == 1 ]]; then
  fixture_base=${AGW_FIXTURE_BASE_IMAGE:-busybox:latest}
  [[ $fixture_base != -* && $fixture_base != *[[:space:]]* ]] || die "AGW_FIXTURE_BASE_IMAGE is invalid"
  run_as_runner env HOME=/var/lib/agw-runner XDG_RUNTIME_DIR="$runner_xdg_runtime_dir" \
    "$podman_binary" pull "$fixture_base" >/dev/null \
    || die "could not preload the requested E2E fixture base image"
  active_sandboxes=$(run_as_runner env HOME=/var/lib/agw-runner XDG_RUNTIME_DIR="$runner_xdg_runtime_dir" \
    "$podman_binary" ps -q --filter label=io.agents-gateway.owner=agw-runner 2>/dev/null || true)
  [[ -z $active_sandboxes ]] || die "refusing E2E while an Agents Gateway sandbox is active"
  e2e_stage=$(mktemp -d /var/lib/agw-runner/e2e.XXXXXX)
  chown "$runner_user:$runner_group" "$e2e_stage"
  chmod 0700 "$e2e_stage"
  install -d -o "$runner_user" -g "$runner_group" -m 0700 "$e2e_stage/scripts" "$e2e_stage/testdata/runtime-fixture"
  install -o "$runner_user" -g "$runner_group" -m 0755 "$script_dir/e2e-standalone.sh" "$e2e_stage/scripts/e2e-standalone.sh"
  install -o "$runner_user" -g "$runner_group" -m 0644 "$v2_dir/testdata/runtime-fixture/Containerfile" "$e2e_stage/testdata/runtime-fixture/Containerfile"
  install -o "$runner_user" -g "$runner_group" -m 0755 "$v2_dir/testdata/runtime-fixture/runtime.sh" "$e2e_stage/testdata/runtime-fixture/runtime.sh"
  systemctl stop "$service_unit"
  e2e_unit="agw-runner-e2e-$RANDOM"
  systemd-run --quiet --wait --pipe --collect --unit "$e2e_unit" \
    --property "User=$runner_user" --property "Group=$runner_group" \
    --property 'WorkingDirectory=/var/lib/agw-runner' --property 'Delegate=yes' \
    --setenv "HOME=/var/lib/agw-runner" --setenv "XDG_RUNTIME_DIR=$runner_xdg_runtime_dir" \
    --setenv "AGW_RUNNER_BINARY=$runner_binary" --setenv "AGW_RUNNER_CONFIG=$config_file" \
    --setenv "AGW_RUNNER_USER=$runner_user" --setenv "AGW_RUNNER_WORKSPACE_ROOT=$workspace_root" \
    --setenv "AGW_FIXTURE_BASE_IMAGE=$fixture_base" \
    "$e2e_stage/scripts/e2e-standalone.sh" || {
      e2e_status=$?
      rm -rf -- "$e2e_stage"
      systemctl start "$service_unit" || true
      die "standalone sandbox E2E failed with status $e2e_status"
    }
  rm -rf -- "$e2e_stage"
  systemctl start "$service_unit"
  for attempt in {1..20}; do
    [[ -S $runtime_dir/runner.sock ]] && break
    sleep 1
  done
  [[ -S $runtime_dir/runner.sock ]] || die "runner socket did not return after E2E"
fi

if docker compose version >/dev/null 2>&1; then
  compose() { docker compose -p "$compose_project" -f "$v2_dir/deploy/compose.standalone.yaml" --env-file "$env_file" "$@"; }
elif podman compose version >/dev/null 2>&1; then
  compose() { podman compose -p "$compose_project" -f "$v2_dir/deploy/compose.standalone.yaml" --env-file "$env_file" "$@"; }
else
  die "a Compose implementation is required (docker compose or podman compose)"
fi
compose config --quiet
if [[ ${AGW_NO_START:-0} != 1 ]]; then
  command -v curl >/dev/null 2>&1 || die "curl is required for the post-start readiness check"
  compose up -d --build
  console_port=$(env_value AGW_CONSOLE_PORT)
  [[ $console_port =~ ^[0-9]+$ && $console_port -ge 1 && $console_port -le 65535 ]] \
    || die "AGW_CONSOLE_PORT in $env_file must be a valid TCP port"
  ready=false
  for attempt in {1..90}; do
    if curl --fail --silent --show-error --max-time 3 "http://127.0.0.1:$console_port/ready" >/dev/null 2>&1; then
      ready=true
      break
    fi
    sleep 1
  done
  if [[ $ready != true ]]; then
    compose ps >&2 || true
    compose logs --tail=100 >&2 || true
    die "standalone control plane did not become ready on loopback port $console_port"
  fi
  printf 'standalone control plane is ready at http://127.0.0.1:%s\n' "$console_port"
else
  printf 'standalone installation prepared without starting Compose (AGW_NO_START=1)\n'
fi
printf 'env file: %s\nrunner config: %s\nrunner socket: %s\n' "$env_file" "$config_file" "$runtime_dir/runner.sock"
