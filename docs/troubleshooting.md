---
title: Troubleshooting
description: Diagnose configuration, provisioning, readiness, routing, rollout, and cleanup failures.
---

# Troubleshooting

Start with `infercrane doctor`, then `status`, `events`, `inspect`, and `explain`. Use `operation ID` when a lifecycle operation is retrying. `inspect` exposes provider request/resource IDs needed to reconcile external inventory.

- **Provisioning appears stuck:** read the durable progress message before changing anything. `provider is allocating capacity` means no provider endpoint exists yet. `provider endpoint assigned; waiting for runtime readiness` means the runtime may be pulling artifacts, initializing, or restarting; it does not prove that a process is listening. Check `operation ID`, `inspect`, provider inventory, and provider boot logs. Do not submit another deployment with a different idempotency key.
- **Deployment is degraded:** run the general explanation and verify expected model readiness on each replica.
- **Not scaling:** use `explain scaling`; check thresholds, consecutive intervals, bounds, and cooldown.
- **Candidate rejected:** use `explain rollout`; compare persisted metrics and policy rather than retrying promotion blindly.
- **Slow first request:** use `explain cold-start`; unavailable provider substages are intentionally not inferred.
- **Delete interrupted:** reconnect and inspect the same durable operation. Confirm provider inventory is empty before considering cleanup complete.
- **AWS deployment is rejected before provisioning:** run `infercrane doctor --aws`; verify the
  complete `INFERCRANE_AWS_*` set, exact requested region/GPU, immutable image digest, role trust,
  encrypted root-volume size, private subnet reachability, and instance-profile access to the worker secret. Retrying a
  configuration error does not create capacity.
- **Kubernetes deployment is rejected before provisioning:** run `infercrane doctor --kubernetes`;
  verify the explicit context, namespace RoleBinding, immutable image, GPU label/resource, worker
  Secret reference, and optional KServe CRD. A server-side field conflict is intentional protection;
  inspect `managedFields` and resolve ownership instead of forcing the apply.
- **External fallback does not activate:** run `infercrane external inspect DEPLOYMENT`, then check
  primary health, privacy acknowledgement, the injected secret reference, external `/models`
  inventory, and remaining request/cost reservations. InferCrane fails closed and never replays a
  possibly transmitted request.
- **External fallback returns 429:** the in-memory lease or persisted hard budget is exhausted.
  Inspect the policy before deliberately increasing its ceilings; InferCrane does not fabricate or
  refund provider cost.

Never paste API keys, prompts, generated content, or unredacted provider responses into a public issue.
