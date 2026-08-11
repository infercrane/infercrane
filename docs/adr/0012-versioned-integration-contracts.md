# ADR 0012: Version provider and runtime contracts with evidence-bearing capabilities

- Status: Accepted
- Date: 2026-08-11

## Context

ADR 0009 separated adapter registration from public qualification, but adapter interfaces,
capability descriptions, and evidence remained distributed across workflows, diagnostics, tests,
and documentation. A new integration could compile while omitting replay, cancellation, drain,
telemetry, or cleanup behavior, and a capability label did not identify the test supporting it.

## Decision

InferCrane defines `infercrane.provider/v1` and `infercrane.runtime/v1` contracts in the integration
package. Lifecycle workflows consume the versioned provider interfaces. Registered profiles declare
stable adapter identity, owner cloud/runtime, compute modes, capabilities, and separate
qualification records.

A supported capability must cite executable evidence. Registration, simulation, local
qualification, real-infrastructure qualification, deferral, and failure are distinct states. A
compiled registry snapshot is available through the authenticated control-plane API and CLI.

Reusable conformance runners exercise stable identity, replay/adoption, observation, idempotent
deletion, absence, serverless inventory, lost responses, and runtime readiness against deterministic
fault fixtures. A commit-addressed qualifier emits sanitized JSON while explicitly deferring real
provider evidence.

Public support remains owned by `support.Matrix`; contract registration never changes that matrix.

## Consequences

Provider and runtime additions become reviewable against one vocabulary and fail composition when
their profile does not match the implementation boundary. Capability drift is caught when an
evidence test disappears. Contract evolution requires a new version rather than silently adding
incompatible semantics.

The V1 runtime inspection interface remains deliberately narrow. Buffered/streaming protocol,
cancellation, drain, structured output, tools, and telemetry are separately qualified behaviors;
InferCrane does not force runtime-specific mechanics into one large interface.

## Alternatives

Documentation-only capability tables were rejected because they drift from executable behavior.
Dynamic plugins were rejected because process-local Go composition is sufficient before third-party
binary extensions exist. Treating every OpenAI-compatible endpoint as fully compatible was rejected
because readiness, cancellation, tools, telemetry and shutdown semantics differ.

## Verification

Integration registry tests validate contract versions, deterministic snapshots, evidence
references, duplicate registration, and honest qualification. Conformance tests use a second
fault-injectable provider implementation for elastic and serverless lost-response scenarios.
Workflow composition rejects mismatched profiles. API, CLI, gateway cancellation, provider adapter,
reconciliation and Docker suites provide the referenced local evidence.

