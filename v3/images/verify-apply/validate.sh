#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "verify-apply Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_regex() {
	local expression=$1
	if ! grep -Eq -- "$expression" "$containerfile"; then
		echo "verify-apply Containerfile does not match: $expression" >&2
		exit 1
	fi
}

require_regex '^ARG GO_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_regex '^ARG RUNTIME_IMAGE=.*@sha256:[0-9a-f]{64}$'
require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/api ./api'
require_text 'COPY v3/cmd ./cmd'
require_text 'COPY v3/internal ./internal'
require_text 'COPY v3/pkg ./pkg'
require_text 'go build -trimpath -buildvcs=false'
require_text '-o /out/agw-verify-apply ./cmd/agw-verify-apply'
require_text 'apk add --no-cache git'
require_text 'COPY --from=build /out/agw-verify-apply /agw/apply'
require_text 'USER 1000:1000'
require_text 'WORKDIR /verify/workspace/repo'
require_text 'ENTRYPOINT ["/agw/apply"]'

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'verify-apply image embeds credentials or exposes a port' >&2
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
	[[ "$user" == '1000:1000' ]] || { echo "verify-apply image user is $user, want 1000:1000" >&2; exit 1; }
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/apply"]' ]] || { echo "verify-apply image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'verify-apply image contract: OK'
