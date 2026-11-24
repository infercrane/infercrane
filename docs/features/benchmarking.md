---
title: Reproducible benchmarking
description: Run AIPerf workloads and persist exact model, runtime, hardware, and workload evidence.
---

# Reproducible benchmarking

InferCrane delegates load generation and measurement to [AIPerf](https://github.com/ai-dynamo/aiperf). It does not contain a second load generator.

```console
infercrane benchmark qwen-prod --requests 1000 --concurrency 32
```

Use a versioned workload profile when you want comparable evidence for a specific operating goal:

```console
infercrane benchmark mistral-prod --profile interactive \
  --ttft-slo-ms 250 --tpot-slo-ms 25

infercrane benchmark mistral-prod --profile throughput
infercrane benchmark mistral-prod --profile long-context
infercrane benchmark mistral-prod --profile buffered
```

For repeatable optimization evidence, use independent AIPerf runs instead of treating one run as a
stable result. Three runs are a useful engineering smoke test; five are the default target for a
decision or public result:

```console
infercrane benchmark mistral-prod --profile interactive \
  --runs 3 --warmup-requests 8 --run-cooldown-seconds 5

infercrane benchmark mistral-prod --profile throughput \
  --runs 5 --warmup-requests 8 --run-cooldown-seconds 10 \
  --arrival-pattern poisson --request-rate 12
```

`constant`, `poisson`, and `gamma` arrival patterns model different request timing. The request rate
and pattern must be supplied together. InferCrane pools the record-level results, retains the run
count and load shape, and stores Student-t 95% intervals and coefficient of variation for
throughput, tail latency, and goodput when at least two runs contain the metric.

Profiles define load shape, not runtime tuning claims. `interactive`, `balanced`, `throughput`,
`buffered`, `long-context`, `long-generation`, and `overload` record their immutable
`benchmark-profile-v1` identity inside the workload evidence. Explicit token or concurrency flags
override profile defaults and remain visible in the result. `buffered` is the only profile that does
not stream, so its runs record p95 request latency but no time to first token or time per output
token.

For a bounded qualification sweep across concurrency 1, 8, 32, and 128 plus long prompt,
long-generation, streaming, buffered, and overload shapes:

```console
scripts/benchmark-matrix.sh mistral-prod --approve-load
```

The script refuses to send load without explicit approval, archives each control-plane result,
validates failure bounds, and produces `.infercrane/performance/RUN_ID/matrix.json`. Set
`INFERCRANE_BENCHMARK_TTFT_SLO_MS`, `INFERCRANE_BENCHMARK_TPOT_SLO_MS`, and/or
`INFERCRANE_BENCHMARK_LATENCY_SLO_MS` to compute measured goodput: successful requests per second
that satisfy every declared threshold. A zero threshold means no SLO was declared and goodput stays
unavailable rather than being invented.

The profile matrix deliberately changes workload shape by objective, so its rows are not a
scaling curve. Use the separate same-workload campaign when comparing concurrency behavior:

```console
scripts/benchmark-concurrency-sweep.sh mistral-prod --approve-load
```

It holds request count, input tokens, output tokens, seed, streaming mode, revision, and SLOs
constant while running concurrency `1`, `8`, `32`, and `128`. The resulting
`.infercrane/performance/RUN_ID-concurrency-sweep/sweep.json` is the only one of these two artifacts
that supports a concurrency scaling comparison. Override the bounded defaults with
`INFERCRANE_BENCHMARK_SWEEP_*` variables; every override remains in persisted workload evidence.

The CLI submits the benchmark through the authenticated control-plane API. The control plane resolves the active immutable revision, runs AIPerf against the logical InferCrane endpoint, and persists the result. A fixed dataset seed defaults to `17` and can be changed with `--random-seed`. AIPerf `0.12.0` is pinned so flag semantics and exports cannot drift between candidates.

Release Guard validation can benchmark an isolated candidate explicitly:

```console
infercrane benchmark qwen-prod --revision active --requests 100 --concurrency 4 --random-seed 17
infercrane benchmark qwen-prod --revision candidate --requests 100 --concurrency 4 --random-seed 17
```

The candidate command selects one healthy ready replica deterministically and runs AIPerf directly
from the control plane using its server-side worker credential. It never exposes that credential
to the CLI. This is explicit synthetic validation, not shadow traffic: InferCrane does not copy any
user request or conversation content. It creates additional inference work and may incur provider
cost. Active and candidate results are eligible for Release Guard comparison only when AIPerf tool
version and workload parameters match exactly.

Each result records the immutable ModelArtifact, runtime and configuration, revision, provider,
region, GPU type and grounded GPU count, compute mode, workload parameters, TTFT, TPOT, request
latency, throughput, SLO goodput when declared, errors, timestamp, and exact reproduction command. The persisted command uses
a portable local export prefix rather than the deleted temporary execution directory, and replaces
the credential with its environment-variable reference. GPU utilization is attached only when
measured DCGM evidence for the exact immutable revision overlaps the complete benchmark window;
provider-reported or stale samples remain `null`. Goodput remains `null` without SLO thresholds.
Cost metadata becomes available only when fresh, revision-bound `deployment_hourly_rate` evidence
covers the run; otherwise it contains `available: false` and a reason. None are fabricated.

## What “fast” means

InferCrane does not promise one globally fastest configuration. It measures the exact combination
and lets the operator choose the objective:

| Objective | Primary evidence |
| --- | --- |
| Interactive latency | TTFT and TPOT percentiles under the declared concurrency |
| Throughput | Output tokens/second and requests/second with bounded errors |
| Cost efficiency | Measured tokens per sourced currency unit; unavailable until cost evidence exists |
| Long context | TTFT, total latency, memory safety, and errors at the declared prompt shape |
| Fast startup | Time-to-ready waterfall and verified image/artifact cache state |
| Reliability | Goodput under explicit SLOs, rollout evidence, and active-stream safety |

Runtime arguments are immutable revision data. Tune a candidate, run the same profile against active
and candidate revisions, and let Release Guard compare matching evidence before promotion. This is
how InferCrane can provide a fast qualified default without turning an upstream vLLM or SGLang flag
into an unsupported universal claim.

For reviewed generation models, scaffold two candidate projects and preserve their different runtime
arguments explicitly:

```console
infercrane workload init ./mistral-interactive \
  --recipe mistral-7b-instruct --profile vllm-interactive
infercrane workload init ./mistral-throughput \
  --recipe mistral-7b-instruct --profile vllm-throughput
```

Deploy them as isolated candidates, benchmark both with the *same* workload profile, then use Lab or
Release Guard. Serving-profile names describe runtime configuration; benchmark-profile names describe
load. Neither is evidence until the resulting measurements are persisted.

Compare multiple measured configurations with the same workload:

```console
infercrane lab 'mistralai/Mistral-7B-Instruct-v0.3@IMMUTABLE_COMMIT' \
  --objective interactive \
  --profile interactive \
  --max-ttft-p95-ms 200ms \
  --max-hourly-cost 3

infercrane lab 'mistralai/Mistral-7B-Instruct-v0.3@IMMUTABLE_COMMIT' \
  --objective cost-efficiency \
  --profile throughput
```

Lab emits `RECOMMENDED` only when candidates share one exact workload digest and the required
metric - and sourced hourly cost for cost efficiency - is present. Unlike workload shapes remain
`UNRANKED`.

The workload digest covers the load that was sent, including the run count and warmup. Results that
are stored beside the load are excluded: the `confidence_95` intervals from repeated runs and an
optimization campaign's `measured` control comparison. Two repeated benchmarks of the same load
therefore compare with each other. Lab versions before `inference-lab-v4` hashed those intervals,
which gave every multi-run benchmark a workload of its own and kept it out of every ranking.

Optimization campaigns use a stricter method than a single `infercrane benchmark` command: warmup,
four candidate runs each paired with the campaign control runs around it, and an interval widened
for the number of candidates and for the measured difference between two machines before a gain
counts. See [how a campaign measures candidates](/features/optimization#how-a-campaign-measures-candidates).

InferCrane normalizes the unit carried by each AIPerf latency record to milliseconds and rejects
unknown units instead of silently mislabeling evidence. Percentiles reconstructed from those records
use the nearest-rank definition. Small samples remain visibly weak evidence: Release Guard separately
requires the persisted minimum request count, and matching workload/tool identity, before a comparison
can pass.

InferCrane asks AIPerf for the `records` export level. This contains per-request measurements but not the raw request/response export, so prompt and generated content are not persisted. Results remain in the InferCrane PostgreSQL database and are never uploaded by default.

The control-plane image pins AIPerf `0.12.0`. For a standalone control plane, install it with
`pipx install aiperf==0.12.0`; the benchmark adapter rejects another version rather than silently
mixing tool semantics.

Each AIPerf run starts in its own process group. AIPerf starts worker processes, and when a model
server dies mid-run the main process can exit while those workers keep running. InferCrane then
waits at most 30 seconds for them and stops the whole group, so a measurement cannot hang on them.
A cancelled measurement stops the group at once. A run whose main process exited successfully keeps
its result; a failed run keeps its error.

## How fast a disconnect frees the engine

When a client stops reading a stream, the serving engine should cancel the request. Until it does,
the request keeps generating and holds a batch slot that a waiting request could use. Under an
admission cap, an abandoned stream that the engine has not cancelled holds one of the capped slots.
Engine upgrades can change how quickly a disconnect is noticed, so optimization campaigns measure it.

When a campaign scrapes a server's engine metrics (its runtime contract declares `/metrics`), every
AIPerf run is followed by three cancel-release probes. Each probe:

1. Reads the engine's running-request gauge from `/metrics` and waits up to 5 seconds for it to read
   0. The gauge is `vllm:num_requests_running`, `sglang:num_running_reqs`, or
   `llamacpp:requests_processing`, read by the same parser as every other engine scrape. If the
   engine does not go idle, the probe is `unknown` and sends nothing.
2. Streams one chat completion to the address AIPerf measured, with `max_tokens` 1024 and
   `ignore_eos`, so the request is still generating when the probe leaves.
3. Reads the stream until the gauge shows the request running, for at most 30 seconds. A gauge that
   never rises makes the probe `unknown`. It is never recorded as 0 ms.
4. Closes the HTTP/1.1 connection and reads `/metrics` every 25 ms until the gauge is back to 0. The
   time from closing the connection to the scrape that reads 0 is the release time. It can be late by
   one poll plus one scrape round trip.
5. If the gauge is not back to 0 within 10 seconds, the probe is `timed_out`, with a reason and no
   number.

Each campaign run keeps the result in `measured.runs[].cancel_release`:

| Field | Meaning |
| --- | --- |
| `engine` | `vllm`, `sglang`, or `llamacpp`, from the metrics the probe read |
| `probes`, `released`, `timed_out` | Probes run, probes with a release time, and probes that timed out |
| `max_release_ms` | The slowest release. `null` when no probe released or any probe timed out, because a timed-out probe was slower than `timeout_ms` by an unknown amount |
| `timeout_ms` | The release bound |
| `samples` | Each probe's `outcome` (`released`, `timed_out`, or `unknown`), its `release_ms` when released, and a `reason` otherwise |

What the number covers, and what it does not:

- The probe sends its request to the address AIPerf loads, and reads `/metrics` from that address,
  the same URL AIPerf scrapes. A runtime contract can declare only `/metrics`, and no API input
  names a metrics URL. In a campaign the address is the candidate's own server, so the release time
  covers the engine and any provider proxy in front of it. It does not include the InferCrane
  gateway, which passes a disconnect on by cancelling its upstream request.
- The gauge is the engine's own report. An engine that does not refresh the gauge once it is idle
  makes its probes time out or stay `unknown`; it never produces a fast release. In the vLLM 0.22.1
  source, an aborted request triggers one more scheduler step that publishes the gauge. SGLang
  0.5.20 republishes the gauge as soon as its scheduler is idle and the gauge is stale. SGLang
  0.5.12 republishes idle metrics only every 30 seconds, so its probes time out or stay `unknown`.
  That is missing evidence, not a measured 10-second hold.
- The probe assumes it is the only traffic on the server, which is true after AIPerf finishes in a
  campaign. A gauge above 1 during a probe makes it `unknown`.
- Probes follow every run of both arms, so the pairing of candidate and control runs is unchanged. A
  probe that times out can leave one request generating up to 1024 tokens before the next run.
- A model whose context window is shorter than the prompt plus 1024 tokens refuses the probe, which
  is then `unknown` with the HTTP status.
- Ordinary `infercrane benchmark` runs do not scrape engine metrics and run no probe.

The result is stored with each run inside the benchmark row's `workload_json`, not as a column of its
own. It describes a single run, and the pooled candidate row has no single value that would stand for
all of them.
