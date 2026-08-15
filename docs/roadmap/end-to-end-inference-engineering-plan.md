---
title: End-to-end inference engineering plan
description: Dependency-ordered control-plane, data-plane, console, documentation, qualification, and licensing work.
---

# End-to-end inference engineering plan

This is the delivery plan for making InferCrane the inference control and optimization platform from
the first managed-model API call through customer-operated or InferCrane-managed compute. It is not
a claim that every row is already production-qualified. Current evidence remains authoritative in
[Project status](/project-status).

## Product boundary

InferCrane owns this closed loop:

```text
connect or build
      ↓
stable endpoint
      ↓
admit and route
      ↓
observe quality, cost, latency, and reliability
      ↓
compare a candidate serving plan
      ↓
Release Guard
      ↓
human-approved promotion
      ↓
measure the actual result
```

It integrates specialist execution systems. It does not become a training framework, vector
database, agent framework, sandbox isolation runtime, inference engine, Kubernetes distribution,
GPU scheduler, or generic workflow engine.

## Engineering rules shared by every phase

1. Core domain types never name one provider, runtime, gateway, evaluator, scheduler, or workflow
   product.
2. Every implementation resolves through a versioned contract and publishes its achieved
   qualification tier.
3. Durable intent is persisted before an external mutation. Provider identity and adoption keys are
   sufficient to recover a lost response without creating a duplicate resource.
4. Credentials remain references. Browser, events, plans, telemetry, and evidence never receive raw
   provider secrets.
5. Unknown measurements remain unknown. Estimated, modeled, heuristic, and measured evidence are
   never merged into one unlabeled value.
6. External transmission, spend, request duplication, and production mutation require explicit
   policy and authorization.
7. The CLI, generated SDKs, console, docs, and test fixtures consume the same control API. No public
   workflow reads PostgreSQL directly.
8. A permissive top-level license does not authorize an integration by itself. The exact pinned
   artifact, included paths, transitive dependencies, notices, vulnerabilities, and distribution
   mode are reviewed before bundling.

## Track A — API-first acquisition

### User outcome

A team can configure OpenRouter or another qualified OpenAI-compatible provider once, expose a
stable application alias, and later add customer-operated inference without changing application
code.

| Surface | Required delivery | Exit evidence |
|---|---|---|
| Domain | `ProviderConnection` references one target and one secret; bindings snapshot consent and budgets | tenant, idempotency, immutable binding, and non-destructive delete tests |
| Data plane | chat, Responses, Embeddings, Completions, batch, streaming, cancellation, request identity | protocol conformance against qualified direct and composed gateways |
| Policy | primary/fallback/manual plans, admission, quotas, hard request and cost reservations | concurrent reservation, cancellation, exhaustion, and no-replay streaming tests |
| CLI/SDK | connect/list/delete provider; bind connection; copyable request examples | black-box product acceptance and generated-client contract check |
| Console | provider configuration, explicit fallback staging, no browser credential material | desktop/mobile Playwright, accessibility, authorization, and real-control-plane tests |
| Docs | API-to-self-hosted showcase, provider setup, privacy/cost boundary | Mintlify validation, link checks, and tested commands |

The direct OpenRouter and generic OpenAI-compatible adapters are the initial execution path.
Operator-managed LiteLLM is adopted as an external OpenAI-compatible gateway. InferCrane does not
fork LiteLLM and does not make LiteLLM YAML or database tables part of the InferCrane domain.

### Completion gate

- A new connection enables no traffic and authorizes no spend.
- Enabling a binding requires privacy acknowledgement plus request, total-cost, and worst-case
  per-request limits.
- A stable model alias survives a change between managed API and self-hosted bindings.
- Provider credentials are absent from browser payloads, PostgreSQL configuration, logs, events,
  plans, request evidence, and errors.
- Direct-provider protocol and billing claims remain unqualified until exercised against that real
  provider.

## Track B — daily reliability and cost control

### User outcome

InferCrane becomes the place an operator visits every day to understand what is failing, slow,
expensive, unsafe to release, or blocked on external capacity.

