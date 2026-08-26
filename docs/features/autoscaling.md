---
title: Autoscaling
description: Scale replicas with bounded decisions, durable evidence, and generation-safe drain.
---

# Autoscaling

Elastic deployments use bounded queue-aware scaling between explicit minimum and maximum replicas.
Enable it by choosing different bounds in a plan or DeploymentSpec:

<Warning>
`deploy` can create billable GPU capacity and the load procedure below can trigger replicas up to
`max`. Record direct provider inventory, approve the worst-case cost, and verify the exact
provider/runtime/model/GPU combination before running it. Local tests prove the controller, not real
capacity, performance, or provider billing.
</Warning>

```bash
infercrane plan Qwen/Qwen3-8B \
  --name support-production \
  --cloud runpod \
  --gpu L40S \
  --min 1 \
  --max 4

infercrane deploy Qwen/Qwen3-8B \
  --name support-production \
  --cloud runpod \
  --gpu L40S \
  --min 1 \
  --max 4
```

`plan` is side-effect free; `deploy` may create billable capacity. The initial public CLI exposes
bounded automatic scaling rather than an imperative `scale-to` command. Change bounds through a
new semantic plan and declarative apply instead of mutating replica state behind the controller.

## Follow a scaling decision

```bash
infercrane status support-production --watch
infercrane logs support-production --follow
infercrane explain scaling support-production
```

Persisted vLLM running/waiting signals, consecutive-interval thresholds, and cooldowns produce an
auditable scale-up, scale-down, or no-op decision. `explain scaling` returns the selected action,
old and new desired capacity, the signal snapshot, reason, and evaluation timestamp. Missing or
stale metrics produce a persisted no-op instead of a guessed decision.

### Active-stream visibility boundary

InferCrane exposes an aggregate `infercrane_gateway_active_requests` gauge, replica lifecycle and
drain state, persisted scaling decisions, and a Request Inspector record after a request produces
accounting evidence. It does **not** currently provide a command that enumerates every live stream
or maps an in-progress connection to a router generation. Do not claim that `status`, `events`, or
the terminal UI is a live-stream list.

During a spike, use the supported aggregate and durable evidence together:

```bash
curl -fsS "$INFERCRANE_URL/metrics" |
  grep '^infercrane_gateway_active_requests '
infercrane status support-production --output json
infercrane explain scaling support-production --output json
infercrane events support-production --output json
```

After a stream finishes or interrupts, correlate its `X-Request-Id` with `request inspect`. If an
incident policy requires per-stream live enumeration before mutation, InferCrane cannot satisfy that
policy in this release: leave replica bounds unchanged, apply load shedding outside the gateway if
already available, and collect the aggregate evidence. Generation-safe drain protects requests that
already acquired a retiring generation, but it is not an observability substitute.

For a reversible new-request mitigation, first export the complete admission policy, then reduce
new concurrency/queue exposure with all fields stated explicitly. Existing admitted streams are not
cancelled by an admission-policy update:

```bash
infercrane admission get support-production --output json \
  > admission-before-spike.json

infercrane admission set support-production \
  --max-concurrency 16 \
  --max-queue 16 \
  --queue-timeout-ms 1000 \
  --max-request-bytes 16777216 \
  --max-output-tokens 8192 \
  --priorities normal,high \
  --retry-budget 0
```

Restore every field from `admission-before-spike.json` after fresh signals and capacity have
recovered; never rely on CLI defaults to reconstruct the previous policy. This limits newly admitted
work and may return explicit `429` responses. It does not create capacity or guarantee a latency
SLO.

## Late observations and cold-start policy

There is no generic cold-start tuning flag in this release. Queue and load observations older than
the configured signal age remain a no-op; they do not trigger speculative capacity. When fresh
evidence returns, the next bounded controller evaluation may scale normally after its breach and
cooldown requirements. A late provider status does not retroactively create another scale-up intent.

Before changing replica bounds, record:

```bash
infercrane explain scaling support-production --output json
infercrane explain cold-start support-production --output json
infercrane inspect support-production --output json
```

Use [artifact cache and prewarming](/features/artifact-cache) only when a provider adapter exposes a
grounded native location. A persisted prefetch intent is not proof of a cache hit; wait for a fresh
`present` observation. Exact cold-start, cache, GPU, and late-provider behavior must be qualified in
the selected real environment. If observations remain missing, leave bounds unchanged or choose a
reviewed higher minimum replica count; InferCrane will not invent an ETA or cache state.

## Bound cost exposure

`max` is the hard capacity ceiling for elastic autoscaling. InferCrane does not currently enforce a
provider-neutral monetary autoscaling budget because trustworthy per-replica prices are not
available for every adapter. Before the billable mutation, review the worst-case replica count and
provider price externally:

```bash
infercrane plan Qwen/Qwen3-8B \
  --name support-production \
  --cloud runpod \
  --gpu L40S \
  --min 1 \
  --max 4 \
  --output json
```

