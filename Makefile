GORELEASER_VERSION := v2.12.7

.PHONY: build context context-check generate-api generate-api-check test-automation test-automation-full docs-dev docs-check test test-container test-stack test-failure test-store test-production-config test-provider-contracts test-acceptance-safety verify audit deadcode release-check snapshot acceptance-local acceptance-preflight acceptance-cleanup qualify-local qualify-rc qualify-contracts dev-check dev-check-full dev-up dev-down

build:
	go build ./cmd/infercrane

context:
	go run ./tools/repo-context

context-check:
	go run ./tools/repo-context -check

generate-api:
	go run ./tools/openapi-codegen

generate-api-check:
	go run ./tools/openapi-codegen -check

test-automation:
	./scripts/test-automation-clients.sh quick

test-automation-full:
	./scripts/test-automation-clients.sh full

docs-dev:
	cd docs && npm run dev

docs-check:
	cd docs && npm run check && npm run check:a11y

test:
	go test -race -count=1 ./...

test-container:
	docker compose --profile test build verifier
	docker compose --profile test up --build --abort-on-container-exit --exit-code-from integration-test integration-test

test-stack:
	./scripts/test-stack.sh

test-failure:
	./scripts/test-failure-recovery.sh

test-production-config:
	./scripts/test-production-compose.sh

test-provider-contracts:
	go test -count=1 -run ProviderContract ./internal/provision

test-acceptance-safety:
	./scripts/test-acceptance-safety.sh

test-store:
	test -n "$$INFERCRANE_TEST_DATABASE_URL"
	go test -race -count=1 -v ./internal/store

verify:
	./scripts/verify-repository.sh

audit:
	go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
	cd integrations/terraform && go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
	npm --prefix sdk/typescript audit --audit-level=high

deadcode:
	./scripts/check-dead-code.sh

release-check:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) check
	ruby -c packaging/homebrew/infercrane.rb

snapshot: release-check
	@command -v syft >/dev/null || { echo "syft is required to generate archive SBOMs; install it from https://github.com/anchore/syft" >&2; exit 1; }
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean

acceptance-local:
	./scripts/release-acceptance.sh local

acceptance-preflight:
	./scripts/release-acceptance.sh preflight

acceptance-cleanup:
	./scripts/release-acceptance.sh cleanup

qualify-local:
	./scripts/qualify-release.sh local

qualify-rc:
	./scripts/qualify-release.sh rc --approve-paid-resources

qualify-contracts:
	go run ./tools/contract-qualifier --output .infercrane/contract-qualification/$$(git rev-parse HEAD)/qualification.json

dev-check:
	./scripts/dev-check.sh quick

dev-check-full:
	./scripts/dev-check.sh full

dev-up:
	docker compose up --build -d

dev-down:
	docker compose down
