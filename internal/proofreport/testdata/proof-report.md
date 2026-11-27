# Proof Report

Prepared for Example Labs · runs measured 2026-11-02 to 2026-11-03 · InferCrane Proof Report

> **Saving verified on 1 of 2 workloads, with at least +10.0% output per GPU.**

| | |
|---|---|
| Workloads | 2 |
| Saving verified | 1 of 2 workloads |
| Verified saving a month | unknown: no workload has a verified saving with a recorded price and your traffic volume |
| Output per GPU, verified workloads | at least +10.0% |

**How to read this report.** A gain counts only when it cleared the measured noise against your current setup on the same servers, a fresh machine measured it again, and the answers held: a setup that can change answers must hold within 1 point of your current setup's task score. Anything less is "not shown". A number marked *unknown* was not measured or not comparable; nothing is estimated.

## Workload 1: Qwen/Qwen3-8B · chat

Saving verified at 16 at once, 64 at once; not at 128 at once, so no saving is stated for the workload.

Traffic: your captured traffic, replayed by shape (lengths and order, no text): sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.

### Verdict per load

One setup wins at every load measured.

| Load | Decision | Setup | Why |
|---|---|---|---|
| 16 at once | Winner | Up to 256 concurrent requests | The judge chose it at this load, and its saving is verified. |
| 64 at once | Winner | Up to 256 concurrent requests | The judge chose it at this load, and its saving is verified. |
| 128 at once | Winner | Up to 256 concurrent requests | The judge chose it at this load, but its saving is not shown: its gain is not shown under the claim rule: it lacks a fresh machine's confirmation of the gain. |

### At 16 requests at once · run run-a

**Roll out “Up to 256 concurrent requests”: it measured better than your current setup on the same traffic.**

| Measured on | Before (your current setup) | After |
|---|---|---|
| Setup | vLLM defaults | Up to 256 concurrent requests |
| Model | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 |
| GPU | L4 × 1 | L4 × 1 |
| Engine | vllm 0.34.0 | vllm 0.34.0 |
| Image | sha256:1111111111111111111111111111111111111111111111111111111111111111 | sha256:1111111111111111111111111111111111111111111111111111111111111111 |
| Flags | engine defaults | \--max-num-seqs 256 |
| Your hourly price | $0.80 per replica, from your-aws-price-list/2026-11-01 | $0.80 per replica, from your-aws-price-list/2026-11-01 |

- **Workload:** your traffic, replayed, profile chat, 480 requests per run, method campaign-interleaved-control-v3
- **Traffic shape:** sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
- **Baseline:** vLLM defaults, measured side by side on the same traffic: 3 runs of it, 2 of the new setup; measured noise ±3.0%; measured again on fresh servers

| Outcome | Before | After | Change | Noise | Decision |
|---|---|---|---|---|---|
| Output per GPU | 1,000 tokens/s | 1,300 tokens/s | \+30.0% | ±3.0% | improved |
| Cost per 1M output tokens | $0.2222 | $0.1709 | \-23.1% | ±3.0% | improved |
| Time to first token, p95 | 420 ms | 400 ms | \-4.8% | ±8.0% | no measurable difference |
| Output speed per user | 40.0 tokens/s | 47.6 tokens/s | \+19.0% | ±5.0% | improved |

| Your limit | Before | After |
|---|---|---|
| Time to first token, p95 ≤ 1,000 ms | 420 ms · met | 400 ms · met |

- **Answer quality:** Pass · score 81.0% → 80.8% (-0.2 points) · on support-tasks@3 (1,000 prompts) · checked by lm-eval. No Release Guard watch band applies to this campaign; the verdict is the signed eval's pass or fail.
- **Claim shown · output token throughput +27.0% against ±3.0% noise, the smaller of both windows.**
- **Saving verified: output per GPU +30.0%, so the same traffic needs 23.1% fewer GPU hours; cost per 1M output tokens 23.1% lower; $135 a month at your captured traffic ($584 → $449); $1,617 a year at that rate (projection); 730 → 562 GPU hours a month. Basis: measured cost per output token times your replayed output-token volume; assumes servers stay busy, so idle capacity is not included.**

