# Contributing to InferCrane

Start with [AGENTS.md](AGENTS.md) and [docs/index.mdx](docs/index.mdx). Humans and coding agents use
the same workflow and quality gates.

## Development

```bash
asdf install
go mod download
make context
make verify
```

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
