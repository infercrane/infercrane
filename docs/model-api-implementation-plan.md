# Model APIs implementation plan

Status: MVP contracts implemented; provider launch qualification remains gated
Updated: 2026-09-02
Repositories: `infercrane` control plane and gateway, `infercrane-web` site and console

Production activation, payment qualification, supplier conformance, canary, and
GPU-lane promotion are tracked in
[Model API production rollout](model-api-production-rollout.md). Catalog presence
is not evidence that a model is callable.

## Outcome

Ship a credible, capital-light Model APIs MVP toward this qualified market
promise:

> **Fast model APIs. Complete inference infrastructure.**
>
> Start with qualified open-model APIs. When you need dedicated capacity,
> InferCrane deploys, autoscales, routes, measures, and continuously improves the
> entire serving stack.

`Fast` and `optimized` are launch claims only after the offered route has current
performance evidence. Before then, investor and preview material presents this
as the product target or scopes the claim to a named measured route.

The MVP must let a user:

1. Discover the six curated models.
2. See a truthful, current price and capability contract.
3. Prepay a small balance.
4. Create a scoped API key.
5. Make an OpenAI-compatible streamed request.
6. See usage, cost, route status, and missing reconciliation clearly.
7. Start a dedicated or own-cloud plan without changing the model/API contract.

The MVP may buy inference from qualified upstream suppliers. Customers see
InferCrane model identities, API keys, prices, usage, support, and migration
path. They do not see or depend on the current supplier.

## Launch catalog

All six products appear in the catalog from the first UI release. An entry is
callable only after its exact supplier route passes the launch gates below.
Before then it is labeled `Catalog only`, `Private preview`, or `Unavailable`.

| Public product | Primary use | Initial display state |
| --- | --- | --- |
| GLM-5.2 | Coding, reasoning, bilingual chat | Catalog only until qualified |
| GLM-5.3 | Flagship reasoning and long context | Catalog only until qualified |
| GLM-5.3-Flash | Low-cost, latency-sensitive work | Catalog only until qualified |
| Kimi-K3 | Coding and agentic workflows | Catalog only until qualified |
| Kimi-K2.6 | Coding, agents, long context | Catalog only until qualified |
| DeepSeek-V4-Flash | High-throughput workloads | Catalog only until qualified |

The prices shown in competitor screenshots are research inputs, not InferCrane
rate cards. A public price is published only after supplier cost, resale terms,
usage semantics, failure charging, taxes/fees, and target margin are known.

## Product and architecture contract

The customer integration remains stable while supply changes underneath it:

```text
Application
   |
   | OpenAI-compatible request + InferCrane API key
   v
InferCrane Gateway
   |-- auth, budgets, reservation, admission, request policy
   |-- immutable serving plan from memory; no database in request path
   |-- usage parsing, settlement, audit, metrics
   v
Qualified supplier route today
   |
   +--> InferCrane-managed or customer-cloud capacity later
        using the same public model identity and API contract
```

Use three separate records. Never merge their responsibilities:

- `ManagedProduct`: public, supplier-neutral identity, description,
  capabilities, context, availability, retail price, and evidence freshness.
- `SupplierOffer`: private supplier model ID, adapter, region, cost, limits,
  health, qualification evidence, capacity, resale terms, and expiry.
- `ServingPlan`: immutable compiled primary/fallback routes for one public model
  and protocol. The data plane reads the active plan from memory.

Hugging Face enriches discovery data, immutable artifact identity, licenses,
tags, and organization assets. It is not authoritative for InferCrane pricing,
availability, performance, supported API features, or qualification.

### Shared hosted-supply tenancy

Hosted supply is operator-owned and shared; customer policy and money remain
customer-owned. Do not duplicate supplier targets or credentials into every
customer tenant.

- The InferCrane operator workspace owns supplier targets, credentials, private
  offers, qualification evidence, and published serving plans.
- A customer workspace owns API keys, wallet, budgets, usage, audit records, and
  product entitlements.
