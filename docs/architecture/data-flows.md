# Data flows

## Inference request

1. The gateway authenticates from a periodically refreshed hashed-credential snapshot and bounds
   the request body and request ID.
2. It reads the alias from the atomic in-memory route directory; PostgreSQL is not queried.
3. It rewrites the alias to the common upstream model and forwards to the local vLLM Router.
4. Response headers and bytes are streamed back; SSE chunks are flushed incrementally.
5. Accounting is queued after completion and persisted asynchronously with bounded capacity.

## Asynchronous apply

1. An operator submits tenant-scoped desired state with an idempotency key.
2. PostgreSQL returns the existing operation or stores a new pending operation.
3. One replica claims it with `SKIP LOCKED`, records a lease, and heartbeats during execution.
4. The handler validates tenant-owned targets and atomically converges the deployment.
5. Completion and audit state are persisted. An expired lease can be recovered by another replica;
   retryable failures return to pending with bounded exponential delay.

## Reconciliation

1. A replica loads active deployments and their targets from PostgreSQL.
2. Worker health/model endpoints are probed concurrently under a fixed bound.
3. Healthy membership and routing policy produce a deterministic worker-set hash.
4. If local state differs, the replica replaces its supervised router process and records an
   instance-owned generation.
5. A complete route snapshot is published atomically; the previous valid snapshot survives
   temporary database or reconciliation failures.

## Schema startup

1. The process connects to PostgreSQL with a bounded pool.
2. One connection obtains the migration advisory lock.
3. Unapplied embedded migrations execute transactionally in filename order.
4. The migration version is recorded in the same transaction.
5. The lock is released before serving commands or traffic.

## Provisioning

SkyPilot is invoked across a process boundary. Secrets are provided using SkyPilot secret
injection, not persisted metadata. Provisioning returns a target that follows the same runtime
and reconciliation path as an externally registered worker.
