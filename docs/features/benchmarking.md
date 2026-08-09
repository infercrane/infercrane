# Reproducible benchmarking

InferCrane delegates load generation and measurement to [AIPerf](https://github.com/ai-dynamo/aiperf). It does not contain a second load generator.

```console
infercrane benchmark qwen-prod --requests 1000 --concurrency 32
```

The CLI submits the benchmark through the authenticated control-plane API. The control plane resolves the active immutable revision, runs AIPerf against the logical InferCrane endpoint, and persists the result. A fixed dataset seed defaults to `17` and can be changed with `--random-seed`.

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
latency, throughput, errors, timestamp, and exact reproduction command. The persisted command uses
a portable local export prefix rather than the deleted temporary execution directory, and replaces
the credential with its environment-variable reference. Goodput and GPU telemetry remain explicit
`null` values, while cost metadata contains `available: false` and a reason when AIPerf or the
provider did not measure them; none are fabricated.

InferCrane asks AIPerf for the `records` export level. This contains per-request measurements but not the raw request/response export, so prompt and generated content are not persisted. Results remain in the InferCrane PostgreSQL database and are never uploaded by default.

The control-plane image pins its AIPerf version. For a standalone control plane, install AIPerf with `pipx install aiperf`; `infercrane doctor` verifies that the configured executable is available.
