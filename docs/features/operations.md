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
finish cancellation safely. Existing-target apply is executed by this leased worker. Direct CLI
apply remains available for administrative recovery. Cloud provisioning and deletion still execute
synchronously and must move to checkpointed handlers before all mutations are crash-recoverable.

A PostgreSQL partial unique index serializes lifecycle mutations by tenant and deployment name.
Only one pending, leased, running, waiting, or cancelling deployment operation may exist at a time;
competing requests receive a conflict and can retry after the active transition becomes terminal.

Scoped credentials are stored as hashes, can be rotated or revoked, and carry viewer, operator, or
admin role. Replicas refresh an in-memory credential snapshot every second, so authentication never
reads PostgreSQL on the inference request path. API resources and inference aliases are tenant-qualified. Admin audit queries support a
bounded RFC3339 `before` cursor. Request-rate quota enforcement is not yet distributed.
