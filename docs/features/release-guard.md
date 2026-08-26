---
title: Release Guard
description: Compare active and candidate evidence with deterministic, persisted promotion policy.
---

# Release Guard

Release Guard is a deterministic, persisted comparison between an active revision and its candidate. It never uses an LLM to decide whether a candidate should be promoted.

<img src="/images/diagrams/release-guard.svg" alt="Animated Release Guard flow comparing persisted active and candidate evidence against deterministic policy before returning wait, reject, or accept." />

<Warning>
`rollout validate` sends bounded inference to active and candidate capacity. It can incur provider
cost and exposes the validation fixture to the selected runtimes. Review the persisted request and
concurrency ceilings, use non-sensitive data, and run it only after exact-environment qualification.
</Warning>

```console
infercrane rollout validate qwen-prod --acknowledge-validation-cost --wait
infercrane rollout inspect qwen-prod
infercrane rollout policy get qwen-prod
```

Inspect the persisted policy before gathering evidence. Deployment policy includes the current
TTFT, latency, error-rate, throughput, compatibility, validation, cost, quality, and rollback
thresholds. The current CLI can change the extended compatibility, validation, cost, quality, and
rollback fields; it preserves the existing core performance thresholds.

For a stable endpoint serving-plan comparison, set an explicit TTFT regression limit and minimum
sample count before evaluation:

```bash
infercrane endpoint guard coder-production \
  --minimum-requests 20 \
  --max-ttft-regression 15

infercrane endpoint guard coder-production --evaluate
```

The first command persists policy only. `--evaluate` compares the active and candidate plans and
never promotes traffic. Use `infercrane endpoint inspect coder-production` to retrieve plan IDs
before a separate, explicit promotion.

For deployment revisions, promotion is an authenticated, traffic-changing operation and requires the
latest persisted evaluation to be `ACCEPT` for the same candidate and still-current active revision:

<Warning>
Promotion changes future production routing and later drains old capacity. Confirm the active and
candidate revision IDs, direct provider ownership, accepted Guard snapshot, stream-drain evidence,
and rollback target before executing it. It does not qualify missing real-environment evidence.
</Warning>

```bash
infercrane rollout inspect qwen-prod
infercrane rollout promote qwen-prod REVISION_ID \
  --reason "Release Guard accepted" \
  --wait
```

`promote` atomically changes the active revision and target generation, then drains the old
generation. Validation traffic may incur the explicit cost and privacy impact acknowledged by
`rollout validate`; promotion itself does not duplicate requests. Release Guard still requires real
active/candidate evidence for the exact environment.

An evaluation returns one of three decisions:

- `ACCEPT`: enough evidence exists and every measured regression is within policy.
- `REJECT`: readiness failed, proven compatibility mismatches, or a measured regression exceeds
  policy.
- `WAIT`: the candidate is ready but there is not enough trustworthy evidence yet.

Release Guard also supports exact model/runtime compatibility evidence, explicit synthetic
validation, sourced cost regression, and a bounded post-promotion automatic rollback window. Each
evaluation snapshots the exact policy, active and candidate metrics, reason codes, and revision
identities. Repeating an explanation therefore produces the same answer from stored evidence.

`infercrane rollout inspect DEPLOYMENT` renders the latest persisted active/candidate comparison as
a metric table and shows unavailable measurements explicitly. JSON output retains the complete
policy, measurements, reason codes, revision identities, and evaluation timestamp.

Missing measurements are not fabricated. Output throughput is compared only when both revisions report token usage. TTFT is required before acceptance. A candidate with no healthy ready replica is rejected immediately.

Endpoint Release Guard never applies one primary deployment's metrics to an arbitrary routing
graph. It currently qualifies single-primary comparisons, a primary change with an unchanged
`primary-fallback` graph, and an append-only managed fallback whose immutable policy includes
explicit privacy consent and hard request/cost reservations. Weighted changes or other unmeasured
topology changes persist `INCONCLUSIVE` with `serving_plan_topology_unqualified`; they cannot be
promoted until per-binding evidence exists.

Infrastructure can be healthy while model behavior has regressed. For quality-sensitive changes,
run a task-specific customer-owned evaluation and attach its
[signed aggregate evidence](/features/semantic-quality). Release Guard can require comparable
suite/evaluator versions, a minimum score, and a bounded regression. InferCrane still does not select
an LLM judge, retain evaluation prompts or outputs, or let an evaluator promote a revision.

