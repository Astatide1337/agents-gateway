#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "lifecycle Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/ ./'
require_text 'ARG GO_IMAGE=docker.io/library/golang:1.26.5@sha256:7caba5286b4c3613a337b709c573047d8ae62ee76106647313b61e72b99f20af'
require_text 'ARG RUNTIME_IMAGE=docker.io/library/alpine:3.22.5@sha256:7c8cb692ae09657cbc4a3f3cbd0e8d5a2690ba38386aaaf252dbb060bf5eb2e6'
if grep -Eiq '(^|[[:space:]])(FROM|ARG)[^#]*:latest([[:space:]]|$)' "$containerfile"; then
	echo 'lifecycle Containerfile must not use mutable latest image references' >&2
	exit 1
fi
require_text 'go build -trimpath -buildvcs=false'
require_text '-o /out/agw-agent-run-lifecycle ./cmd/agw-agent-run-lifecycle'
require_text 'COPY --from=build /out/agw-agent-run-lifecycle /agw/agent-run-lifecycle'
require_text 'USER 1000:1000'
require_text 'ENTRYPOINT ["/agw/agent-run-lifecycle"]'
require_text 'org.opencontainers.image.licenses="MIT"'
require_text 'apk add --no-cache ca-certificates'
require_text 'Bounded Argo clone, Sandbox wait, capture, and authenticated lifecycle handoff producer'

if grep -Fqi 'fail-closed-scaffold' "$containerfile" || grep -Fq 'args: ["produce"]' "$containerfile"; then
	echo 'lifecycle image still contains the retired fail-closed producer scaffold' >&2
	exit 1
fi

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'COPY .env' "$containerfile"; then
	echo 'lifecycle image embeds a credential or host environment file' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'lifecycle image must not expose a network port' >&2
	exit 1
fi

if [[ "${1:-}" == "--image" ]]; then
	if [[ -z "${2:-}" || -n "${3:-}" ]]; then
		echo 'usage: validate.sh [--image IMAGE]' >&2
		exit 2
	fi
	engine=${CONTAINER_ENGINE:-docker}
	command -v "$engine" >/dev/null 2>&1 || { echo "container engine not found: $engine" >&2; exit 2; }
	user=$("$engine" image inspect --format '{{.Config.User}}' "$2")
	[[ "$user" == '1000:1000' ]] || { echo "image user is $user, want 1000:1000" >&2; exit 1; }
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/agent-run-lifecycle"]' ]] || { echo "image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'agent-run-lifecycle image contract: OK'