| Surface | Required delivery | Exit evidence |
|---|---|---|
| Request evidence | binding, provider, runtime, revision, replica when known, queue, TTFT, generation, tokens, retries, fallback, cost provenance | content-free persistence and tenant-isolation tests |
| Monitoring | traffic, latency, runtime pressure, capacity, cold starts, release and alert overlays | bounded queries, explicit partial states, responsive console tests |
| Doctor | deterministic evidence → rule → finding pipeline | repeatable findings with evidence digests; no LLM-authored cause |
| Quality | signed evaluator-neutral aggregate evidence | schema rejection, signature, revision binding, expiry, and key-rotation tests |
| Release Guard | PASS, REJECT, or INCONCLUSIVE over performance, errors, cost, compatibility, and configured quality | immutable evidence windows and promotion/rollback race tests |
| Alerts | signed webhooks for availability, capacity, guard, budget, auth, cold-start, and orphan conditions | SSRF, signing, retry, cancellation, and secret-redaction tests |
| FinOps | sourced spend by endpoint/binding/deployment/revision; unknown cost stays unavailable | currency, timestamp, source, staleness, and aggregation tests |

### Completion gate

- An operator can move from fleet alert to request, operation, revision, provider observation, and
  deterministic explanation without joining raw database records.
- A healthy infrastructure signal cannot override a failing configured semantic-quality gate.
- Monitoring remains useful when GPU, cache, price, or runtime metrics are unavailable.
- No dashboard chart fabricates or interpolates absent operational evidence.

## Track C — verified migration and optimization

### User outcome

InferCrane can determine whether a workload should remain on a managed API, move to customer
compute, or use a hybrid plan—and test the recommendation before production changes.

| Surface | Required delivery | Exit evidence |
|---|---|---|
| Workload projects | init, validate, local dev, build, immutable image/model identity, plan, deploy | clean-project black-box acceptance and registry qualification |
| Artifact delivery | provider-neutral observation and prefetch intent; native cache adapters | cache miss/hit/stale/restart tests plus real-provider timing evidence |
| Replay | content-free arrival, token, concurrency, session, prefix, and pause shape | privacy tests, deterministic digest, bounded retention, AIPerf approximation disclosure |
| Benchmark | exact runtime/image/model/hardware/workload reproduction record | AIPerf result ingestion and real-GPU rerun within declared tolerance |
| Capacity intelligence | placement success, failure, allocation latency, readiness, interruption, and price provenance | out-of-order, stale, missing, and cross-tenant tests |
| Recommendations | candidate serving plans with SLO, constraints, provenance, and confidence | deterministic ordering; measured/modeled/heuristic separation |
| Verified change | recommendation → candidate → replay/benchmark → Release Guard → human approval → actual result | no autonomous mutation and complete audit lineage |

### Completion gate

- The same logical endpoint can compare managed API, existing target, elastic deployment, and
  serverless candidate plans.
- Every recommendation exposes the evidence fraction and missing constraints.
- Forecast savings are never reported as realized savings.
- InferCrane records predicted versus actual outcome after promotion.

## Track D — enterprise and hosted operations

### User outcome

Teams can put InferCrane in a critical request path without accepting a new identity, networking,
availability, upgrade, or recovery risk.

| Surface | Required delivery | Exit evidence |
|---|---|---|
| Hosted identity | external identity mapping, organizations, projects, environments, preview entitlement | server-side authn/authz and confused-deputy tests |
| Local identity | local token/session adapter with no hosted dependency | clean self-hosted install and production-bypass rejection |
| Networking | private exposure, VPC/subnet/firewall, private DNS, proxy, mTLS, egress policy | provider-specific real-network qualification |
| Workload identity | short-lived role/service-account references | expiry, rotation, revocation, wrong-audience and propagation tests |
| HA | stateless API replicas, fenced workers, PostgreSQL source of truth | process kill, lease expiry, stale worker, and thundering-herd tests |
| DR | backup, restore, reconcile with newer provider state | declared RTO/RPO drill and wrong-resource deletion prevention |
| Upgrades | mixed-version admission, migration checksums, rollback boundary | every-prefix migration and N/N+1 overlap qualification |
| Audit/support | immutable operator evidence, support bundles, redaction | tenant isolation and secret-canary scans |

### Completion gate

- The hosted console and Go control API independently reject unapproved users.
- An API or worker replica can die without losing an accepted durable operation.
- A restored database reconciles external state without duplicating or deleting an unowned
  resource.