Because a candidate is deliberately absent from the logical route, operators gather bounded
candidate evidence explicitly with AIPerf rather than duplicating production requests:

This implements **candidate → measure → gate → active** without silently replaying live prompts.
Teams that require production-shape validation can use content-free Inference Replay fingerprints or
run their own reviewed evaluator dataset against the isolated candidate. InferCrane does not make
silent shadowing the default: raw prompts may be sensitive, duplicate calls add cost, and tool calls
may have side effects.

<Warning>
`rollout create` and `provision` may create billable candidate resources; `validate` sends the
approved fixture to both revisions; `promote` changes production traffic. Record provider inventory,
approve cost and data handling, and verify the exact model/runtime/provider/GPU combination before
running this sequence. Stop on missing ownership or qualification evidence.
</Warning>

```console
infercrane rollout create qwen-prod \
  --model Qwen/Qwen3-8B \
  --cloud runpod \
  --gpu L40S \
  --wait

infercrane rollout provision qwen-prod CANDIDATE_REVISION_ID --wait

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

infercrane rollout inspect qwen-prod

infercrane rollout promote qwen-prod CANDIDATE_REVISION_ID \
  --reason "Release Guard accepted" \
  --wait
```

`rollout create` persists an immutable candidate and returns its revision ID. `provision` creates
revision-scoped capacity without adding it to the active route. Configure policy, run the bounded
validation, and inspect the persisted result. Execute `promote` only when the decision is `ACCEPT`
for that same candidate and active revision; `WAIT` and `REJECT` are not authorization to move
traffic.

Release Guard uses a persisted active/candidate benchmark pair only when tool version and workload
parameters match. The evaluation snapshot records `aiperf_benchmark` plus both benchmark IDs as its
evidence source. Otherwise it waits for comparable evidence instead of mixing measurements.

## Change model, provider, and scaling bounds in one candidate

Replica bounds are immutable revision configuration. A candidate with `min < max` enables bounded
autoscaling, but Release Guard validation alone does not prove a real provider can complete
`1 → N → 1` or drain a long stream. Qualify that dynamic behavior first on an isolated staging
deployment with the exact intended model, runtime, provider, GPU, region, and bounds.

```bash
infercrane integrations --output json
infercrane models inspect MODEL_CATALOG_NAME --output json
infercrane plan candidate-staging.yaml --output json
```

