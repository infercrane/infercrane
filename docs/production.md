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
- `INFERCRANE_TLS_CERT_FILE` and `INFERCRANE_TLS_KEY_FILE`: optional native server identity. Both are
  required together. Add `INFERCRANE_TLS_CLIENT_CA_FILE` to require and verify client certificates.
- `INFERCRANE_PASSPORT_SIGNING_KEY_FILE`: optional mounted Ed25519 private-key file for issuing
  Inference Passports. It must be readable only by its owner (`0600`); the private key is never
  persisted in PostgreSQL. Back it up and rotate it through the workload secret manager.
For a single-host first installation, copy `.env.production.example` to a private path, replace
every example secret and URL, then render and start the maintained production stack:

```sh
docker compose --env-file /private/path/infercrane.env \
  -f compose.production.yaml config --quiet
docker compose --env-file /private/path/infercrane.env \
  -f compose.production.yaml up -d
```

Unlike `compose.yaml`, this stack contains no fake workers or development router. Unlike
`compose.runpod-acceptance.yaml`, it contains no fault proxy or acceptance credential. PostgreSQL
is private to the Compose network; only the InferCrane API port is published. The bundled database
generates a persistent, self-signed server certificate and requires an encrypted
`sslmode=require` connection across that bridge. This encrypts the single-host transport but does
not provide CA-backed server identity; use managed PostgreSQL with `verify-full`, external secret
management, and multiple control-plane instances for a production service that must survive a host
failure.

The base production stack is provider-neutral: it does not require a RunPod credential and does
not start SkyPilot. Provider adapters remain dormant until a DeploymentSpec selects them. The
image contains pinned AWS CLI v2, Google Cloud CLI, and `kubectl` clients because those are the
explicit process boundaries used by the AWS, GCP, and Kubernetes adapters; credentials and
kubeconfig are always supplied by the operator at runtime.

For RunPod, add the explicit production overlay and the variables from `.env.runpod.example`:

```sh
docker compose --env-file /private/path/infercrane.env \
  -f compose.production.yaml \
  -f compose.production.runpod.yaml config --quiet
docker compose --env-file /private/path/infercrane.env \
  -f compose.production.yaml \
  -f compose.production.runpod.yaml up -d
```

The overlay mounts the RunPod key read-only and persists only the provider client's local state.
The entrypoint configures the RunPod client without printing the key and supervises SkyPilot only
when this overlay explicitly enables it. `INFERCRANE_SKYPILOT_API=disabled` is available for
diagnostics, but disables RunPod elastic provisioning from that control-plane instance.

AWS and Kubernetes follow the same explicit composition pattern:

```sh
# AWS: values from .env.aws.example and a read-only AWS config directory
docker compose --env-file /private/path/infercrane-aws.env \
  -f compose.production.yaml -f compose.production.aws.yaml up -d

# GCP: complete INFERCRANE_GCP_* values and read-only Application Default Credentials
docker compose --env-file /private/path/infercrane-gcp.env \
  -f compose.production.yaml -f compose.production.gcp.yaml up -d

# Kubernetes: values from .env.kubernetes.example and a read-only kubeconfig
docker compose --env-file /private/path/infercrane-kubernetes.env \
  -f compose.production.yaml -f compose.production.kubernetes.yaml up -d
```

The AWS overlay exposes only the complete adapter configuration and mounts the source profile
read-only; each provider call still assumes the configured role and keeps temporary STS credentials
in the child process. The GCP overlay mounts Application Default Credentials read-only. The
Kubernetes overlay mounts one kubeconfig read-only and preserves the explicit
context/namespace/RBAC boundary. Never combine overlays merely because their clients are present in
the image—enable only providers this control-plane instance is intended to operate.

The real RunPod qualification stack is isolated from the development Compose stack. It persists
PostgreSQL, RunPod configuration, and SkyPilot state across control-plane restarts:

```sh
export RUNPOD_KEY_FILE=/absolute/path/to/runpod-key
# Required only for the Serverless acceptance demo:
export INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID=immutable-vllm-template-id
docker compose -p infercrane-runpod -f compose.runpod-acceptance.yaml up --build -d
```

This command starts the control plane but does not provision a GPU or create a Serverless endpoint.
Provider mutation begins only after a deployment is submitted. The key file is mounted read-only;
the entrypoint configures SkyPilot/RunPod and passes the key only to its runtime child processes so
Serverless API calls work without declaring the secret value in Compose environment metadata.

