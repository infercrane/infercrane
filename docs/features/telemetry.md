---
title: Inference telemetry
description: Normalize latency, token, error, runtime, deployment, revision, and provider evidence.
---

# Inference telemetry

InferCrane records operational measurements for each request without storing prompts or generated content. Request bodies are used only to forward the OpenAI-compatible request, and response chunks are observed transiently to extract timing and usage metadata when the runtime supplies it.

Persisted request dimensions are:

- deployment and active revision
- provider, runtime, and compute mode
- GenAI operation name (`chat`)
- requested logical model and runtime-reported response model
- OpenTelemetry GenAI schema identity (`https://opentelemetry.io/schemas/gen-ai/1.42.0`)
- HTTP status, error type, and whether the response streamed
- response model, when returned by the runtime

Persisted measurements are request latency, time to first response byte/chunk at the InferCrane
gateway boundary, and input/output token counts when vLLM returns OpenAI usage fields. For streaming
requests this timing maps to `gen_ai.response.time_to_first_chunk`; InferCrane does not claim it is
the model server's internal `gen_ai.server.time_to_first_token`. Missing token usage remains unknown;
InferCrane does not estimate it. Replica identity remains unset when the standalone router does not
provide a trustworthy selected-worker identity.

The corresponding OpenTelemetry GenAI concepts are `gen_ai.operation.name`,
`gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`,
`gen_ai.request.stream`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
`gen_ai.server.request.duration`, and `gen_ai.response.time_to_first_chunk`. InferCrane-specific
deployment, revision, provider/runtime, compute-mode, and operation dimensions are retained with
the durable request record so decisions can be reproduced later. `runpod` is a grounded custom
provider value because the convention permits provider-specific names outside its well-known list.

Aggregated deployment statistics expose request rate, error rate, latency p50/p95, TTFT p50/p95, and observed input/output tokens per second over the selected window. Token throughput is emitted only from runtime-reported usage.

The endpoint monitoring API returns the same content-free evidence as a bounded time series, plus
binding/deployment/revision attribution and persisted lifecycle overlays. The console uses this API;
it never queries PostgreSQL or an OpenTelemetry collector from the browser. Missing buckets and
unsupported runtime metrics stay null or explicitly unavailable rather than becoming zero.

Each operator-visible metric also carries a normalized evidence envelope: value, unit,
availability, evidence class, source, observation time, freshness boundary, sample count, and a
machine-readable absence reason. Availability distinguishes `available`, `stale`, `not_observed`,
and `unsupported`; evidence class distinguishes measured, provider-reported, modeled, and estimated
values. This lets CLIs, SDKs, and consoles preserve the same truth boundary instead of inferring one
from a nullable number.

```bash
infercrane observe coder-production
```

The human view includes the latest traffic summary. Use the authenticated console for 1-hour to
30-day charts and correlated scaling or release events.

## Import existing production telemetry

Customers can keep Baseten, Fireworks, Datadog, or an OpenTelemetry pipeline as their monitoring
source and post a strict content-free observation batch:

```bash
curl -fsS "$INFERCRANE_API_URL/api/v1/deployments/coder-production/traffic-observations" \
  -H "Authorization: Bearer $INFERCRANE_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "schema_version":"infercrane.traffic-observations/v1",
    "source":"opentelemetry",
    "observations":[{
      "source_request_id":"trace-1/span-1",
      "started_at":"2026-09-20T10:00:00Z",
      "latency_ms":184,
      "input_tokens":512,
      "output_tokens":96,
      "status_code":200
    }]
  }'
```

The endpoint accepts at most 1,000 observations per batch and rejects unknown fields, including
prompt or response content. `source_request_id` is deterministically scoped to the tenant,
deployment, and source, so retrying an import updates the same request record.

After import, `POST /api/v1/deployments/{name}/replays` captures an immutable workload-shape trace;
`GET /api/v1/deployments/{name}/replays` lists its history. Replay evidence contains arrival,
duration, token-count, session-hash, streaming, and concurrency shapes only. It does not duplicate
live traffic. Real prompt shadowing remains a separate explicit data-policy and approval boundary.

