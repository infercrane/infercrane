# Configuration and operations

Status: Implemented baseline; environment qualification required

## Entry points

- Configuration: `internal/config/config.go`
- Process lifecycle: `cmd/infercrane/main.go`
- Container targets: `Dockerfile`
- Development stack: `compose.yaml`
- Runbook: `docs/production.md`
- Lifecycle operations: `internal/store/lifecycle.go`
- Leased operation worker: `internal/operations`
- Versioned control-plane API: `internal/controlapi`
- Autoscaling policy engine: `internal/autoscale`
- Capacity placement and adapter contracts: `internal/capacity`
- Authorization policy: `internal/authz`
- Provider pricing contract: `internal/pricing`

Server configuration is environment-driven, validated at startup, and has no production API-key
default. `infercrane init` writes an already-issued client URL/auth configuration to
`$XDG_CONFIG_HOME/infercrane/config.json` (or `~/.config/infercrane/config.json`) with mode `0600`;
environment variables override that file. Public lifecycle, status, event, inspection, and
explanation commands use only the authenticated control-plane API and never open PostgreSQL.
It never generates a client-only credential or claims to register one with the control plane.
`infercrane doctor` also uses that API: dependency and optional provider checks execute in the
control-plane environment, where the corresponding binaries and credentials live.
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

The autoscaling evaluator and controller scrape vLLM running/waiting gauges, enforce bounds,
stability windows, and cooldowns, and persist every evaluation. Capacity changes are fenced by a
durable scale operation. Scale-down first withdraws the worker set and waits for a matching router
generation before provider deletion. Real RunPod scale acceptance remains a release gate.

Deployment reads expose a derived `lifecycle_status` with independent serving and convergence
states. Serving is based on the currently published healthy route. Convergence covers active
durable work plus provisioning and draining replicas. This prevents a healthy active revision from
being labelled unavailable merely because candidate or scale-up capacity is still starting. The
same object reports ready versus desired capacity, candidate state, and the exact blocking
operation without replacing raw replica, target, or operation data in `inspect`.

Queued work uses PostgreSQL leases and `SKIP LOCKED` so concurrent replicas cannot own the same
operation. The durable state model distinguishes pending, leased, running, waiting, cancelling,
cancelled, failed, and succeeded work. Every claim increments a lease generation; heartbeat,
checkpoint, failure, cancellation, and completion writes require the matching owner and generation
while the lease is unexpired. This fencing prevents a stale worker from publishing progress or a
terminal result after ownership changes.

Retryable work enters `waiting` with bounded exponential backoff and jitter. Operation progress is
persisted as a monotonic high-water mark, so replaying an idempotent checkpoint after a retry never
makes clients appear to move backwards. The terminal renders stable lifecycle phases such as
`WAITING FOR CAPACITY`, `PREPARING ARTIFACT`, and `STARTING RUNTIME`; retry counts and the next
scheduled check remain separate from progress. Resumable handlers persist named JSON checkpoints;
each checkpoint emits an ordered structured operation event.
Cancellation is cooperative, and a cancelling operation whose worker dies is reclaimed only to
finish cancellation safely. Existing-target apply, cloud convergence, and cloud deletion are
executed by this leased worker. The deploy/apply/delete CLI commands submit through the control API;
terminating the CLI does not terminate lifecycle execution. Human wait output prints the durable
operation ID and `infercrane operation watch ID` before blocking; a later terminal can resume the
same persisted operation without submitting another provider request. Progress is written to
stderr so `--output json` remains one valid document on stdout. Deployment results include the
stable OpenAI-compatible logical endpoint, model, runtime, provider, compute mode, operation ID,
and safe retry key.

Provider optimizations are exposed as normalized capabilities in `infercrane doctor`. Supported,
unsupported, and unknown are distinct states. InferCrane reports model-cache, image-streaming, or
fast-resume behavior as unknown when an adapter cannot observe it; it never infers an optimization
or silently substitutes hardware when capacity is constrained.

Cloud submission has an atomic storage primitive that creates the targetless desired deployment
and its queued converge operation in one PostgreSQL transaction. Its required idempotency key makes
submission retries return the original deployment and operation. `POST /api/v1/deployments` uses
this primitive and returns the durable operation immediately; the leased worker, not the request
handler, owns cloud execution.

Queued operations with no side effects may be cancelled immediately. Once work has entered a
retry/wait cycle, cancellation is itself leased work: the worker runs the operation-specific
`.cancel` cleanup handler before publishing `cancelled`. Provider cleanup failures remain in
`cancelling` and are retried, so cancellation cannot silently orphan a billable resource.
RunPod Serverless cleanup inventories endpoints before deletion and reconciles them against both
the persisted provider resource ID and the deterministic name derived from the replica external
key. This closes the create-response-loss window: an endpoint accepted by RunPod before a lost
response is still found, deleted, and confirmed absent before its replica or deployment is marked
deleted. Final account inventory remains a required real-provider acceptance check.

A PostgreSQL partial unique index serializes lifecycle mutations by tenant and deployment name.
Only one pending, leased, running, waiting, or cancelling deployment operation may exist at a time;
competing requests receive a conflict and can retry after the active transition becomes terminal.

Scoped credentials are stored as hashes, can be rotated or revoked, and carry viewer, operator, or
admin role. Replicas refresh an in-memory credential snapshot every second, so authentication never
reads PostgreSQL on the inference request path. API resources and inference aliases are tenant-qualified. Admin audit queries support a
bounded RFC3339 `before` cursor. Request-rate quota enforcement is not yet distributed.
