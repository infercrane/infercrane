.PHONY: build context context-check test test-container test-stack test-failure test-store verify audit deadcode dev-up dev-down

build:
	go build ./cmd/infercrane

context:
	go run ./tools/repo-context

context-check:
	go run ./tools/repo-context -check

test:
	go test -race -count=1 ./...

test-container:
	docker compose --profile test build verifier
	docker compose --profile test up --build --abort-on-container-exit --exit-code-from integration-test integration-test

test-stack:
	./scripts/test-stack.sh

test-failure:
	./scripts/test-failure-recovery.sh

test-store:
	test -n "$$INFERCRANE_TEST_DATABASE_URL"
	go test -race -count=1 -v ./internal/store

verify:
	./scripts/verify-repository.sh

audit:
	govulncheck ./...

deadcode:
	./scripts/check-dead-code.sh

dev-up:
	docker compose up --build -d

dev-down:
	docker compose down
