# End-to-end inference platform execution plan

InferCrane owns the operational lifecycle of inference, regardless of where model execution happens:

```text
choose → build or connect → deploy → endpoint → route → scale
       → observe → evaluate → release → optimize
```

This plan is dependency ordered. A later layer may not become authoritative over an earlier one.
Provider, runtime, gateway, evaluator, and sandbox implementations remain replaceable adapters.

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
| 9 | Sandbox composition | Planned after endpoint adoption evidence | Narrow E2B/Modal-style adapter; InferCrane must not build a microVM or agent framework |
| 10 | Training lineage | Planned after deployment adoption evidence | External training run → checkpoint → ModelArtifact → revision; no training scheduler |
| 11 | Managed compute | Demand-gated business milestone | Billing, abuse controls, support, capacity supply, and private beta are prerequisites |

## Product rules

- Do not build a second inference engine, GPU scheduler, router, cloud provisioner, sandbox runtime,
  or workflow engine.
- Do not use an LLM for routing, promotion, budget, or rollback decisions.
- Do not turn simulated provider behavior into a real-provider claim.
- Do not publish managed compute before billing and abuse risk have explicit owners.
- Prefer a five-minute existing-endpoint connection over forcing migration.

## Success measures

- First successful proxied request in under five minutes for an existing endpoint.
- One application model alias remains stable while its serving plan changes.
- Every external transmission has consent, provider identity, and a pre-authorized hard budget.
- Every release decision can be reconstructed from immutable evidence.
- Hosted-console use does not weaken local/self-hosted authentication or authorization.
