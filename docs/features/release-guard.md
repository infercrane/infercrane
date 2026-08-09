# Release Guard

Release Guard is a deterministic, persisted comparison between an active revision and its candidate. It never uses an LLM to decide whether a candidate should be promoted.

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

Missing measurements are not fabricated. Output throughput is compared only when both revisions report token usage. TTFT is required before acceptance. A candidate with no healthy ready replica is rejected immediately.

InferCrane does not silently duplicate inference requests. Candidate evidence must come from explicit validation or traffic that the operator knowingly sends to the candidate. Bounded shadow traffic is not enabled in v0.1 until it can be implemented without a second router and with explicit privacy and incremental-cost controls.