Evidence:

- This run's signed report: `sha256:a09c806d40367e52911a4b0b860faa6073791e5bddf6eda8e016fb4c191fb3eb at /api/v1/optimization/campaigns/run-a/report`
- Check it: `infercrane optimize report run-a --output json > run.json && infercrane optimize report verify run.json`
- Campaign input: `run-a · sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc · Qwen/Qwen3-8B`
- Proposal: `sha256:0cc9a1e4ad13e73cb360882317d8d42c49325d16d357d807df74530f0eb1fec5 · catalog-v1`
- Winner benchmark: `bench-run-a-winner · sha256:78c80827372bcfb89363de9c4d3077d5b922e3e3217e0a2a2f96b9ca6a62fc13 · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-a-winner`
- Winner quality: `q-run-a-winner · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Baseline benchmark: `bench-run-a-control · sha256:815dbae9dbe0364f3053fc26763daa2d8bba403ca9fc687bbe8369174bf78353 · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-a-control`
- Baseline quality: `q-run-a-control · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Replayed traffic: `trace-1 · sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb · 100 requests, content not stored`

Measure it again:

```text
infercrane optimize create Qwen/Qwen3-8B --provider aws --region eu-central-1 --gpu L4 --objective throughput --profile chat --workload-fingerprint sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --target-concurrency 16 --max-ttft-p95-ms 1000 --max-candidates 4
infercrane optimize watch NEW_RUN
infercrane optimize report NEW_RUN --output json > new-run.json
```

### At 64 requests at once · run run-b

**Roll out “Up to 256 concurrent requests”: it measured better than your current setup on the same traffic.**

| Measured on | Before (your current setup) | After |
|---|---|---|
| Setup | vLLM defaults | Up to 256 concurrent requests |
| Model | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 |
| GPU | L4 × 1 | L4 × 1 |
| Engine | vllm 0.34.0 | vllm 0.34.0 |
| Image | sha256:1111111111111111111111111111111111111111111111111111111111111111 | sha256:1111111111111111111111111111111111111111111111111111111111111111 |
| Flags | engine defaults | \--max-num-seqs 256 |
| Your hourly price | $0.80 per replica, from your-aws-price-list/2026-11-01 | $0.80 per replica, from your-aws-price-list/2026-11-01 |

- **Workload:** your traffic, replayed, profile chat, 480 requests per run, method campaign-interleaved-control-v3
- **Traffic shape:** sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
- **Baseline:** vLLM defaults, measured side by side on the same traffic: 3 runs of it, 2 of the new setup; measured noise ±3.0%; measured again on fresh servers

| Outcome | Before | After | Change | Noise | Decision |
|---|---|---|---|---|---|
| Output per GPU | 1,000 tokens/s | 1,200 tokens/s | \+20.0% | ±3.0% | improved |
| Cost per 1M output tokens | $0.2222 | $0.1852 | \-16.7% | ±3.0% | improved |
| Time to first token, p95 | 420 ms | 400 ms | \-4.8% | ±8.0% | no measurable difference |
| Output speed per user | 40.0 tokens/s | 47.6 tokens/s | \+19.0% | ±5.0% | improved |

| Your limit | Before | After |
|---|---|---|
| Time to first token, p95 ≤ 1,000 ms | 420 ms · met | 400 ms · met |

- **Answer quality:** Pass · score 81.0% → 80.8% (-0.2 points) · on support-tasks@3 (1,000 prompts) · checked by lm-eval. No Release Guard watch band applies to this campaign; the verdict is the signed eval's pass or fail.
- **Claim shown · output token throughput +18.0% against ±3.0% noise, the smaller of both windows.**
- **Saving verified: output per GPU +20.0%, so the same traffic needs 16.7% fewer GPU hours; cost per 1M output tokens 16.7% lower; $97.33 a month at your captured traffic ($584 → $487); $1,168 a year at that rate (projection); 730 → 608 GPU hours a month. Basis: measured cost per output token times your replayed output-token volume; assumes servers stay busy, so idle capacity is not included.**

