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
operation. Workers heartbeat leases, recover expired work, cap retry attempts, and delay retries
exponentially. Existing-target apply can be queued through the API and is executed by a leased
worker. Direct CLI apply remains available for administrative recovery. Cloud provisioning and
deletion still execute synchronously and must move to handlers before all mutations are
crash-recoverable.

Scoped credentials are stored as hashes, can be rotated or revoked, and carry viewer, operator, or
admin role. Replicas refresh an in-memory credential snapshot every second, so authentication never
reads PostgreSQL on the inference request path. API resources and inference aliases are tenant-qualified. Admin audit queries support a
bounded RFC3339 `before` cursor. Request-rate quota enforcement is not yet distributed.
