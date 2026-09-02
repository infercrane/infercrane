# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

InferCrane's control plane and inference gateway are implemented in Go. The
customer-facing landing site and authenticated console are a separately
deployed Next.js/React application consuming the authenticated control API.

## Users

- Developers and AI-native teams that want production access to a curated set
  of high-value open models without operating inference infrastructure.
- Growing companies that need to move a workload from shared serverless APIs to
  dedicated capacity without changing the application-facing API.
- Enterprise platform and infrastructure teams that need governed deployment,
  observability, release safety, private networking, residency, or customer-cloud
  operation.

## Product Purpose

InferCrane is designed to give an application one stable OpenAI-compatible model
API while qualified suppliers, runtimes, providers, revisions, and deployment
modes can change underneath it. The initial hosted product is a curated
pay-as-you-go model catalog. An exact model contract can later move to
InferCrane-managed dedicated capacity or the customer's own cloud only when its
model, revision, license, artifacts, and API behavior are legally and technically
self-hostable.

Success means a new user can select a model, create a key, run a streamed request,
and see its usage and cost in minutes; a growing customer can later change the
serving mode without rewriting their integration.

## Positioning

Target market position, after route-level performance qualification:

**Fast model APIs. Complete inference infrastructure.**

Start with qualified open-model APIs. When dedicated capacity becomes useful,
InferCrane deploys, autoscales, routes, measures, and improves the serving stack
behind the same stable API contract. Public use of `fast` or `optimized` must be
supported by current evidence for the routes being offered.

The catalog is the acquisition surface. The durable advantage is the continuous
path from serverless to dedicated or customer-owned infrastructure, with shared
policy, evidence, billing, and release controls.

## Operating Context

- The initial public catalog is planned around GLM-5.2, GLM-5.3, GLM-5.3-Flash,
  Kimi-K3, Kimi-K2.6, and DeepSeek-V4-Flash-0731-Fast.
- Customers use one InferCrane API key and OpenAI-compatible endpoint.
- Public model identities and prices are supplier-neutral. Private supply
  offers contain upstream identity, cost, health, region, policy, and capacity.
- The MVP may use qualified external suppliers. LiteLLM can remain a replaceable
  internal translation boundary; it is not the customer-facing product owner.
- Customer usage is prepaid. Funds are reserved before supplier transmission,
  settled from actual usage, and blocked when credit is insufficient.

## Capabilities and Constraints

- Curated model discovery, stable logical model identities, API keys, streaming,
  usage, spend controls, prepaid billing, health-aware primary/fallback routing,
  and supplier-neutral public contracts are core hosted-model capabilities.
- Planning, deployment, adoption, observation, benchmarking, optimization,
  Release Guard, rollback, and provider/runtime adapters are existing control-plane
  capabilities; exact real-infrastructure support remains evidence-bound.
- Exact model requests must never silently substitute a different model. Optional
  outcome aliases may route across models only with explicit customer consent.
- Hosted-to-dedicated migration under the same public model ID is allowed only
  for the exact legally and technically self-hostable model/revision and a
  qualified equivalent API contract. A different model requires a new public ID
  and an explicit customer-approved quality migration.
- Hugging Face can supply candidate metadata, immutable artifact identity, and
  organization assets. InferCrane remains authoritative for customer price,
  capabilities, availability, qualification, and performance evidence.
- The hosted cloud service is not generally available yet. Real supplier
  behavior, commercial resale rights, current rates, reconciliation, and each
  model/runtime/provider/GPU tuple require qualification before public claims.
- Public claims such as fastest, zero data retention, residency, or self-hosted
  must be shown only when the complete selected route has current evidence.

Open decisions include initial supplier contracts, final retail rate cards,
which catalog entries launch as Recommended versus Available or Preview, and
the first model whose traffic justifies InferCrane-managed warm capacity.

## Brand Commitments

- Product name: InferCrane.
- Target lead after route-level qualification: **Fast model APIs. Complete
  inference infrastructure.** Before then, use evidence-neutral hosted-model
  language or scope performance claims to a named measured route.
- Approved supporting idea: start serverless, then move to dedicated or the
  customer's cloud without changing the application integration.
- Product language must be concise, evidence-led, technically credible, and
  suitable for both developers and enterprise buyers.

## Evidence on Hand

- `docs/product-capabilities.md` is the short product-truth map.
- `docs/testing/feature-qualification-matrix.md` records implemented contracts,
  qualification evidence, and real-infrastructure gaps.
- `docs/architecture/system.mdx` defines the database-free request path and
  replaceable provider/runtime boundaries.
- The repository contains local contracts for the hosted catalog, prepaid
  billing, supplier planning, routing, RunPod Serverless, OpenAI-compatible
  providers, LiteLLM composition, and Hugging Face catalog ingestion.
- No customer testimonial, production SLA, public benchmark for the six planned
  models, or generally available hosted-cloud claim is currently established;
  future surfaces must not fabricate them.

## Product Principles

1. Keep the customer contract stable while supply remains replaceable.
2. Curate and qualify models rather than presenting an unbounded model dump.
3. Measure price, availability, performance, quality, and compatibility as
   separate claims; leave missing evidence unknown.
4. Spend only after authority: prepaid hosted usage, reversible plans before
   deployment, and customer or traffic commitments before warm GPU capacity.
5. Let serverless adoption graduate into dedicated or customer-owned operation
   without creating a second product model.

## Accessibility & Inclusion

The web experience must be responsive, keyboard operable, compatible with
automated accessibility testing, and must not communicate availability,
qualification, errors, or cost using color alone.