Evidence:

- This run's signed report: `sha256:d4b155d8afdb54813161ff4ea39a73e0616f9421e78705eb5975c7e9c76ebd55 at /api/v1/optimization/campaigns/run-b/report`
- Check it: `infercrane optimize report run-b --output json > run.json && infercrane optimize report verify run.json`
- Campaign input: `run-b · sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc · Qwen/Qwen3-8B`
- Proposal: `sha256:28cf54a6ac729f5bcb1fb60a8391ccf8ae47d0fae2336ae00683e0a2038e89ea · catalog-v1`
- Winner benchmark: `bench-run-b-winner · sha256:87d363b92c9f6fc454cdaea8a60c84f40364af1e092d648399963216a2493146 · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-b-winner`
- Winner quality: `q-run-b-winner · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Baseline benchmark: `bench-run-b-control · sha256:768c4166cebf80c7ecac11e8b92fb64e073de1abf3b7c92261dc814b6a434b83 · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-b-control`
- Baseline quality: `q-run-b-control · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Replayed traffic: `trace-1 · sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb · 100 requests, content not stored`

Measure it again:

```text
infercrane optimize create Qwen/Qwen3-8B --provider aws --region eu-central-1 --gpu L4 --objective throughput --profile chat --workload-fingerprint sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --target-concurrency 64 --max-ttft-p95-ms 1000 --max-candidates 4
infercrane optimize watch NEW_RUN
infercrane optimize report NEW_RUN --output json > new-run.json
```

### At 128 requests at once · run run-d

**Roll out “Up to 256 concurrent requests”: it measured better than your current setup on the same traffic.**

| Measured on | Before (your current setup) | After |
|---|---|---|
| Setup | vLLM defaults | Up to 256 concurrent requests |
| Model | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 | Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 |
| GPU | L4 × 1 | L4 × 1 |
| Engine | vllm 0.34.0 | vllm 0.34.0 |
| Image | sha256:1111111111111111111111111111111111111111111111111111111111111111 | sha256:1111111111111111111111111111111111111111111111111111111111111111 |
| Flags | engine defaults | \--max-num-seqs 256 |
| Your hourly price | $0.80 per replica, from your-aws-price-list/2026-11-01 | $0.80 per replica, from your-aws-price-list/2026-11-01 |

- **Workload:** your traffic, replayed, profile chat, 480 requests per run, method campaign-interleaved-control-v3
- **Traffic shape:** sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
- **Baseline:** vLLM defaults, measured side by side on the same traffic: 3 runs of it, 2 of the new setup; measured noise ±3.0%; not measured again on fresh servers

| Outcome | Before | After | Change | Noise | Decision |
|---|---|---|---|---|---|
| Output per GPU | 1,000 tokens/s | 1,250 tokens/s | \+25.0% | ±3.0% | improved |
| Cost per 1M output tokens | $0.2222 | $0.1778 | \-20.0% | ±3.0% | improved |
| Time to first token, p95 | 420 ms | 400 ms | \-4.8% | ±8.0% | no measurable difference |
| Output speed per user | 40.0 tokens/s | 47.6 tokens/s | \+19.0% | ±5.0% | improved |

| Your limit | Before | After |
|---|---|---|
| Time to first token, p95 ≤ 1,000 ms | 420 ms · met | 400 ms · met |

- **Answer quality:** Pass · score 81.0% → 80.8% (-0.2 points) · on support-tasks@3 (1,000 prompts) · checked by lm-eval. No Release Guard watch band applies to this campaign; the verdict is the signed eval's pass or fail.
- **Claim not shown · lacks a fresh machine's confirmation of the gain.**
- **Saving not shown.**
  - Its gain is not shown under the claim rule: it lacks a fresh machine's confirmation of the gain.

