#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root/v3"

go test ./internal/criticworkload ./cmd/agw-critic
go test -race ./internal/criticworkload ./cmd/agw-critic
test -z "$(gofmt -l cmd/agw-critic/*.go)"

grep -q 'ENTRYPOINT \["/agw/critic"\]' images/critic/Containerfile
grep -q 'USER 1000:1000' images/critic/Containerfile
grep -q 'materialize-input' images/critic/README.md
grep -q 'retry.attempts: 1' images/critic/README.md
grep -Eq '^ARG GO_IMAGE=.*@sha256:[0-9a-f]{64}$' images/critic/Containerfile
grep -q 'ca-certificates.crt' images/critic/Containerfile
