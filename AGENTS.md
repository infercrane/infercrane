# InferCrane engineering contract

This file is the entry point for coding agents and engineers. Keep it concise. Detailed and
authoritative knowledge is routed through [docs/index.mdx](docs/index.mdx).

## Product

InferCrane is a production-oriented Go control plane and OpenAI-compatible gateway for vLLM.
PostgreSQL is the source of truth. Each gateway replica supervises instance-local upstream
vLLM Router processes. Development fakes are never production substitutes.

## Before changing code

1. Read [docs/index.mdx](docs/index.mdx).
2. Read [docs/project-status.md](docs/project-status.md) to distinguish implemented,
   experimental, and planned behavior.
3. Read only the relevant feature document and ADRs it links.
4. Inspect the implementation and tests; documentation never overrides executable behavior.
5. Check [docs/generated/repository-map.md](docs/generated/repository-map.md) for entry points.

## Non-negotiable invariants

The complete list is in [docs/architecture/invariants.md](docs/architecture/invariants.md).

- The inference data path never performs a PostgreSQL lookup.
- Loopback router endpoints are owned by exactly one gateway instance.
- Streaming responses must not be subject to a server write timeout.
- Schema changes use forward-only embedded migrations; deployed migrations are immutable.
- Secrets have no production defaults and are never persisted in provider metadata.
- Development fakes stay under `internal/testtools` and outside the production image target.

## Change protocol

- Behavior changes require tests and an update to the relevant `docs/features/*.md` file.
- Architecture, security-boundary, storage, or dependency decisions require a new ADR. Never
  rewrite an accepted ADR to make history look different; supersede it.
- New configuration must be validated, documented, and represented in the generated map.
- New packages need a clear owner in [docs/ownership.md](docs/ownership.md) and must not create
  import cycles or bypass existing interfaces.
- Remove replaced code in the same change. Verify references with `rg` and generated inventory.
- Run `make context` after structural, endpoint, configuration, migration, or test changes.
- Run `make verify` before handoff. PostgreSQL integration requires
  `INFERCRANE_TEST_DATABASE_URL`; CI always supplies it.

## Engineering standards

- Prefer the Go standard library and small explicit interfaces at process/network boundaries.
- Optimize measured hot paths; document benchmark conditions and never infer production
  performance from development fakes.
- Wrap errors with operation context; preserve sentinel errors for callers.
- Bound concurrency, queues, request bodies, identifiers, retries, and external-call timeouts.
- Treat observability, graceful shutdown, migration safety, and failure recovery as feature work.
- Keep generated files deterministic. Do not hand-edit `docs/generated/*`.

## Commands

```bash
make context          # regenerate factual repository inventory
make test             # race-enabled tests; store tests skip without test PostgreSQL
make verify           # format check, generated-doc check, tests, vet, and build
make dev-check        # fast repository and provider-contract feedback
make dev-check-full   # isolated Docker, failure, production-config, and docs gates
make qualify-local    # resumable local release/package proof and JSON manifest
make deadcode         # reject unreachable functions with the pinned deadcode tool
make dev-up           # development PostgreSQL, fake workers, and fake router
make dev-down         # stop development services without deleting data
```

If this contract becomes larger than roughly 150 lines, move detail into `docs/` and keep this
file as a router. Add nested `AGENTS.md` files only when a subsystem has genuinely different
commands or constraints; nested files must link back here and may not contradict root policy.