Evidence:

- This run's signed report: `sha256:66385524e2b8000a61c50710907dfa76ab4de511c89376ebfbbc37d491c482b7 at /api/v1/optimization/campaigns/run-d/report`
- Check it: `infercrane optimize report run-d --output json > run.json && infercrane optimize report verify run.json`
- Campaign input: `run-d · sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc · Qwen/Qwen3-8B`
- Proposal: `sha256:ad447be969d4fd7e0f49818a7eb71f911a2d30faf3cdf7af4ee1e3ffc3c49c1e · catalog-v1`
- Winner benchmark: `bench-run-d-winner · sha256:13906abe7eaa0538922551fb1dc3f2f415b7049b4e728d7599e8811784e0390f · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-d-winner`
- Winner quality: `q-run-d-winner · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Baseline benchmark: `bench-run-d-control · sha256:a972a45b5ef174224917cc9329ed0a499e8885c9548be804efc00677ef7133c4 · Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567 revision rev-run-d-control`
- Baseline quality: `q-run-d-control · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Replayed traffic: `trace-1 · sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb · 100 requests, content not stored`

Measure it again:

```text
infercrane optimize create Qwen/Qwen3-8B --provider aws --region eu-central-1 --gpu L4 --objective throughput --profile chat --workload-fingerprint sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --target-concurrency 128 --max-ttft-p95-ms 1000 --max-candidates 4
infercrane optimize watch NEW_RUN
infercrane optimize report NEW_RUN --output json > new-run.json
```

## Workload 2: zai-org/GLM-5 · chat

Saving verified at every load measured (16 at once).

Traffic: your captured traffic, replayed by shape (lengths and order, no text): sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.

- **Output per GPU:** at least +10.0%
- **GPU hours for the same traffic:** at least 9.1% fewer

### At 16 requests at once · run run-c

**Roll out “Up to 256 concurrent requests”: it measured better than your current setup on the same traffic.**

| Measured on | Before (your current setup) | After |
|---|---|---|
| Setup | vLLM defaults | Up to 256 concurrent requests |
| Model | zai-org/GLM-5@0123456789abcdef0123456789abcdef01234567 | zai-org/GLM-5@0123456789abcdef0123456789abcdef01234567 |
| GPU | L4 × 1 | L4 × 1 |
| Engine | vllm 0.34.0 | vllm 0.34.0 |
| Image | sha256:1111111111111111111111111111111111111111111111111111111111111111 | sha256:1111111111111111111111111111111111111111111111111111111111111111 |
| Flags | engine defaults | \--max-num-seqs 256 |
| Your hourly price | unknown: none recorded | unknown: none recorded |

- **Workload:** your traffic, replayed, profile chat, 480 requests per run, method campaign-interleaved-control-v3
- **Traffic shape:** sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
- **Baseline:** vLLM defaults, measured side by side on the same traffic: 3 runs of it, 2 of the new setup; measured noise ±3.0%; measured again on fresh servers

| Outcome | Before | After | Change | Noise | Decision |
|---|---|---|---|---|---|
| Output per GPU | 1,000 tokens/s | 1,100 tokens/s | \+10.0% | ±3.0% | improved |
| Cost per 1M output tokens | unknown | unknown | — | — | unknown: baseline: no sourced hourly price |
| Time to first token, p95 | 420 ms | 400 ms | \-4.8% | ±8.0% | no measurable difference |
| Output speed per user | 40.0 tokens/s | 47.6 tokens/s | \+19.0% | ±5.0% | improved |

| Your limit | Before | After |
|---|---|---|
| Time to first token, p95 ≤ 1,000 ms | 420 ms · met | 400 ms · met |