- A product entitlement maps an authorized customer workspace and public model
  ID to the operator-published serving plan plus customer-specific limits. It
  grants no read access to the operator target or secret.
- The gateway snapshot contains the entitlement, public product/rate version,
  and opaque active backend bindings needed for routing. Supplier credentials
  are resolved only inside the trusted gateway boundary.
- Retail settlement is written to the customer ledger. Supplier cost and invoice
  reconciliation are written to a separate operator ledger and joined through
  request and route IDs for margin reporting.
- Existing tenant-scoped endpoint bindings remain the contract for customer-owned
  dedicated/BYOC capacity. Shared hosted bindings use an explicit operator-owned
  publication path instead of pretending to be customer resources.

## Capital and margin strategy

Use capital in this order:

1. **BYOK/BYOC and paid qualification.** Customer pays the cloud or supplier;
   InferCrane earns setup, platform, and support revenue without financing GPUs.
2. **Prepaid hosted APIs.** InferCrane buys each request from qualified suppliers
   and keeps a transparent margin. Spend is reserved before transmission.
3. **Customer-funded dedicated capacity.** Use minimum-spend or annual
   commitments to cover non-cancellable capacity.
4. **InferCrane-owned warm capacity.** Only after stable demand and measured unit
   economics show at least 30% fully-loaded cost advantage over the current
   supplier route.

MVP commercial rules:

- Wallet funds are credited only by a verified paid webhook, never a browser
  redirect.
- Reserve the worst-case authorized request cost before calling a supplier.
- Reject a request when balance, price, cost basis, margin, health, or evidence
  is stale.
- Set ceilings per request, key, workspace, model, supplier, and operator.
- Settle known actual usage. Keep ambiguous usage `Pending reconciliation`; do
  not silently release or invent a charge.
- Launch without automatic top-up. Add it only after reconciliation and fraud
  behavior are understood.
- Target a minimum gross margin per route. Do not subsidize an unprofitable
  route to make the catalog look larger.

## Workstreams and ownership boundaries

These lanes are designed for parallel agents without overlapping files.

| Lane | Repository / owned paths | Responsibility |
| --- | --- | --- |
| A. Product contracts | `internal/modelapicatalog/**`, new `internal/modelapiproduct/**`, catalog API/schema files, assigned `*_model_api_catalog.sql` migrations | Managed products, rate cards, entitlements, public projection |
| B. Supply and qualification | `internal/modelapisupply/**`, new `internal/supplieradapter/**`, assigned `*_model_api_supply.sql` migrations | Private offers, adapters, probes, planning, evidence |
| C. Gateway and billing | `internal/gateway/**`, `internal/routes/**`, `internal/managedbilling/**`, named store files, assigned `*_model_api_billing.sql` migrations | Admission, shared-plan publication, reserve/settle, safe failover, metrics |
| D. Design system and console | `infercrane-web/packages/ui`, `packages/design-tokens`, `apps/console` | Primitives, catalog, quickstart, onboarding, usage, dedicated |
| E. Landing and messaging | `infercrane-web/apps/site` only | Positioning, interactive plan preview, enterprise conversion |
| F. End-to-end verification | dedicated integration/browser fixture paths only | Cross-package, browser, accessibility, billing, and failure journeys |

Only lane A changes shared API schemas. It lands those changes first. Each
feature lane owns its migration and package tests; lane F owns only cross-package
and browser suites. Other lanes consume generated or versioned contracts and do
not redefine them. Lane D owns reusable UI primitives; lane E consumes them and
does not edit the shared package during its page pass.

## Implementation phases

### Phase 0 — Freeze truth and the public contract

Goal: make the six model products stable before designing around temporary data.

Backend:

- Define the six public model IDs, names, descriptions, context limits, and
  capability fields.
- Separate discovery entries, managed products, supplier offers, and active
  serving plans.
- Define availability states: `catalog_only`, `private_preview`, `available`,
  `degraded`, and `unavailable`.
