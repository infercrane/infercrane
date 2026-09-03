# Model API production rollout

Status: execution plan; callable routes remain gated
Updated: 2026-09-02
Owners: gateway/platform, supplier qualification, billing/finance, product operations

## Decision

Expose one stable InferCrane API implementing a versioned, documented subset of
the OpenAI Chat Completions contract. “OpenAI-compatible” must mean the exact
supported fields, streaming grammar, errors, limits, and deviations published
for that API version; it must not imply every OpenAI extension is accepted.

```text
Developer
   |
   v
api.infercrane.com/v1
   |
   v
Auth -> prepaid admission -> entitlement -> immutable route snapshot
   |
   +-- Qualified upstream API
   +-- Qualified serverless GPU
   +-- Qualified dedicated or BYOC deployment
```

The public model name, API key, request contract, usage record, and retail rate
belong to InferCrane. The active supplier is private and may change only when the
replacement preserves the exact qualified model identity and behavioral
contract. Never retry after response bytes have reached the customer. Before
that point, failover is allowed only when transmission did not occur or the
supplier proves that the attempt was not charged.

This plan separates three truths:

- a **catalog product** may exist without callable capacity;
- a **qualified offer** proves one exact supplier/model/runtime contract;
- a **published route** authorizes one tenant to spend against a current retail
  rate and qualified offer.

## Verified production baseline

Read-only production checks on 2026-09-02 found:

- `api.infercrane.com` resolves to the Fly application and has an active
  certificate;
- HTTP redirects to HTTPS and `/livez` and `/readyz` return `200`;
- the Fly application has one 2 GB `shared-cpu-1x` Machine in `fra`;
- the database-aware Fly service check is passing;
- required Stripe, hosted-auth, database, RunPod, and API secret **names** exist;
  values were not inspected;
- `/metrics` is currently reachable through the public service;
- the deployment is single-machine and therefore is not highly available.

DNS and TLS are not the current blocker. Supplier qualification, money
reconciliation, route publication, and operational safety are.

## Launch scope

The MVP has four supplier-neutral product identities. Procurement costs stay
private and are not public performance or availability claims. A product stays
`Catalog only` until the exact endpoint, model revision, commercial terms,
usage contract, and failure behavior have passed qualification.

| InferCrane product | Private execution target | Honest launch boundary |
| --- | --- | --- |
| `glm-5.2` | Z.ai general pay-as-you-go API | Catalog only until the exact direct route is qualified |
| `glm-5.3` | Z.ai general pay-as-you-go API | Catalog only until the exact direct route is qualified |
| `glm-5.3-flash` | Z.ai general pay-as-you-go API | Catalog only until the exact direct route is qualified |
| `qwen3.8-27b` | InferCrane-operated RunPod load-balanced SGLang endpoint | Provisional recipe; 18,432-token context and 512-token output cap until RunPod qualification |

Supplier identities, credentials, costs, and routing targets never appear in
the customer catalog response. The public contract contains only the stable
InferCrane model ID, current retail rate, supported features, and evidence
state.

Commercial authorization is a hard gate, not a documentation checkbox. Default
supplier terms generally distinguish embedding a service in a customer
application from reselling or sublicensing the supplier service itself. Before
operating a transparent multi-tenant Model API, attach a signed order form,
reseller agreement, or written supplier approval to the offer. Legal and
privacy review must confirm the exact account, geography, data path, and end-user
contract before traffic is enabled.

Do not make all four callable merely to fill the shelf. Launch the first exact
offer that passes every gate, then repeat the same qualification. A product
without a current offer remains visible as `Catalog only` or `Private preview`.

## Existing implementation to preserve

InferCrane already has:

- supplier-neutral products, private offers, qualifications, plans, rates, and
  tenant entitlements;
- append-only, versioned retail rate contracts with validity windows;
- deterministic supply planning with a hard minimum 15% supplier-token margin
  floor; a landed-COGS floor is still required before paid launch;
- atomic in-memory route generations with last-known-good retention;
- tenant-isolated prepaid reservation before supplier transmission;
- explicit `reserved -> transmitted -> response_started -> settled` fencing;
- `pending_reconciliation` for ambiguous usage instead of guessed charges;
- initial buffered and SSE usage extraction, stable public model rewriting, and
  an HTTPS-only configured endpoint registry; the strict streaming conformance
  and cached/reasoning-token gates below remain launch blockers;
