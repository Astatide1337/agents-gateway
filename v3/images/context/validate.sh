#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "context Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/api ./api'
require_text 'COPY v3/cmd ./cmd'
require_text 'COPY v3/internal ./internal'
require_text 'COPY v3/pkg ./pkg'
require_text 'go build -trimpath -buildvcs=false'
require_text '-o /out/agw-context ./cmd/agw-context'
require_text 'FROM docker.io/library/debian:bookworm-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241'
require_text 'apt-get install --no-install-recommends --yes ca-certificates git'
require_text 'COPY --from=build /out/agw-context /agw/context'
require_text 'COPY v3/internal ./internal'
require_text 'USER 0:0'
require_text 'ENTRYPOINT ["/agw/context"]'

if [[ ! -f "$script_dir/README.md" ]] || ! grep -Fq -- 'AGW_CONTEXT_SYMBOL_ADAPTER_SHA256' "$script_dir/README.md"; then
	echo 'context image README must document the optional local symbol adapter' >&2
	exit 1
fi

if grep -Eq 'AGW_CONTEXT_SYMBOL_ADAPTER(_SHA256)?=' "$containerfile"; then
	echo 'default context image must not enable an unreviewed symbol provider' >&2
	exit 1
fi

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'COPY .env' "$containerfile"; then
	echo 'context image embeds a credential or host environment file' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'context image must not expose a network port' >&2
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
		*) echo "context image user is $user, want namespaced root for sealing" >&2; exit 1 ;;
	esac
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/context"]' ]] || { echo "context image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'context image contract: OK'
