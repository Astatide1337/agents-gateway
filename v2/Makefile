.PHONY: test test-race vet validate web-test web-build compose-config image-build security e2e-control-plane e2e-standalone-api e2e-standalone-podman ci

GO ?= go
NPM ?= npm
COMPOSE ?= docker compose

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

validate:
	$(GO) run ./cmd/agw validate --production -f examples/quickstart.yaml

web-test:
	cd web && $(NPM) ci --ignore-scripts && $(NPM) test -- --passWithNoTests

web-build:
	cd web && $(NPM) ci --ignore-scripts && $(NPM) run build

compose-config:
	POSTGRES_ADMIN_PASSWORD=$${POSTGRES_ADMIN_PASSWORD:-ci-postgres-admin} \
	AGW_DB_PASSWORD=$${AGW_DB_PASSWORD:-ci-agw} \
	AGW_AUTH_DB_PASSWORD=$${AGW_AUTH_DB_PASSWORD:-ci-auth} \
	MINIO_ROOT_USER=$${MINIO_ROOT_USER:-ci-minio} \
	MINIO_ROOT_PASSWORD=$${MINIO_ROOT_PASSWORD:-ci-minio-password} \
	$(COMPOSE) -f deploy/compose.yaml --profile bundled config >/dev/null
	MINIO_ROOT_USER=$${MINIO_ROOT_USER:-ci-minio} \
	MINIO_ROOT_PASSWORD=$${MINIO_ROOT_PASSWORD:-ci-minio-password} \
	$(COMPOSE) -f deploy/compose.yaml --profile artifacts config >/dev/null
	AGW_MIGRATION_DATABASE_URL=$${AGW_MIGRATION_DATABASE_URL:-postgres://migrator:secret@db.example/agents_gateway} \
	$(COMPOSE) -f deploy/compose.yaml --profile migrate config >/dev/null
	POSTGRES_ADMIN_PASSWORD=$${POSTGRES_ADMIN_PASSWORD:-ci-postgres-admin} \
	AGW_DB_PASSWORD=$${AGW_DB_PASSWORD:-ci-agw} \
	AGW_AUTH_TOKEN=$${AGW_AUTH_TOKEN:-0123456789abcdef0123456789abcdef} \
	$(COMPOSE) -f deploy/compose.standalone.yaml config >/dev/null

image-build:
	docker build --pull -f Dockerfile.server -t agents-gateway-v2-server:ci ..
	docker build --pull -f Dockerfile.worker -t agents-gateway-v2-worker:ci ..
	docker build --pull -f Dockerfile.cli -t agents-gateway-v2-cli:ci ..
	docker build --pull -f Dockerfile.web -t agents-gateway-v2-console:ci ..
	docker build --pull -f Dockerfile.runtime-codex -t agents-gateway-v2-runtime-codex:ci ..

security:
	docker run --rm -v "$(CURDIR):/src:ro" ghcr.io/google/osv-scanner:latest scan source -L /src/go.mod -L /src/web/package-lock.json
	cd web && $(NPM) audit --audit-level=high

e2e-control-plane:
	./scripts/e2e-control-plane.sh

e2e-standalone-api:
	AGW_STANDALONE_LIVE=1 $(GO) test -race -count=1 -run '^TestStandaloneLocalEngineLive$$' -v ./cmd/agw-server # gitleaks:allow

e2e-standalone-podman:
	./scripts/e2e-standalone.sh

ci: test test-race vet validate web-test web-build compose-config security image-build
