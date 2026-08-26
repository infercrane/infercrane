# AWS real-infrastructure evidence

This evidence was captured on 2026-08-23 from the private AWS BYOC qualification in
`eu-central-1`. It is release qualification, not a public performance comparison.

## Complete Mistral matrix and bounded concurrency sweep on A10G

Run `20260823T195259Z-07efd96-aws-mistral-a10g` exercised the pinned
`mistralai/Mistral-7B-Instruct-v0.3` revision at InferCrane commit `07efd96` using vLLM 0.22.0 on
one AWS A10G (`g5.xlarge`). AWS had reported definitive `InsufficientInstanceCapacity` for the
requested L40S in each configured Frankfurt availability zone, so the operator explicitly selected
the separately qualified A10G path; InferCrane did not silently change accelerator class.

The seven-profile matrix completed 1,920 requests with zero HTTP failures:

| Workload profile | Concurrency | Requests | TTFT p95 | TPOT p95 | Latency p95 | Output tok/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| interactive | 1 | 256 | 164.3 ms | 31.86 ms | 4,208.7 ms | 30.36 |
| balanced | 8 | 256 | 915.5 ms | 41.95 ms | 5,584.0 ms | 184.44 |
| throughput | 32 | 512 | 1,350.8 ms | 58.13 ms | 15,377.8 ms | 544.95 |
| buffered | 8 | 256 | unavailable | unavailable | 5,550.6 ms | 184.77 |
| long-context (8,192 input tokens) | 4 | 64 | 4,467.1 ms | 75.29 ms | 21,627.7 ms | 51.04 |
| long-generation (1,024 output tokens) | 4 | 64 | 315.6 ms | 35.97 ms | 18,828.0 ms | 108.31 |
| overload | 128 | 512 | 26,875.4 ms | 147.83 ms | 52,242.6 ms | 726.63 |

Buffered TTFT and TPOT remain unavailable by protocol rather than being fabricated as zero. The
overload row proves completion under the bounded campaign; its 26.9-second TTFT p95 is evidence that
admission policy must bound concurrency for latency-sensitive workloads, not evidence that 128-way
concurrency is an acceptable interactive configuration.

A second campaign held request count, token shape, random seed, streaming mode, revision, and
runtime configuration constant while changing only concurrency. All 512 requests succeeded:

| Concurrency | Requests | TTFT p95 | TPOT p95 | Latency p95 | Output tok/s |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 128 | 168.6 ms | 31.83 ms | 2,170.9 ms | 29.42 |
| 8 | 128 | 1,000.7 ms | 48.26 ms | 3,339.3 ms | 156.58 |
| 32 | 128 | 3,312.4 ms | 90.87 ms | 7,560.2 ms | 322.07 |
| 128 | 128 | 19,017.0 ms | 204.92 ms | 22,298.6 ms | 363.94 |

The deployment reached the stable route in 12 minutes 19 seconds. Closed startup markers measured
4 minutes 50 seconds for the immutable runtime-image miss. The vLLM container started 23 seconds
later; model materialization, runtime initialization, health, and stable-route publication accounted
for the remaining interval. This is a cold-miss baseline, not a prewarmed-startup claim.

Durable deletion completed in 4 minutes 45 seconds. The retained local archive has SHA-256
`24ad89818f758b70f5c29e4587eb50fbe8b4393bdcce8162c6b95432217bb52b` and excludes credentials.
Independent final inventory returned zero active InferCrane-managed instances and volumes. The
qualification runner, its root volume, NAT gateway, Elastic IP, three private test subnets, route
table, security groups, IAM roles/profiles, artifact bucket, and test secret were then deleted.

## Model-diverse workload matrix: partial Mistral qualification

Run `20260823T092044Z-c310780-mistral-final` exercised the pinned
`mistralai/Mistral-7B-Instruct-v0.3` revision at InferCrane commit `c310780` using vLLM 0.22.0 on
one AWS L40S (`g6e.xlarge`). The three completed profiles persisted 1,024 successful requests with
no failed requests:

