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

## Hugging Face model search

1. An authenticated principal with read permission calls `GET /api/v1/catalog/hugging-face/search`.
2. The control API validates `query` and `limit`, then answers from a bounded in-memory cache when
   the same query and limit were answered in the last 10 minutes.
3. Otherwise it sends two unauthenticated HTTPS requests to the public Hugging Face models API, one
   per pipeline, under one 4-second deadline and an 8-search concurrency bound. Redirects are not
   followed.
4. Results are merged, marked `reviewed: false`, cached on success, and returned.

The route reads no tenant state, never touches PostgreSQL, and is not on the inference request path.
Results do not enter the reviewed catalog, the checkpoint catalog, or optimization proposals.

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

## GPU profile capture

1. A collector posts one bounded `infercrane.gpu-profile/v1` capture to
   `POST /api/v1/deployments/{name}/gpu-profiles` with deploy permission. The control API rejects
   unknown fields, bodies over 8 MiB, captures that claim content, and anything but measured
   evidence before touching PostgreSQL.
2. In one transaction the store resolves the deployment's active revision, inserts the capture into
   `gpu_profiles` as one JSONB document keyed by tenant, deployment, revision, tool, start, and end,
   and removes all but the newest eight captures for that deployment. A retry with the same samples
   returns the stored receipt; a retry with different samples is a conflict.
3. `GET /api/v1/endpoints/{name}/gpu-profile` reads the newest capture whose revision is still the
   active revision of a deployment bound to the endpoint. None is a typed 404.
4. Neither step runs on the inference path, and the gateway never reads `gpu_profiles`.

## Optimization run notifications

1. A workspace admin adds a destination with `POST /api/v1/notifications/subscriptions`. The
   control API seals the destination URL and a generated signing secret with
   `INFERCRANE_CREDENTIAL_ENCRYPTION_KEY` (AES-256-GCM bound to tenant and destination ID) and
   stores them in `notification_subscriptions`. Only a host-level hint is stored in the clear.
2. Every candidate transition that changes a campaign's aggregate state also records the change in
   `optimization_campaign_transitions`, in the same transaction as the state update. A failed
   campaign keeps the first candidate failure code in `optimization_campaigns.failure_code`.
   Cleanup rewrites the aggregate state seconds after a decision; these rows keep the decision
   readable.
3. Every 15 seconds each control-plane replica derives events for workspaces with an enabled
   destination: waiting states from campaigns updated in the last 24 hours, and decisions, failures,
   and rollouts from the recorded transitions. A decision reads the outcome report. Each new event
   becomes one `notification_deliveries` row per destination, holding the exact body;
   `UNIQUE(subscription_id, event_id)` makes a second replica's insert a no-op.
4. The same pass leases due rows with `FOR UPDATE SKIP LOCKED`, decrypts the destination in memory,
   and sends one signed POST through `internal/alert`'s public-network-checked client. Retries back
   off from 30 seconds to an hour, at most six attempts; a 4xx other than 429 stops at once.
5. None of this runs on the inference path, and the gateway never reads these tables.

## Agent sync

1. `infercrane agent run` probes local servers (`/v1/models`, version and loaded-model endpoints,
   `/metrics`) and reads the host's accelerators. Nothing listens for the control plane.
2. Each new healthy server is registered with `POST /api/v1/agent/servers`: one observe-only
   endpoint whose target carries the agent's ID and the agent-local server key.
3. Every poll interval the agent sends `POST /api/v1/agent/sync` (gzip JSON): hardware, servers and
   health, one metric relay per server with a new scrape (the current scrape and the oldest scrape of
   the same engine process in the trailing five minutes, both filtered on the machine), and request
   shapes from the optional local gateway.
4. The control plane re-checks that the agent is active, validates each item on its own (refused
   items come back in `rejected`), records the heartbeat, sets each registered server's health, parses both scrapes with `metrics.ParseEngine`, derives rows with
   `metrics.EngineMeasurements`, and stores them in `target_measurements` keyed by the target.
   Request shapes become `request_records` with the endpoint's identities and compute mode
   `agent-gateway`; shape IDs make retries idempotent and `sent_at` corrects clock skew.
5. Endpoint monitoring and optimization readiness read deployment-bound and target-bound
   measurements through the endpoint's bindings. The reconciler never probes an agent's target and
   marks it unhealthy when its agent has been silent for three minutes.
6. Revocation disables the agent's principal; the next sync is refused with HTTP 401
   (`agent_revoked`, or `agent_unauthenticated` once the credential snapshot has dropped the key),
   and the agent stops.