- fixed $25, $50, $100, $250, and $500 Stripe Checkout top-ups;
- signed raw-body Stripe webhook verification, atomic idempotent crediting,
  partial cumulative refunds, wallet debt, and an append-only wallet ledger;
- `/livez`, database-aware `/readyz`, Prometheus metrics, and durable operation
  fencing.

These are strong safety foundations. They are not yet proof of unattended paid
production.

## Phase 0 — Freeze public and private contracts

Before buying traffic, create one versioned operator bundle containing:

1. public product and public operation;
2. exact supplier and supplier model ID;
3. supplier endpoint, adapter, and credential reference;
4. exact identity or documented mutable-alias policy;
5. normalized streaming, usage, error, context, tool, and idempotency behavior;
6. supplier cost schedule and commercial authorization;
7. retail rate, target margin, hard floor, entitlement, and validity window;
8. canary cohort, fallback predicates, rollback route, and qualification expiry.

Build an authenticated operator API/CLI for offers, qualifications, plans,
rates, entitlements, and publication. Operators must not edit production tables
directly. Every mutation emits an audit event and produces a canonical digest.

Acceptance:

- a public catalog response never exposes supplier name, URL, offer, cost, or
  credential;
- a missing, expired, or inconsistent contract fails closed before a supplier
  call;
- changing an identity-bearing field requires a new qualification and route
  generation;
- durable route-generation history and a tested operator action can republish
  the exact prior generation. This is planned work; in-memory
  last-known-good retention alone is not a production rollback mechanism.

## Phase 1 — Gateway and `api.infercrane.com`

### Current launch posture

Keep Fly.io as the control/gateway host for the MVP. It is inexpensive, already
deployed, and does not need to host GPUs. Confirm the deployed
`INFERCRANE_URL` value is exactly `https://api.infercrane.com` before publishing
customer examples.

### Required work

1. Add a deploy runbook or GitHub workflow using an immutable image digest.
2. Before a migration-bearing release, record the previous image digest, create
   and restore-test a PostgreSQL backup, and prefer roll-forward migrations.
3. Add an explicit Fly deploy strategy only after choosing between accepted
   single-Machine interruption and qualified two-Machine rolling deployment.
4. For rolling or canary deployments, add a deploy-only Machine check so a bad
   candidate stops before replacement. For a single-Machine immediate deploy,
   use an independently started pre-deploy candidate and run the same checks
   before replacing production.
5. Keep the existing service `/readyz` check because it correctly removes a
   database-disconnected Machine from routing.
6. Add Fly custom metrics scraping, long-term log export, alert contacts, and
   dashboards. Move or protect the currently public `/metrics` surface.
7. Benchmark the current `soft=100`, `hard=200` Fly concurrency settings with
   long-lived streams. They are load-balancing signals, not customer rate
   limits.
8. Configure tenant quotas and verify `429` plus `Retry-After`; a missing tenant
   quota is currently unlimited.
9. Rotate secrets through staged Fly secret deployments. Separate staging and
   production organizations and deploy credentials before live payments.
10. Drill rollback by deploying the previous image digest. A database schema
    incompatibility requires the database restore runbook, not only an image
    rollback.

Do not market the MVP as multi-region or highly available. Move to two Machines
in distinct failure domains only after database failover, route reconstruction,
unique instance identity, connection-budget, rolling-upgrade, and region-loss
tests pass.

Gateway acceptance:

- DNS, certificate, HTTPS redirect, `/livez`, `/readyz`, authenticated control
  read, and route reconstruction pass after deploy. A supplier-backed synthetic
  Model API call is added only after that exact offer passes Phase 2;
- a bad candidate never receives customer traffic;
- a Machine or region failure demonstrates the documented service behavior;
- no secret value appears in Fly configuration, logs, metrics, or audit output.

## Phase 2 — Supplier qualification

Introduce `infercrane.supplier-qualification/v1`. Its canonical digest must bind:

- product, offer version, supplier, region, validity, and resale authority;
- supplier model ID, revision/fingerprint, accepted response identities, and
  drift policy;
