# System invariants

Changes that violate an invariant require an explicit superseding ADR and coordinated migration.

## Data plane

- No PostgreSQL read occurs while routing an inference request.
- Authentication and aliases are resolved from immutable tenant-scoped in-memory snapshots.
- A published route is immutable; updates replace snapshots atomically.
- The gateway rewrites only the model alias and preserves streaming semantics.
- Server write timeouts remain disabled for long-lived inference streams.
- Retries are not duplicated between gateway and router.

## Control plane

- Desired state is durable; observed state can be reconstructed.
- A loopback router endpoint is consumed only by its owning gateway instance.
- Worker membership changes create a new router generation.
- Reconciliation work is bounded and cancellation-aware.
- Durable operation work has one unexpired lease owner and bounded attempts.
- Lifecycle core does not branch on provider or runtime names; composition binds versioned adapters.
- Adapter registration, hermetic qualification, real qualification, and public support are distinct.
- A supported integration capability cites executable evidence and cannot be inferred from protocol labels.

## Persistence

- PostgreSQL is the only production source of truth.
- Applied migrations are immutable and forward-only.
- Migration application is serialized and transactional.
- High-volume request records have indexed access paths and bounded retention.

## Security and operations

- Production secrets have no built-in defaults and do not enter logs or provider metadata.
- Credential secrets are never stored; only cryptographic hashes are persisted.
- External input sizes, concurrency, queues, timeouts and identifiers are bounded.
- Liveness does not depend on downstream systems; readiness verifies required dependencies.
- Development fakes are excluded from the production image target.
- Performance claims require reproducible tests against real vLLM and real vLLM Router.
