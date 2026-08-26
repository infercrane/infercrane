---
title: Deployment lifecycle
description: How durable operations survive disconnects, retries, restarts, updates, and deletion.
---

# Deployment lifecycle

<Snippet file="_snippets/safe-retry.mdx" />

Create and apply requests are validated and persisted before external provisioning begins. A leased worker resumes incomplete steps after a control-plane restart. Each replica intent has one deterministic provider identity, so replay adopts the same resource rather than submitting another.

Readiness requires the provisioned runtime to serve the expected model. Updates create immutable candidates, Release Guard records a deterministic decision, promotion switches routing generations atomically, and old capacity drains before termination. A bad candidate never replaces the active revision.

A production endpoint combines this lifecycle with stable endpoint identity, admission, distributed
tenant quotas, and qualified autoscaling. Follow the self-contained,
[version-controlled production service recipe](/runbooks/production-service) for those commands,
their safe order, isolated bad-candidate testing, and the mandatory real-provider gate. Test
failures against isolated candidate/staging traffic; the active route does not become a safe test
target merely because its lifecycle is durable.

## Recover a failed worker or replace a deployment

InferCrane has no imperative `restart` command. If an owned provider resource disappears, the
reconciler uses the persisted replica intent and provider identity to observe, adopt, or recreate it;
closing the CLI is not a reason to submit another deployment. Follow the same durable work:

```bash
infercrane status coder-production
infercrane events coder-production
infercrane inspect coder-production --output json
infercrane operation watch OPERATION_ID --wait-timeout 15m
```

If the desired serving configuration itself must change, create an immutable replacement through a
version-controlled DeploymentSpec:

```bash
infercrane plan deployment-recovery.yaml
infercrane apply deployment-recovery.yaml \
  --idempotency-key coder-recovery-v1 \
  --wait
infercrane rollout inspect coder-production
```

The active revision remains routed until the candidate passes readiness and Release Guard. After the
operation converges, verify the stable data path and inspect the exact request attribution:

```bash
infercrane request coder-production --message "recovery probe"
infercrane request inspect REQUEST_ID --output json
infercrane observe coder-production
```

Use the `X-Request-Id` response header as `REQUEST_ID`. Provider-native systems may not expose a
physical replica identity; the inspection leaves that field unavailable instead of guessing.

### Decide whether provider drift is recoverable

Compare the persisted provider identity, intent digest, ownership tags, revision, and replica
ordinal from `inspect` with a read-only provider inventory before allowing reconciliation:

| Provider observation | Safe decision |
| --- | --- |
| Exact identity and configuration match | Let the existing durable operation adopt and continue. |
| Resource is missing and provider inventory is complete | Let the original replica intent recreate it; do not submit a new deployment. |
| Resource exists but mutable health/readiness changed | Keep the identity, preserve events, and let reconciliation recover or surface a terminal runtime failure. |
| Name matches but intent digest, ownership, revision, or ordinal differs | Pause mutation. Do not adopt, recreate, or delete; resolve ownership manually. |
| Resource was deleted outside InferCrane while delete/create is active | Pause until persisted desired state and provider absence are both established; never guess which operation won. |
| Inventory is stale, incomplete, unauthorized, or returns an unknown state | Manual intervention required. Preserve PostgreSQL and retry read-only observation; no create or cleanup. |

An empty `infercrane orphans` result is not enough when provider credentials cannot see the full
account or namespace. Manual intervention ends only when one authoritative provider identity can be
matched to one durable replica intent - or absence is proven by the owning provider boundary.

Delete first withdraws desired routing, then drains and removes external resources. Restarting midway resumes cleanup. Completion requires provider absence, removed targets and replicas, and a deployment tombstone. Operators should still verify provider inventory after paid acceptance tests.

## Delete without losing recovery state

Preview the exact effect before mutation. Confirm explicitly, give the request a stable retry key,
and wait for the durable provider cleanup operation:

```bash
infercrane inspect coder-production --output json
infercrane rollout inspect coder-production --output json
infercrane endpoints --output json
infercrane delete coder-production --plan

# Stop here and review dependencies before confirmation.

infercrane delete coder-production \
  --yes \
  --wait \
  --idempotency-key delete-coder-production-v1
```

Before `--yes`, verify the plan names only the intended deployment, revisions, targets, replicas,
and provider identities. Inspect every endpoint binding that references the deployment and decide
whether it needs a replacement plan. Confirm ownership mode: observe-only and traffic-managed
adoptions do not grant lifecycle deletion of the upstream; lifecycle-managed capacity must have
matching durable provider ownership. Also check external fallback, async jobs, active streams, and
retained release evidence required by your policy. InferCrane will not guess how an application
should replace a deleted endpoint dependency.

If the terminal disconnects, do not submit a new intent with a new key. Reattach with the operation
ID returned by `delete`, or inspect the persisted deployment events. After completion, verify both
InferCrane state and the provider boundary:

```bash
infercrane deployments --output json
infercrane orphans --output json
```

The deployment must be absent from the first result and no run-owned orphan may appear in the
second. For a paid or real-cluster operation, independently inspect the provider inventory too.
Provider deletion is not proven solely by a missing database row. Never erase PostgreSQL or use
`--keep-resources` while cleanup is unresolved unless you intentionally accept external ownership.