- adapter version, endpoint family, runtime/engine/image/model/tokenizer/chat
  template/tool parser/reasoning parser/configuration digests;
- operations, modalities, streaming grammar, terminal event, usage placement,
  tool modes, structured output, and reasoning behavior;
- effective context and output limits measured on the deployed route;
- input, output, cached-input, and reasoning-token fields;
- supplier request ID, authoritative usage source, reconciliation and explicit
  no-charge methods;
- supplier pricing revision, retry behavior, idempotency guarantee, queue/cold
  start configuration, and safe failover predicates;
- conformance suite version, sample counts, artifacts, completion time, and
  expiry.

### Qualification suite

For each exact supplier/model tuple, run:

1. buffered and streamed deterministic requests;
2. valid SSE framing, multi-line events, comments, terminal event, premature EOF,
   malformed JSON, cancellation, and errors inside a `200` stream;
3. no-tools, automatic, required, named and parallel tool calls where claimed,
   including streamed arguments and assistant/tool round trips;
4. context tests immediately below, at, and above the effective limit, with
   tools and multimodal overhead included;
5. normal, cached, reasoning, tool-only, and zero-output usage;
6. invalid auth/model/body/context, `429`, capacity failure, `5xx`, timeout,
   disconnect before headers, and disconnect after output;
7. warm and scale-to-zero samples for queue delay, headers, TTFT, output rate,
   p50/p95/p99, initialization failures, and cold-start maximum;
8. request-ID correlation and any documented supplier idempotency behavior;
9. supplier usage/invoice sample reconciliation against the gateway record;
10. mutable-alias drift detection and automatic quarantine.

The current normalized `supplieradapter` contract should become the production
runtime boundary. Do not keep the generic one-size-fits-all bearer/OpenAI
transport as the only adapter. Normalize supplier errors, billing outcome,
`Retry-After`, response model/fingerprint, cached and reasoning tokens, and
supplier request ID before committing the public response.

Do not enable supplier-internal retries or hidden fallbacks unless their exact
behavior is part of the qualified tuple. A gateway route is not the same offer
as its underlying model endpoint.

## Phase 3 — Prepaid Stripe production qualification

The Stripe webhook is already connected in code. Production work is
configuration and missing lifecycle hardening, not a new balance system.

### Required configuration

For separate test and live environments configure:

- secret key and pinned Stripe account identity;
- five one-time USD Price IDs for $25/$50/$100/$250/$500;
- Checkout success/cancel return URL;
- dedicated endpoint signing secret, with a rotation overlap strategy;
- expected live/test mode.

At startup, retrieve and reject a Price that is inactive, recurring, wrong
currency, wrong amount, wrong mode, or owned by the wrong Stripe account.

### Required hardening

1. Persist a funding intent before creating Checkout.
2. Use its immutable ID as the local and Stripe idempotency key. The durable
   local funding intent is the source of truth: reuse a stored Session when it
   exists and conflict on changed parameters. Stripe may prune an idempotency
   key after 24 hours, so recovery must query/persist Session identity rather
   than assuming Stripe will return the original forever. Define and test the
   crash window between remote Session creation and local persistence.
3. Copy tenant, environment, and funding-intent IDs onto both Checkout Session
   and PaymentIntent metadata.
4. Persist a verified event into a durable inbox, acknowledge quickly, and
   process wallet mutation asynchronously and idempotently.
5. Track refund IDs and pending/succeeded/failed/canceled states. Add disputes
   and chargeback holds/debt.
6. Recover undelivered events and reconcile Stripe Balance/Payout reports to
   funding intents, wallet entries, refunds, disputes, and fees daily.

### End-to-end tests

Run the complete journey in Stripe test mode before supplier canary. Only after
Phases 4 and 5 pass, repeat it with one capped pilot tenant and one small
controlled live payment:

1. create one funding intent and Checkout Session;
2. complete payment and observe a signed event;
3. verify exactly one append-only credit and correct wallet balance;
4. retry browser return and webhook deliveries and verify no extra credit;
5. after supplier qualification and admission gates exist, make one qualified
   Model API request and settle usage;
6. issue a partial refund and verify debit/debt behavior;
7. issue the remaining refund and reconcile Stripe cash to the wallet ledger;
8. export and archive redacted evidence; never put card or secret data in it.

