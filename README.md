# InferCrane

Give InferCrane a model. Get a production endpoint.

InferCrane deploys, operates, observes, optimizes, and safely evolves open-weight and custom-model
inference. Keep one OpenAI-compatible application endpoint while models, runtimes, accelerators,
providers, and revisions change underneath it. Start in your cloud, connect infrastructure you
already run, or compose a governed model API without rebuilding the application.

```text
Application / agent
        ↓
stable OpenAI-compatible endpoint
        ↓
InferCrane · deploy · observe · optimize · release
        ↓
Qwen · Kimi · DeepSeek · GLM · gpt-oss · Llama · any compatible model
        ↓
vLLM · SGLang · TensorRT-LLM · custom OCI
        ↓
AWS · GCP · Kubernetes · RunPod · read-only bare-metal discovery · existing inference
```

<p align="center">
  <img alt="InferCrane endpoint monitoring console" src="https://infercrane.com/product/endpoint-monitoring.jpg" width="100%" />
</p>

Start from a model family people already use, or pass any compatible Hugging Face identity:

```bash
infercrane models
infercrane models inspect qwen3-8b
infercrane workload init ./support-model --recipe qwen3-8b
infercrane optimize propose llama-3.1-8b-instruct \
  --provider aws --region eu-central-1 --gpu L40S \
  --objective interactive --write-dir .infercrane/candidates
```

Optimization proposals are immutable configuration candidates, not benchmark claims. InferCrane
ranks a serving plan only after comparable measured performance, quality, and sourced cost evidence.

```bash
infercrane workload init ./agent-model \
  --model moonshotai/Kimi-K3
cd agent-model
infercrane workload plan
infercrane workload deploy --wait
```

The catalog is not an allowlist and the model is not hard-coded. The local
qualification corpus spans dense instruction, coding, reasoning/distilled, embedding, and
multimodal families; real performance remains bound to the exact model commit, runtime, accelerator,
provider, cache state, and workload. InferCrane also accepts other Hugging Face model identities and
custom OCI workloads. Evidence from one serving plan is never silently reused for another.

- No Kubernetes required
- Deterministic Release Guard
- Signed, independently verifiable Inference Passports
- Reproducible benchmarks
- Evidence-based SLO recommendations
- Explainable durable operations
- Signed external task-quality evidence
- Evaluator-neutral quality-result ingestion for CI, Ragas, DeepEval, or custom suites
- Safe staging-to-production candidate promotion
- Elastic and provider-native serverless compute

The common path stays intentionally small:

```bash
infercrane workload init ./support --recipe qwen3-8b
cd support
infercrane workload deploy --wait
```

Advanced operators can keep the same project and add an exact provider/runtime/GPU tuple, workload
fingerprint, SLO, cost boundary, cache policy, candidate campaign, and Release Guard evidence. The
simple path is an opinionated default—not a separate product or a hidden loss of control.

Already running a model server on a GPU host? Inspect the local hardware without mutation, then
connect the runtime in observe-only mode:

```bash
infercrane discover local
infercrane connect http://gpu-host.internal:8000/v1 --as support-production
```

