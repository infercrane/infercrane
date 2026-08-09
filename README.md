# InferCrane

Production inference without the platform engineering.

Deploy, autoscale, benchmark, and safely update vLLM workloads on GPU infrastructure you control.

```bash
infercrane deploy Qwen/Qwen3-8B
```

- No Kubernetes required
- Deterministic Release Guard
- Reproducible benchmarks
- Explainable durable operations
- Elastic and provider-native serverless compute

Read the [InferCrane documentation](https://infercrane.mintlify.site) for the five-minute local quickstart, product concepts, operations, integrations, and references. The Mintlify source lives in [`docs/`](docs/index.mdx); run `npm install && npm run dev` there to preview documentation changes locally.

## Why InferCrane?

Inference workers are ephemeral, but application endpoints should not be. InferCrane separates
the request data plane from the deployment control plane so clients can use a logical model alias
while workers are replaced, recovered, or reconfigured.

- Stable OpenAI-compatible model aliases.
- Persistent desired and observed state in PostgreSQL.
- Health and served-model reconciliation for vLLM workers.
- Supervised upstream vLLM Router processes with multiple routing strategies.
- Streaming chat completions without database reads on the request-routing path.
- Existing-worker registration, SkyPilot elastic provisioning, and RunPod Serverless lifecycle.
- Prometheus telemetry, readiness/liveness probes, request accounting, and graceful shutdown.
- Multi-replica gateway architecture with instance-owned local router generations.

## Project status

InferCrane is under active development. The core gateway, PostgreSQL control plane, existing-worker
workflow, reconciliation, router supervision, and durable bounded autoscaling are implemented and
tested locally. SkyPilot provisioning and autoscaling remain experimental until real RunPod
acceptance and soak testing complete.

See the authoritative [capability status](docs/project-status.md) before relying on a feature.
The [product vision and roadmap](docs/product-vision.md) defines the developer experience and the
evidence required before production claims are made.
The [enterprise readiness plan](docs/enterprise-readiness.md) orders the remaining production
blockers by reliability and security impact.

## Quick start

The development stack starts PostgreSQL, two GPU-free fake vLLM workers, a development router,
and InferCrane. These fixtures validate functionality only; they are not performance substitutes
for real vLLM or vLLM Router.

Requirements:

- Docker with Compose
- Go 1.26.5 for local development

Before changing infrastructure, validate the environment and preview the exact deployment plan:

```bash
infercrane doctor
infercrane doctor --cloud
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S --output json
```

`plan` is side-effect free. Pricing is reported as unavailable until a live, timestamped provider
pricing integration exists.

Start the stack:

```bash
docker compose up --build -d
docker compose ps
curl -fsS http://127.0.0.1:18000/readyz
```

List logical models:

```bash
curl -fsS \
  -H 'Authorization: Bearer infercrane' \
  http://127.0.0.1:18000/v1/models
```

Send a chat-completion request:

```bash
curl -fsS \
  -H 'Authorization: Bearer infercrane' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-prod","messages":[{"role":"user","content":"Hello"}]}' \
  http://127.0.0.1:18000/v1/chat/completions
```

Inspect the deployment:

```bash
docker compose exec infercrane infercrane deployments
docker compose exec infercrane infercrane status qwen-prod
docker compose exec infercrane infercrane inspect qwen-prod
```

Stop the stack without deleting PostgreSQL data:

```bash
docker compose down
```

## Use existing vLLM workers

Configure PostgreSQL and a strong API key, then register workers and create a logical deployment:

```bash
export INFERCRANE_DATABASE_URL='postgres://USER:PASSWORD@HOST:5432/infercrane?sslmode=require'
export INFERCRANE_API_KEY='REPLACE_WITH_A_STRONG_SECRET'
export INFERCRANE_INSTANCE_ID="$(hostname)"
export INFERCRANE_URL='https://infercrane.example.com'

infercrane target add gpu-a --url http://gpu-a:8000 --runtime vllm
infercrane target add gpu-b --url http://gpu-b:8000 --runtime vllm
infercrane plan Qwen/Qwen3-8B --name qwen-prod --targets gpu-a,gpu-b
infercrane apply Qwen/Qwen3-8B --name qwen-prod --targets gpu-a,gpu-b --idempotency-key release-001 --wait
infercrane route qwen-prod --strategy cache-aware
infercrane serve
```

Inspect lifecycle state and perform destructive changes explicitly:

```bash
infercrane operation OPERATION_ID
infercrane orphans
infercrane delete qwen-prod --plan
infercrane delete qwen-prod --yes
```

`deploy`, `apply`, and `delete` submit durable operations to `INFERCRANE_URL`; they never execute
cloud mutations or open PostgreSQL from the CLI process. Omitting `--wait` returns after submission,
so closing the terminal cannot interrupt provisioning or cleanup.

Provider-native serverless uses an operator-configured RunPod vLLM worker template validated against the requested
immutable model artifact:

```bash
export RUNPOD_API_KEY='...'
export INFERCRANE_RUNPOD_SERVERLESS_TEMPLATE_ID='...'
infercrane doctor --serverless
infercrane deploy Qwen/Qwen3-8B --compute serverless --cloud runpod --gpu L40S --max 4 --wait
```

See [RunPod Serverless setup and lifecycle](docs/features/serverless.md) before creating an endpoint.

Create tenant-scoped credentials; the token is shown only once:

```bash
infercrane tenant create team-a --name "Team A"
infercrane principal create automation --role operator
infercrane principal rotate PRINCIPAL_ID
infercrane principal revoke PRINCIPAL_ID
```

Queue a crash-recoverable existing-target apply through the versioned API:

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer $INFERCRANE_API_KEY" \
  -H 'Idempotency-Key: release-001' \
  -H 'Content-Type: application/json' \
  -d '{"name":"qwen-prod","model":"Qwen/Qwen3-8B","targets":["gpu-a","gpu-b"]}' \
  "$INFERCRANE_URL/api/v1/deployments/apply"

curl -fsS -H "Authorization: Bearer $INFERCRANE_API_KEY" \
  "$INFERCRANE_URL/api/v1/operations/OPERATION_ID"
```

Run a reproducible functional load check against a real deployment:

```bash
infercrane benchmark qwen-prod --requests 1000 --concurrency 32
```

Supported routing strategies are delegated to the pinned upstream vLLM Router:

- `round-robin`
- `consistent-hash`
- `power-of-two`
- `cache-aware`

## Architecture

PostgreSQL stores targets, deployments, desired and observed state, events, router generations,
and bounded request-accounting history. Embedded migrations run transactionally under a PostgreSQL
advisory lock during startup.

Each gateway replica maintains an atomic in-memory route directory. Inference requests resolve
aliases entirely from this snapshot, so PostgreSQL latency or a temporary control-plane failure
does not enter the routing decision. A reconciliation loop probes workers concurrently, computes
healthy membership, supervises instance-local vLLM Router processes, and atomically publishes new
routes.

Read the [system architecture](docs/architecture/system.mdx),
[data flows](docs/architecture/data-flows.md), and
[system invariants](docs/architecture/invariants.md) for the complete design.

## Operations

Health and telemetry endpoints:

| Endpoint | Purpose |
|---|---|
| `/livez` | Process liveness without downstream dependencies |
| `/readyz` | Bounded PostgreSQL readiness check |
| `/metrics` | Prometheus-format gateway telemetry |
| `/health` | Development-compatible health summary |

Production requires:

- PostgreSQL with backups, TLS, monitoring, and a tested recovery procedure.
- Unique stable `INFERCRANE_INSTANCE_ID` values per gateway replica.
- Secrets supplied through the deployment secret manager.
- Real vLLM workers and the pinned real vLLM Router.
- Load, cancellation, worker-loss, database-failover, rolling-upgrade, and soak qualification.

The production image includes InferCrane and the pinned upstream vLLM Router. Development fakes
exist only in the Docker `development` target under `internal/testtools`.

See [production operations](docs/production.md) and the
[RunPod provider setup](docs/provider-setup.md).

## Development

Install the repository toolchain with ASDF:

```bash
asdf install
go mod download
```

Run the standard workflow:

```bash
make context   # regenerate the source-derived repository map
make test      # race-enabled tests
make verify    # formatting, docs freshness, tests, vet, build, Compose validation
```

PostgreSQL integration tests run when `INFERCRANE_TEST_DATABASE_URL` is set:

```bash
export INFERCRANE_TEST_DATABASE_URL='postgres://infercrane:infercrane@127.0.0.1:15432/infercrane_test?sslmode=disable'
go test -race -count=1 -v ./internal/store
```

Coding agents and contributors begin with [AGENTS.md](AGENTS.md) and the
[engineering knowledge index](docs/index.mdx). Architectural decisions, feature contracts,
ownership, capability maturity, and a generated source inventory are maintained in the repository
and checked by CI.

## Documentation

- [Engineering knowledge index](docs/index.mdx)
- [Product vision and roadmap](docs/product-vision.md)
- [Project status](docs/project-status.md)
- [Architecture decisions](docs/adr/index.md)
- [Production operations](docs/production.md)
- [Stage 1 existing-worker guide](docs/stage1-poc.md)
- [Stage 2 SkyPilot guide](docs/stage2-skypilot.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Governance](GOVERNANCE.md)

## Contributing

Contributions are welcome as the public contribution process is established. Changes must include
tests, relevant documentation, regenerated repository context, and an ADR when they alter a durable
architectural or security decision. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

InferCrane is licensed under the [Apache License 2.0](LICENSE).
