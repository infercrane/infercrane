#!/usr/bin/env sh
set -eu

unformatted=$(gofmt -l cmd internal tools)
if [ -n "$unformatted" ]; then
  echo "Go files require gofmt:" >&2
  echo "$unformatted" >&2
  exit 1
fi

go mod verify
./scripts/check-repository-hygiene.sh
./scripts/check-license-boundaries.sh
go run ./tools/openapi-codegen -check
go test -race -count=1 ./...
./scripts/test-automation-clients.sh quick
go vet ./...
go build ./cmd/infercrane
release_version=$(jq -r '.version' .release/version.json)
grep -Fq "ARG INFERCRANE_VERSION=$release_version" Dockerfile
grep -Fq 'main.version=${version}' Dockerfile
grep -Fq 'INFERCRANE_VERSION=${{ github.ref_name }}' .github/workflows/release.yml
docker compose config --quiet
./scripts/test-runtime-image-contract.sh
