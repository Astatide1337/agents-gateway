#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Local, non-deploying equivalent of the repository's v3 CI checks.
#
# This script intentionally does not call kubectl, kind, argo submit, gh, a
# GitHub API, a container push, or a Docker cleanup command.  Phase-0 is run
# only in plan mode and CRD generation is compared from a temporary directory.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
V3_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
REPO_ROOT=$(CDPATH= cd -- "$V3_DIR/.." && pwd)

GO_BIN=${GO_BIN:-go}
GOMAXPROCS_VALUE=${AGW_GOMAXPROCS:-2}
MODULE_GO_VERSION=$(awk '/^go / { print $2; exit }' "$V3_DIR/go.mod")
CONTROLLER_GEN_VERSION=${CONTROLLER_GEN_VERSION:-v0.21.0}
CONTAINER_ENGINE=${CONTAINER_ENGINE:-docker}
LOCAL_IMAGE_PREFIX=${AGW_LOCAL_IMAGE_PREFIX:-agents-gateway-v3-local}

RUN_VULN=1
RUN_PHASE0=1
RUN_CRD_DRIFT=1
RUN_BINARY_BUILD=1
BUILD_ALL_IMAGES=0
IMAGE_REQUESTS=()
CURRENT_STEP=''
TEMP_DIRS=()

usage() {
  cat <<'EOF'
Usage: ./scripts/validate-local.sh [options]

Runs the Agents Gateway v3 checks locally. The default mode is non-deploying:
it does not contact a Kubernetes API, create a Kind cluster, invoke GitHub
Actions, use a GitHub API, push images, or prune Docker/Podman state.

Default checks:
  formatting and whitespace, unit tests, serialized race tests, vet,
  govulncheck, Bash syntax, Helm/chart contracts, all image contracts,
  Phase-0 static guards and plan mode, CRD regeneration drift, and all
  production Go command builds.

Options:
  --build-images             Build and contract-check all discovered images.
  --build-image NAME         Build one image (repeatable; use --list-images).
  --list-images              Print image names accepted by --build-image.
  --list-commands             Print production Go commands built by this check.
  --skip-vulnerability-scan  Skip govulncheck (useful only when offline).
  --skip-phase0              Skip Phase-0 static guards and plan-mode checks.
  --skip-crd-drift           Skip the temporary controller-gen comparison.
  --skip-binary-build        Skip the production command compile loop.
  -h, --help                 Show this help.

Image builds use the selected local engine without --pull and retain their
explicit local tags. The runtime image build helpers may resolve the current
harness package from npm; no image is pushed.
EOF
}

die() {
  printf 'validate-local: %s\n' "$*" >&2
  exit 2
}

require_command() {
  local command_name=$1
  command -v "$command_name" >/dev/null 2>&1 || die "required command is missing: $command_name"
}

print_command() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
}

run_step() {
  local label=$1
  shift
  CURRENT_STEP=$label
  printf '\n==> %s\n' "$label"
  print_command "$@"
  "$@"
  printf '<== %s: PASS\n' "$label"
  CURRENT_STEP=''
}

on_exit() {
  local status=$?
  if (( status != 0 )); then
    printf '\nvalidate-local: FAILED%s (exit %d)\n' \
      "${CURRENT_STEP:+ during $CURRENT_STEP}" "$status" >&2
  fi
  local directory
  for directory in "${TEMP_DIRS[@]}"; do
    [[ -n "$directory" && -d "$directory" ]] || continue
    rm -rf -- "$directory"
  done
  exit "$status"
}
trap on_exit EXIT

image_containerfile() {
  local image_dir="images/$1"
  if [[ -f "$image_dir/Containerfile" ]]; then
    printf '%s\n' "$image_dir/Containerfile"
  elif [[ -f "$image_dir/Dockerfile" ]]; then
    printf '%s\n' "$image_dir/Dockerfile"
  else
    die "image $1 has no Containerfile or Dockerfile"
  fi
}

image_validator() {
  printf 'images/%s/validate.sh\n' "$1"
}

image_build_helper() {
  local helper="images/$1/build.sh"
  if [[ -f "$helper" ]]; then
    printf '%s\n' "$helper"
  else
    printf '%s\n' ''
  fi
}

discover_image_names() {
  local validator image_dir
  while IFS= read -r validator; do
    image_dir=$(dirname -- "$validator")
    basename -- "$image_dir"
  done < <(find "$V3_DIR/images" -mindepth 2 -maxdepth 2 -type f -name validate.sh -print | sort)
}

discover_go_commands() {
  local containerfile
  {
    # Preserve the existing Makefile production-command contract, including
    # the kubectl plugin whose main function is not in main.go.
    awk '
      /^GO_COMMANDS :=/ { inside = 1; next }
      inside {
        if ($1 ~ /^[A-Za-z0-9_-]+$/) print $1
        if ($0 !~ /\\[[:space:]]*$/) exit
      }
    ' "$V3_DIR/Makefile"

    # Add command entrypoints referenced by image build inputs. This makes a
    # newly introduced runtime image (such as agw-critic) part of local
    # validation without maintaining a second frozen command list.
    for containerfile in "$V3_DIR"/images/*/Containerfile "$V3_DIR"/images/*/Dockerfile; do
      [[ -f "$containerfile" ]] || continue
      rg -o --no-filename '\./cmd/[A-Za-z0-9_-]+' "$containerfile" 2>/dev/null \
        | sed 's#^\./cmd/##' || true
    done
  } | sort -u
}

