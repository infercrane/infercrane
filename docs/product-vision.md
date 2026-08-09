# Product vision and roadmap

InferCrane should make operating high-performance, production inference feel as dependable
as operating a mature database: simple at the entry point, explicit at risky boundaries, and
observable all the way down.

## Product promise

Developers choose a model and an infrastructure intent. InferCrane explains the resulting plan,
validates the environment, provisions or adopts capacity, exposes an OpenAI-compatible endpoint,
and continuously reconciles the deployment. It does not conceal the provider resources, routing
policy, runtime configuration, cost uncertainty, or failure state.

The experience is guided by five principles:

1. **Fast first success.** A local evaluation and an existing-runtime deployment should take minutes.
2. **Preview before mutation.** Potentially expensive actions have deterministic, machine-readable
   plans and explicit inputs.
3. **Progressive disclosure.** Strong defaults serve the common path; YAML and APIs expose control
   without creating a second product model.
4. **Actionable operations.** Errors identify the failed boundary and provide a concrete next step.
5. **Honest production claims.** Availability, performance, and cost claims require reproducible
   tests against real infrastructure.

## Golden path

```text
infercrane doctor
infercrane plan Qwen/Qwen3-8B --cloud runpod --gpu L40S
infercrane deploy Qwen/Qwen3-8B --cloud runpod --gpu L40S
infercrane status qwen3-8b
```

The same plan will be available as JSON for CI, user interfaces, and coding agents. A plan must be
side-effect free. Deploy remains the explicit mutation boundary.

## Roadmap

Delivery status describes repository implementation. Production qualification remains separate and
is governed by the capability table and compatibility policy.

### Milestone 1 — Trust before deploy

**Delivery status: implemented.**

- Dependency-free CLI help and version output.
- Deterministic human and JSON deployment plans.
- Read-only environment diagnostics for secrets, PostgreSQL, vLLM Router, and SkyPilot.
- Product terminology, errors, and documentation aligned around one golden path.

### Milestone 2 — Reproducible lifecycle

**Delivery status: implemented; real-cloud acceptance remains experimental.**

- Declarative apply semantics with plan/apply parity and idempotency keys.
- Durable operation records, progress events, cancellation, and retry classification.
- Provider credential diagnostics and real cloud acceptance tests.
- Safe deletion plans, confirmation policies, and orphan-resource detection.

### Milestone 3 — Production confidence

**Delivery status: tooling implemented; HA, GPU, upgrade, and recovery evidence remains unqualified.**

- Prometheus histograms, traces, structured audit events, dashboards, and alert templates.
- Reproducible load, failure, upgrade, and recovery suites using real vLLM workers.
- HA control-plane qualification, backup/restore drills, and published compatibility policy.
- Provider pricing adapters with timestamps and confidence boundaries.

### Milestone 4 — Efficient fleet operations

**Delivery status: policy and adapter foundations implemented; fleet execution and tenant identity integration remain experimental.**

- Policy-driven autoscaling with bounded decisions and explainable events.
- GPU-aware placement, capacity pools, warm starts, and model-cache management.
- Additional provider and runtime adapters behind stable interfaces.
- Team tenancy, scoped credentials, quotas, and role-based access control.

## Success measures

- Time from installation to a successful existing-target request.
- Percentage of failed deploys caught by `doctor` or `plan` before mutation.
- Deployment success rate and median/p95 time to readiness.
- Recovery time from worker, router, database, and control-plane failures.
- Upgrade success rate without gateway downtime or state loss.
- Operator time required to identify and remediate a failed deployment.

These metrics describe desired evidence, not current claims. Capability maturity remains tracked in
[Project status](project-status.md).
