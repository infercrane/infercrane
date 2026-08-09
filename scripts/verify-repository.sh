#!/usr/bin/env sh
set -eu

unformatted=$(gofmt -l cmd internal tools)
if [ -n "$unformatted" ]; then
  echo "Go files require gofmt:" >&2
  echo "$unformatted" >&2
  exit 1
fi

go mod verify
go run ./tools/repo-context -check
go test -race -count=1 ./...
go vet ./...
go build ./cmd/infercrane
docker compose config --quiet