is_known_image() {
  local requested=$1
  local name
  while IFS= read -r name; do
    [[ "$name" == "$requested" ]] && return 0
  done < <(discover_image_names)
  return 1
}

build_one_image() {
  local name=$1
  local tag="$LOCAL_IMAGE_PREFIX-$name:validation"
  local helper
  helper=$(image_build_helper "$name")

  if [[ -n "$helper" ]]; then
    run_step "build local image: $name" env \
      IMAGE="$tag" CONTAINER_ENGINE="$CONTAINER_ENGINE" \
      bash "$V3_DIR/$helper"
  else
    run_step "build local image: $name" \
      "$CONTAINER_ENGINE" build \
      --file "$V3_DIR/$(image_containerfile "$name")" \
      --tag "$tag" \
      "$REPO_ROOT"
  fi
  run_step "validate built image contract: $name" \
    bash "$V3_DIR/$(image_validator "$name")" --image "$tag"
}

parse_args() {
  while (($#)); do
    case "$1" in
      --build-images)
        BUILD_ALL_IMAGES=1
        ;;
      --build-image)
        [[ $# -ge 2 ]] || die '--build-image requires a name'
        is_known_image "$2" || die "unknown image '$2' (use --list-images)"
        IMAGE_REQUESTS+=("$2")
        shift
        ;;
      --list-images)
        discover_image_names
        exit 0
        ;;
      --list-commands)
        discover_go_commands
        exit 0
        ;;
      --skip-vulnerability-scan)
        RUN_VULN=0
        ;;
      --skip-phase0)
        RUN_PHASE0=0
        ;;
      --skip-crd-drift)
        RUN_CRD_DRIFT=0
        ;;
      --skip-binary-build)
        RUN_BINARY_BUILD=0
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      --)
        shift
        (($# == 0)) || die "unexpected arguments after --: $*"
        ;;
      *)
        die "unknown option: $1"
        ;;
    esac
    shift
  done
}

check_formatting() {
  local files
  files=$(gofmt -l .)
  if [[ -n "$files" ]]; then
    printf 'gofmt required for:\n%s\n' "$files" >&2
    return 1
  fi
}

check_whitespace() {
  git -C "$REPO_ROOT" diff --check -- \
    v3 docs/v3 .github/workflows/v3-ci.yml .github/workflows/v3-release.yml

  # git diff does not inspect untracked files. The CI checkout sees new files
  # as a diff, so cover the same source surfaces while they are still local.
  if rg -n \
    --glob '*.go' --glob '*.sh' --glob '*.yaml' --glob '*.yml' \
    --glob '*.md' --glob 'Containerfile*' --glob 'Dockerfile' --glob 'Makefile' \
    '[[:blank:]]+$' \
    "$V3_DIR" "$REPO_ROOT/docs/v3" "$REPO_ROOT/.github/workflows"; then
    printf 'trailing whitespace found in local v3/CI files\n' >&2
    return 1
  fi
}

run_go_test() {
  env GOMAXPROCS="$GOMAXPROCS_VALUE" GOTOOLCHAIN="go$MODULE_GO_VERSION" \
    "$GO_BIN" test -p 1 -count=1 ./...
}

run_go_race() {
  env GOMAXPROCS="$GOMAXPROCS_VALUE" GOTOOLCHAIN="go$MODULE_GO_VERSION" \
    "$GO_BIN" test -race -p 1 -count=1 ./...
}

run_go_vet() {
  env GOMAXPROCS="$GOMAXPROCS_VALUE" GOTOOLCHAIN="go$MODULE_GO_VERSION" \
    "$GO_BIN" vet ./...
}

run_vulnerability_scan() {
  env GOTOOLCHAIN="go$MODULE_GO_VERSION" \
    "$GO_BIN" run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
}

