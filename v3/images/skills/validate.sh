#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "skills Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/ ./'
require_text 'go build -trimpath -ldflags='
require_text '-o /out/agw-skills ./cmd/agw-skills'
require_text 'FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a'
require_text 'install -d -m 0700 -o 1000 -g 1000 /out/run/agw/skills'
require_text 'install -m 0400 -o 1000 -g 1000 /dev/null /out/run/agw/skills/token'
require_text 'test ! -L /out/run/agw/skills/token'
require_text 'COPY --from=build /out/agw-skills /agw/skills'
require_text 'COPY --from=build --chown=1000:1000 /out/run/ /run/'
require_text 'ENTRYPOINT ["/agw/skills"]'

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'COPY .env' "$containerfile"; then
	echo 'skills image embeds a credential or host environment file' >&2
	exit 1
fi
if grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'skills image must not expose a network port' >&2
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
		nonroot|nonroot:nonroot|65532|65532:65532|1000|1000:1000) ;;
		*) echo "skills image user is $user, want a non-root image identity compatible with Pod UID 1000" >&2; exit 1 ;;
	esac
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/skills"]' ]] || { echo "skills image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'skills image contract: OK'