- Provider/runtime support labels are generated from qualification evidence rather than marketing
  configuration.

## Track E — managed compute, only after demand

Managed compute is a business system, not another provider adapter. It requires:

- supplier capacity and failure ownership;
- customer metering and invoice reconciliation;
- pre-funded or bounded credit risk;
- abuse, fraud, sanctions, and acceptable-use controls;
- quota and denial-of-wallet protection;
- image/model license and access enforcement;
- support, incident response, service credits, and status communication;
- regional inventory and data-residency commitments.

The first private beta should use one qualified capacity supplier and keep physical GPU ownership
out of scope. Customer-owned compute remains the default. Do not start this track until design
partners repeatedly reject InferCrane solely because they do not want to configure a provider.

## Adjacent-system decisions

| System requested | Difficulty to support safely | First integration | Decision |
|---|---|---|---|
| Training framework | Medium | verify a signed external run/checkpoint and attach its immutable artifact to a candidate revision | API/metadata integration; no fork |
| Vector database | Low for observability linkage; high for lifecycle | record a reference, health/SLO evidence, and endpoint dependency only when a real RAG workflow needs it | external dependency; no storage/indexing |
| Agent framework | Low | OpenAI-compatible endpoint, SDK examples, request/context identity, OpenTelemetry propagation | protocol integration; no agent framework |
| Sandbox runtime | Medium | external sandbox reference plus expiring endpoint-scoped credential | backend adapter later; never own isolation |
| Inference engine | Medium per engine | Runtime Contract descriptor, immutable workload, probes, protocols, drain/cancel, metrics | in-tree adapter; no fork of engine |
| Kubernetes distribution | Low | qualify upstream APIs against conformant clusters | support Kubernetes, not a distribution |
| GPU scheduler | Medium | observe Kueue or Volcano admission and scheduling state through CRDs | optional cluster adapter; no scheduler logic |
| Workflow engine | Low | idempotent API action, durable operation ID, signed event/webhook | Argo/Temporal examples; no workflow runtime |

No candidate above currently justifies a fork. Forking is permitted only after an ADR identifies the
exact licensed paths, maintenance owner, security-update SLA, bounded divergence, upstreaming plan,
and removal strategy.

## License and supply-chain gate

Before an upstream changes from external integration to bundled or executed component:

1. Pin an immutable version or digest.
2. Record upstream repository, exact paths used, SPDX expression, copyright/NOTICE obligations, and
   commercial/enterprise exclusions.
3. Generate an SPDX SBOM for the resulting archive or image.
4. Scan vulnerabilities and verify image provenance.
5. Confirm the license permits the intended SaaS, on-premises, modification, and redistribution
   model.
6. Add upgrade, compatibility, rollback, and removal tests.
7. Re-run the review on every version change.

Current direction:

- LiteLLM: integrate an operator-managed endpoint; do not bundle or fork. Its root license excludes
  separately licensed enterprise paths, so any future managed-process proposal requires a
  path-level audit.
- Envoy AI Gateway: Apache-2.0 candidate for a future Kubernetes-native gateway backend; use
  upstream releases behind a contract rather than fork.
- Kueue and Volcano: Apache-2.0 candidates for optional scheduler observation/admission adapters;
  never make either required.
- Kubeflow Trainer and MLflow: external Apache-2.0 training owners; exchange signed lineage and
  artifacts.
- E2B and Kubernetes Agent Sandbox: external sandbox owners; exchange scoped identity and evidence.
- Ollama and llama.cpp: MIT runtime candidates; qualify through Runtime Contract rather than absorb
  their runtime code.
- TensorRT-LLM: no bundling decision until the exact container, plugins, model code, and transitive
  components pass a path-level license review.

See the shorter [integration ownership matrix](/roadmap/integration-ownership-matrix) for the
decision summary.

## Delivery and release discipline

Each track lands in vertical slices. A slice is complete only when it includes:

```text
domain + migration
control API + OpenAPI
CLI and generated clients
console workflow
documentation and examples
unit/state-machine/fault tests
PostgreSQL/Docker integration
browser/accessibility qualification
manual real-system procedure where simulation is insufficient
```

No slice receives a real-provider, real-runtime, performance, savings, isolation, or billing claim
from fixture evidence alone.