- **Answer quality:** Pass · score 81.0% → 80.8% (-0.2 points) · on support-tasks@3 (1,000 prompts) · checked by lm-eval. No Release Guard watch band applies to this campaign; the verdict is the signed eval's pass or fail.
- **Claim shown · output token throughput +9.0% against ±3.0% noise, the smaller of both windows.**
- **Saving verified: output per GPU +10.0%, so the same traffic needs 9.1% fewer GPU hours; 730 → 664 GPU hours a month. Monthly amount unknown: cost per output token is unknown: baseline: no sourced hourly price. Basis: measured cost per output token times your replayed output-token volume; assumes servers stay busy, so idle capacity is not included.**

Evidence:

- This run's signed report: `sha256:0160492a02f8ef8b51cc9ddff4a56eec9a50c933e132cc01d4ba3ea8ecc2ac7c at /api/v1/optimization/campaigns/run-c/report`
- Check it: `infercrane optimize report run-c --output json > run.json && infercrane optimize report verify run.json`
- Campaign input: `run-c · sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc · zai-org/GLM-5`
- Proposal: `sha256:0a292af805ba477eed8a617e23749e52ce65f7b9b7542d82ef88e987bdd785a2 · catalog-v1`
- Winner benchmark: `bench-run-c-winner · sha256:466342b1fffd6a9c87c74f09af9f92a5adadc22d93afcfac3c1ac19f451138fb · zai-org/GLM-5@0123456789abcdef0123456789abcdef01234567 revision rev-run-c-winner`
- Winner quality: `q-run-c-winner · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Baseline benchmark: `bench-run-c-control · sha256:9703906c5628be1ce1c3dcbb2bb9041bc272ddd475efbc0bceb1b7002d3004cd · zai-org/GLM-5@0123456789abcdef0123456789abcdef01234567 revision rev-run-c-control`
- Baseline quality: `q-run-c-control · sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd · support-tasks@3 signed by customer-key`
- Replayed traffic: `trace-1 · sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb · 100 requests, content not stored`

Measure it again:

```text
infercrane optimize create zai-org/GLM-5 --provider aws --region eu-central-1 --gpu L4 --objective throughput --profile chat --workload-fingerprint sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb --target-concurrency 16 --max-ttft-p95-ms 1000 --max-candidates 4
infercrane optimize watch NEW_RUN
infercrane optimize report NEW_RUN --output json > new-run.json
```

## Limits

- Every number was measured on the hardware, engine version and traffic named beside it. It says nothing about other GPUs, engines, versions, models or traffic.
- Prompts and answers were not stored: traffic is replayed by its shape, and answers are compared by their check's score.
- Qwen/Qwen3-8B · chat: answer quality was checked on support-tasks@3 (1,000 prompts) only; other tasks are not measured.
- Qwen/Qwen3-8B · chat was measured at 16, 64, 128 requests at once; other loads are not covered.
- zai-org/GLM-5 · chat: answer quality was checked on support-tasks@3 (1,000 prompts) only; other tasks are not measured.
- zai-org/GLM-5 · chat was measured at 16 requests at once; other loads are not covered.
- Cost uses the hourly price recorded with each measurement, which is your compute's price; where none was recorded, cost is unknown, never estimated.
- Monthly figures are cost per output token times your captured output-token volume. They assume servers stay busy, so idle capacity is not included.
- A year is twelve months at the captured rate; growth and seasonality are not included.

## Unknown, never estimated

- zai-org/GLM-5 · chat at 16 at once: Cost per 1M output tokens: baseline: no sourced hourly price
- zai-org/GLM-5 · chat at 16 at once: monthly saving: cost per output token is unknown: baseline: no sourced hourly price

---

Report digest `sha256:f6f9cb3fe5bb7d70dd330b4b929ac77c0509caa0fa108ed63b1d6cf9a3f24527`. Every number comes from the runs' persisted measurements.

Signed by key sha256:3ab86a91c1e46782. Verify with infercrane report proof verify FILE, where FILE is this report saved with --output json.
