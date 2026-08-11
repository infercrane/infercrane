# ADR 0013: Govern external capacity and keep BYOC credentials ephemeral

- Status: Accepted
- Date: 2026-08-11

## Context

An OpenAI-compatible URL is not sufficient evidence that traffic may be sent to a third party.
External transmission changes privacy, availability and cost boundaries. AWS BYOC also needs
provider credentials, but persisting temporary credentials would expand the control plane's breach
surface. The existing tenant already owns credentials, quotas, deployments and audit records, so a
second organization hierarchy would create ambiguous ownership.

## Decision

`tenant` remains the v0.3 organization boundary. Service accounts receive role-bounded explicit
scopes. Secret objects store only a resolver kind and opaque reference; secret values are resolved
into memory at the last responsible moment and never returned by control APIs.

External targets require a persisted policy containing explicit enablement, privacy acknowledgement,
model mapping and hard budgets. Selection happens before transmission and is recorded. InferCrane
never retries a request to an external provider after bytes may have been sent, and never silently
duplicates or shadows a request. OpenRouter is one governed OpenAI-compatible adapter, not a model
marketplace abstraction.

The AWS adapter uses short-lived role credentials, EC2 `RunInstances` client-token idempotency,
explicit VPC inputs, immutable workload identity and InferCrane ownership tags. Provider translation
stays behind Provider Contract V1. Cost remains unknown unless accompanied by source and timestamp.

## Consequences

External fallback is intentionally narrower than general hybrid routing. Cost budgets reserve a
configured worst-case amount before transmission; if a trustworthy bound is absent, cost-governed
traffic fails closed. AWS requires explicit network and image configuration instead of convenient
but unsafe defaults. Additional secret stores implement the same resolver interface without schema
changes.

## Verification

Adversarial tests cover scope escalation, tenant isolation, secret exfiltration, budget races and
stream replay prevention. Provider conformance fixtures cover EC2 idempotency, adoption, timeout,
delete and redaction behavior. Paid AWS and external-provider tests remain deferred to the single v1
manual qualification.
