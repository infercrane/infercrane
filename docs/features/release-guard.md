---
title: Release Guard
description: Compare active and candidate evidence with deterministic, persisted promotion policy.
---

# Release Guard

Release Guard is a deterministic, persisted comparison between an active revision and its candidate. It never uses an LLM to decide whether a candidate should be promoted.

<img src="/images/diagrams/release-guard.svg" alt="Animated Release Guard flow comparing persisted active and candidate evidence against deterministic policy before returning wait, reject, or accept." />

```console
infercrane rollout validate qwen-prod --acknowledge-validation-cost --wait
infercrane rollout inspect qwen-prod
infercrane rollout policy get qwen-prod
```

An evaluation returns one of three decisions:

- `ACCEPT`: enough evidence exists and every measured regression is within policy.
- `REJECT`: readiness failed, proven compatibility mismatches, or a measured regression exceeds
  policy.
- `WAIT`: the candidate is ready but there is not enough trustworthy evidence yet.

Release Guard V2 also supports exact model/runtime compatibility evidence, explicit synthetic
validation, sourced cost regression, and a bounded post-promotion automatic rollback window. Each
evaluation snapshots the exact policy, active and candidate metrics, reason codes, and revision
identities. Repeating an explanation therefore produces the same answer from stored evidence.

`infercrane rollout inspect DEPLOYMENT` renders the latest persisted active/candidate comparison as
a metric table and shows unavailable measurements explicitly. JSON output retains the complete
policy, measurements, reason codes, revision identities, and evaluation timestamp.

Missing measurements are not fabricated. Output throughput is compared only when both revisions report token usage. TTFT is required before acceptance. A candidate with no healthy ready replica is rejected immediately.

Because a candidate is deliberately absent from the logical route, operators gather bounded
candidate evidence explicitly with AIPerf rather than duplicating production requests:

```console
infercrane rollout policy set qwen-prod \
  --require-compatibility \
  --require-synthetic \
  --auto-rollback \
  --auto-rollback-window 300 \
  --validation-max-requests 100 \
  --validation-max-concurrency 4

infercrane rollout validate qwen-prod \
  --requests 100 \
  --concurrency 4 \
  --acknowledge-validation-cost \
  --wait
```

Release Guard uses a persisted active/candidate benchmark pair only when tool version and workload
parameters match. The evaluation snapshot records `aiperf_benchmark` plus both benchmark IDs as its
evidence source. Otherwise it waits for comparable evidence instead of mixing measurements.

InferCrane does not silently duplicate inference requests. `rollout validate` prints a cost/privacy
notice, enforces persisted request and concurrency ceilings, runs AIPerf against each revision, and
then queues the durable evaluation. It does not retain prompts or generated output.

When automatic rollback is enabled, promotion retains the previous healthy capacity until the
persisted observation monitor reaches `ACCEPT` or `REJECT`. The monitor snapshots its policy and
deadline at creation, so a concurrent policy edit cannot silently weaken an in-flight decision. Rejection atomically restores the old
revision and target set, waits for its router generation, drains active streams safely, and deletes
only failed revision capacity. A restart resumes the same monitor and deadline.

After acceptance, issue an [Inference Passport](/features/inference-passports) to make the exact
revision and release evidence independently verifiable.
