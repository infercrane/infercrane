# Release Guard

Release Guard is a deterministic, persisted comparison between an active revision and its candidate. It never uses an LLM to decide whether a candidate should be promoted.

<img src="/images/diagrams/release-guard.svg" alt="Animated Release Guard flow comparing persisted active and candidate evidence against deterministic policy before returning wait, reject, or accept." />

```console
infercrane rollout evaluate qwen-prod --wait
infercrane rollout inspect qwen-prod
infercrane rollout policy qwen-prod
```

An evaluation returns one of three decisions:

- `ACCEPT`: enough evidence exists and every measured regression is within policy.
- `REJECT`: readiness failed or a measured regression exceeds policy.
- `WAIT`: the candidate is ready but there is not enough trustworthy evidence yet.

The v0.1 policy persists minimum request counts and maximum permitted TTFT, request-latency, error-rate, and output-throughput regressions. Each evaluation snapshots the exact policy, active and candidate metrics, reason codes, and revision identities. Repeating an explanation therefore produces the same answer from stored evidence.

`infercrane rollout inspect DEPLOYMENT` renders the latest persisted active/candidate comparison as
a metric table and shows unavailable measurements explicitly. JSON output retains the complete
policy, measurements, reason codes, revision identities, and evaluation timestamp.

Missing measurements are not fabricated. Output throughput is compared only when both revisions report token usage. TTFT is required before acceptance. A candidate with no healthy ready replica is rejected immediately.

Because a candidate is deliberately absent from the logical route, operators gather bounded
candidate evidence explicitly with AIPerf rather than duplicating production requests:

```console
infercrane benchmark qwen-prod --revision active --requests 100 --concurrency 4 --random-seed 17
infercrane benchmark qwen-prod --revision candidate --requests 100 --concurrency 4 --random-seed 17
infercrane rollout evaluate qwen-prod --wait
```

Release Guard uses a persisted active/candidate benchmark pair only when tool version and workload
parameters match. The evaluation snapshot records `aiperf_benchmark` plus both benchmark IDs as its
evidence source. Otherwise it waits for comparable evidence instead of mixing measurements.

InferCrane does not silently duplicate inference requests. Candidate evidence must come from explicit validation or traffic that the operator knowingly sends to the candidate. Bounded shadow traffic is not enabled in v0.1 until it can be implemented without a second router and with explicit privacy and incremental-cost controls.
