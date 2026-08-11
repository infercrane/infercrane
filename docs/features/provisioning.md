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

The v0.1 support matrix and the backend registry are deliberately separate. The matrix qualifies
vLLM with RunPod for public use. At process composition, an elastic backend binds a stable adapter
name, cloud, runtime, and provider implementation; durable workflows select it without provider
conditionals. Serverless and direct-target reconciliation follow the same pattern. Registering an
adapter does not make it supported until its configuration, documentation, and real lifecycle
acceptance are complete. See [ADR 0009](/adr/0009-qualified-support-and-backend-registration).

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

Capacity availability is an optional backend capability, not a provider conditional in the
lifecycle state machine. Before the first create, the workflow discovers the deterministic
resource. If it already exists, InferCrane adopts it without consulting mutable stock. If it is
absent, the registered advisor may return `available`, `constrained`, `unavailable`, or `unknown`;
the complete result is persisted in operation progress. Explicitly unavailable capacity defers
creation with a retryable error. Constrained capacity proceeds with a warning because a stock query
is a point-in-time signal, not a reservation. An unavailable advisory service does not make an
otherwise healthy provider unusable.

Provider observation details are also classified at the integration boundary. Known container
bootstrap failures such as an interrupted image pull or exhausted host storage remain retryable
while the provider retains the resource, but operation progress exposes the concrete boundary and
an explicit cancel-before-replacement instruction. InferCrane does not create a second resource or
silently select different hardware in response to these diagnostics.

The v0.1 RunPod advisor queries secure-cloud GPU stock without creating a Pod. A region-qualified
request is clearly labeled as using a global signal because the provider response cannot prove
availability in one requested region. Credentials are sent in an authorization header and are never
written to URLs, checkpoints, or durable events.

The superseded synchronous `Deploy`/`Destroy` adapter and direct-running operation store APIs have
been removed. Provider mutation is reachable only through leased workflow handlers.

Runtime inspection requires both a healthy endpoint and the expected served model. Metrics parsing
normalizes supported vLLM metric aliases and ignores malformed or non-finite samples.

## Qualification

SkyPilot requires credentialed acceptance tests for each supported provider/region/GPU combination.
The production image pins SkyPilot 0.13.0 with its RunPod extra; changing this pin requires repeating
the real-cloud lifecycle and zero-leak acceptance gate.
Development fake workers demonstrate behavior only and cannot support performance or reliability
claims.

Placement prefers eligible warm model caches before known lower prices and is deterministic for
equal candidates. A missing or stale price is explicit and must never be represented as zero cost.