- Define a versioned retail/cost rate-card contract with validity windows.
- Define an anonymous, cacheable public projection and an authenticated
  projection with account-specific limits.
- Define the operator-owned hosted-supply workspace, customer product
  entitlements, shared-plan publication, secret isolation, customer retail
  ledger, and operator supplier-cost ledger.
- Keep exact model requests exact. Cross-model outcome routing requires a
  separate, explicit product and customer consent.

Web:

- Map all visible catalog labels to fields in the public contract.
- Delete or hide unsupported switches such as ZDR and reasoning effort. Render
  them only when the selected route proves support.

Exit criteria:

- One documented schema drives catalog, billing, gateway, and UI fixtures.
- No UI value depends on supplier-private data.
- Unknown values render as unknown, never zero or an implied promise.

### Phase 1 — Design-system primitives

Goal: a calm, simple visual grammar influenced by the supplied Wafer references,
without copying its brand assets or introducing a second CSS system.

- Keep the current Next.js/React design tokens and CSS architecture.
- Use shadcn's accessible composition patterns, adapted to InferCrane tokens.
- Do not add Tailwind only to install shadcn defaults.
- Add only the missing primitives to `packages/ui`: `Button`, `Input`, `Tabs`,
  `Accordion`, `Dialog`, mobile `Sheet`, `Select`/`Combobox`, `Skeleton`, and
  `Toast`.
- Reuse the existing badge, code, heading, status, empty-state, definition-list,
  wizard, explorer, canvas, chart, and shell components.
- Standardize focus rings, 44px touch targets, compact density, elevation,
  empty/loading/error states, reduced motion, and responsive behavior.
- Use one restrained signature motion: the route in Plan Preview resolves from
  application to hosted, dedicated, or customer-cloud capacity.

Exit criteria:

- Primitives have keyboard and screen-reader tests.
- No stock shadcn theme appears in the product.
- Core screens work at 320px, 390px, and desktop widths.

### Phase 2 — Public catalog and private supply

Goal: turn static catalog data into an operated supply system.

- Persist `ManagedProduct`, `SupplierOffer`, qualification, rate card, and
  compiled-plan records. JSON remains bootstrap/fixture data, not production
  state.
- Create a private `SupplierAdapter` interface for:
  - authentication and request construction;
  - buffered and streaming protocols;
  - tool/structured-output capabilities;
  - health, inventory, and rate-limit signals;
  - usage parsing and ambiguous billing;
  - normalized errors, retry hints, and reconciliation.
- Implement one exact supplier/model route first. An internal LiteLLM adapter is
  allowed behind this interface, but LiteLLM does not own the public contract.
- Import Hugging Face metadata on a schedule with manual review, immutable
  revision pinning, license checks, and field-level provenance.
- Wire the existing supply planner into persistence and the existing endpoint
  materialization path. Store accepted routes and rejection reasons rather than
  rebuilding plan publication.

Exit criteria:

- Operators can inspect why each offer was accepted or rejected.
- Public responses never expose supplier identity, cost, or credentials.
- Catalog display state and callable state are controlled independently.

### Phase 3 — Gateway, prepaid billing, and safe routing

Goal: make one qualified route safe enough for paid preview traffic.

- Extend the existing scoped-key and tenant billing path with a product
  entitlement that can reference an operator-owned shared serving plan without
  exposing its target or secret.
- Make one canonical immutable rate-card version feed public catalog projection,
  supply-planner candidates, request reservation, and settlement policy.
- Connect the existing reservation and append-only settlement contracts to the
  canonical product/rate/offer records.
- Add automated reconciliation plus the missing operator supplier-cost and
  invoice ledger.
- Add margin and cost-variance alarms.
- Extend the existing in-memory route snapshot and publication flow for opaque
  shared hosted bindings while preserving customer-owned endpoint bindings.
- Implement circuit breaking and hysteresis.
- Add same-request fallback only before response bytes are sent. Never replay a
  stream after bytes have reached the client.
- Mark uncertain supplier charges for reconciliation and prevent double spend.

