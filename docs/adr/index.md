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
| [0015](/adr/0015-embedded-evidence-dashboard) | Superseded by 0030 | Embed a static evidence dashboard that uses only authenticated public APIs. |
| [0016](/adr/0016-portable-oci-runtime-workloads) | Accepted | Persist immutable runtime workload declarations and delegate container execution to providers. |
| [0017](/adr/0017-deterministic-inference-decisions) | Accepted | Persist evidence-based recommendations, SLO policy, and bounded overflow decisions. |
| [0018](/adr/0018-signed-release-evidence) | Accepted | Use explicit validation, durable rollback monitors, and canonical signed release evidence. |
| [0019](/adr/0019-kubernetes-integration-boundaries) | Accepted | Integrate Kubernetes without duplicating cluster, serving, or routing controllers. |
| [0020](/adr/0020-provider-neutral-production-artifact) | Accepted | Keep the base production artifact provider-neutral and make client boundaries executable. |
| [0021](/adr/0021-stable-endpoint-serving-plans) | Accepted | Separate stable application endpoints from concrete lifecycle resources with immutable serving plans. |
| [0022](/adr/0022-incremental-adoption-and-deterministic-diagnostics) | Accepted | Adopt incrementally and derive request inspection, Doctor findings, and signed alerts from persisted evidence. |
| [0023](/adr/0023-in-memory-admission-and-encrypted-async) | Accepted | Enforce admission from memory and persist async inference only as encrypted, fenced jobs. |
| [0024](/adr/0024-provider-product-profiles) | Accepted | Model provider products as explicit profiles with honest ownership and qualification boundaries. |
| [0025](/adr/0025-fenced-ha-and-protocol-overlap) | Accepted | Use PostgreSQL-fenced work and explicit protocol overlap for recoverable multi-replica operation. |
| [0026](/adr/0026-content-addressed-recipes-and-evidence-lab) | Accepted | Capture immutable measured recipes and keep Lab comparisons provenance-explicit. |
| [0027](/adr/0027-privacy-preserving-replay-and-observed-capacity) | Accepted | Capture content-free workload shape and keep capacity evidence observed. |
| [0028](/adr/0028-trustworthy-finops-and-human-approved-autopilot) | Accepted | Keep cost evidence sourced and optimization advisory until human approval. |
| [0029](/adr/0029-context-identity-delegated-survival-and-bounded-burst) | Accepted | Persist logical context identity while delegating runtime state survival. |
| [0030](/adr/0030-separate-web-products) | Accepted | Separate the public site and authenticated console from the inference gateway release artifact. |
| [0031](/adr/0031-managed-external-endpoint-bindings) | Accepted | Make authenticated external APIs governed, hard-budgeted endpoint bindings. |
| [0032](/adr/0032-sandbox-and-training-integration-boundaries) | Accepted | Keep sandbox isolation and training schedulers external while importing identity, evidence, and immutable artifact handoffs. |
| [0033](/adr/0033-replaceable-external-composition-contracts) | Accepted | Make LiteLLM, sandbox access, and signed training lineage executable through a versioned composition contract while leaving execution external. |
| [0034](/adr/0034-provider-connections-and-gateway-boundary) | Accepted | Make external APIs reusable provider connections while keeping gateway execution replaceable and spend fail-closed. |

Use the next sequential number. Include context, decision, consequences, alternatives, and
verification. Link affected feature documents.
