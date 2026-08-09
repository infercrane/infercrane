# ADR 0009: Separate qualified support from backend registration

- Status: Accepted
- Date: 2026-08-09

## Context

InferCrane v0.1 is qualified only for vLLM on RunPod, using SkyPilot for elastic capacity and the
RunPod API for provider-native serverless capacity. That release boundary must not become an
architectural dependency. Provider names, runtime defaults, and cleanup allowlists had begun to
leak into generic lifecycle and reconciliation code, making a future integration require edits to
unrelated control-plane logic.

## Decision

Public support policy and executable integration registration are separate concerns. The versioned
support matrix states which cloud, runtime, and compute-mode combinations are qualified for a
release. Composition registers elastic and serverless backends that satisfy narrow lifecycle
contracts. Registration alone never advertises an integration as supported.

An elastic backend has a stable adapter name, cloud, runtime, and `ReplicaProvider`. Registries
resolve provisioning by cloud/runtime and replay or deletion by the adapter identity persisted on
the replica. Direct serverless targets use the same composition principle during reconciliation.
Cleanup queries receive the registered provider identity; persistence contains no provider
allowlist. Runtime identity is preserved from the deployment specification and defaulted only at
an input boundary.

Adding a qualified integration therefore requires an adapter, explicit process composition,
support-matrix qualification, configuration and documentation, and real lifecycle acceptance. It
does not require a provider conditional in the durable workflow.

## Consequences

RunPod and vLLM remain the honest v0.1 support statement while other adapters can be added without
changing lifecycle algorithms. A cloud may register different backends for different runtimes.
Historical replicas with no adapter identity can be replayed only when exactly one backend is
registered, avoiding an unsafe guess in an ambiguous process.

Runtime-specific health and metrics implementations remain adapters. vLLM Router remains the
single router for qualified standalone vLLM replicas; this decision does not introduce a second
general-purpose router or promise arbitrary engines.

## Alternatives

A provider switch inside each workflow was rejected because every integration would fork durable
semantics. Treating every registered adapter as publicly supported was rejected because existence
is not qualification. A plugin framework and dynamic loading were rejected as unnecessary before
there is a second implementation.

## Verification

Registry tests cover independent cloud/runtime resolution, durable adapter lookup, duplicate
registration rejection, and unambiguous legacy replay. Support-policy tests prove the v0.1 matrix
is explicit and that a later release can qualify a new combination without changing validation
logic. Workflow, reconciliation, control API, store, and CLI unit suites cover the composed v0.1
backends.