Exit criteria:

- Insufficient credit fails before an upstream call.
- Expired rates and stale evidence fail closed.
- Every successful or ambiguous request has an auditable financial state.
- A supplier outage does not silently change the requested public model.

### Phase 4 — Model API catalog, quickstart, and API keys

Goal: the shortest credible path from discovery to first request.

Catalog:

- Keep `/models` for deployable/self-hosted compatibility and `/model-apis` for
  callable hosted products.
- Add a compact `Hosted APIs / Deploy your own` surface switcher.
- Render one accordion row per model. Collapsed rows show identity, concise use,
  input/output/cache price, context, capabilities, and availability.
- Expanded rows show cURL/Python/JavaScript quickstarts, the exact capability
  contract, price validity, qualification freshness, and `Deploy this model`
  migration action.
- Support search and filters for task, price, context, capability, and status.
- Avoid decorative scenery behind operational content. Let typography, spacing,
  thin borders, and one subtle atmospheric field establish the brand.

Quickstart and keys:

- Use an environment variable in every snippet; never render the raw secret
  after its one-time creation reveal.
- Provide `Create key`, `Copy`, `Run in playground`, and a small funded-balance
  prompt in context.
- Show missing-key, insufficient-balance, unavailable-model, stale-price,
  streaming, success, and copy-failure states.
- Keep reasoning, ZDR, residency, and other options absent unless the route's
  current contract supports them.

Exit criteria:

- A funded user can reach a successful streamed request in under five minutes.
- A catalog-only model cannot be called accidentally.
- Examples work against the real gateway contract in CI.

### Phase 5 — Landing positioning and guided onboarding

Goal: explain the two-stage business in seconds and reduce the current chat-like
input burden.

Landing:

- Lead with the approved positioning and two actions: `Try model APIs` and
  `Plan dedicated capacity`.
- Show proof through the real product mechanism: one stable API contract moving
  from serverless to dedicated or customer cloud.
- Keep the planner compact: `Deploy`, `Improve`, or `Connect`; then model or
  endpoint, workload, and priority.
- Show a read-only Plan Preview before signup or spend.
- Use concise sections for hosted APIs, the stable migration path, optimization
  evidence, enterprise control, and final conversion.
- Do not claim fastest, lowest cost, ZDR, or enterprise controls without current
  evidence on the selected route.

Onboarding:

- Replace the long conversational questionnaire with two progressive screens:
  1. intent, model/endpoint, workload, and priority;
  2. Plan Canvas, unknowns, delivery choice, and explicit authorization.
- Default a new developer toward hosted APIs; keep BYOK, dedicated, and own-cloud
  visible as alternative delivery modes.
- Keep provider, GPU, runtime, scaling, and networking under `Advanced` until
  those choices materially affect the plan.
- Never create resources or charge money during anonymous planning.

Exit criteria:

- A first-time user creates a useful plan in under three minutes.
- The plan states what is known, modeled, measured, or missing.
- Authorization is explicit before funding, provider connection, or deployment.

### Phase 6 — Dedicated dashboard, usage, and billing

Goal: make the expansion path and paid operation real without fake enterprise
inventory.

Dedicated:

- Use the existing endpoint area with a dedicated view, not a disconnected
  product shell.
- Empty state: explain reserved/customer-cloud capacity and offer `Plan dedicated
  capacity` plus `Talk to an engineer`.
- Populated states show actual endpoint, model/revision, provider/region, runtime,
  capacity, autoscaling, SLO, cost, health, operations, optimization candidates,
  Release Guard, and rollback only when backed by authoritative APIs.
- For early enterprise work, default to customer-owned provider billing and a
  paid fixed-scope qualification engagement.

Usage:

- Show requests, input/output/cached tokens, TTFT, latency, errors, spend, route
  status, and reconciliation status.
- Offer graph/table views and content-free request inspection.
- Make zero traffic, delayed metrics, partial data, and unavailable telemetry
  explicit states.

Billing:

