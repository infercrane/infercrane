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

The Prometheus endpoint also exposes accounting queue depth and capacity, persisted and dropped
request-record counters, and persistence failures. These distinguish inference-path health from
telemetry backpressure without putting PostgreSQL on the request path. vLLM running/waiting and
cache signals are persisted with autoscaling decisions; streaming cancellations and upstream
disconnects are persisted as `client_cancelled` and `upstream_disconnect` error types. GPU metrics
remain unavailable unless a qualified provider/runtime collector supplies a trustworthy fresh
measurement.

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
