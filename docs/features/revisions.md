# Revisions and rollout operations

Every deployment starts with an immutable active revision. A rollout creates a separate candidate revision; it does not mutate or replace the active revision. Candidate creation, promotion, rejection, and rollback are durable control-plane operations and may be safely retried after a CLI disconnect or control-plane restart.

```console
infercrane rollout create qwen-prod --model Qwen/Qwen3-8B --min 1 --max 4 --wait
infercrane rollout inspect qwen-prod
infercrane rollout promote qwen-prod REVISION_ID --wait
infercrane rollout reject qwen-prod REVISION_ID --reason "readiness failed" --wait
infercrane rollout rollback qwen-prod REVISION_ID --reason "operator rollback" --wait
```

Only one candidate may exist for a deployment. Rejections and rollbacks require a persisted reason. Replaying the operation that created a candidate returns the same candidate, and replaying an already committed transition is a no-op.

Manual promotion is an explicit operator action. Release Guard will provide the deterministic policy decision and evidence required by the guarded rollout path; candidate creation alone never routes traffic.
