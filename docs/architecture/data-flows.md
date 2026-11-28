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
2. Worker health/model endpoints are probed concurrently under a fixed bound. A healthy worker
   keeps its place through one probe that fails or times out, recorded as a
   `replica_probe_missed` event, and leaves when a second probe in a row fails: a probe can time
   out while the server admits a burst and serves every running request, and withdrawing the
   route then turns away every new request until the next pass. A worker that answers without
   its model, a new or unhealthy worker, and a worker whose probe cannot run (no runtime backend,
   no credential evidence) get no such grace. The count lives in each control-plane process. The
   cost: a worker that has died keeps its route up to one more pass, and requests sent to it then
   fail as upstream errors instead of being refused for want of a route.
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
3. Otherwise it sends three unauthenticated HTTPS requests to the public Hugging Face models API,
   one per pipeline (`text-generation`, `image-text-to-text`, `feature-extraction`), under one
   4-second deadline and an 8-search concurrency bound. Redirects are not followed.
4. Results are merged, marked `reviewed: false`, cached on success, and returned.

The route reads no tenant state, never touches PostgreSQL, and is not on the inference request path.
Results do not enter the reviewed catalog, the checkpoint catalog, or optimization proposals.

## Automatic model check

Only when the operator set `INFERCRANE_HUGGING_FACE_METADATA_ENABLED=true`:

1. A planning request (`POST /api/v1/planning/intents`), a check (`GET
   /api/v1/catalog/hugging-face/check`), or an optimization proposal names a Hugging Face repository
   that is not a reviewed model; or a deployment (`POST /api/v1/deployments`) of any Hugging Face
   repository on a built-in runtime sets no engine arguments, and InferCrane sizes its context
   length.
2. When a `model_secret_reference_id` is named, the control API loads that tenant's secret reference
   and resolves the token through the configured secret resolver.
3. `internal/modelcheck` pins the revision to a commit with one model-info request, then reads
   `config.json` at that commit: two HTTPS requests under one 8-second deadline and an 8-request
   concurrency bound, redirects followed only on the same host. Results are cached in memory by
   commit; reads made with a token are cached under the tenant and credential.
4. The check compares the architecture with the committed runtime registry snapshot, sizes the GPU
   from the price catalog already in memory, and returns. For a deployment it sizes one request on
   the chosen GPU instead and, when the model's own maximum context does not fit, adds a context
   length to the deployment's engine arguments before the deployment is persisted. A failed read
   never fails the deployment; it keeps the model's own context and says why. For a proposal
   planned from an endpoint diagnosis, it also adds the served weights' bytes and the KV cache bytes
   per token to that diagnosis, from which the planner predicts the cache room of precision changes.
   The planner itself never reads the network.

Nothing is written to PostgreSQL, and the inference request path is not involved.

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

1. `infercrane profile` runs where the serving engine's HTTP port and trace directory are reachable.
   It calls the engine's `/start_profile`, waits at most 30 seconds, calls `/stop_profile` (always,
   even when interrupted), and reads the Kineto trace the engine wrote. The parser keeps kernel,
   operator, and runtime-call names, times, devices, streams, correlations, and matrix-multiply shapes,
   and skips Python frames, operator arguments, and every annotation other than engine-step markers.
   `internal/gputrace` analyzes the trace on that host; the full trace never leaves it.
2. The CLI posts one bounded `infercrane.gpu-profile/v2` recording (analysis, call stacks, and an
   excerpt of at most 20,000 events) to `POST /api/v1/deployments/{name}/gpu-profiles` with deploy
   permission. Other collectors may post a call-stack-only v1 recording. The control API rejects
   unknown fields, bodies over 8 MiB, out-of-range analyses, captures that claim content, and anything
   but measured evidence before touching PostgreSQL.
3. In one transaction the store resolves the deployment's active revision, inserts the capture into
   `gpu_profiles` as one JSONB document keyed by tenant, deployment, revision, tool, start, and end,
   and removes captures that ended more than 30 days ago and all but the newest eight for that
   deployment. A retry with the same samples returns the stored receipt; a retry with different
   samples is a conflict.
4. `GET /api/v1/endpoints/{name}/gpu-profile` reads the newest capture whose revision is still the
   active revision of a deployment bound to the endpoint; `GET …/gpu-profiles` lists every retained
   capture of those deployments, `GET …/gpu-profiles/{id}` reads one, and `GET …/gpu-profiles/compare`
   compares two analyzed captures. None is a typed 404.
5. A proposal planned from an endpoint diagnosis reads the newest capture's kernel-class shares.
   Neither capture nor read runs on the inference path, and the gateway never reads `gpu_profiles`.

## Results scrape

1. Prometheus calls `GET /api/v1/metrics` with an API key that has read permission. The control API
   authenticates it like any other route and reads only the key's workspace.