- Show available, reserved, spent, and pending-reconciliation balances.
- Show the append-only ledger and prepaid funding action.
- Do not add invoices, auto-reload, or per-key filtering until their backend
  contracts exist and have been qualified.

Exit criteria:

- Financial totals reconcile with the ledger.
- Missing metrics do not render as healthy zeros.
- Dedicated empty and populated states are driven by the same endpoint model.

### Phase 7 — Exact self-host migration and continuous optimization

Goal: convert Model API adoption into the defensible infrastructure product.

- Mark a hosted product migration-eligible only when its exact model, revision,
  license, weights, tokenizer, protocol, and capability contract are legally and
  technically self-hostable.
- Pin that exact model repository, revision, tokenizer, protocol, and capability
  contract used by the hosted product.
- Build the candidate on InferCrane-managed or customer-cloud capacity.
- Qualify and benchmark the exact runtime/provider/region/GPU configuration.
- Replay representative, content-safe workload samples.
- Extend Release Guard to accept a qualified external hosted route as the active
  baseline, then compare it with a deployment-backed candidate. This is a new
  backend dependency; the current deployment-only primary rule is insufficient.
- Promote explicitly only when correctness, SLO, cost, and rollback gates pass.
- Keep the hosted route as rollback until the new capacity is stable.
- If exact migration is impossible, create a new public model ID and require an
  explicit customer-approved quality migration. Never imply equivalence.
- Continuously test bounded alternatives in engine configuration, batching,
  quantization, caching, scaling, hardware, and routing; never mutate production
  from an advisory result alone.

Exit criteria:

- For a migration-eligible exact model/revision, the public model ID and
  application integration do not change.
- Migration evidence is exact-tuple, current, and auditable.
- Rollback is proven before promotion.

## Model launch gates

Each of the six models advances independently:

| Gate | Required evidence | Public state |
| --- | --- | --- |
| G0 — Catalog | reviewed identity, description, license/source provenance | Catalog only |
| G1 — Contract | retail rate, context, API capability contract, expiry | Private preview |
| G2 — Supplier | real credentials, bounded load test, usage/error/retry semantics | Private preview |
| G3 — Billing | reserve/settle/reconcile and invoice variance proven | Private preview |
| G4 — Pilot | customer traffic or 30-day operated window meets SLO/margin | Available |
| G5 — Redundancy | second qualified supplier or exact self-hosted fallback | Available + resilient |

At G2, run buffered, streaming, tool use where supported, structured output
where supported, cancellation, deadline, malformed request, rate limit, provider
5xx, disconnect, and ambiguous-usage tests. Capture TTFT and throughput only as
measured evidence, not universal model claims.

## Verification matrix

Backend and integration:

- Catalog schema compatibility and private-field redaction.
- Rate expiry, cost ceiling, minimum margin, stale health, and stale evidence.
- Wallet concurrency, idempotent reservation/settlement, ambiguous usage, and
  verified Stripe webhook behavior.
- Exact model identity, no silent substitution, supplier failover before bytes,
  no streamed replay, circuit breaker, and plan snapshot updates.
- Supplier adapter conformance and reconciliation against upstream invoices.

Web and experience:

- Catalog loading, available, catalog-only, private-preview, degraded,
  unavailable, no-results, and API-error states.
- Missing key, one-time key reveal, copy failure, insufficient balance, price
  expiry, successful streamed quickstart, and no raw key in code examples.
- Anonymous plan causes no mutation; browser payment return grants no funds.
- Empty and populated dedicated, usage, and billing views.
- Keyboard-only flows, automated accessibility checks, reduced motion, and
  responsive snapshots at 390px and 1440px.

Success-path scenarios:

1. New user -> GLM-5.3-Flash -> fund -> key -> streamed request -> usage row.
2. Developer -> Kimi model -> inspect tools/context -> Python snippet -> success.
3. Catalog-only model -> clear waitlist/private-preview path; no callable action.
4. Existing endpoint -> connect -> inspect plan -> explicit authorization.
5. Hosted customer -> plan exact dedicated candidate -> Release Guard -> promote
   -> retain hosted rollback.
