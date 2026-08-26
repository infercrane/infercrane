GORELEASER_VERSION := v2.12.7
GORELEASER ?= go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)
RELEASE_CANDIDATE_TAG ?= $(shell jq -r '.candidate_tag' .release/version.json)

.PHONY: build generate-api generate-api-check hygiene-check license-check test-automation test-automation-full docs-dev docs-check demo demo-connect demo-record test test-container test-stack test-failure test-ha test-backup-restore test-store test-production-config test-provider-contracts test-simulated-clouds test-network-chaos test-kubernetes-manifests test-kubernetes-kind test-kubernetes-kwok test-kubernetes-versions test-fuzz test-reliability-soak test-acceptance-safety test-benchmark-matrix test-benchmark-concurrency-sweep test-aiconfigurator-upstream test-release-tag test-product test-product-qualification test-user-lifecycle acceptance-product verify audit deadcode release-check release-tag-check release-tag-candidate release-tag-stable snapshot candidate-artifacts release-artifacts acceptance-local acceptance-preflight acceptance-cleanup qualify-local qualify-product qualify-product-nightly qualify-product-status qualify-rc qualify-v1 qualify-contracts qualify-aws-performance build-aws-artifact-cache qualify-aws-artifact-cache cleanup-aws-artifact-cache delete-aws-artifact-cache dev-check dev-check-full dev-up dev-down

build:
	go build ./cmd/infercrane

generate-api:
	go run ./tools/openapi-codegen

generate-api-check:
	go run ./tools/openapi-codegen -check

hygiene-check:
	./scripts/check-repository-hygiene.sh

license-check:
	./scripts/check-license-boundaries.sh

test-automation:
	./scripts/test-automation-clients.sh quick

test-automation-full:
	./scripts/test-automation-clients.sh full

docs-dev:
	cd docs && npm run dev

docs-check:
	cd docs && npm run check && npm run check:a11y

demo:
	./scripts/demo-product.sh

demo-connect:
	./scripts/demo-connect.sh

demo-record:
	./scripts/record-demo.sh

test:
	go test -race -count=1 ./...

test-container:
	docker compose --profile test build verifier
	docker compose --profile test rm -sf integration-test test-postgres
	docker compose --profile test up --build --abort-on-container-exit --exit-code-from integration-test integration-test

test-stack:
	./scripts/test-stack.sh

test-failure:
	./scripts/test-failure-recovery.sh

test-ha:
	./scripts/test-ha-control-plane.sh

test-backup-restore:
	./scripts/test-backup-restore-safety.sh
	./scripts/test-backup-restore-docker.sh

test-production-config:
	./scripts/test-production-compose.sh
	./scripts/test-entrypoint.sh

test-provider-contracts:
	go test -count=1 -run ProviderContract ./internal/provision

test-simulated-clouds:
	./scripts/test-simulated-clouds.sh

test-network-chaos:
	./scripts/test-network-chaos.sh

test-kubernetes-manifests:
	./scripts/test-kubernetes-manifests.sh

test-kubernetes-kind:
	./scripts/test-kubernetes-kind.sh

test-kubernetes-kwok:
	./scripts/test-kubernetes-kwok.sh

test-kubernetes-versions:
	./scripts/test-kubernetes-versions.sh

test-fuzz:
	./scripts/test-fuzz.sh

test-reliability-soak:
	./scripts/test-reliability-soak.sh

test-acceptance-safety:
	./scripts/test-acceptance-safety.sh

test-benchmark-matrix:
	./scripts/test-benchmark-matrix.sh

test-benchmark-concurrency-sweep:
	./scripts/test-benchmark-concurrency-sweep.sh

test-aiconfigurator-upstream:
	./scripts/test-aiconfigurator-upstream.sh

test-release-tag:
	./scripts/test-release-tag.sh

qualify-aws-performance:
	./scripts/aws-performance-qualification.sh $(MODEL) --approve-paid-resources

build-aws-artifact-cache:
	./scripts/aws-artifact-cache-build.sh build --approve-paid-resources

qualify-aws-artifact-cache:
	./scripts/aws-artifact-cache-qualification.sh $(MODEL) --approve-paid-resources

cleanup-aws-artifact-cache:
	./scripts/aws-artifact-cache-build.sh cleanup

delete-aws-artifact-cache:
	./scripts/aws-artifact-cache-build.sh delete-snapshot --approve-snapshot-deletion

test-product-qualification:
	./scripts/test-product-qualification.sh

test-user-lifecycle:
	./scripts/qualify-user-lifecycle.sh run

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
	$(GORELEASER) check
	ruby -c packaging/homebrew/infercrane.rb

release-tag-check:
	./scripts/release-tag.sh check

release-tag-candidate:
	./scripts/release-tag.sh candidate

release-tag-stable:
	./scripts/release-tag.sh stable

snapshot: release-check
	@command -v syft >/dev/null || { echo "syft is required to generate archive SBOMs; install it from https://github.com/anchore/syft" >&2; exit 1; }
	GORELEASER_CURRENT_TAG=$(RELEASE_CANDIDATE_TAG) $(GORELEASER) release --snapshot --clean

candidate-artifacts: release-check
	@command -v syft >/dev/null || { echo "syft is required to generate archive SBOMs; install it from https://github.com/anchore/syft" >&2; exit 1; }
	@case "$(RELEASE_CANDIDATE_TAG)" in v*-rc.*) ;; *) echo "RELEASE_CANDIDATE_TAG must be an RC tag" >&2; exit 1;; esac
	./scripts/build-release-candidate.sh $(RELEASE_CANDIDATE_TAG) $(GORELEASER_VERSION)
	./scripts/verify-release-artifacts.sh dist $(RELEASE_CANDIDATE_TAG)
	./scripts/generate-homebrew-formula.sh dist $(RELEASE_CANDIDATE_TAG)

release-artifacts: release-check
	@command -v syft >/dev/null || { echo "syft is required to generate archive SBOMs; install it from https://github.com/anchore/syft" >&2; exit 1; }
	@printf '%s\n' "$(RELEASE_TAG)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$$' || { echo "RELEASE_TAG must be a stable semantic version" >&2; exit 1; }
	./scripts/build-release-candidate.sh $(RELEASE_TAG) $(GORELEASER_VERSION)
	./scripts/verify-release-artifacts.sh dist $(RELEASE_TAG)
	./scripts/generate-homebrew-formula.sh dist $(RELEASE_TAG)

acceptance-local:
	./scripts/release-acceptance.sh local

acceptance-preflight:
	./scripts/release-acceptance.sh preflight

acceptance-cleanup:
	./scripts/release-acceptance.sh cleanup

qualify-local:
	./scripts/qualify-release.sh local

qualify-product:
	./scripts/qualify-product.sh local

qualify-product-nightly:
	./scripts/qualify-product.sh nightly

qualify-product-status:
	./scripts/qualify-product.sh status

qualify-rc:
	./scripts/qualify-release.sh rc --approve-paid-resources

qualify-v1:
	./scripts/v1-acceptance.sh qualify --approve-paid-resources

qualify-contracts:
	go run ./tools/contract-qualifier --output .infercrane/contract-qualification/$$(git rev-parse HEAD)/qualification.json

dev-check:
	./scripts/dev-check.sh quick

dev-check-full:
	./scripts/dev-check.sh full

test-product:
	./scripts/product-acceptance.sh first-value

acceptance-product:
	./scripts/product-acceptance.sh local

dev-up:
	docker compose up --build -d

dev-down:
	docker compose down
