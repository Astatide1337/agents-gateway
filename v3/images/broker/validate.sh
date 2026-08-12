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

require_text 'go build -trimpath -ldflags='
require_text 'COPY --from=build /out/agw-broker /agw/broker'
require_text 'AGW_LISTEN_ADDRESS=127.0.0.1:8081'
require_text 'USER 1337:1337'
require_text 'ENTRYPOINT ["/agw/broker"]'
require_text 'com.astatide.agw.completion="operator-observed-exit"'

if grep -Eiq '0\.0\.0\.0|/v1/runtime/process-exit|process-exit-token|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN=' "$containerfile"; then
	echo 'Containerfile embeds an unsafe listener, process-exit path, or credential environment' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'broker image must not declare a public port' >&2
	exit 1
fi

if [[ "${1:-}" == "--image" ]]; then
	if [[ -z "${2:-}" ]]; then
		echo 'usage: validate.sh [--image IMAGE]' >&2
		exit 2
	fi
	engine=${CONTAINER_ENGINE:-docker}
	if ! command -v "$engine" >/dev/null 2>&1; then
		echo "container engine not found: $engine" >&2
		exit 2
	fi
	user=$("$engine" image inspect --format '{{.Config.User}}' "$2")
	if [[ "$user" != "1337:1337" ]]; then
		echo "image user is $user, want 1337:1337" >&2
		exit 1
	fi
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	case "$entrypoint" in
		*'"/agw/broker"'*) ;;
		*) echo 'image entrypoint is not /agw/broker' >&2; exit 1 ;;
	esac
fi

echo 'broker image contract: OK'