The production image includes the local `git`, OpenSSH, and `rsync` executables required by
SkyPilot's task packaging and remote synchronization paths. When the RunPod overlay is enabled,
the entrypoint supervises SkyPilot's foreground API server and exits if that required subprocess
stops. Without that overlay, InferCrane is executed directly and has no SkyPilot subprocess.
- `INFERCRANE_INSTANCE_ID`: stable and unique per control-plane replica.
- `INFERCRANE_DATABASE_MAX_OPEN` and `INFERCRANE_DATABASE_MAX_IDLE`: size these with the total
  replica count below PostgreSQL's connection budget or place PgBouncer in transaction mode.
- `INFERCRANE_REQUEST_RETENTION_HOURS`: bounds high-volume request-accounting storage; the
  default is 24 hours and cleanup runs in small batches.

Startup applies embedded migrations transactionally under a PostgreSQL advisory lock. The ledger
records migration SHA-256 checksums and startup rejects modified, gapped, or newer unknown histories.
Back up the database before deploying a release that contains a new migration. Roll forward after a
migration; do not run mixed application versions unless the release notes explicitly allow it. See
[Upgrade and compatibility](/upgrade).

Health and telemetry endpoints:

- `/livez`: process liveness; it does not depend on downstream services.
- `/readyz`: PostgreSQL connectivity with a bounded timeout.
- `/metrics`: Prometheus-format gateway request, failure, active request, byte, duration histogram,
  and operation claim, completion, failure, retry, and cancellation counters.

Import the baseline rules at `deploy/prometheus-rules.yaml` from the private-preview repository, then tune thresholds
from real model latency and traffic. Follow the [compatibility policy](compatibility.md) and perform
the [backup/restore drill](runbooks/backup-restore.md) for every release containing migrations.

Use a disruption budget, topology spread constraints, anti-affinity across failure domains, and
at least two replicas. Termination grace must exceed `INFERCRANE_SHUTDOWN_TIMEOUT_SECONDS` so
streaming requests and buffered request accounting can drain. Do not configure an HTTP server
write timeout: it would terminate legitimate long-running streaming inference responses.

Each replica publishes its live binary and protocol interval. Use
`infercrane system instances --output json` before and during a rolling upgrade. Membership does not
elect a leader: durable operation claims are independently fenced in PostgreSQL. Configure CLI mTLS
with `INFERCRANE_CLIENT_TLS_CA_FILE`, `INFERCRANE_CLIENT_TLS_CERT_FILE`, and
`INFERCRANE_CLIENT_TLS_KEY_FILE`; Python SDK callers can pass `ca_file`, `cert_file`, and `key_file`.
Put private DNS, firewalls, and workload identity at the deployment boundary; TLS does not replace
network policy.

The production image includes InferCrane, the pinned upstream vLLM Router, the pinned SkyPilot
RunPod client, AWS CLI v2, and `kubectl`. Including provider clients does not select or configure a
provider. Development
workers and the simple development router exist only in the `development` image target and are
not performance or reliability substitutes for vLLM and vLLM Router.

Before rollout, qualify the exact PostgreSQL, vLLM, vLLM Router, model, GPU, and provider versions
with sustained load, streaming cancellation, worker loss, database failover, pod termination,
and soak tests. Capacity limits must be based on those measurements rather than defaults.

Release maintainers can validate packaging metadata without publishing with `make release-check`.
With `syft` installed, `make release-artifacts RELEASE_TAG=v2.0.0` creates and
verifies four exact-version archives, checksums, archive SBOMs, and a generated Homebrew formula
under `dist/`. It pushes no tag, image, package, or release. See [Release packaging](/release-packaging).

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
break-glass credential and use scoped principals for normal automation. Request-rate quotas are
reserved transactionally per UTC minute and consumed from instance-local leases, keeping PostgreSQL
off the inference request path. A configured zero limit blocks requests; a missing tenant quota is
unlimited. Policy refresh is eventually visible within one second and lease prefetch fails closed.
A decrease cannot revoke leases already consumed or issued to another gateway, so the lower
aggregate ceiling is fully effective at the next UTC-minute boundary; setting zero is observed as
an immediate local deny after refresh.
