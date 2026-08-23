# AWS real-infrastructure evidence

This evidence was captured on 2026-08-23 from the private AWS BYOC qualification in
`eu-central-1`. It is release qualification, not a public performance comparison.

## Final cache-aware implementation run

The post-fix matrix passed again at InferCrane commit
`292935f9899d61149d08d34232b6b50b0389e9e5` with run ID
`20260823T053950Z-292935f-aws`. The commit-addressed acceptance image prevented a stale local image
from being mistaken for the checked-out source. The AWS gate completed in 4,485 seconds and the
retained qualification document reports `status: passed`.

| Runtime path | Success | TTFT p50 | TTFT p95 | TPOT p95 | Latency p95 | Output tok/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| vLLM 0.22.0 | 5/5 | 49.0 ms | 56.4 ms | 21.8 ms | 718.5 ms | 44.3 |
| SGLang 0.5.12 | 5/5 | 95.6 ms | 97.2 ms | 21.3 ms | 712.6 ms | 44.6 |
| custom OCI using the qualified vLLM image | 5/5 | 52.9 ms | 56.8 ms | 21.7 ms | 717.4 ms | 44.5 |

Every row used AIPerf 0.9.0 with five streaming chat requests, concurrency one, 128 input tokens,
32 output tokens, and random seed 17. It remains qualification evidence rather than a throughput,
cost, or runtime-comparison claim.

The run also exercised the new cold-start boundaries. vLLM waited about six minutes for zonal AWS
capacity and completed the full deploy in 17 minutes 6 seconds. Its immutable image miss took 4
minutes 4 seconds. SGLang waited about 11.5 minutes for capacity, transferred its image in 5 minutes
20 seconds, and completed in 23 minutes 50 seconds. The custom-OCI path obtained capacity
immediately, transferred the same vLLM image in 4 minutes 4 seconds, and completed in 11 minutes 10
seconds. SGLang and vLLM both served the exact model commit named below.

All three managed instances reached `terminated`. The independent final AWS inventory returned no
active InferCrane-managed instances and no active managed EBS volumes. The dedicated qualification
runner remained intentionally available and is not tagged as a managed workload.

These observations prove the miss path and the stage instrumentation. They do not prove an
operator-prewarmed AMI image hit or provider-native model-artifact cache. Those remain distinct
qualification boundaries.

## Model-diverse performance qualification

The portable harness defaults to all qualified runtime paths. A focused model run can set
`INFERCRANE_V1_RUNTIMES=vllm` and point `INFERCRANE_V1_VLLM_SPEC` at one pinned model spec. This
reduces paid repetition while preserving the complete vLLM protocol smoke, performance matrix,
durable deletion, and independent inventory checks. The next model-diverse sequence uses the pinned
Mistral 7B, DeepSeek R1 Distill 7B, and Granite 3.3 8B examples separately. One passing model does
not imply that another model family, tokenizer, or architecture is qualified. Set
`INFERCRANE_V1_VLLM_FEATURES` to the capabilities claimed by that model profile; chat and streaming
remain mandatory, while `tools` and `structured` must not be claimed from a model that was not
actually tested for them.

## Identity and scope

- InferCrane commit: `48d957d3e1014c038456d9b4ebd604921aaef792`
- Acceptance run: `20260823T031051Z-48d957d-aws`
- Accelerator: one L40S (`g6e.xlarge`) per workload
- Storage: encrypted 100 GiB `gp3` root volume, delete on termination
- Model: `Qwen/Qwen3-8B`
- Immutable model commit: `b968826d9c46dd6066d109eabc6255188de91218`
- Network: private worker address; the runtime worker key was obtained from AWS Secrets Manager and was not placed in argv

The gate completed in 3,306 seconds and passed real vLLM 0.22.0, SGLang 0.5.12, and
digest-pinned custom-OCI lifecycle paths. Each path proved readiness/model identity, a buffered
request, a streaming request, AIPerf execution, durable deletion, and route withdrawal. The vLLM
path additionally proved tool calling and structured output. SGLang's live startup evidence named
the intended immutable model commit.

## Qualification benchmark

Each row is one AIPerf qualification sample with five requests, concurrency one, and random seed
17. Values are retained to make the test reproducible and diagnosable. Five requests are not enough
for a product performance claim, provider comparison, or capacity recommendation.

| Runtime path | Success | TTFT p50 | TTFT p95 | TPOT p95 | Latency p95 | Output tok/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| vLLM 0.22.0 | 5/5 | 54.2 ms | 55.9 ms | 21.7 ms | 717.1 ms | 44.4 |
| SGLang 0.5.12 | 5/5 | 94.4 ms | 187.4 ms | 21.3 ms | 806.6 ms | 43.5 |
| custom OCI using the qualified vLLM image | 5/5 | 48.6 ms | 56.3 ms | 21.7 ms | 718.1 ms | 44.5 |

## Startup and cleanup evidence

Observed deployment readiness was approximately 11 minutes 14 seconds for vLLM, 11 minutes 10
seconds for SGLang, and 10 minutes 36 seconds for custom OCI. Console evidence for the custom-OCI
path separated approximately five minutes of image transfer, 94 seconds of model download, and the
remaining model/GPU initialization. These cold paths are correct but are not presented as
world-class startup performance.

All three workload instances reached `terminated`; their delete-on-termination root volumes were
removed. The final InferCrane provider inventory contained zero managed active workload instances.
The qualification runner itself remained intentionally available and is not a workload resource.

The follow-up startup change at `e71b655` adds exact-digest image-cache reuse, credential-free stage
timestamps, statistically guarded observed-readiness estimates, and corrected capacity accounting.
That change passes the full local, PostgreSQL, Kind, container race, security-audit, and documentation
matrix. The final run above proves its real image-miss instrumentation; its prewarmed-image hit path
remains a separate qualification boundary.

A follow-up run at `429e466` exercised the smaller official SGLang 0.5.12 `runtime` image. The
cache-aware stage evidence measured an image miss from 05:03:39Z through 05:08:47Z. The container
then crash-looped before readiness because the published image lacked the Python `distro`
dependency. The candidate was rejected and reverted; the qualified full SGLang image remains the
default. This failed run is compatibility evidence, not a completed qualification or performance
comparison.
