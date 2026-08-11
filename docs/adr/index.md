---
title: Architecture decisions
description: Accepted decisions that define InferCrane's system boundaries and invariants.
---

# Architecture decision records

ADRs preserve why the system looks the way it does. Accepted ADRs are immutable except for typo
or link corrections. A changed decision gets a new ADR whose status supersedes the old record.

| ADR | Status | Decision |
|---|---|---|
| [0001](/adr/0001-control-data-plane) | Accepted | Separate the control and request data planes. |
| [0002](/adr/0002-postgresql-source-of-truth) | Accepted | Use PostgreSQL as the production source of truth. |
| [0003](/adr/0003-instance-owned-router-generations) | Accepted | Scope loopback routers by gateway instance. |
| [0004](/adr/0004-repository-native-engineering-memory) | Accepted | Store durable engineering memory in the repository. |
| [0005](/adr/0005-durable-operations-and-policy-boundaries) | Accepted | Persist mutations and separate fleet policies from provider execution. |
| [0006](/adr/0006-leased-operation-execution) | Accepted | Lease durable work through PostgreSQL for crash-safe execution. |
| [0007](/adr/0007-tenant-scoped-identity) | Accepted | Hash scoped credentials and qualify all resource access by tenant. |
| [0008](/adr/0008-immutable-deployment-revisions) | Accepted | Persist immutable deployment revisions for safe update and rollback. |
| [0009](/adr/0009-qualified-support-and-backend-registration) | Accepted | Separate release qualification from registered provider and runtime backends. |
| [0010](/adr/0010-cobra-cli-and-progressive-terminal-output) | Accepted | Use Cobra for command discovery and progressively enhance terminal output. |
| [0011](/adr/0011-tiered-provider-qualification) | Accepted | Tier provider contracts, Docker qualification, and explicitly paid acceptance. |
| [0012](/adr/0012-versioned-integration-contracts) | Accepted | Version provider/runtime contracts and require evidence for supported capabilities. |
| [0013](/adr/0013-governed-external-capacity-and-byoc) | Accepted | Require explicit policy for external capacity and keep BYOC credentials ephemeral. |
| [0014](/adr/0014-generated-automation-clients) | Accepted | Generate clients from one API contract and preserve durable operation ownership. |
| [0015](/adr/0015-embedded-evidence-dashboard) | Accepted | Embed a static evidence dashboard that uses only authenticated public APIs. |
| [0016](/adr/0016-portable-oci-runtime-workloads) | Accepted | Persist immutable runtime workload declarations and delegate container execution to providers. |
| [0017](/adr/0017-deterministic-inference-decisions) | Accepted | Persist evidence-based recommendations, SLO policy, and bounded overflow decisions. |
| [0018](/adr/0018-signed-release-evidence) | Accepted | Use explicit validation, durable rollback monitors, and canonical signed release evidence. |
| [0019](/adr/0019-kubernetes-integration-boundaries) | Accepted | Integrate Kubernetes without duplicating cluster, serving, or routing controllers. |
| [0020](/adr/0020-provider-neutral-production-artifact) | Accepted | Keep the base production artifact provider-neutral and make client boundaries executable. |
| [0021](/adr/0021-stable-endpoint-serving-plans) | Accepted | Separate stable application endpoints from concrete lifecycle resources with immutable serving plans. |
| [0022](/adr/0022-incremental-adoption-and-deterministic-diagnostics) | Accepted | Adopt incrementally and derive request inspection, Doctor findings, and signed alerts from persisted evidence. |
| [0023](/adr/0023-in-memory-admission-and-encrypted-async) | Accepted | Enforce admission from memory and persist async inference only as encrypted, fenced jobs. |

Use the next sequential number. Include context, decision, consequences, alternatives, and
verification. Link affected feature documents.
