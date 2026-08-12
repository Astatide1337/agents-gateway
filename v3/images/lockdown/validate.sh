#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
containerfile="$script_dir/Containerfile"

require_text() {
	local needle=$1
	if ! grep -Fq -- "$needle" "$containerfile"; then
		echo "lockdown Containerfile is missing: $needle" >&2
		exit 1
	fi
}

require_text 'FROM docker.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1'
require_text 'RUN apk add --no-cache iptables'
require_text 'ENTRYPOINT ["/bin/sh"]'

if grep -Eq '^USER[[:space:]]' "$containerfile"; then
	echo 'lockdown image must leave root identity selection to the namespaced NET_ADMIN Pod contract' >&2
	exit 1
fi
if grep -Eiq '(^|[[:space:]])(AWS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY|SESSION_TOKEN)|OPENAI_API_KEY|ANTHROPIC_API_KEY|AGW_.*TOKEN)=' "$containerfile" || grep -Fq 'EXPOSE ' "$containerfile"; then
	echo 'lockdown image embeds credentials or exposes a port' >&2
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
		*) echo "lockdown image user is $user, want default/root for namespaced NET_ADMIN" >&2; exit 1 ;;
	esac
	entrypoint=$("$engine" image inspect --format '{{json .Config.Entrypoint}}' "$2")
	[[ "$entrypoint" == '["/bin/sh"]' ]] || { echo "lockdown image entrypoint is $entrypoint" >&2; exit 1; }
fi

echo 'lockdown image contract: OK'
