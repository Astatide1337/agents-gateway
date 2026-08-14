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
require_text 'ARG COSIGN_SOURCE_COMMIT=11926fa5bbbbde47e88fc006b625a17769b743b2'
require_text 'ARG COSIGN_SOURCE_SHA256=3a718446bac51466efff6853639e1ca108b456ecbf07cd92938f548715d22d6b'
require_text 'ARG COSIGN_X_TEXT_VERSION=v0.40.0'
require_text 'ARG COSIGN_GRPC_VERSION=v1.82.1'
require_text 'go build -mod=mod -trimpath -buildvcs=false'
require_text 'COPY --from=cosign /out/cosign /usr/local/bin/cosign'
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
