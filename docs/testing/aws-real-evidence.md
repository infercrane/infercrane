# AWS real-infrastructure evidence

This evidence was captured on 2026-08-23 from the private AWS BYOC qualification in
`eu-central-1`. It is release qualification, not a public performance comparison.

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
matrix. Its real prewarmed-image hit path remains a separate qualification boundary.
