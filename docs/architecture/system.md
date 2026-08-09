# System architecture

InferCrane separates persistent control-plane state from the latency-sensitive inference data
plane.

```text
CLI / provisioners ──> PostgreSQL <── reconciler
                                      │
                                      ├──> vLLM worker health/model APIs
                                      └──> instance-local vLLM Router processes

OpenAI client ──> gateway ──> atomic route snapshot ──> local router ──> vLLM workers
                       └──> buffered request accounting ──> PostgreSQL
```

## Boundaries

- `cmd/infercrane` composes dependencies and owns process lifecycle.
- `internal/gateway` handles authentication, OpenAI request validation, streaming, and telemetry.
- `internal/routes` is the lock-protected in-memory routing snapshot used by the data path.
- `internal/reconcile` converts persisted desired state and worker health into local routes.
- `internal/router` supervises the upstream router as an opaque process.
- `internal/store` owns all PostgreSQL access and embedded schema migrations.
- Provider, runtime, and metrics packages isolate external system contracts.

Horizontal replicas share PostgreSQL but not loopback routers. Each replica uses a stable unique
instance ID, records instance-owned router generations, and rebuilds its local route directory.

The root [architecture overview](../architecture.md) remains the short public explanation. This
document is the engineering-level component map.
