#!/usr/bin/env bash
set -Eeuo pipefail
IFS=$'\n\t'

# Provider-free object-store release gate. The Go tests create their own
# loopback TLS S3-shaped server and never contact a configured endpoint.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
V3_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
GO_BIN=${GO_BIN:-go}
GOMAXPROCS_VALUE=${AGW_GOMAXPROCS:-2}
MODULE_GO_VERSION=$(awk '/^go / { print $2; exit }' "$V3_DIR/go.mod")
RACE=0

usage() {
	cat <<'EOF'
Usage: ./scripts/objectstore-conformance.sh [--race]

Run the provider-free object-store release gate. It exercises the production
AWS SDK Store against an ephemeral loopback S3-shaped fixture, then runs the
artifact, retention, Gate-evidence, and cosign-adapter contract suites.

The command does not read object-store credentials, contact a cloud endpoint,
start a container, or leave a service running. --race enables Go's race
detector for the same bounded package set.
EOF
}

while (($#)); do
	case "$1" in
		--race) RACE=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) printf 'objectstore-conformance: unknown argument: %s\n' "$1" >&2; exit 2 ;;
	esac
done

command -v "$GO_BIN" >/dev/null 2>&1 || {
	printf 'objectstore-conformance: required command is missing: %s\n' "$GO_BIN" >&2
	exit 2
}

cd -- "$V3_DIR"
go_args=(-p 1 -count=1)
if ((RACE)); then
	go_args+=(-race)
fi

printf '%s\n' '==> provider-free object-store conformance'
printf '    GOMAXPROCS=%s GOTOOLCHAIN=go%s race=%s\n' "$GOMAXPROCS_VALUE" "$MODULE_GO_VERSION" "$RACE"
env GOMAXPROCS="$GOMAXPROCS_VALUE" GOTOOLCHAIN="go$MODULE_GO_VERSION" \
	"$GO_BIN" test "${go_args[@]}" \
	./internal/objectstore \
	./internal/retention \
	./internal/evidenceattestation \
	./internal/verificationattestation \
	./internal/cosignattestation
printf '%s\n' '<== provider-free object-store conformance: PASS'
