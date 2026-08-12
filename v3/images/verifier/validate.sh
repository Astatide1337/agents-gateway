#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "verifier Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'COPY v3/go.mod v3/go.sum ./'
require_text 'COPY v3/api ./api'
require_text 'COPY v3/internal ./internal'
require_text 'COPY v3/pkg ./pkg'
require_text 'COPY v3/cmd ./cmd'
require_text 'go build -trimpath -buildvcs=false'
require_text '-o /out/agw-verifier ./cmd/agw-verifier'
require_text 'apt-get install --no-install-recommends --yes ca-certificates git'
require_text 'groupadd --gid 1000 agw'
require_text 'useradd --uid 1000 --gid 1000'
require_text 'install -d -o 1000 -g 1000 -m 0700 /verify /verify/workspace'
require_text 'COPY --from=build /out/agw-verifier /agw/verifier'
require_text 'com.astatide.agw.role="verifier"'
require_text 'com.astatide.agw.evidence.protocol="AGW_VERIFY_EVIDENCE_V1"'
require_text 'HOME=/nonexistent'
require_text 'AGW_VERIFY_NETWORK=disabled'
require_text 'USER 1000:1000'
require_text 'WORKDIR /verify/workspace/repo'
require_text 'ENTRYPOINT ["/agw/verifier"]'

if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'verifier image embeds credentials or exposes a port' >&2
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
	[[ "$user" == '1000:1000' ]] || { echo "verifier image user is $user, want 1000:1000" >&2; exit 1; }
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/agw/verifier"]' ]] || { echo "verifier image entrypoint is $entrypoint" >&2; exit 1; }
	labels=$("$engine" image inspect --format '{{json .Config.Labels}}' "$2")
for needle in 'com.astatide.agw.role' 'verifier' 'AGW_VERIFY_EVIDENCE_V1'; do
	case "$labels" in
		*"$needle"*) ;;
		*) echo "verifier image labels are missing: $needle" >&2; exit 1 ;;
	esac
done
fi

echo 'verifier image contract: OK'
