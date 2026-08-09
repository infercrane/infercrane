# Configuration and operations

Status: Implemented baseline; environment qualification required

## Entry points

- Configuration: `internal/config/config.go`
- Process lifecycle: `cmd/infercrane/main.go`
- Container targets: `Dockerfile`
- Development stack: `compose.yaml`
- Kubernetes baseline: `deploy/kubernetes.yaml`
- Runbook: `docs/production.md`
- Lifecycle operations: `internal/store/lifecycle.go`
- Leased operation worker: `internal/operations`
- Versioned control-plane API: `internal/controlapi`
- Autoscaling policy engine: `internal/autoscale`
- Capacity placement and adapter contracts: `internal/capacity`
- Authorization policy: `internal/authz`
- Provider pricing contract: `internal/pricing`

Configuration is environment-driven, validated at startup, and has no production API-key default.
The production image contains InferCrane and the pinned real vLLM Router. The `development` target
adds fake workers/router and bootstrap script.

`/livez` reports process liveness without downstream dependency. `/readyz` performs a bounded
PostgreSQL check. `/metrics` exposes Prometheus text telemetry. Graceful shutdown must be allowed to
drain streams and buffered accounting within the configured timeout.

Any change to probes, timeouts, pool sizing, retention, security context, image contents, or rollout
behavior must update [production operations](../production.md).

Deploy/apply creates a durable operation record. Idempotency keys prevent duplicate mutations;
progress, retryability, and cancellation requests survive process restarts. Cancellation is
cooperative and checked at safe provider boundaries. Deletion requires `--yes`, supports a
side-effect-free `--plan`, and provisioned targets without an active deployment appear in
`infercrane orphans`.

The autoscaling evaluator and controller enforce bounds, stability windows, and cooldowns and
record every decision. No production fleet scaler is enabled yet.

Queued work uses PostgreSQL leases and `SKIP LOCKED` so concurrent replicas cannot own the same
operation. The durable state model distinguishes pending, leased, running, waiting, cancelling,
cancelled, failed, and succeeded work. Every claim increments a lease generation; heartbeat,
checkpoint, failure, cancellation, and completion writes require the matching owner and generation
while the lease is unexpired. This fencing prevents a stale worker from publishing progress or a
terminal result after ownership changes.

Retryable work enters `waiting` with bounded exponential backoff and jitter. Resumable handlers can
persist named JSON checkpoints; each checkpoint emits an ordered structured operation event.
Cancellation is cooperative, and a cancelling operation whose worker dies is reclaimed only to
finish cancellation safely. Existing-target apply, cloud convergence, and cloud deletion are
executed by this leased worker. The deploy/apply/delete CLI commands submit through the control API;
terminating the CLI does not terminate lifecycle execution.

Cloud submission has an atomic storage primitive that creates the targetless desired deployment
and its queued converge operation in one PostgreSQL transaction. Its required idempotency key makes
submission retries return the original deployment and operation. `POST /api/v1/deployments` uses
this primitive and returns the durable operation immediately; the leased worker, not the request
handler, owns cloud execution. CLI migration and durable deletion remain in progress.

Queued operations with no side effects may be cancelled immediately. Once work has entered a
retry/wait cycle, cancellation is itself leased work: the worker runs the operation-specific
`.cancel` cleanup handler before publishing `cancelled`. Provider cleanup failures remain in
`cancelling` and are retried, so cancellation cannot silently orphan a billable resource.

A PostgreSQL partial unique index serializes lifecycle mutations by tenant and deployment name.
Only one pending, leased, running, waiting, or cancelling deployment operation may exist at a time;
competing requests receive a conflict and can retry after the active transition becomes terminal.

Scoped credentials are stored as hashes, can be rotated or revoked, and carry viewer, operator, or
admin role. Replicas refresh an in-memory credential snapshot every second, so authentication never
reads PostgreSQL on the inference request path. API resources and inference aliases are tenant-qualified. Admin audit queries support a
bounded RFC3339 `before` cursor. Request-rate quota enforcement is not yet distributed.
