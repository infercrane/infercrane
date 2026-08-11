# Runtime Contract V1

Status: implemented contract foundation for v0.2.0. vLLM is locally qualified; real-GPU runtime
evidence remains deferred until the consolidated v1 qualification.

## Ownership

Inference engines own execution, batching, model loading, cache behavior and engine-native metrics.
InferCrane owns immutable runtime identity, capability validation, lifecycle coordination,
normalized evidence, routing membership and safe revision policy.

## Required contract

A runtime profile declares:

- stable adapter and semantic contract version
- engine/version and immutable workload identity
- protocol and supported operations
- readiness and model-identity inspection
- buffered and streaming behavior
- cancellation and graceful drain/shutdown behavior
- telemetry endpoints and normalized metric mappings
- tool, structured-output and embedding capabilities where tested
- compatibility constraints for artifact, accelerator and runtime arguments

Declared, probed, simulated, locally qualified and real-qualified capabilities remain distinct.
Unsupported behavior fails before paid provisioning whenever it is knowable.

Production composition binds each runtime inspector to its validated `RuntimeProfile`. Provisioning
and reconciliation resolve that immutable binding by runtime identity; an adapter cannot execute
under a different runtime's capability claims, and an unregistered runtime remains unroutable.

## Custom OCI workloads

InferCrane may accept an immutable OCI image plus explicit protocol, port, health, telemetry and
shutdown declarations. It does not build or execute an image builder, sandbox arbitrary code, or
infer compatibility from an `OpenAI-compatible` label alone.
