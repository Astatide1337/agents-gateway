#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Build the smallest image set needed for a direct Codex AgentRun proof.
#
# This is deliberately separate from the release workflow. It does not use
# GitHub Actions, QEMU, a registry, Trivy, SBOM generation, Cosign, or any
# cleanup command. The resulting tags are for a disposable local cluster only;
# production admission still requires registry-published digest references.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
V3_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
REPO_ROOT=$(CDPATH= cd -- "$V3_DIR/.." && pwd)

CONTAINER_ENGINE=${CONTAINER_ENGINE:-docker}
IMAGE_PREFIX=${AGW_PROOF_IMAGE_PREFIX:-agw-v3-proof}
OUTPUT_FILE=${AGW_PROOF_IMAGES_FILE:-}
VCS_REF=${VCS_REF:-$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || printf 'local')}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
SKIP_CONTRACT=0

# The direct backend does not need Argo's lifecycle producer, the Claude
# runtime, or the optional critic. preflight is included because it is enabled
# by the chart's safe default.
DIRECT_CODEX_IMAGES=(
  operator
  clone
  skills
  context
  lockdown
  broker
  capture
  verify-fetch
  verify-apply
  preflight
  runtime-codex
  verifier
)

declare -a REQUESTED_IMAGES=()
declare -a BUILT_IMAGES=()
declare -A IMAGE_REFS=()
declare -A IMAGE_IDS=()

usage() {
  cat <<'EOF'
Usage: ./scripts/build-local-proof-images.sh [options]

Build the local direct-Codex proof profile. It builds amd64 images only and
does not push, scan, sign, attest, or prune anything.

Options:
  --image NAME       Build only this image (repeatable). Without this option,
                     build the direct-codex profile.
  --list             List images in the direct-codex profile and exit.
  --output PATH      Write a shell-readable image map to PATH.
  --skip-contract    Skip the image validator (not recommended).
  -h, --help         Show this help.

The local tags and Docker image IDs are intentionally written to the output
map. They are not production image references; production admission still
requires a registry reference pinned with @sha256:...

Environment:
  CONTAINER_ENGINE       docker or podman (default: docker)
  AGW_PROOF_IMAGE_PREFIX local tag prefix (default: agw-v3-proof)
  AGW_PROOF_IMAGES_FILE  default output path, if set
EOF
}

die() {
  printf 'build-local-proof-images: %s\n' "$*" >&2
  exit 2
}

is_profile_image() {
  local requested=$1 image
  for image in "${DIRECT_CODEX_IMAGES[@]}"; do
    [[ "$image" == "$requested" ]] && return 0
  done
  return 1
}

image_containerfile() {
  local name=$1 image_dir="$V3_DIR/images/$1"
  if [[ -f "$image_dir/Containerfile" ]]; then
    printf '%s\n' "$image_dir/Containerfile"
  elif [[ -f "$image_dir/Dockerfile" ]]; then
    printf '%s\n' "$image_dir/Dockerfile"
  else
    die "image $name has no Containerfile or Dockerfile"
  fi
}

image_validator() {
  local name=$1
  printf '%s\n' "$V3_DIR/images/$name/validate.sh"
}

image_build_helper() {
  local helper="$V3_DIR/images/$1/build.sh"
  if [[ -f "$helper" ]]; then
    printf '%s\n' "$helper"
  else
    printf '%s\n' ''
  fi
}

