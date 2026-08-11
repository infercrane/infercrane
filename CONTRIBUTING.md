# Contributing to InferCrane

Start with [AGENTS.md](AGENTS.md) and [docs/index.mdx](docs/index.mdx). Humans and coding agents use
the same workflow and quality gates.

## Development

```bash
asdf install
go mod download
make context
make dev-check
```

Use `make dev-check` for the normal seconds-scale loop. Before opening or updating a pull request,
run `make dev-check-full` to exercise the isolated Docker stack, failure recovery, production
configuration, and documentation. Logs are retained under `.infercrane/dev-check/`.

Provider adapters must pass `make test-provider-contracts`. Real-cloud tests are explicit paid
qualification, not part of ordinary development; see [development and testing](docs/development.mdx).
Release maintainers use `make qualify-local` for machine-readable package proof and `make
qualify-rc` only on a clean frozen commit with explicit paid-resource authorization.

PostgreSQL-backed tests run when `INFERCRANE_TEST_DATABASE_URL` is set. `docker compose up -d
postgres` starts development PostgreSQL, but it is intentionally not exposed on a host port; CI
provides an isolated test service.

## Pull requests

Keep changes cohesive and explain the operational consequences. A pull request is complete when:

- Behavior is covered by race-enabled tests.
- `make verify` passes.
- CI dead-code and vulnerability analysis passes; reported code is reviewed before removal.
- Relevant feature documentation is updated.
- A new ADR records any durable architectural or security decision.
- `docs/generated/repository-map.md` is regenerated and committed.
- Migrations are forward-only, immutable after merge, and safe for rolling deployment.
- New dependencies have a clear purpose, compatible license, and security review.
- Removed behavior has no remaining references, configuration, documentation, or deployment assets.

Generated artifacts are review aids, not substitutes for understanding the implementation.
