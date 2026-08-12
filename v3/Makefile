.PHONY: generate manifests test race vet vuln bash-check chart-check image-contract-check phase0-plan build ci fmt-check diff-check

GO ?= go
MODULE_GO_VERSION := $(shell awk '/^go / { print $$2; exit }' go.mod)
GOVULNCHECK_VERSION := v1.6.0
CONTROLLER_GEN_VERSION ?= v0.21.0
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

# Keep this list explicit: every production entrypoint must compile in CI, even
# when it is only used by a helper image or the kubectl plugin.
GO_COMMANDS := \
	agw-operator \
	agw-attest \
	agw-broker \
	agw-capture \
	agw-clone \
	agw-context \
	agw-critic \
	agw-runtime-codex \
	agw-runtime-claude \
	agw-skills \
	agw-verifier \
	agw-verify-fetch \
	agw-verify-apply \
	agw-preflight \
	agw-agent-run-lifecycle \
	kubectl-agw

# v3 currently adopts controller-gen for CRD manifest generation. The
# hand-owned deepcopy file remains outside this slice until the API foundation
# switches fully to controller-gen object output.
generate:
	$(CONTROLLER_GEN) crd:crdVersions=v1 paths="./api/..." output:crd:artifacts:config="./config/crd"

manifests: generate

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

vuln:
	GOTOOLCHAIN=go$(MODULE_GO_VERSION) $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

bash-check:
	bash -n scripts/*.sh images/*/*.sh phase0/agentgateway-guard/scripts/*.sh phase0/argo-sandbox/scripts/*.sh

chart-check:
	bash charts/agw-operator/tests/validate.sh

image-contract-check:
	@set -eu; \
	first="$$(find images -mindepth 2 -maxdepth 2 -type f -name validate.sh -print -quit)"; \
	test -n "$$first" || { echo 'no image validators found' >&2; exit 1; }; \
	find images -mindepth 2 -maxdepth 2 -type f -name validate.sh -print \
		| sort \
		| while IFS= read -r validator; do \
			bash "$$validator"; \
		done

fmt-check:
	@files="$$(gofmt -l .)"; if test -n "$${files}"; then printf '%s\n' "gofmt required for:" $${files}; exit 1; fi

diff-check:
	git -C .. diff --check -- v3 docs/v3 .github/workflows/v3-ci.yml .github/workflows/v3-release.yml

phase0-plan:
	@set -eu; \
	for script in scripts/phase0-airlock.sh scripts/phase0-credentials.sh scripts/phase0-inventory.sh scripts/phase0-objectstore.sh scripts/phase0-sandbox.sh scripts/phase0-userns.sh; do \
		run_id="$$(basename "$${script}" .sh)-ci-plan"; \
		PHASE0_MODE=plan PHASE0_RUN_ID="$${run_id}" PHASE0_OUTPUT_DIR="$${TMPDIR:-/tmp}/agw-phase0-plan" \
			"$${script}" --plan --namespace agw-phase0-ci; \
	done

build:
	@set -eu; \
	out_dir="$${TMPDIR:-/tmp}/agw-v3-build"; \
	mkdir -p "$$out_dir"; \
	for command in $(GO_COMMANDS); do \
		printf '%s\n' "building ./cmd/$$command"; \
		$(GO) build -trimpath -o "$$out_dir/$$command" "./cmd/$$command"; \
	done; \
	printf '%s\n' "built $(words $(GO_COMMANDS)) production commands in $$out_dir"

ci: fmt-check diff-check test race vet vuln bash-check chart-check image-contract-check phase0-plan generate build
