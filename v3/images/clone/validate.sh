#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "clone Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/ ./'
require_text 'go build -trimpath -ldflags='
require_text '-o /out/agw-clone ./cmd/agw-clone'
require_text 'apk add --no-cache ca-certificates git'
require_text 'install -m 0400 -o 0 -g 0 /dev/null /run/agw/clone/token'
require_text 'test ! -L /run/agw/clone/token'
require_text 'COPY --from=build /out/agw-clone /agw/clone'
require_text 'ENTRYPOINT ["/agw/clone"]'

if grep -Eq '^USER[[:space:]]' "$containerfile"; then
	echo 'clone image must leave identity selection to the hostUsers:false Pod contract' >&2
	exit 1
fi
if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'COPY .env' "$containerfile"; then
	echo 'clone image embeds a credential or host environment file' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'clone image must not expose a network port' >&2
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
		''|0|0:0|root|root:root) ;;
		*) echo "clone image user is $user, want default/root for Pod override" >&2; exit 1 ;;
	esac
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/clone"]' ]] || { echo "clone image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'clone image contract: OK'
