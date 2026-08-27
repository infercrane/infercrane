# RunPod Qwen real-infrastructure evidence

Date: 2026-08-27  
Baseline verdict: **passed for the exact Qwen3-8B/L40S tuple below**  
Frontier verdict: **Qwen3.8-Flash-Next on two RunPod B200s is not qualified**  
Passing run: `qwen3-8b-l40s-e-20260827`

This record is intentionally tuple-specific. It does not qualify every vLLM model, GPU, RunPod
datacenter, or concurrency shape. Credential-free raw captures remain outside the repository at
`/tmp/infercrane-e2e-report/qwen3-8b-l40s-e-20260827/` on the qualification host.

## Passing baseline tuple

| Component | Identity |
| --- | --- |
| Model | `Qwen/Qwen3-8B@b968826d9c46dd6066d109eabc6255188de91218` |
| Runtime | `vLLM 0.22.1` |
| Runtime image | `vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7` |
| InferCrane | `05093a375ffd797aa5f69cfdf56110966198b06f` |
| Provider | RunPod Secure Cloud, `US-NC-1`, one native Pod, one NVIDIA L40S |

The foreground deploy client was interrupted after InferCrane persisted its deterministic provider
identity. The local control plane was restarted, resumed the same durable operation, and reached the
stable route. Deploy submission to the acceptance harness's ready boundary took 242 seconds. The
provider reported the Pod allocation at `05:47:57Z`; the vLLM model loaded 15.27 GiB in 9.82 seconds,
including 2.54 seconds for five safetensors shards. InferCrane recorded the runtime ready at
`05:51:13Z`.

The first bounded streaming request returned HTTP 200 with time-to-first-response-byte 0.480 seconds
and total time 1.849 seconds. This is an HTTP response boundary, not a provider-internal token event.

## Request measurements

Every cell used 16 requested output tokens and fixed random seed 53. Concurrency 1 and 8 used 20
requests; concurrency 32 used 32 requests so concurrency never exceeded the request count.

| Mode | Concurrency | Succeeded / failed | TTFT p95 | TPOT p95 | Latency p95 | Requests/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Streaming | 1 | 20 / 0 | 266.23 ms | 21.72 ms | 591.25 ms | 1.96 |
| Streaming | 8 | 20 / 0 | 7208.18 ms | 23.87 ms | 7565.75 ms | 2.25 |
| Streaming | 32 | 32 / 0 | 499.90 ms | 30.57 ms | 870.58 ms | 36.61 |
| Buffered | 1 | 20 / 0 | unavailable | unavailable | 603.91 ms | 1.93 |
| Buffered | 8 | 20 / 0 | unavailable | unavailable | 828.67 ms | 9.91 |
| Buffered | 32 | 32 / 0 | unavailable | unavailable | 935.49 ms | 34.13 |

The streaming concurrency-8 result is anomalous relative to both concurrency 1 and 32. A single
short run is functional evidence, not a representative performance distribution; do not use these
numbers for sizing without repeated exact-tuple trials.

The real provider path initially exposed an inference compression defect: gzip bytes could reach the
CLI without a preserved encoding header. The passing run exercised the remediation: upstream
compression negotiation is isolated at the gateway and the CLI defensively recognizes unlabelled
gzip. Buffered and streaming requests both passed.

## Release Guard, durability, and cleanup

- Release Guard deterministically returned `REJECT` with reason `candidate_not_ready`.
- Active revision `a4aa50ae0fcc02a20aeb185f2308ecf6-rev-1` was unchanged after rejection and a
  post-rejection inference request succeeded.
- Delete was submitted durably, the control plane was restarted, and the operation completed.
- Direct RunPod inventory ended with zero Pods and zero Serverless endpoints.

## Qwen3.8-Flash-Next negative evidence

The exact attempted frontier tuple was
`Qwen/Qwen3.8-Flash-Next-FP8@bcd9f01ddc9cff2316eb84281bebcd5b058bddce`, dedicated vLLM integration
image `sha256:fc120ece0a388cc0aa1caad4a9f1cd92113484ab7ec2fd0efadd62585be05bf8`, and two B200 GPUs in
`US-NC-2` with an identity-bound 220 GiB network volume.

One run loaded the 172.78 GiB checkpoint in 211.69 seconds, completed engine initialization and
multimodal warmup, and published a stable InferCrane route. Its first request exposed the gzip defect,
so no benchmark claim was accepted. Two later fresh engine starts failed closed with no KV-cache
blocks after graph compilation; a forced safetensors-prefetch experiment reduced weight load to
92.90 seconds but was also memory-unsafe and was removed. Every run cleaned provider compute to zero.

The upstream recipe validates TP2 on GB300, not this RunPod B200 topology. InferCrane therefore keeps
the B200 profile as an explicit unqualified candidate. A production claim requires a stable exact
topology—preferably the upstream-recommended four-GPU tray or available GB300 capacity—plus repeated
ready/request/benchmark/cleanup passes.
