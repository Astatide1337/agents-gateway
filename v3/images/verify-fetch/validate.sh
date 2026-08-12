#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "verify-fetch Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_regex() {
	local expression=$1
	if ! grep -Eq -- "$expression" "$containerfile"; then
		echo "verify-fetch Containerfile does not match: $expression" >&2
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
require_text '-o /out/agw-verify-fetch ./cmd/agw-verify-fetch'
require_text 'apk add --no-cache ca-certificates git'
require_text '/run/agw/fetch/clone-token'
require_text '/run/agw/fetch/artifact-access-key-id'
require_text '/run/agw/fetch/artifact-secret-access-key'
require_text '/run/agw/fetch/artifact-session-token'
require_text 'test ! -L /run/agw/fetch/clone-token'
require_text 'COPY --from=build /out/agw-verify-fetch /agw/fetch'
require_text 'USER 0:0'
require_text 'WORKDIR /verify/workspace'
require_text 'ENTRYPOINT ["/agw/fetch"]'

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'verify-fetch image embeds credentials or exposes a port' >&2
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
	case "$user" in
		0|0:0|root|root:root) ;;
		*) echo "verify-fetch image user is $user, want 0:0" >&2; exit 1 ;;
	esac
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/fetch"]' ]] || { echo "verify-fetch image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'verify-fetch image contract: OK'