2. `store.ResultMetrics` runs bounded, tenant-scoped reads: deployments and endpoints (500 each, by
   name), the latest and counted `release_guard_evaluations` and `endpoint_release_guard_evaluations`,
   the latest `release_guard_monitors` row, rollback events in `deployment_events`, the active
   serving plan's bindings and weights, and the latest optimization run per targeted endpoint.
   Counters are counts of stored rows, so every replica returns the same value and a restart never
   lowers one.
3. For at most four runs per scrape, the control API assembles the run's report to read its outcome
   and cost per million output tokens, and caches the result in memory by run and last change.
4. `internal/resultmetrics` writes the Prometheus text format, or JSON when `Accept` is
   `application/json`. Unknown values have no series. None of this runs on the inference path, and
   the gateway's own `/metrics` carries no workspace names.

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

## Agent data plane

1. A key with `deploy` makes an agent's endpoint agent-managed
   (`PUT /api/v1/adoptions/endpoints/{name}/ownership`). In one transaction the adoption and its
   binding become `agent-managed` and the endpoint's residency `customer-cloud`. The reconciler
   removes any hosted route for the endpoint, marks it resident in the route directory, and sets
   its state from the agent's health reports.
2. Every poll interval the agent (`infercrane agent run --serve`) calls
   `GET /api/v1/agent/data-plane?generation=`. The relay reads the agent's agent-managed active-plan
   bindings, the tenant's live non-agent credential digests narrowed to keys that may call those
   endpoints, their admission policies, and the request quota, digests the content, and answers
   with the snapshot, or `unchanged` and a new `valid_until`.
3. The agent validates the snapshot, maps each binding's server key to a server it detected and
   sees healthy, and publishes the routes in its own in-memory directory. Its quota pool leases
   requests with `POST /api/v1/agent/quota-leases`, which uses the same minute-window allocator as
   the hosted gateway.
4. An application calls the agent with an InferCrane API key. The agent's gateway (the code of
   `internal/gateway`) authenticates against the digests, applies endpoint restrictions,
   admission, and quota, routes, streams, and records the request in memory. No database is read.
5. The next sync carries each request as a shape with `source: data-plane` and its binding. The
   relay checks the binding belongs to an agent-managed server of this agent and stores a request
   record with compute mode `agent-data-plane`.
6. The hosted gateway answers a request for the endpoint with `403 residency_error` and forwards
   nothing; asynchronous inference refuses it before storing anything.

## Agent candidate run

1. A key with `deploy` calls `POST /api/v1/agents/{id}/candidate-runs`. The relay validates the
   spec, resolves the endpoint to the agent's server key, and stores a queued `agent_commands` row
   (at most 16 open per agent).
2. The agent's long poll (`GET /api/v1/agent/commands?wait=25s`) leases it with
   `FOR UPDATE SKIP LOCKED`; a replica checks every 2 seconds and answers at once when the command
   was queued through it. Commands past their deadline expire; a lease that lapses three times
   fails the command. If the agent goes away before the answer is written, or the answer cannot be
   written, the lease is undone at once: the next poll takes the command, and the lost attempt is
   not counted.
3. The agent checks the image and accelerator against its allowlists, applies one Deployment and
   one Service with `provision.Kubernetes` in its namespace, waits for readiness, runs AIPerf
   against the current setup, the candidate, and the current setup again, and runs the answer check
   on both, all inside the cluster. It reports each phase with `POST /api/v1/agent/commands/{id}`,
   which extends the lease and tells it when the run was cancelled.
4. The agent deletes the candidate and posts the result: AIPerf summaries, the noise figure,
   per-prompt agreement scores and digests, readiness time, and cleanup. Nothing else is sent.

## Measured results and their ranking

1. `infercrane results import` reads a record bundle, JSON Lines of records, or a planner-gym cases
   file on the caller's machine, and seals each record (`internal/resultstore`): its ID is derived
   from its content, and its fields are numbers and bounded names, never prompts or answers. With
   `--store FILE` the records go to a local JSON Lines file and nothing else is contacted.
2. Otherwise the CLI posts at most 2,000 sealed records at a time to `POST
   /api/v1/optimization/results` with deploy permission. The control API rejects unknown fields,
   bodies over 8 MiB, and any record whose ID does not match its content, and sets every record's
   sharing to the request's (`private` unless `numbers` was asked for).
3. The store inserts each record into `result_records`, keyed by tenant and record ID, in one
   transaction. A record already there is kept and takes the new sharing. A trigger refuses any
   other change to a stored record. The row's `claim` column is the claim when it was written; reads
   decide it again with the technique contract's rule (`Record.Claim` calls `Result.Claim`).
4. `GET /api/v1/optimization/results` and `GET …/results/{id}` read the caller's own rows only.
5. `POST /api/v1/optimization/results/rank` reads the caller's rows and other tenants' rows whose
   sharing is `numbers`; the store reduces each of those to its numbers (`Record.NumbersOnly`)
   before it leaves the store, and the ranking names such a cell only by those numbers. The prior
   (`internal/resultprior`) is deterministic. Neither the gateway nor the inference path reads
   `result_records`.

