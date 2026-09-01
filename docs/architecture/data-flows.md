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

The durable workflow resolves a registered provider backend by cloud, runtime, and persisted adapter
identity. RunPod elastic uses the native Pods adapter by default. Explicit SkyPilot manifests may
add long-tail execution targets, but do not supply InferCrane's price authority. AWS EC2 delegates
API compatibility and STS to AWS CLI v2 while InferCrane retains idempotency, adoption,
private-network, tag, and deletion policy.
Both return a target that follows the same runtime and reconciliation path as an existing worker.

## Governed external fallback

1. Reconciliation excludes policy-owned external targets from ordinary primary membership.
2. Only when no primary is healthy, the coordinator loads an enabled, privacy-acknowledged policy,
   resolves its reference-only credential in memory, and checks the exact model mapping.
3. A bounded request/cost batch is atomically reserved in PostgreSQL and prefetched into memory.
4. The gateway authorizes each request from that in-memory lease before transmission; PostgreSQL
   never enters the inference path.
5. The selected target and any denial are persisted in bounded request evidence. Requests are not
   replayed or duplicated after a possible send.