6. Enterprise prospect -> dedicated empty state -> paid qualification scope ->
   customer-cloud plan.

## Delivery sequence

This is a gate-based sequence, not a promise that every feature fits a fixed
calendar.

### Milestone 1 — Investor-quality operated demo

- Product records and six-model public catalog fixtures.
- Design primitives and finished catalog/quickstart/key flow.
- One real qualified supplier/model route or an explicitly labeled sandbox.
- Prepaid ledger and verified funding flow.
- Real request, streaming response, usage, and cost trail.
- Landing and two-screen onboarding tied to real destinations.

### Milestone 2 — Paid private preview

- One production supplier route, reconciliation, ceilings, margin alarms, and
  operator runbook.
- Two or three callable models; all six remain visible with honest states.
- Usage/billing evidence and dedicated planning conversion.
- Five design-partner conversations, three paid scopes, two qualified
  candidates, and one recurring conversion as commercial learning targets.

### Milestone 3 — Small self-serve catalog

- All six independently pass G4.
- At least one high-demand model has supplier redundancy.
- Support, incident, rate-change, reconciliation, and catalog update operations
  are exercised.
- Self-serve limits remain conservative until traffic history supports changes.

### Milestone 4 — Dedicated and self-host expansion

- Exact migration pipeline through benchmark, Release Guard, promotion, and
  rollback.
- Customer-funded dedicated capacity first.
- InferCrane-owned capacity only after the capital gate is met.

## Metrics

Activation:

- Useful plan created in less than 3 minutes.
- First funded hosted request in less than 5 minutes.
- First BYOK request in less than 15 minutes.
- Conversion from catalog view to successful request.

Operation:

- Success rate, TTFT, output throughput, p95 latency, cold starts, and error rate
  per exact route.
- Reconciliation age and supplier invoice variance.
- Gross margin per model and route.
- Percentage of requests served by qualified fallback.
- Rate/evidence expiry incidents caught before traffic.

Expansion:

- Hosted customers starting a dedicated plan.
- Paid qualification conversion and recurring platform revenue.
- Cost improvement and SLO stability after an exact self-host migration.

## Explicitly out of scope for the first MVP

- Hosting all six models on InferCrane-funded GPUs.
- A public claim that InferCrane is universally the fastest or cheapest.
- Public `fast` or `optimized` positioning before route-level evidence exists.
- Automatic cross-model substitution.
- Unbounded Hugging Face catalog deployment.
- Automatic provider price scraping as billing truth.
- Fake dedicated inventory or unsupported enterprise feature claims.
- Full autonomous optimization or promotion without approval.
- Auto-reload, complex invoicing, or broad marketplace economics before ledger
  and reconciliation behavior are proven.

## First implementation tickets

1. Land the six-product schema, fixtures, availability states, and public/private
   projections.
2. Land the operator hosted-supply tenancy, customer entitlement, secret
   isolation, and dual retail/supplier-ledger contract.
3. Land one canonical versioned rate-card contract and connect it to catalog,
   planner candidates, reservation, and settlement.
4. Add the minimal token-native UI primitives in `packages/ui`.
5. Build the model API accordion and quickstart against generated fixtures.
6. Implement one supplier adapter and one exact qualification record.
7. Connect private offers through the existing supply planner and endpoint
   materialization path, including persisted rejection reasons.
8. Connect the canonical product/rate/offer records to existing reservation and
   settlement; add automated reconciliation and supplier-cost accounting.
9. Extend the route snapshot for operator-published shared bindings and add
   cross-binding pre-response failover.
10. Connect the console to the real catalog/key/wallet/request APIs.
11. Refine the site hero and two-step planner around the stable API migration
   story.
12. Extend Release Guard for an external hosted baseline and enforce exact
    migration eligibility.
13. Run the six success-path scenarios, accessibility checks, financial
    invariants, and failure tests before enabling paid preview.