Stop before `deploy` or `apply` if the price, currency, quota, or maximum exposure is unknown. If a
running deployment exceeds the approved capacity envelope, lower `max` through the complete,
version-controlled plan shown below; generation-safe drain still protects active streams. An
automatic dollar-denominated autoscaling stop is unsupported, so a policy that requires one must
keep autoscaling disabled or use an externally enforced provider budget until that capability is
qualified.

## Qualify scale-up, streaming drain, and scale-down

Use an isolated staging deployment with `min=1`, a reviewed `max`, and a runtime that exposes the
required vLLM running/waiting metrics. This is a paid real-infrastructure qualification, not a local
fixture claim.

1. Record the exact plan, baseline inventory, and one-ready-replica state:

   ```bash
   infercrane integrations --output json
   infercrane inspect support-production --output json
   infercrane status support-production
   infercrane explain scaling support-production --output json
   ```

2. In one terminal, start a long SSE request and retain headers and output:

   ```bash
   curl -N -D stream.headers -o stream.sse \
     "$INFERCRANE_URL/v1/chat/completions" \
     -H "Authorization: Bearer $INFERCRANE_API_KEY" \
     -H 'Content-Type: application/json' \
     -d '{
       "model":"support-production",
       "messages":[{"role":"user","content":"Count upward continuously."}],
       "stream":true,
       "max_tokens":2048
     }'
   ```

3. In a second terminal, create bounded representative pressure with AIPerf through InferCrane:

   ```bash
   infercrane benchmark support-production \
     --requests 200 \
     --concurrency 32 \
     --input-tokens 256 \
     --output-tokens 128 \
     --output json
   ```

   Choose request shape and concurrency from a non-sensitive production workload model. Do not
   increase them merely to force a result after the approved request or cost bound is reached.

4. While load is active, observe persisted decisions until ready capacity becomes greater than one
   without exceeding `max`:

   ```bash
   infercrane status support-production --watch
   infercrane explain scaling support-production --output json
   infercrane events support-production --output json
   ```

5. Stop the benchmark, let the full recovery intervals and cooldown elapse, and confirm capacity
   returns to `min=1`. Do not manually delete a replica to simulate scale-down. The long stream must
   finish with valid SSE and terminal `data: [DONE]`; inspect its `X-Request-Id` and confirm the
   recorded request remains attributed to one route generation.

6. Record before/peak/after desired and ready counts, every scaling-decision reason and timestamp,
   provider resource IDs, benchmark parameters, stream request evidence, errors, and direct provider
   inventory. Then delete the staging deployment with the normal plan-first lifecycle and verify
   provider inventory reaches zero run-owned resources.

Acceptance requires observed `1 → N → 1`, no duplicate provider identity, no capacity above `max`,
no new request on a draining generation, an intact active stream, and zero leaked resources. Missing
queue metrics, an unclassified provider delay, a stream without `[DONE]`, capacity that never returns
to `min`, or unavailable provider inventory is a failed or inconclusive qualification, not permission
to claim autoscaling support. See [production qualification](/testing/production-qualification-runbook)
for provider-specific evidence and cleanup.

## What happens to active streams

Scale-up creates durable replica intents. Scale-down first withdraws the selected worker from the
matching router generation. Requests that already acquired that generation keep it until they
finish or the bounded drain timeout expires; only then can provider deletion begin. New requests
cannot enter the retiring generation.

Closing `status --watch` or `logs --follow` stops only the local view. It does not cancel the durable
scale operation. Use the blocking operation ID shown by `status` with
`infercrane operation watch OPERATION_ID` to reconnect.

To lower the permitted capacity, preview and apply a complete replacement plan. Repeat the current
model, provider, GPU, region, and every setting you intend to preserve; omitted CLI defaults are not
an instruction to retain unknown values. For production automation, prefer a version-controlled
DeploymentSpec:

```bash
infercrane plan Qwen/Qwen3-8B \
  --name support-production \
  --cloud runpod \
  --gpu L40S \
  --min 1 \
  --max 2

infercrane apply Qwen/Qwen3-8B \
  --name support-production \
  --cloud runpod \
  --gpu L40S \
  --min 1 \
  --max 2 \
  --idempotency-key support-capacity-1-2

infercrane status support-production --watch
infercrane explain scaling support-production
```

Reducing `max` changes the controller boundary; it does not force an active stream onto another
replica. A later scale-down decision still follows generation withdrawal and bounded drain.

<Note>
Generation-safe drain and the 1→N→1 controller pass local fault tests. Exact provider capacity,
cold-start time, GPU behavior, and real long-stream drain remain independent real-infrastructure
qualification evidence; see [compatibility and qualification](/compatibility).
</Note>

Serverless deployments delegate zero-to-N worker scheduling to the registered provider-native
backend and retain one logical endpoint. InferCrane does not implement a GPU serverless scheduler.
RunPod supplies the first registered native Serverless implementation.