No browser redirect can grant credit. Test objects must never mutate live
wallets and live events must never mutate test wallets.

## Phase 4 — Admission, settlement, and reconciliation

Before supplier transmission, one transaction must enforce:

- API key and tenant;
- product entitlement state;
- RPM, TPM, monthly spend, maximum request cost, and wallet balance;
- current retail rate, hard margin floor, and supplier/operator budget;
- qualified route and unexpired commercial/evidence contract;
- customer idempotency key plus canonical request hash.

Configured RPM, TPM, monthly-spend, or other dimensions that are not yet
atomically enforced must make the entitlement non-callable. Likewise, a retail
rate containing cached-input or reasoning-token dimensions remains
non-callable until those tokens are normalized, reserved, settled, and
reconciled end to end. The gateway must never silently bill them as ordinary
input or omit them.

Add a durable settlement outbox and sweep every aging state:

- stale `reserved`: release only after proving transmission never occurred;
- stale `transmitted` or `response_started`: resolve with supplier evidence;
- `pending_reconciliation`: bounded retries and age escalation.

Persist gateway usage, supplier usage, cached/reasoning tokens, supplier request
ID, supplier price revision, actual COGS, invoice reference, evidence digest,
and variance independently. Retail settlement stays pinned to the customer rate
contract; later supplier cost changes become COGS variance, not a retroactive
customer price change.

Reconcile all calls, not only ambiguous calls. Never release a reservation on a
timer because usage is missing.

## Phase 5 — Circuit breakers and canary

Circuit-break per exact supplier/model/operation/region tuple. Initial policy:

- require at least 20 observed attempts for a rate window;
- open after five consecutive pre-response failures or more than 50% failure;
- allow one half-open probe every 30 seconds;
- close only after three successful probes;
- open immediately on qualification/rate expiry, margin-floor breach, drift,
  supplier-budget exhaustion, or commercial invalidation.

Circuits affect future routing only. They do not replay a transmitted request.

Canary stages:

```text
synthetic paid probes -> opted-in pilot 1% -> 5% -> 25% -> 50% -> 100%
```

Use a stable HMAC cohort. Before the first canary, implement durable route
generation history and a tested republish action for the previous generation;
the current in-memory retention is insufficient for operator rollback. Abort
on semantic mismatch, missing or incorrect usage, SSE/tool/context failure,
double billing, unresolved invoice variance, cost/margin breach, supplier drift,
or SLO regression. Do not shadow customer prompts without explicit consent.

A route becomes `callable` only when:

- qualification and commercial evidence are current;
- customer and supplier accounting reconcile;
- the retail margin is above the hard floor and launch target;
- canary gates and rollback drill pass;
- support, alert, and incident ownership are assigned.

## Phase 6 — Promote hot routes to InferCrane GPUs

Use one target interface:

```text
Target = ExternalAPI | ServerlessGPU | DedicatedDeployment | CustomerCloud
```

Every target declares exact artifact/tokenizer/protocol identity, metering
authority, idempotency/transmission semantics, cost model, lifecycle owner,
credential boundary, qualification expiry, and rollback path.

Promotion policy:

1. start on token-priced external supply for near-zero fixed cost;
2. when measured traffic makes it cheaper, qualify the same exact model on a
   scale-to-zero RunPod vLLM/SGLang target;
3. keep the upstream offer as cold-start/capacity fallback where its contract
   makes pre-transmission failover safe;
4. use `workersMin=0`, bounded `workersMax`, request-count autoscaling,
   FlashBoot/cached models, explicit execution timeout, and measured idle
   timeout for the first experiment;
5. keep one worker warm only when latency value exceeds its idle cost;
6. move sustained committed traffic to dedicated capacity only after measured
   utilization beats elastic landed cost;
7. use BYOC when the customer pays infrastructure directly; keep InferCrane
   quota, audit, release, and platform-fee policy without supplier COGS reserve.

Do not migrate under the same public model ID unless artifact, tokenizer,
runtime behavior, context, tools, usage, and quality are exact. Otherwise issue
a new product/version.

Break-even is measured, not guessed:

```text
self-host landed cost per 1M tokens =
  (GPU seconds + warm idle + storage + network + operations + failures) /
  reconciled billable tokens
```