| Workload profile | Concurrency | Requests | TTFT p95 | TPOT p95 | Latency p95 | Output tok/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| interactive | 1 | 256 | 59.7 ms | 20.23 ms | 2,623.9 ms | 48.65 |
| balanced | 8 | 256 | 130.3 ms | 21.97 ms | 2,891.2 ms | 355.54 |
| throughput | 32 | 512 | 199.0 ms | 29.64 ms | 7,666.8 ms | 1,103.55 |

These rows compare workload shapes on one runtime configuration. They do not prove that the
throughput-oriented runtime profile is faster than the interactive runtime profile. That claim
requires two immutable candidate revisions to run the same workload digest before Inference Lab or
Release Guard may rank them.

They are also not a concurrency scaling curve because the profile shapes differ. The
`scripts/benchmark-concurrency-sweep.sh` campaign holds request count, token shape, seed, streaming
mode, revision, and SLOs constant at concurrency 1, 8, 32, and 128. Real AWS rows for that campaign
remain pending and must not be inferred from this table.

The deployment completed in 11 minutes 10 seconds. Closed startup markers measured approximately
4 minutes 4 seconds for the immutable runtime-image pull and 45 seconds from runtime start to the
container-start marker. The remaining time included AWS placement, model-artifact materialization,
vLLM initialization, health, and stable-route publication. This is a measured cold miss, not a
prewarmed startup claim.

The subsequent buffered profile exposed a real AIPerf 0.9 integration defect: InferCrane emitted
the unsupported `--no-streaming` option. Commit `c5f8987` fixes buffered mode by omitting the
opt-in `--streaming` flag and adds a regression test that rejects the nonexistent inverse flag.
Repository verification and the focused race test pass after the fix. A paid retry was attempted
in each existing qualification subnet across `eu-central-1a`, `eu-central-1b`, and
`eu-central-1c`; AWS reported insufficient `g6e.xlarge` capacity before creating an instance.

Therefore the real-AWS qualification state is deliberately split:

- `interactive`, `balanced`, and `throughput`: passed for the exact Mistral/vLLM/L40S tuple above;
- `buffered`: implementation fixed and locally regression-tested, real-GPU requalification pending;
- `long-context`, `long-generation`, and `overload`: real-GPU qualification pending;
- DeepSeek R1 Distill 7B and Granite 3.3 8B: pinned qualification paths prepared, real runs pending.

The failed matrix run deleted its workload instance and root volume. Every capacity retry created
zero workload resources. Direct final inventory returned zero active InferCrane-managed instances
and zero active managed EBS volumes, and the reusable qualification runner was stopped.

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

## Artifact-snapshot qualification discovery

Run `20260824T232100Z-aws-mistral-cache-hit` attempted the first exact-identity EBS artifact-cache
qualification for
`mistralai/Mistral-7B-Instruct-v0.3@c170c708c41dac9275d15a8fff4eca08d52bab71`. The encrypted,
tagged 40 GiB snapshot passed all pre-launch validation and exactly one private `g6e.xlarge` worker
was created. Before the artifact volume could be mounted, Docker exhausted the worker's explicit
100 GiB root filesystem while extracting a current vLLM FlashInfer layer. Provider console evidence
ended at `image_pull_start` with `no space left on device`; InferCrane never reported an artifact
cache hit or runtime readiness.

The operation was cancelled and direct final inventory confirmed that the instance and all three
attached volumes were absent. The first 200 GiB retry then exposed the root cause: the AMI declared
`/dev/sda1`, while the adapter had hard-coded `/dev/xvda`; AWS kept the 75 GiB AMI root and attached
the larger encrypted disk as an unused secondary volume. That worker was also cancelled and removed.
The adapter now discovers the AMI root device, validates the source snapshot size, overrides that
exact mapping, records the device in its adoption identity, and keeps the conservative 200 GiB
default.
This failed attempt is storage-sizing and startup-diagnosis evidence only. It is not cache-hit,
latency, runtime, or model-serving evidence. A clean 200 GiB retry must still prove
`startup_evidence.artifact_cache: hit`, serve buffered and streaming requests, persist the bounded
benchmark row, and restore provider inventory to baseline.

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
