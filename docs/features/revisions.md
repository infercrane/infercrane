---
title: Revisions and rollouts
description: Create immutable candidates, inspect evidence, promote safely, and roll back.
---

# Revisions and rollout operations

Every deployment starts with an immutable active revision. A rollout creates a separate candidate revision; it does not mutate or replace the active revision. Candidate creation, promotion, rejection, and rollback are durable control-plane operations and may be safely retried after a CLI disconnect or control-plane restart.

```console
infercrane rollout create qwen-prod --model Qwen/Qwen3-8B --min 1 --max 4 --wait
infercrane rollout create qwen-prod --model Qwen/Qwen3-8B --cloud runpod --gpu H100 --wait
infercrane rollout provision qwen-prod REVISION_ID --wait
infercrane rollout inspect qwen-prod
infercrane rollout promote qwen-prod REVISION_ID --wait
infercrane rollout reject qwen-prod REVISION_ID --reason "readiness failed" --wait
infercrane rollout rollback qwen-prod REVISION_ID --reason "operator rollback" --wait
```

Only one candidate may exist for a deployment. Rejections and rollbacks require a persisted reason. Replaying the operation that created a candidate returns the same candidate, and replaying an already committed transition is a no-op.

Elastic candidates persist their RunPod cloud, GPU, region, vLLM version and arguments, model revision, and replica bounds in the immutable revision spec. Candidate provisioning uses revision-scoped provider identities and does not publish those workers into active routing. Cancelling provisioning or rejecting the candidate removes only candidate capacity; the active revision is never part of that cleanup set.

Promotion is an explicit operator action and is refused unless the latest persisted Release Guard evaluation accepted that candidate against the still-current active revision. The revision and candidate target set are committed atomically. InferCrane then waits for the newest router generation to publish exactly that worker set before it drains and deletes the old revision. Candidate creation and provisioning alone never route traffic.

If the control plane restarts after the database cutover but before provider cleanup, the durable promotion resumes at router-generation verification. Cancellation before cutover removes candidate capacity; cancellation after cutover finishes safe draining because abandoning old billable capacity would leak resources.