The Prometheus endpoint also exposes accounting queue depth and capacity, persisted and dropped
request-record counters, and persistence failures. These distinguish inference-path health from
telemetry backpressure without putting PostgreSQL on the request path. Serving-engine
running/waiting gauges are persisted with autoscaling decisions, and the wider engine summary is
persisted as operational measurements (see [Serving-engine metrics](#serving-engine-metrics));
streaming cancellations and upstream disconnects are persisted as `client_cancelled` and
`upstream_disconnect` error types. GPU metrics remain unavailable unless a qualified
provider/runtime collector supplies a trustworthy fresh measurement.

## Serving-engine metrics

Every 30 seconds the control plane reads the Prometheus endpoint (`{target}/metrics`) of each
healthy, self-hosted replica of a running deployment and persists one bounded, content-free
summary per scrape. The engine is recognized from its metric namespace (`vllm:`, `sglang:`, or
`llamacpp:`), not from the deployment's runtime name. Enable the endpoint where it is off by
default: SGLang needs `--enable-metrics` and llama.cpp server needs `--metrics`.

The scrape presents the same credential the gateway route already presents to that replica.
External provider targets and direct serverless provider targets are never scraped, because their
URLs are provider APIs. A tenant compute replica without a derivable worker credential is
skipped rather than scraped unauthenticated. A payload from an unsupported runtime is ignored and
produces no evidence.

| Measurement | Unit | vLLM | SGLang | llama.cpp server | Across replicas |
|---|---|---|---|---|---|
| `runtime_requests_running` | count | `num_requests_running` | `num_running_reqs` | `requests_processing` | sum |
| `runtime_requests_waiting` | count | `num_requests_waiting` | `num_queue_reqs` | `requests_deferred` | sum |
| `runtime_kv_cache_timeseries` | percent | `kv_cache_usage_perc` | `token_usage` | `kv_cache_usage_ratio` (older builds) | maximum |
| `runtime_queue_time_p95` | ms | `request_queue_time_seconds` | `queue_time_seconds` | not exported | maximum |
| `runtime_prefill_time_p95` | ms | `request_prefill_time_seconds` | `per_stage_req_latency_seconds{stage="prefill_forward"}` | not exported | maximum |
| `runtime_decode_time_p95` | ms | `request_decode_time_seconds` | not exported | not exported | maximum |
| `runtime_ttft_p95` | ms | `time_to_first_token_seconds` | `time_to_first_token_seconds` | not exported | maximum |
| `runtime_internal_itl` (p95) | ms | `inter_token_latency_seconds` | `inter_token_latency_seconds` | not exported | maximum |
| `runtime_internal_tpot` (p95) | ms | `request_time_per_output_token_seconds` | `request_time_per_output_token_seconds` | not exported | maximum |
| `runtime_tpot_mean` | ms | TPOT histogram mean | TPOT histogram mean | `tokens_predicted_seconds_total / tokens_predicted_total` | maximum |
| `runtime_requests_completed` | count | `e2e_request_latency_seconds` count | `e2e_request_latency_seconds` count | not exported | sum |
| `runtime_preemptions` | count | `num_preemptions_total` | `num_retracted_requests_total` | not exported | sum |
| `runtime_prefix_cache_hit_rate` | ratio | `prefix_cache_hits / prefix_cache_queries` | `cached_tokens_total / prompt_tokens_total` | not exported | weighted by tokens |
| `runtime_spec_decode_acceptance` | ratio | `spec_decode_num_accepted_tokens / spec_decode_num_draft_tokens` | `spec_accept_rate` | `spec_decode_num_accepted_tokens / spec_decode_num_draft_tokens` | weighted by drafts |

Engine metric names omit their namespace prefix in the table. The three names that existed before
engine collection (`runtime_internal_itl`, `runtime_internal_tpot`, and
`runtime_kv_cache_timeseries`) keep their API spelling; they now carry a p95 latency or the latest
KV-cache occupancy.

The running, waiting, and KV-cache values come from the current scrape. Every other value is a
window: the difference between the current scrape and the oldest retained scrape of the same
engine process, up to five minutes back. The first scrape after the control plane starts, after a
rollout changes the active revision, after the replica URL changes, or after the engine restarts
(any cumulative counter moving backwards) therefore records gauges only. A window with no
completed requests records latency as unavailable, while preemption and completion counts are a
measured zero. Speculative acceptance needs at least one draft token in the window, so an engine
with speculation off reports it as unavailable rather than 0%. SGLang reports its acceptance as an
engine-computed gauge, used only while `spec_num_draft_tokens` is positive.

Latency p95 values are estimated from the engine histogram's bucket deltas with the same linear
interpolation Prometheus `histogram_quantile` uses. A p95 that falls in the `+Inf` bucket has no
finite bound and is reported as unavailable. Series that differ only by tensor-, pipeline-, or
expert-parallel rank labels are counted once. Data-parallel series are added. SGLang per-priority
queue breakdowns are ignored in favor of its `priority=""` total. `runtime_tpot_mean` is
request-weighted for vLLM and SGLang and token-weighted for llama.cpp server.

Each summary is valid for 90 seconds (three scrapes) and is attached to the deployment's active
revision at ingestion, like other collector snapshots. Monitoring shows the latest summary per
replica. Without one, every engine measurement is `not_observed` with the reason
`no serving-engine metrics scrape has reported for the active revision`. When the engine is
reachable but does not export a signal, the reason says so instead. After the validity window the
envelope becomes `stale` with no value. Engine values are never shown as zero because they are
missing.

The monitoring API also reports `shared_prefix_request_ratio`. It is the share of chat requests
whose leading system messages repeat those of another request in the window, computed from the
gateway's keyed digest of those messages. Non-chat operations and imported observations without
a digest are left out of the denominator, because their prefix shape is unknown.

Metric names were checked against the vLLM, SGLang, and llama.cpp server sources in November
2026. The tests use synthetic expositions in `internal/metrics/testdata`. Collection against a
live engine of each kind has not yet been qualified.

## Diagnose a bottleneck from engine signals

`GET /api/v1/endpoints/{name}/optimization-readiness` (algorithm `monitoring-stage-triage-v2`)
adds engine signals to the gateway's stage triage. Engine rules run only after the existing gates:
fresh request evidence, at least 20 requests, and an error rate at or below 2%. Each rule that
fires is returned in `findings` with the observed value, the threshold, and the technique families
worth measuring. Families are candidates for a measured comparison, never changes InferCrane
applies by itself, and the custom-kernel gate stays closed.

```bash title="Read engine findings"
curl -fsS "$INFERCRANE_API_URL/api/v1/endpoints/coder-production/optimization-readiness" \
  -H "Authorization: Bearer $INFERCRANE_API_KEY" | jq '.assessment.findings'
```

| Rule | Fires when | Technique families |
|---|---|---|
| `kv_cache.preemption` | preemptions per completed request ≥ 0.01 (at least one request in the denominator) | FP8 KV cache, KV cache offload, max-sequence budget |
| `kv_cache.saturation` | KV-cache occupancy ≥ 90% while requests wait in the engine or the queue rule holds | FP8 KV cache, KV cache offload |
| `queue.engine_wait` | engine queue p95 ≥ 20% of gateway p95 latency and ≥ 25 ms, with no gateway queue signal | batch-token budget, continuous batching, replica scaling |
| `prefill.long_input` | first-token time is the bottleneck and mean reported input length ≥ 2,048 tokens | chunked prefill, batch-token budget |
| `decode.low_concurrency` | generation is the bottleneck and running requests per replica ≤ 4 | speculative decoding, weight quantization (reversed when acceptance is already ≥ 0.5) |
| `prefix.low_reuse` | ≥ 50% of known-shape chat requests share a system prefix and the prefix-cache hit rate is < 0.25 over ≥ 100 tokens | prefix cache, prefix-aware routing |
| `speculative.low_acceptance` | speculative acceptance < 0.5 over ≥ 100 draft tokens | speculative method or draft model, speculation length |

KV-cache pressure takes precedence over the other stages, because a full cache causes queueing
and preemption rather than following from them. Below it, the stage order is unchanged: queueing,
then first-token, then generation. When the gateway has no first-token time (for example
non-streaming traffic), the engine's TTFT p95 is used. Prefix-reuse and speculative findings apply
to any stage. Candidate families are ordered with families from findings about the diagnosed
bottleneck first, then families from other findings, then the stage's general families. A missing
engine signal never satisfies a rule. When shared prefixes are common but the hit rate is unknown,
the engine prefix-cache hit rate is listed in `required_evidence`, as is the running-requests
gauge for a generation bottleneck without it.

These defaults are policy choices, not measured optimums:

- 90% KV occupancy leaves little headroom for growing sequences. High occupancy without waiting
  requests is a well-packed batch and does not fire.
- One preemption per hundred completed requests means the engine regularly recomputes evicted
  work.
- 2,048 mean input tokens is where the per-step batched-token budget starts to decide how prefill
  interleaves with decode.
- Four or fewer running sequences per replica leaves decode memory-bandwidth bound, which is where
  fewer target-model steps or smaller weights help most.
- A 25% token hit rate with half the requests sharing a system prefix suggests the cache is off,
  too small, or split across replicas. The rate is token-weighted, so long unique suffixes lower
  it.
- Below 50% acceptance, speculative decoding commonly spends more on drafts than it saves.

The running-requests value is the latest scrape, not a window average, so treat the low-concurrency
finding as a reason to measure rather than proof.

## NVIDIA GPU evidence

When an environment already runs [NVIDIA DCGM Exporter](https://docs.nvidia.com/datacenter/dcgm/latest/reference/dcgm-exporter-metrics.html),
an operator can import a bounded snapshot without exposing the exporter to the browser:

```bash
infercrane telemetry collect dcgm coder-production \
  --url http://127.0.0.1:9400/metrics \
  --selector deployment=coder-production \
  --replica replica-03 \
  --ttl 2m
```

Use `--file dcgm.prom` instead of `--url` for an offline snapshot. `--selector` accepts exact,
comma-separated Prometheus label matches. Shared exporters should always be scoped to the intended
workload. `DCGM_FI_DEV_GPU_UTIL` is percent by default; pass `--utilization-unit ratio` only when an
upstream compatibility layer deliberately rewrites it to `0..1`.

InferCrane normalizes only:

| DCGM input | InferCrane measurement | Aggregation |
|---|---|---|
| `DCGM_FI_DEV_GPU_UTIL` | `gpu_utilization` in percent | maximum selected GPU pressure |
| `DCGM_FI_PROF_GR_ENGINE_ACTIVE` | `gpu_utilization` in percent | maximum; fallback only |
| `DCGM_FI_DEV_FB_USED` | `gpu_memory` in bytes | sum of selected GPUs |
| `DCGM_FI_DEV_GPU_TEMP` | `gpu_temperature` in Celsius | maximum selected temperature |
| `DCGM_FI_DEV_POWER_USAGE` | `gpu_power` in watts | sum of selected GPUs |
| `DCGM_FI_DEV_XID_ERRORS` | `gpu_xid_errors` count | sum of selected GPUs |

The snapshot is attached to the active revision at ingestion time. It cannot silently move to a
later rollout. Once its TTL expires, the monitoring API returns a stale envelope with no value.
This integration proves the control-plane semantics locally; actual DCGM field availability varies
by GPU and DCGM version and still requires real-hardware qualification. Collector snapshots follow
the configured high-volume request-evidence retention period (`INFERCRANE_REQUEST_RETENTION_HOURS`).
