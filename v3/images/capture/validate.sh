#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Dockerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "capture Dockerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/api ./api'
require_text 'COPY v3/cmd ./cmd'
require_text 'COPY v3/internal ./internal'
require_text 'COPY v3/pkg ./pkg'
require_text 'go build -trimpath -buildvcs=false'
require_text '-o /out/agw-capture ./cmd/agw-capture'
require_text 'apt-get install --no-install-recommends --yes ca-certificates git'
require_text 'groupadd --gid 1000 agw'
require_text 'useradd --uid 1000 --gid 1000'
require_text 'COPY --from=build /out/agw-capture /agw/capture'
require_text 'USER 1000:1000'
require_text 'WORKDIR /workspace/repo'
require_text 'ENTRYPOINT ["/agw/capture"]'

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'COPY .env' "$containerfile"; then
	echo 'capture image embeds a credential or host environment file' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'capture image must not expose a network port' >&2
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
	[[ "$user" == '1000:1000' ]] || { echo "capture image user is $user, want 1000:1000" >&2; exit 1; }
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/capture"]' ]] || { echo "capture image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'capture image contract: OK'