Read the [InferCrane documentation](https://docs.infercrane.com) for the five-minute local quickstart, product concepts, operations, integrations, and references. The Mintlify source lives in [`docs/`](docs/index.mdx); run `npm install && npm run dev` there to preview documentation changes locally.

See the complete safety loop locally with one hermetic command:

```bash
make demo
```

The demo connects an existing OpenAI-compatible worker, sends and inspects a request, creates an
intentionally unready candidate, records a deterministic Release Guard rejection, proves the active
revision did not change, and removes the isolated stack. It uses fixtures and creates no cloud or GPU
resources.

For coding agents and LLM indexing, Mintlify continuously generates
[`llms.txt`](https://docs.infercrane.com/llms.txt) and
[`llms-full.txt`](https://docs.infercrane.com/llms-full.txt) from the current public navigation.
Every public page is also available as Markdown by appending `.md` to its documentation URL.
`infercrane mcp` exposes six read-only operational-evidence tools over stdio; it cannot deploy,
scale, promote, delete, change budgets, or read secrets.

Already running inference? Connect it without transferring lifecycle ownership:

```bash
infercrane connect https://vllm.internal/v1 --as coder-production
infercrane doctor coder-production
```

Keep specialist systems in their lane while InferCrane owns the production inference decision:

```bash
# Keep LiteLLM provider translation and credentials.
infercrane connect https://litellm.internal/v1 \
  --as support-production --type litellm

# Give an external agent sandbox short-lived access to one endpoint.
infercrane sandbox connect --provider e2b --external-id sandbox-01JCGW \
  --endpoint coder-production --ttl 30m

# Verify externally trained artifact lineage before release qualification.
infercrane training attach coder-runtime coder-42.handoff.json
```

InferCrane does not fork LiteLLM, execute sandbox commands, or schedule training. It connects those
systems through replaceable, versioned composition contracts and stores no sandbox commands, files,
prompts, outputs, or training data.

See the [production showcase](https://docs.infercrane.com/showcase) for model-to-endpoint deployment,
existing-workload adoption, LiteLLM and OpenRouter composition, agent sandbox integration, safe
rollouts, and cold-start evidence.

## Why InferCrane?

Inference workers are ephemeral, but application endpoints should not be. InferCrane separates
the request data plane from the deployment control plane so clients can use a logical model alias
while workers are replaced, recovered, or reconfigured.

- Stable OpenAI-compatible model aliases.
- Candidate-only, authenticated OpenRouter and generic OpenAI-compatible bindings with server-side
  credential references, explicit privacy consent, and hard request/cost reservations.
- Persistent desired and observed state in PostgreSQL.
- Runtime health and served-model reconciliation through an adapter contract.
- Bounded in-memory admission with explicit `429`/`Retry-After`, one end-to-end request deadline,
  bounded retries, end-to-end queue timing, and a live
  instance-scoped saturation signal that remains separate from liveness and runtime health.
- Supervised replica routing with deterministic generation safety.
- Streaming chat completions without database reads on the request-routing path.
- Existing-worker registration plus registered elastic and provider-native serverless backends.
- Prometheus telemetry, readiness/liveness probes, request accounting, and graceful shutdown across qualified runtime contracts.
- Multi-replica gateway architecture with instance-owned local router generations.

## Project status

InferCrane is preparing its v2.0 release candidate. Its lifecycle, persistence, routing, and policy
layers are provider-neutral; adapters declare versioned capabilities and independent qualification
evidence. The candidate includes durable lifecycle/autoscaling, governed external fallback, AWS EC2
BYOC, RunPod elastic and native Serverless, namespaced Kubernetes/KServe, vLLM, SGLang, immutable
custom OCI workloads, generated SDKs, Terraform, GitHub delivery checks, a terminal workspace, an
authenticated private-preview operations console, LiteLLM composition, endpoint-scoped external
sandbox access, signed training artifact lineage, Release Guard V2, deterministic recommendations,
and signed Inference Passports.

Local race, PostgreSQL, fault-injection, Docker, Kind, package, migration, security, and
documentation qualification is automated. Registration is not real-provider proof: RunPod, AWS,
GCP, and Kubernetes GPU acceptance remains explicitly deferred until the consolidated manual
qualification runs against the frozen release candidate. No public performance, pricing, or universal
provider/runtime claim is made from simulated evidence.

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
- Go 1.26.6 for local development

Before changing infrastructure, validate the environment and preview the exact deployment plan:

```bash
infercrane doctor
infercrane doctor --cloud
infercrane doctor --kubernetes
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S --output json
```

Open the optional terminal operations workspace at any time:

```bash
infercrane inbox
infercrane ui
```

`inbox` ranks persisted fleet state requiring attention without running diagnosis or recording
request content. The workspace reconnects to durable control-plane state, exposes only state-valid
guarded actions, and supports `--read-only` for shared incident screens. It does not require tmux,
and closing it does not cancel deployments.

The browser console is maintained as a separate web application so its authentication and release
boundary can evolve independently from the inference gateway. The initial public release remains
CLI- and API-first; the hosted console is deny-by-default private preview. Local self-hosted console
setup is documented in the [operations console](docs/features/dashboard.mdx).

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

The provider-neutral production image includes InferCrane, the pinned upstream vLLM Router, and the
explicit provider client boundaries used by registered adapters. Provider configuration is supplied
through separate Compose overlays. Development fakes exist only in the Docker `development` target
under `internal/testtools`.

See [production operations](docs/production.md) and [provider setup](docs/provider-setup.md).

## Development

Install the repository toolchain with ASDF:

```bash
asdf install
go mod download
```

Run the standard workflow:

```bash
make dev-check       # fast repository and provider-contract feedback
make test-kubernetes-kind # disposable, GPU-free Kubernetes lifecycle
make dev-check-full  # isolated Docker, recovery, production-config, and docs gates
make test-automation-full # SDK, GitHub Action, and real Terraform protocol fixtures
```

Each run keeps compact evidence under `.infercrane/dev-check/`. Provider adapters pass the same
replay-safe lifecycle contract locally; paid cloud qualification is explicit and reserved for a
locally green release candidate. See [development and testing](docs/development.mdx).

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
- [Python SDK](docs/integrations/python.mdx)
- [TypeScript SDK](docs/integrations/typescript.mdx)
- [Terraform provider](docs/integrations/terraform.mdx)
- [GitHub Actions](docs/integrations/github-actions.mdx)
- [LiteLLM gateway](docs/integrations/litellm.mdx)
- [External agent sandboxes](docs/integrations/sandboxes.mdx)
- [Training artifact handoffs](docs/integrations/training-artifacts.mdx)
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
