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

require_text 'go build -trimpath -buildvcs=false'
require_text 'COPY --from=build /out/agw-operator /agw/operator'
require_text 'gcr.io/projectsigstore/cosign:v3.1.3@sha256:9e5c2f2edc34351160407ca3416c61855bdf9403c3c5936e0f0be7fc261611b8'
require_text 'COPY --from=cosign /ko-app/cosign /usr/local/bin/cosign'
require_text 'USER 65532:65532'
require_text 'ENTRYPOINT ["/agw/operator"]'
require_text 'org.opencontainers.image.licenses="MIT AND Apache-2.0"'

if grep -Eiq 'AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)=|OPENAI_API_KEY=|ANTHROPIC_API_KEY=|AGW_.*TOKEN=' "$containerfile"; then
	echo 'operator image embeds a credential environment value' >&2
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
	[[ "$user" == "65532:65532" ]] || { echo "image user is $user, want 65532:65532" >&2; exit 1; }
	[[ "$entrypoint" == '["/agw/operator"]' ]] || { echo "image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'operator image contract: OK'
