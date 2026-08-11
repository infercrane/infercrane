# ADR 0024: Persist provider-product profile identity

## Status

Accepted for v1.5.

## Context

A cloud is not a lifecycle contract. EC2, Auto Scaling Groups, EKS, SageMaker, and Bedrock have
different ownership and convergence semantics even though all are AWS products. Selecting only by
`cloud + runtime` becomes ambiguous and can replay a durable operation through a different adapter
after a restart or binary upgrade.

## Decision

Immutable deployment revisions may persist `provider_adapter`. Composition registers one or more
runtime-specific adapters for a cloud. An omitted adapter is valid only when the registration is
unambiguous or exactly one adapter is explicitly marked as the default. Persisted replica operations
continue resolving by their durable adapter identity.

Provider profiles remain separate from public qualification policy. Registration documents a
capability and ownership boundary; only executable conformance evidence permits local qualification,
and real-provider evidence is a separate state.

Managed groups and endpoints are not forced into the one-resource-per-replica `ElasticProvider`
contract. ASG/MIG own their instances, SageMaker/Vertex own endpoint children, Kubernetes services
own pods, and Bedrock remains governed external capacity.

## Consequences

- Existing unambiguous deployments remain backward compatible.
- Multiple provider products can coexist without provider conditionals in lifecycle workflows.
- Operators can inspect the exact adapter selected by a revision.
- Installing multiple non-default adapters requires explicit selection rather than an unsafe guess.
- Registered managed profiles remain non-executable until their own contracts and simulations land.
