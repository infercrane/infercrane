# Provisioning, runtimes and metrics

Status: Existing targets implemented; SkyPilot experimental

## Entry points

- SkyPilot adapter: `internal/provision/skypilot.go`
- Declarative specification: `internal/spec/spec.go`
- vLLM runtime inspection: `internal/runtime/vllm.go`
- vLLM Prometheus parsing: `internal/metrics/vllm.go`
- Provider-neutral capacity contracts and placement: `internal/capacity`
- Timestamped pricing contract: `internal/pricing`

## Contract

Provisioners return ordinary targets so routing and reconciliation remain provider-neutral.
Provider resource IDs and non-secret details may be stored for inspection and cleanup. Worker API
keys are injected as SkyPilot secrets and are never stored in provider detail JSON.

The SkyPilot replica lifecycle is discovery-first and keyed by a stable external name. `Ensure`
checks JSON cluster inventory before an asynchronous named launch, `Observe` refreshes cluster state
and resolves the exposed endpoint, `Delete` treats an absent cluster as success, and `Inventory`
returns owned clusters for leak reconciliation. Repeating ensure or delete does not create or
destroy a second resource. The durable workflow persists the external key and deterministic
cluster identity before calling these methods.

The `deployment.converge`/`replica.provision` workflow persists replica intent and that deterministic
cluster identity before calling `sky launch`. Every retry re-runs discovery, provider observation,
and vLLM readiness checks before registering a route. Its delete path re-observes asynchronous
deletion and does not mark a replica deleted while it remains in provider inventory. These handlers
are locally qualified with fault-injected providers; real RunPod qualification is still required.

The superseded synchronous `Deploy`/`Destroy` adapter and direct-running operation store APIs have
been removed. Provider mutation is reachable only through leased workflow handlers.

Runtime inspection requires both a healthy endpoint and the expected served model. Metrics parsing
normalizes supported vLLM metric aliases and ignores malformed or non-finite samples.

## Qualification

SkyPilot requires credentialed acceptance tests for each supported provider/region/GPU combination.
Development fake workers demonstrate behavior only and cannot support performance or reliability
claims.

Placement prefers eligible warm model caches before known lower prices and is deterministic for
equal candidates. A missing or stale price is explicit and must never be represented as zero cost.
