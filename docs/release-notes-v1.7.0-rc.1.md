# InferCrane v1.7.0-rc.1

v1.7 adds immutable evidence-backed Model Recipes and a deterministic Inference Lab over persisted
benchmark history.

## Highlights

- Capture a deployment's exact artifact, runtime, provider, workload, and benchmark provenance.
- Search content-addressed recipes through CLI, API, and SDKs.
- Compare candidate configurations by measured TTFT, throughput, errors, SLO, and sourced cost metadata.
- Persist every Lab input and result digest without prompts or generated content.

## Known limitations

- Lab v1 compares evidence already present in the tenant; it does not orchestrate paid candidate runs.
- Only measured evidence is emitted. Modeled and heuristic sources remain future explicit adapters.
- No public recipe registry or trust-signing workflow exists.
- Real GPU benchmark evidence remains deferred.

