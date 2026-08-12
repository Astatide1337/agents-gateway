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

require_text 'FROM ${GO_IMAGE} AS adapter-build'
require_text 'FROM ${NODE_IMAGE} AS claude-install'
require_text 'npm install --global --omit=dev "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"'
require_text 'com.astatide.agw.claude.version="${CLAUDE_CODE_VERSION}"'
require_text 'com.astatide.agw.runtime.protocol="agw.runtime.v1"'
require_text 'COPY --from=adapter-build /out/agw-runtime-claude /agw/agent'
require_text 'ENTRYPOINT ["/agw/agent"]'
require_text 'USER 1000:1000'
require_text 'AGW_HARNESS=claude-code'
require_text 'AGW_CLAUDE_WORKSPACE=/workspace/repo'
require_text 'org.opencontainers.image.licenses="MIT"'

if grep -Eq '(^|[[:space:]])(OPENAI_API_KEY|ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|CODEX_AUTH|AGW_.*TOKEN)=' "$containerfile"; then
	echo 'Containerfile embeds a provider credential or token environment variable' >&2
	exit 1
fi
if grep -Fq 'COPY .env' "$containerfile" || grep -Fq 'COPY ~/.claude' "$containerfile"; then
	echo 'Containerfile copies host credentials' >&2
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
	labels=$("$engine" image inspect --format '{{json .Config.Labels}}' "$2")
	for needle in 'com.astatide.agw.runtime.protocol' 'agw.runtime.v1' 'com.astatide.agw.claude.version'; do
		case "$labels" in
			*"$needle"*) ;;
			*) echo "image labels are missing: $needle" >&2; exit 1 ;;
		esac
	done
fi

echo 'runtime-claude image contract: OK'