Promote only when the lower confidence bound of savings remains above the
target margin after cold starts, headroom, payment fees, refunds, and incident
reserve.

## Alerts and operating limits

Add alerts for:

- customer spend at 50/75/90/100%;
- supplier/operator budget at 70/85/95/100%;
- low wallet balance and refund debt;
- margin target or hard-floor breach;
- missing usage, reconciliation age, invoice variance, and stale reservations;
- circuit open/half-open duration and supplier capacity errors;
- Stripe webhook/inbox lag, dead-letter events, disputes, and cash variance;
- Fly readiness, CPU/memory, hard concurrency, 4xx/5xx/TLS, DB pressure, route
  refresh failure, accounting drops, and operation backlog.

The first paid launch should target a 30% landed gross-margin buffer. The
existing 15% supplier-token rejection floor remains a preliminary guard, not a
landed profitability guarantee. Add a separate landed-COGS publication gate
that includes supplier usage,
request/startup fees, warm idle, network/storage, payment fees, refund/fraud
reserve, and reconciliation/incident reserve.

## Release gates

Track every gate in an append-only launch ledger with: gate ID, product/offer,
owner, status (`planned`, `implemented`, `wired`, `deployed`,
`production_verified`, or `failed`), evidence digest/URL, environment, and
timestamp. “Implemented” is never treated as “production verified.”

The launch order is strict:

1. operator mutation API and versioned qualification contract;
2. one supplier adapter and exact offer;
3. payment idempotency, lifecycle, and reconciliation hardening;
4. entitlement limit enforcement and durable settlement;
5. supplier/accounting qualification fixtures;
6. synthetic canary and rollback;
7. test-mode payment/refund journey;
8. private pilot with a low supplier and tenant spend ceiling, including
   concurrent admission tests for wallet balance, request idempotency, RPM,
   TPM, monthly spend, maximum request cost, and supplier budget;
9. controlled live payment/refund/reconciliation for the pilot tenant;
10. public callable status for that one model;
11. repeat per model;
12. only later add serverless GPU, dedicated, and BYOC targets.

Any failed gate leaves the product catalog-visible but not callable.

## Evidence sources

Fly.io:

- [Custom domains and certificates](https://fly.io/docs/networking/custom-domain/)
- [Health checks](https://fly.io/docs/reference/health-checks/)
- [Seamless deploys](https://fly.io/docs/blueprints/seamless-deployments/)
- [Rollback](https://fly.io/docs/blueprints/rollback-guide/)
- [Load balancing](https://fly.io/docs/reference/load-balancing/)
- [Secrets](https://fly.io/docs/apps/secrets/)
- [Metrics](https://fly.io/docs/monitoring/metrics/)
- [Production checklist](https://fly.io/docs/apps/going-to-production/)

Stripe:

- [Checkout fulfillment](https://docs.stripe.com/checkout/fulfillment)
- [Webhooks](https://docs.stripe.com/webhooks)
- [Idempotent requests](https://docs.stripe.com/api/idempotent_requests)
- [Refunds](https://docs.stripe.com/refunds)
- [Testing environments](https://docs.stripe.com/testing-use-cases)
- [Balance and payout reconciliation](https://docs.stripe.com/reports/payout-reconciliation)

Suppliers and runtimes:

- [RunPod endpoint configuration](https://docs.runpod.io/serverless/endpoints/endpoint-configurations)
- [RunPod load-balancer health](https://docs.runpod.io/serverless/load-balancing/overview)
- [vLLM OpenAI-compatible server](https://docs.vllm.ai/en/latest/serving/openai_compatible_server/)
- [vLLM production metrics](https://docs.vllm.ai/en/latest/usage/metrics/)
- [SGLang server arguments](https://docs.sglang.io/docs/advanced_features/server_arguments)
- [Z.ai chat completion](https://docs.z.ai/api-reference/llm/chat-completion)
- [DeepSeek chat completion](https://api-docs.deepseek.com/api/create-chat-completion/)
- [Kimi API models](https://platform.kimi.ai/docs/overview)

Public compatibility references:

- [OpenAI Chat Completions API](https://platform.openai.com/docs/api-reference/chat)
- [OpenAI streaming responses](https://platform.openai.com/docs/api-reference/chat-streaming)
