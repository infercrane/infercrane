---
title: End-to-end inference platform execution plan
description: The dependency-ordered path from an API-first endpoint to verified, optimized inference infrastructure.
---

# End-to-end inference platform execution plan

InferCrane owns the operational lifecycle of inference, regardless of where model execution happens:

```text
choose → build or connect → deploy → endpoint → route → scale
       → observe → evaluate → release → optimize
```

This plan is dependency ordered. A later layer may not become authoritative over an earlier one.
Provider, runtime, gateway, evaluator, and sandbox implementations remain replaceable adapters.

## Three product phases

### Phase 1 — one endpoint for API and self-hosted inference

API-first teams configure a provider once, expose a stable application alias, and can later add
self-hosted vLLM or SGLang without changing application code. `ProviderConnection` stores only a
target and secret reference. Traffic remains disabled until an endpoint binding has explicit
privacy acknowledgement and hard request/cost limits.

### Phase 2 — daily reliability and cost control

The same endpoint gains admission, quotas, request inspection, monitoring, deterministic Doctor
findings, alerts, governed fallback, semantic release evidence, and FinOps. This is the retention
layer: it answers what happened, why, what it cost, and whether a change is safe.

### Phase 3 — verified migration and optimization

Inference Replay, AIPerf, artifact/cache evidence, capacity history, Release Guard, and advisory
Autopilot compare serving plans. A team can move traffic between an API provider and customer-owned
compute only after measured evidence and human approval. No autonomous production mutation is
implied.

After these phases, the remaining company-building work is hosted operations, enterprise identity
and private networking, multi-region reliability, qualified managed compute, support, billing, and
a growing measured evidence corpus—not adding unrelated AI infrastructure categories.

| Order | Product outcome | Repository state | Exit evidence |
|---|---|---|---|
| 1 | One stable endpoint across self-hosted and external inference | Implemented locally | Immutable deployment and authenticated external bindings; explicit primary/fallback/weighted plans; hard budgets; no replay |
| 2 | Adopt before migrating | Local-qualified | Observe-only → traffic-managed ownership, discovery, Inspector, Doctor |
| 3 | Build inference from a project | Local-qualified | Schema-backed `workload init`, validate, build, dev, plan, deploy |
| 4 | Quality-aware safe releases | Local-qualified | Signed evaluator-neutral evidence plus deterministic Release Guard |
| 5 | Private hosted operations plane | Private-preview boundary implemented | Clerk identity mapping and entitlement are implemented; hosted deployment and design-partner exercise remain external |
| 6 | Faster materialization | Contract local-qualified | Provider-neutral cache observation/prefetch contract; real cache execution remains provider qualification |
| 7 | Public configuration evidence | Local-qualified | Recipes, AIPerf-backed Inference Lab, explicit measured/modeled labels |
| 8 | Managed overflow | Local-qualified policy | Budgeted external bindings and Burst Guard; real billing qualification remains |
| 9 | Sandbox composition | Local-qualified boundary | External reference plus expiring endpoint-scoped access; sandbox lifecycle remains external |
| 10 | Training lineage | Local-qualified boundary | Signed external run → checkpoint → ModelArtifact → revision; no training scheduler |
| 11 | Managed compute | Demand-gated business milestone | Billing, abuse controls, support, capacity supply, and private beta are prerequisites |

## Product rules

- Do not build a second inference engine, GPU scheduler, router, cloud provisioner, sandbox runtime,
  or workflow engine.
- Do not use an LLM for routing, promotion, budget, or rollback decisions.
- Do not turn simulated provider behavior into a real-provider claim.
- Do not publish managed compute before billing and abuse risk have explicit owners.
- Prefer a five-minute existing-endpoint connection over forcing migration.
- Keep sandbox and training integrations behind the boundaries in [ADR 0032](/adr/0032-sandbox-and-training-integration-boundaries); they are not immediate-launch capabilities.

## Success measures

- First successful proxied request in under five minutes for an existing endpoint.
- One application model alias remains stable while its serving plan changes.
- Every external transmission has consent, provider identity, and a pre-authorized hard budget.
- Every release decision can be reconstructed from immutable evidence.
- Hosted-console use does not weaken local/self-hosted authentication or authorization.
