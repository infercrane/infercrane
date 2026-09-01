# InferCrane product capability map

This is the short product-truth checklist for the website, console, demos, and
planning work. It summarizes implemented contracts; the qualification matrix
remains the source of truth for exact evidence and real-infrastructure status.

## Core promise

InferCrane turns a model, an existing endpoint, or an application requirement
into one stable OpenAI-compatible endpoint. A user can use InferCrane-hosted
capacity, deploy reviewed capacity into their own infrastructure, adopt what
already exists, or combine those paths behind the same endpoint contract.

## Product capabilities

### 1. Plan and select

- Resolve a bounded intent into an exact model, runtime, GPU, provider, region,
  scaling policy, and editable deployment draft without creating resources.
- Use curated, content-addressed recipes and immutable Hugging Face artifact
  identities; never silently substitute a model.
- Compare sourced GPU price observations separately from stock, quota,
  launchability, and measured performance.
- Probe launch-time capacity and connection readiness before a paid mutation.
- Preserve evidence as measured, modeled, compatibility-only, or unknown.

### 2. Deploy or adopt capacity

- Provision through AWS, GCP, RunPod, Kubernetes/KServe, and declared SkyPilot
  multi-cloud targets where their exact adapter is configured and qualified.
- Serve with vLLM, SGLang, a reviewed custom OCI runtime, or adopt a compatible
  external endpoint such as LiteLLM/OpenRouter without exposing credentials.
- Support elastic and provider-native serverless deployment contracts, including
  zero/warm/cold lifecycle semantics where the provider adapter supports them.
- Persist idempotent operations so launch, watch, cancel, restart, reconcile,
  cleanup, and orphan recovery survive client or control-plane interruption.
- Track immutable model artifacts and provider cache/prefetch observations.

### 3. Serve through one stable endpoint

- Keep the application-facing endpoint stable while backends, providers, and
  revisions change behind it.
- Proxy capability-qualified Chat Completions, Responses, Embeddings, legacy
  Completions, streaming, tool calls, structured output, and online batch.
- Apply request admission, deadlines, bounded retries, distributed quota,
  health-aware routing, session hints, and explicit governed overflow.
- Support active/candidate/fallback bindings and deterministic weighted plans
  without putting PostgreSQL in the request data path.

### 4. Observe and operate

- Show request rate, tokens, latency/TTFT, errors, spend, route/revision identity,
  cold starts, and fresh hardware evidence without storing prompt content.
- Provide a content-free Request Inspector, deterministic Doctor/explanations,
  signed alert webhooks, audit history, and durable operation progress.
- Attribute managed Model API usage through a prepaid, append-only ledger while
  keeping missing supplier usage pending instead of inventing a charge.
- Surface capacity history, queue pressure, SLO attainment, OpenCost/FinOps data,
  and evidence freshness for operator decisions.

### 5. Benchmark and optimize

- Run explicit AIPerf benchmarks and bounded replays against exact immutable
  model/runtime/version/args/provider/region/GPU tuples.
- Create immutable optimization proposals and durable campaigns without changing
  production until cost authority and approval are explicit.
- Compare measured candidates in the Inference Lab; attach external task-quality
  evidence and optimized-artifact provenance when available.
- Recommend scaling, cold-start, capacity, and cost actions with missing evidence
  left unknown; advisory autopilot does not silently mutate production.

### 6. Release and recover safely

- Stage immutable candidates, validate them, and return Release Guard decisions
  of ACCEPT, REJECT, or WAIT from persisted evidence.
- Promote explicitly, preserve the active revision when evidence is missing or
  rejected, fence stale actions, and support rollback and cleanup.
- Issue verifiable inference passports for approved deployment evidence.

### 7. Interfaces and platform controls

- Web console, CLI/TUI, control API/OpenAPI, Python and TypeScript SDKs, Terraform,
  and release artifacts share the same control-plane contracts.
- Tenant isolation, scoped principals, audit logs, secret references/redaction,
  TLS/mTLS boundaries, PostgreSQL durability, migrations, and backup/restore are
  platform capabilities rather than landing-page features.

## Hosted Model APIs

The customer sees a curated, supplier-neutral model identity, public price,
context/capability contract, availability state, usage, and one InferCrane API
key. Internally, InferCrane compiles fresh private supply offers into immutable
primary/fallback plans, reserves wallet funds before transmission, settles from
returned usage, and fails closed when rates, health, evidence, or usage are stale.

The same reviewed model configuration can later become a customer-owned
deployment. Hosted API and own-cloud deployment are two delivery modes of one
model contract, not separate product catalogs.

## Truth boundaries

- Compatibility, price, availability, capacity, performance, and quality are
  different claims and must never be collapsed into a generic score.
- A provider or runtime name means an implemented contract, not universal
  qualification. Every real claim is exact-tuple and evidence-bound.
- Public GPU prices do not prove inventory, account quota, or launch success.
- Managed Model API availability and economics require a fresh operated supply
  catalog and real supplier reconciliation; local contracts alone do not prove it.
- Unsupported accelerators, runtimes, and optimizers remain absent or explicitly
  experimental until an adapter and evidence exist.

## UI coverage rule

- **Landing page:** communicate six verbs only — plan, deploy, serve, observe,
  optimize, guard — plus the choice of InferCrane-hosted or customer-owned compute.
- **Build:** ask for delivery mode, model/outcome, workload, and primary objective;
  reveal infrastructure only when it changes the plan.
- **Plan Canvas:** show architecture, exact configuration, evidence, alternatives,
  expected cost, and the next reversible action before mutation.
- **Dashboard:** lead with real endpoints, requests/spend, attention, operations,
  optimization candidates, and Release Guard state.
- **Catalog/detail:** show identity, capabilities, price, measured performance,
  availability, provenance, usage, and deploy/hosted-API actions with unknowns clear.
- **Advanced surfaces:** keep policy, security, provider credentials, recipes,
  telemetry, automation, and evidence administration out of the primary flow.