parse_args() {
  while (($#)); do
    case "$1" in
      --image)
        [[ $# -ge 2 ]] || die '--image requires a name'
        is_profile_image "$2" || die "image '$2' is not in the direct-codex profile"
        REQUESTED_IMAGES+=("$2")
        shift 2
        ;;
      --list)
        printf '%s\n' "${DIRECT_CODEX_IMAGES[@]}"
        exit 0
        ;;
      --output)
        [[ $# -ge 2 ]] || die '--output requires a path'
        OUTPUT_FILE=$2
        shift 2
        ;;
      --skip-contract)
        SKIP_CONTRACT=1
        shift
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
        die "unknown argument: $1"
        ;;
    esac
  done
}

build_image() {
  local name=$1 tag="$IMAGE_PREFIX-$name:local" helper
  helper=$(image_build_helper "$name")

  printf '\n==> build local image: %s\n' "$name"
  if [[ -n "$helper" ]]; then
    IMAGE="$tag" \
      CONTAINER_ENGINE="$CONTAINER_ENGINE" \
      VCS_REF="$VCS_REF" \
      BUILD_DATE="$BUILD_DATE" \
      bash "$helper"
  else
    "$CONTAINER_ENGINE" build \
      --file "$(image_containerfile "$name")" \
      --tag "$tag" \
      --build-arg "VCS_REF=$VCS_REF" \
      --build-arg "BUILD_DATE=$BUILD_DATE" \
      "$REPO_ROOT"
  fi

  if (( SKIP_CONTRACT == 0 )); then
    printf '\n==> validate local image contract: %s\n' "$name"
    bash "$(image_validator "$name")" --image "$tag"
  fi

  local image_id
  image_id=$("$CONTAINER_ENGINE" image inspect --format '{{.Id}}' "$tag")
  [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "engine returned an invalid image ID for $name: $image_id"
  IMAGE_REFS["$name"]=$tag
  IMAGE_IDS["$name"]=$image_id
  BUILT_IMAGES+=("$name")
  printf '<== %s: %s (%s)\n' "$name" "$tag" "$image_id"
}

write_output() {
  [[ -n "$OUTPUT_FILE" ]] || return 0
  local parent
  parent=$(dirname -- "$OUTPUT_FILE")
  [[ -d "$parent" ]] || die "output directory does not exist: $parent"
  {
    printf '# Generated by build-local-proof-images.sh; local proof only.\n'
    printf 'AGW_PROOF_SOURCE_REVISION=%q\n' "$VCS_REF"
    printf 'AGW_PROOF_BUILD_DATE=%q\n' "$BUILD_DATE"
    local name
    for name in "${BUILT_IMAGES[@]}"; do
      local key=${name//-/_}
      printf 'AGW_PROOF_%s_IMAGE=%q\n' "${key^^}" "${IMAGE_REFS[$name]}"
      printf 'AGW_PROOF_%s_IMAGE_ID=%q\n' "${key^^}" "${IMAGE_IDS[$name]}"
    done
  } >"$OUTPUT_FILE"
  chmod 0600 "$OUTPUT_FILE"
  printf '\nWrote local image map: %s\n' "$OUTPUT_FILE"
}

parse_args "$@"

command -v -- "$CONTAINER_ENGINE" >/dev/null 2>&1 || die "container engine not found: $CONTAINER_ENGINE"
command -v -- git >/dev/null 2>&1 || die 'git is required to identify the local source revision'
[[ "$IMAGE_PREFIX" =~ ^[a-z0-9][a-z0-9._/-]*$ ]] || die 'AGW_PROOF_IMAGE_PREFIX contains unsupported characters'
[[ "$VCS_REF" != *$'\n'* && "$VCS_REF" != *$'\r'* ]] || die 'VCS_REF contains a newline'
[[ "$BUILD_DATE" != *$'\n'* && "$BUILD_DATE" != *$'\r'* ]] || die 'BUILD_DATE contains a newline'

if ((${#REQUESTED_IMAGES[@]} == 0)); then
  REQUESTED_IMAGES=("${DIRECT_CODEX_IMAGES[@]}")
fi

declare -A seen=()
for image in "${REQUESTED_IMAGES[@]}"; do
  [[ -n "${seen[$image]:-}" ]] && continue
  seen["$image"]=1
  build_image "$image"
done

write_output
printf '\nLocal direct-Codex proof image build completed. No registry or CI action was used.\n'
