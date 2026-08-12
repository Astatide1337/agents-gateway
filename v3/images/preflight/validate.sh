#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY --from=build /out/agw-preflight /agw/preflight'
require_text 'USER 1337:1337'
require_text 'ENTRYPOINT ["/agw/preflight"]'
require_text 'org.opencontainers.image.licenses="MIT"'
require_text 'COPY v3/go.mod v3/go.sum ./'

if grep -Eiq 'AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)=|OPENAI_API_KEY=|ANTHROPIC_API_KEY=|AGW_.*TOKEN=' "$containerfile"; then
	echo 'preflight image embeds a credential environment value' >&2
	exit 1
fi

if [[ "${1:-}" == "--image" ]]; then
	if [[ -z "${2:-}" ]]; then
		echo 'usage: validate.sh [--image IMAGE]' >&2
		exit 2
	fi
	engine=${CONTAINER_ENGINE:-docker}
	user=$("$engine" image inspect --format '{{.Config.User}}' "$2")
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$user" == "1337:1337" ]] || { echo "image user is $user, want 1337:1337" >&2; exit 1; }
	[[ "$entrypoint" == '["/agw/preflight"]' ]] || { echo "image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'preflight image contract: OK'
