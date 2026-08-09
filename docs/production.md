# Production operations

InferCrane is designed to run as multiple stateless gateway replicas backed by PostgreSQL.
Each replica has a stable `INFERCRANE_INSTANCE_ID` and supervises its own loopback vLLM Router
processes. Router generations are scoped by instance, so one replica never publishes another
replica's loopback endpoint.

Required configuration:

- `INFERCRANE_ENV=production`: enables production security validation.
- `INFERCRANE_DATABASE_URL`: PostgreSQL URL with TLS enabled outside a trusted private network.
- `INFERCRANE_API_KEY`: at least 32 characters, supplied by the workload secret manager; no production default exists.
- `INFERCRANE_URL`: absolute HTTP(S) control-plane base URL used by lifecycle CLI commands.
- `RUNPOD_API_KEY`: optional RunPod secret. The container entrypoint writes the RunPod CLI's
  required user-scoped configuration with mode `0600` before starting InferCrane; it never logs the
  value.
- `RUNPOD_API_KEY_FILE`: preferred alternative containing a mounted RunPod secret. It takes effect
  when `RUNPOD_API_KEY` is unset, avoiding secret exposure in container environment metadata.
The container user must have read permission on the mounted file.

The real RunPod qualification stack is isolated from the development Compose stack. It persists
PostgreSQL, RunPod configuration, and SkyPilot state across control-plane restarts:

```sh
export RUNPOD_KEY_FILE=/absolute/path/to/runpod-key
docker compose -p infercrane-runpod -f compose.runpod-acceptance.yaml up --build -d
```

This command starts the control plane but does not provision a GPU. GPU creation only begins after
a cloud deployment is submitted.

The production image includes the local `git`, OpenSSH, and `rsync` executables required by
SkyPilot's task packaging and remote synchronization paths.
When running `infercrane serve`, its entrypoint also supervises SkyPilot's foreground API server;
the container exits if that required subprocess stops.
- `INFERCRANE_INSTANCE_ID`: stable and unique per control-plane replica.
- `INFERCRANE_DATABASE_MAX_OPEN` and `INFERCRANE_DATABASE_MAX_IDLE`: size these with the total
  replica count below PostgreSQL's connection budget or place PgBouncer in transaction mode.
- `INFERCRANE_REQUEST_RETENTION_HOURS`: bounds high-volume request-accounting storage; the
  default is 24 hours and cleanup runs in small batches.

Startup applies embedded migrations transactionally under a PostgreSQL advisory lock. Back up
the database before deploying a release that contains a new migration. Roll forward after a
migration; do not run mixed application versions unless the release notes explicitly allow it.

Health and telemetry endpoints:

- `/livez`: process liveness; it does not depend on downstream services.
- `/readyz`: PostgreSQL connectivity with a bounded timeout.
- `/metrics`: Prometheus-format gateway request, failure, active request, byte, duration histogram,
  and operation claim, completion, failure, retry, and cancellation counters.

Import [the baseline Prometheus alert rules](https://github.com/infercrane/infercrane/blob/main/deploy/prometheus-rules.yaml), then tune thresholds
from real model latency and traffic. Follow the [compatibility policy](compatibility.md) and perform
the [backup/restore drill](runbooks/backup-restore.md) for every release containing migrations.

Use a disruption budget, topology spread constraints, anti-affinity across failure domains, and
at least two replicas. Termination grace must exceed `INFERCRANE_SHUTDOWN_TIMEOUT_SECONDS` so
streaming requests and buffered request accounting can drain. Do not configure an HTTP server
write timeout: it would terminate legitimate long-running streaming inference responses.

The production image includes InferCrane, the pinned upstream vLLM Router, and the pinned SkyPilot
RunPod client used by durable provider workers. Development
workers and the simple development router exist only in the `development` image target and are
not performance or reliability substitutes for vLLM and vLLM Router.

Before rollout, qualify the exact PostgreSQL, vLLM, vLLM Router, model, GPU, and provider versions
with sustained load, streaming cancellation, worker loss, database failover, pod termination,
and soak tests. Capacity limits must be based on those measurements rather than defaults.

The control-plane API accepts the bootstrap bearer secret or hashed tenant-scoped credentials:

- `GET /api/v1/operations/{id}`
- `POST /api/v1/operations/{id}/cancel`
- `POST /api/v1/deployments/apply`
- `POST /api/v1/deployments`
- `DELETE /api/v1/deployments/{name}`
- `GET /api/v1/deployments`
- `GET|POST /api/v1/targets`
- `GET /api/v1/orphans`
- `GET /api/v1/audit-events`
- `PUT /api/v1/tenant/quota`
- principal creation, rotation, and revocation endpoints

Do not expose it publicly without TLS and network policy. Store the bootstrap key as a restricted
break-glass credential and use scoped principals for normal automation. Distributed request-rate
quota enforcement and adversarial tenant-isolation qualification remain release blockers.
