# ADR 0008: Persist immutable deployment revisions

- Status: Accepted
- Date: 2026-08-09

## Context

Safe updates and rollback require a stable identity for the configuration that produced capacity.
Mutating a logical deployment in place cannot distinguish active capacity from a candidate, cannot
audit promotion decisions, and cannot reproduce a failed rollout.

## Decision

The logical deployment owns an ordered sequence of immutable revisions. A revision stores the
normalized deployment specification used to create it and has a persisted lifecycle status.
Deployments point explicitly to at most one active revision and one candidate revision. Replicas
belong to a revision, so candidate and active capacity can coexist without sharing provider
identity or ordinal uniqueness.

Revision creation, candidate selection, promotion, rejection, and rollback are transactional
PostgreSQL operations. Provider adapters continue to own only infrastructure mutation. Existing
vLLM Router generation publication remains the sole request-routing cutover mechanism. Release
Guard evaluates persisted measurements and policy; it does not mutate revision history or use an
LLM to decide promotion.

## Consequences

Updates provision and validate candidate capacity before routing changes. Failed candidates leave
the active revision unchanged and can be inspected later. Rollback selects an existing immutable
revision and creates an auditable lifecycle operation; it never rewrites historical specifications.
Replica external keys include revision identity to keep provider mutations idempotent.

## Alternatives

Mutating deployments in place was rejected because it cannot preserve rollback identity. Encoding
revision state in SkyPilot names alone was rejected because provider state is not the source of
truth. A second router was rejected because vLLM Router already owns replica request routing.

## Verification

PostgreSQL integration tests cover monotonic revision numbers, immutable specifications, exclusive
active/candidate pointers, promotion, rejection, and rollback selection. Workflow tests cover
candidate failure without active-route mutation and generation-safe cutover.