run_bash_syntax() {
  local -a shell_files=()
  mapfile -d '' shell_files < <(find . -type f -name '*.sh' -print0 | sort -z)
  ((${#shell_files[@]} > 0)) || die 'no Bash files found'
  run_step 'Bash syntax checks' bash -n "${shell_files[@]}"
}

run_image_contracts() {
  local -a validators=()
  mapfile -d '' validators < <(
    find images -mindepth 2 -maxdepth 2 -type f -name validate.sh -print0 | sort -z
  )
  ((${#validators[@]} > 0)) || die 'no image validators found'
  local validator
  for validator in "${validators[@]}"; do
    run_step "image contract: ${validator#images/}" bash "$validator"
  done
}

run_phase0_checks() {
  run_step 'Phase-0 agentgateway static guard' \
    bash phase0/agentgateway-guard/scripts/verify-static.sh
  run_step 'Phase-0 Argo Sandbox static guard' \
    bash phase0/argo-sandbox/scripts/verify-static.sh
  run_step 'Phase-0 orchestrator interface tests' \
    bash scripts/phase0-run.test.sh

  local phase0_dir
  phase0_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-v3-local-phase0.XXXXXX")
  TEMP_DIRS+=("$phase0_dir")

  local -a plan_scripts=(
    scripts/phase0-airlock.sh
    scripts/phase0-credentials.sh
    scripts/phase0-inventory.sh
    scripts/phase0-objectstore.sh
    scripts/phase0-sandbox.sh
    scripts/phase0-userns.sh
  )
  local script run_id
  for script in "${plan_scripts[@]}"; do
    run_id="validate-local-$(basename "$script" .sh)"
    run_step "Phase-0 plan: $script" env \
      PHASE0_MODE=plan \
      PHASE0_RUN_ID="$run_id" \
      PHASE0_OUTPUT_DIR="$phase0_dir" \
      bash "$script" --plan --namespace agw-phase0-local
  done
}

run_crd_drift_check() {
  local generated_dir
  generated_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-v3-local-crd.XXXXXX")
  TEMP_DIRS+=("$generated_dir")

  if [[ -n "${CONTROLLER_GEN_BIN:-}" ]]; then
    run_step 'generate CRDs into a temporary directory' \
      "$CONTROLLER_GEN_BIN" \
      crd:crdVersions=v1 \
      paths='./api/...' \
      "output:crd:artifacts:config=$generated_dir"
  else
    run_step 'generate CRDs into a temporary directory' \
      env GOTOOLCHAIN="go$MODULE_GO_VERSION" \
      "$GO_BIN" run "sigs.k8s.io/controller-tools/cmd/controller-gen@$CONTROLLER_GEN_VERSION" \
      crd:crdVersions=v1 \
      paths='./api/...' \
      "output:crd:artifacts:config=$generated_dir"
  fi

  run_step 'compare generated CRDs with v3/config/crd' \
    diff -ru --exclude=kustomization.yaml config/crd "$generated_dir"
}

run_binary_build() {
  local output_dir
  output_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-v3-local-build.XXXXXX")
  TEMP_DIRS+=("$output_dir")

  local -a commands=()
  mapfile -t commands < <(discover_go_commands)
  ((${#commands[@]} > 0)) || die 'no Go command entrypoints found under cmd/'
  local command_name
  for command_name in "${commands[@]}"; do
    run_step "build production command: $command_name" \
      env GOMAXPROCS="$GOMAXPROCS_VALUE" GOTOOLCHAIN="go$MODULE_GO_VERSION" \
      "$GO_BIN" build -trimpath -o "$output_dir/$command_name" "./cmd/$command_name"
  done
}

parse_args "$@"

require_command bash
require_command git
require_command rg
require_command python3
require_command jq
require_command "$GO_BIN"
require_command gofmt

if ((${#IMAGE_REQUESTS[@]} > 0 || BUILD_ALL_IMAGES == 1)); then
  require_command "$CONTAINER_ENGINE"
fi

cd "$V3_DIR"

run_step 'gofmt check' check_formatting
run_step 'git and local whitespace checks' check_whitespace
run_step 'manual-only v3 workflow and release contract' bash "$V3_DIR/scripts/validate-release-contract.sh"
run_step 'unit tests (package parallelism = 1)' run_go_test
run_step 'race tests (package parallelism = 1)' run_go_race
run_step 'go vet' run_go_vet

if (( RUN_VULN )); then
  run_step 'reachable Go vulnerability scan' run_vulnerability_scan
else
  printf '\n==> reachable Go vulnerability scan: SKIP (explicit flag)\n'
fi

run_bash_syntax
run_step 'Helm/chart validation' bash charts/agw-operator/tests/validate.sh
run_step 'trusted phase supervisor deployment contract' bash scripts/validate-phase-supervisor-contract.sh
run_image_contracts

if (( RUN_PHASE0 )); then
  run_phase0_checks
else
  printf '\n==> Phase-0 checks: SKIP (explicit flag)\n'
fi

if (( RUN_CRD_DRIFT )); then
  run_crd_drift_check
else
  printf '\n==> CRD regeneration drift: SKIP (explicit flag)\n'
fi

if (( RUN_BINARY_BUILD )); then
  run_binary_build
else
  printf '\n==> production command builds: SKIP (explicit flag)\n'
fi

if (( BUILD_ALL_IMAGES )); then
  mapfile -t IMAGE_REQUESTS < <(discover_image_names)
fi

if ((${#IMAGE_REQUESTS[@]} > 0)); then
  declare -A seen_images=()
  local_image=''
  for local_image in "${IMAGE_REQUESTS[@]}"; do
    [[ -n "${seen_images[$local_image]:-}" ]] && continue
    seen_images["$local_image"]=1
    build_one_image "$local_image"
  done
else
  printf '\n==> local Docker/Podman image builds: SKIP (use --build-images or --build-image NAME)\n'
fi

printf '\nLocal validation completed successfully. No cluster, GitHub Actions run, image push, or Docker/Podman prune was requested.\n'
