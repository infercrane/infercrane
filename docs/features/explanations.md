# Deterministic explanations

InferCrane explanations are projections of persisted state, events, decisions, policies, and measurements. They do not call an LLM and do not infer facts that were not recorded.

```console
infercrane explain qwen-prod
infercrane explain scaling qwen-prod
infercrane explain rollout qwen-prod
infercrane explain cold-start qwen-prod
```

The general explanation reports observed deployment state and unhealthy replica evidence. Scaling explanations reproduce the latest persisted autoscaling action, reason, signal snapshot, and evaluation timestamp. Rollout explanations reproduce the Release Guard evaluation ID, active and candidate revisions, decision, reason codes, metric snapshot, policy snapshot, and timestamp. Cold-start explanations use the provider-worker observation and TTFT evidence described in [Cold-start intelligence](cold-starts.md).

Every form supports `--output json`. Repeating an explanation against unchanged persisted state produces the same explanation code and evidence. If no evaluation exists, InferCrane says so instead of inventing a cause.
