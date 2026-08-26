# Contributing to InferCrane

Start with [docs/index.mdx](docs/index.mdx), [docs/compatibility.md](docs/compatibility.md), and
the relevant feature documentation. Implementation and tests are authoritative when prose and
behavior disagree.

## Engineering contract

InferCrane is a production-oriented Go control plane and OpenAI-compatible gateway. PostgreSQL is
the source of truth for control-plane state, but the inference request path must never query it.
Each gateway owns its loopback router processes, streaming responses have no server write timeout,
schema migrations are forward-only and immutable after release, and production secrets never have
defaults or enter provider metadata. Development fakes remain under `internal/testtools` and are
excluded from production images.

Behavior changes require tests and relevant feature documentation. Architecture, security,
storage, and dependency changes must update the authoritative architecture, security, ownership,
or integration page in the same pull request. Remove replaced code in the same change and validate
new configuration before review.

## Development

```bash
asdf install
go mod download
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
- Architecture, security, ownership, and integration documentation reflects the resulting design.
- Migrations are forward-only, immutable after merge, and safe for rolling deployment.
- New dependencies have a clear purpose, compatible license, and security review.
- Removed behavior has no remaining references, configuration, documentation, or deployment assets.
- Every commit includes a `Signed-off-by` line certifying the
  [Developer Certificate of Origin](https://developercertificate.org/).

Generated artifacts are review aids, not substitutes for understanding the implementation.

Sign commits with:

```bash
git commit -s
```

By contributing, you agree that your contribution is licensed under the repository's MIT License.