Stop if the exact-combination evidence is missing. If it passes, follow the
[autoscaling qualification runbook](/features/autoscaling#qualify-scale-up-streaming-drain-and-scale-down)
on `candidate-staging` and retain its benchmark, stream, scaling-decision, provider-inventory, and
cleanup evidence.

<Warning>
The following candidate can create billable provider capacity and validation sends inference to
both revisions. It changes no production traffic until `promote`, but its provider, cost, model-data,
and ownership boundaries must be approved first. The scaling evidence from staging is applicable
only when its exact serving-plan inputs match this candidate.
</Warning>

```bash
infercrane rollout create qwen-prod \
  --model MODEL_REPOSITORY \
  --model-revision IMMUTABLE_MODEL_COMMIT \
  --runtime vllm \
  --runtime-version QUALIFIED_RUNTIME_VERSION \
  --cloud PROVIDER \
  --gpu ACCELERATOR \
  --region REGION \
  --min 1 \
  --max 4 \
  --idempotency-key qwen-prod-candidate-v2 \
  --wait

infercrane rollout inspect qwen-prod --output json
infercrane rollout provision qwen-prod CANDIDATE_REVISION_ID --wait
infercrane rollout policy set qwen-prod \
  --require-compatibility \
  --require-synthetic \
  --validation-max-requests 100 \
  --validation-max-concurrency 4
infercrane rollout validate qwen-prod \
  --requests 100 \
  --concurrency 4 \
  --acknowledge-validation-cost \
  --wait
infercrane rollout inspect qwen-prod --output json
```

The candidate inspection must show the reviewed immutable model/runtime/provider/region and
`min=1,max=4`; the Guard snapshot must refer to that candidate and the still-current active revision.
Topology evidence is `INCONCLUSIVE` for unmeasured weighted graphs or arbitrary binding changes.
Reject on mismatch or regression. Promote only on `ACCEPT`, then keep the automatic rollback window
or operator recovery ready and re-observe the real scaling policy. If policy requires dynamic
autoscaling proof before any production promotion, the matching staging qualification is mandatory;
Release Guard does not silently substitute a synthetic benchmark for it.

InferCrane does not silently duplicate inference requests. `rollout validate` prints a cost/privacy
notice, enforces persisted request and concurrency ceilings, runs AIPerf against each revision, and
then queues the durable evaluation. It does not retain prompts or generated output.

### Retry provisioning or validation safely

Transient capacity, transport, or readiness failures do not authorize a second candidate. Keep the
same deployment, candidate revision ID, and durable operation:

<Warning>
Retrying `provision` can continue a billable provider operation. It is safe only for the same
persisted candidate and provider identity after read-only ownership comparison; never create a new
candidate to recover an uncertain response.
</Warning>

```bash
infercrane rollout inspect qwen-prod --output json
infercrane operation watch OPERATION_ID --wait-timeout 15m
infercrane inspect qwen-prod --output json
infercrane orphans --output json

infercrane rollout provision qwen-prod CANDIDATE_REVISION_ID --wait
```

`rollout provision` re-observes and adopts capacity for the same immutable revision. Do not run
`rollout create` again or change provider identity after an uncertain response. Compare the
persisted provider resource IDs with read-only provider inventory before retrying when ownership is
ambiguous.

Validation sends explicit inference to both revisions and may incur provider cost and expose the
validation workload to the configured runtime. Retry it only after reviewing the persisted policy
ceilings and acknowledging that boundary again:

```bash
infercrane rollout validate qwen-prod \
  --requests 100 \
  --concurrency 4 \
  --acknowledge-validation-cost \
  --wait
```

A permanent model-health or compatibility failure should be recorded as `REJECT`, followed by
candidate rejection and candidate-only cleanup, not a loop that creates new capacity. Preserve the
Guard evaluation, operation timeline, and provider identities until direct inventory confirms no
orphan.

When automatic rollback is enabled, promotion retains the previous healthy capacity until the
persisted observation monitor reaches `ACCEPT` or `REJECT`. The monitor snapshots its policy and
deadline at creation, so a concurrent policy edit cannot silently weaken an in-flight decision. Rejection atomically restores the old
revision and target set, waits for its router generation, drains active streams safely, and deletes
only failed revision capacity. A restart resumes the same monitor and deadline.

## Recover manually and preserve evidence

An evaluation `REJECT` does not move traffic. Record the decision before cleaning candidate
capacity, then reject only that candidate:

<Warning>
`rollout reject` mutates candidate lifecycle and can delete candidate-only provider capacity. Verify
the candidate revision and provider identities from the persisted inspection; never target the
active revision or use deployment deletion as release recovery.
</Warning>

```bash
infercrane rollout inspect qwen-prod --output json
infercrane explain rollout qwen-prod
infercrane rollout reject qwen-prod CANDIDATE_REVISION_ID \
  --reason "Release Guard rejected candidate" \
  --wait
```

If unhealthy traffic appears after promotion and the automatic observation monitor has not already
restored the prior target set, roll back explicitly to the retained known-good revision:

<Warning>
Rollback changes production routing and later removes only the failed revision after generation
drain. Confirm the retained known-good revision ID, current active revision, ownership, and active
stream state before running it.
</Warning>

```bash
infercrane rollout inspect qwen-prod --output json
infercrane rollout rollback qwen-prod PREVIOUS_REVISION_ID \
  --reason "operator recovery after post-promotion regression" \
  --wait
infercrane status qwen-prod --watch
infercrane explain rollout qwen-prod
```

Rollback is traffic-changing and may later delete only the failed revision's capacity after router
generation drain. It does not delete the persisted Guard evaluation, event timeline, benchmark, or
passport evidence. Confirm the revision ID from `rollout inspect`; never guess it or delete the
deployment to recover a release.

After acceptance, issue an [Inference Passport](/features/inference-passports) to make the exact
revision and release evidence independently verifiable.
For CI collection and provider-outcome failure semantics, use the
[release evidence runbook](/runbooks/release-evidence-ci).
For one reviewer-facing quality, performance, stream, recovery, rollback, and cleanup gate, use the
[production release approval checklist](/runbooks/release-approval).
