# System invariants

Changes that violate an invariant require maintainer review, an explicit migration and rollback
plan, and an update to this document in the same pull request.

## Data plane

- No PostgreSQL read occurs while routing an inference request.
- Authentication and aliases are resolved from immutable tenant-scoped in-memory snapshots.
- A published route is immutable; updates replace snapshots atomically.
- The gateway rewrites only the model alias and preserves streaming semantics. One addition is
  allowed: on a streamed chat or completions request to an external (closed API) route, it asks for
  token usage with `stream_options.include_usage` and, when the caller did not ask for usage,
  removes the usage-only chunk that adds, so the caller receives the stream it asked for.
- Server write timeouts remain disabled for long-lived inference streams.
- Retries are not duplicated between gateway and router.
- A control-plane route may hand a principal it has already authenticated to the gateway
  (`POST /api/v1/playground/chat/completions`). Authentication for `/v1/*` is unchanged, and
  routing the handed-over request reads no PostgreSQL.

## Control plane

- Desired state is durable; observed state can be reconstructed.
- A loopback router endpoint is consumed only by its owning gateway instance.
- Worker membership changes create a new router generation.
- Reconciliation work is bounded and cancellation-aware.
- Durable operation work has one unexpired lease owner and bounded attempts.
- Lifecycle core does not branch on provider or runtime names; composition binds versioned adapters.
- The control plane never dials, probes, or routes to a target reached through an InferCrane agent;
  agents only dial out, and such a target's health comes from its agent's reports.
- Agent-relayed engine metrics go through the same parser and window rules as a direct scrape.
- Adapter registration, hermetic qualification, real qualification, and public support are distinct.
- A supported integration capability cites executable evidence and cannot be inferred from protocol labels.
- Custom runtime revisions contain an immutable OCI digest and bounded argv; the control plane never
  builds an image or evaluates image-provided build/probe code.
- Recommendations and overflow decisions are versioned, persisted, reproducible, and fail closed on
  missing policy-required evidence; they never use an LLM or mutate a DeploymentSpec implicitly.

## Persistence

- PostgreSQL is the only production source of truth.
- Applied migrations are immutable and forward-only.
- Migration application is serialized and transactional.
- High-volume request records have indexed access paths and bounded retention.

## Security and operations

- Production secrets have no built-in defaults and do not enter logs or provider metadata.
- Credential secrets are never stored; only cryptographic hashes are persisted.
- An agent key calls only the agent API, never other control-plane routes or the inference gateway.
- Prompts, completions, and request content never leave an agent's machine; it sends hardware,
  server details, filtered engine metrics, and content-free request shapes only.
- External input sizes, concurrency, queues, timeouts and identifiers are bounded.
- Liveness does not depend on downstream systems; readiness verifies required dependencies.
- Development fakes are excluded from the production image target.
- Performance claims require reproducible tests against the exact real runtime, router and provider combination.
