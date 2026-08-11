---
title: HTTP control API
description: Authenticated v1 endpoints, roles, idempotency, and stable errors implemented by InferCrane.
---

# HTTP control API

Base path: `/api/v1`. All requests require `Authorization: Bearer TOKEN`. Mutations require operator
or admin role; tenant/principal/audit administration requires admin. Cross-tenant resources return
`404` to avoid existence disclosure.

InferCrane does not currently publish an OpenAPI document, so this reference is maintained against the registered routes in `internal/controlapi/api.go`. Interactive OpenAPI consoles and generated SDK navigation are intentionally not shown until an executable specification becomes authoritative.

Errors are stable JSON objects:

```json
{"error":{"code":"forbidden","message":"principal is not allowed to perform this action"}}
```

## Resources

| Method and path | Minimum role | Purpose |
|---|---|---|
| `GET /doctor` | viewer | Run control-plane dependency diagnostics; optional `cloud`, `serverless`, and `aws` booleans add read-only provider checks. |
| `GET /whoami` | viewer | Return the authenticated principal, role, and tenant without exposing credential material. |
| `GET /integrations` | viewer | Return versioned provider/runtime contracts, registered capabilities, and honest qualification evidence. |
| `POST /deployments/apply` | operator | Queue existing-target convergence; requires `Idempotency-Key`. |
| `POST /deployments` | operator | Atomically create desired cloud deployment and queue convergence; requires `Idempotency-Key`. |
| `GET /deployments` | viewer | List tenant deployments. |
| `GET /deployments/{name}` | viewer | Inspect deployment, targets, replicas, revisions, immutable model artifacts, active durable operation, and persisted request statistics. |
| `GET /deployments/{name}/events` | viewer | List durable deployment events. |
| `POST /deployments/{name}/benchmarks` | operator | Run and persist AIPerf evidence for `active`, `candidate`, or an explicit revision. |
| `GET /deployments/{name}/benchmarks` | viewer | List persisted benchmark history and reproduction metadata. |
| `GET /deployments/{name}/revisions` | viewer | List immutable revision history. |
| `POST /deployments/{name}/rollouts` | operator | Create an immutable candidate revision. |
| `POST /deployments/{name}/rollouts/{revision}/provision` | operator | Queue candidate capacity provisioning. |
| `POST /deployments/{name}/rollouts/guard/evaluate` | operator | Persist a deterministic Release Guard evaluation. |
| `GET /deployments/{name}/release-guard/policy` | viewer | Read the persisted Release Guard policy. |
| `PUT /deployments/{name}/release-guard/policy` | operator | Replace the deterministic guard thresholds. |
| `POST /deployments/{name}/rollouts/{revision}/promote` | operator | Promote an accepted ready candidate. |
| `POST /deployments/{name}/rollouts/{revision}/reject` | operator | Reject a candidate with a persisted reason. |
| `POST /deployments/{name}/rollback` | operator | Queue rollback to an immutable revision. |
| `GET /deployments/{name}/scaling-decisions` | viewer | List deterministic scaling evaluations and their persisted signals. |
| `PUT /deployments/{name}/route` | operator | Change the persisted routing strategy. |
| `DELETE /deployments/{name}` | admin | Withdraw routing and queue verified provider cleanup; requires `Idempotency-Key`. |
| `GET /operations/{id}` | viewer | Inspect durable progress and result. |
| `GET /operations/{id}/events` | viewer | List ordered durable operation progress events. |
| `POST /operations/{id}/cancel` | operator | Request cooperative cancellation. |
| `GET /targets` | viewer | List registered tenant targets. |
| `POST /targets` | operator | Register an existing HTTP(S) inference target. |
| `GET /orphans` | viewer | List unowned provisioned resources. |
| `GET /audit-events` | admin | List up to 500 events; `before` accepts RFC3339. |
| `PUT /tenant/quota` | admin | Set deployment, replica, and request policy limits. |
| `POST /tenants` | bootstrap admin | Create a tenant. |
| `POST /principals` | admin | Create a credential; secret is returned once. |
| `POST /principals/{id}/rotate` | admin | Replace a credential immediately. |
| `DELETE /principals/{id}` | admin | Revoke a credential immediately. |
| `GET /secrets` | admin with `manage_secrets` | List resolver metadata; values are never resolved into the response. |
| `POST /secrets` | admin with `manage_secrets` | Create a reference-only secret object. |
| `DELETE /secrets/{id}` | admin with `manage_secrets` | Delete a tenant-scoped secret reference. |
| `GET /deployments/{name}/external-policy` | viewer | Inspect the persisted fallback policy and reserved hard budgets. |
| `PUT /deployments/{name}/external-policy` | operator/admin with `manage_external` | Replace an explicit external policy; enablement requires privacy acknowledgement and positive hard limits. |

Existing-target apply request:

```json
{
  "name": "qwen-prod",
  "model": "Qwen/Qwen3-8B",
  "targets": ["gpu-a", "gpu-b"],
  "routing_strategy": "round-robin"
}
```

An accepted mutation returns `202`, an operation object, and a `Location` header. Repeating the
same tenant, operation kind, and idempotency key returns the original operation.

Diagnostics execute inside the control-plane process. The public CLI receives only check status,
messages, and remediation; it never receives or opens the PostgreSQL URL or provider credentials.

Non-success responses use one stable envelope:

```json
{
  "error": {
    "code": "candidate_not_ready",
    "category": "conflict",
    "message": "selected revision has no healthy ready endpoint",
    "retryable": false,
    "remediation": "Inspect current status and active durable operations before retrying with the same idempotency key."
  }
}
```

Categories are `authentication`, `authorization`, `validation`, `not_found`, `conflict`,
`rate_limit`, `dependency`, `internal`, or `request`. The mapping is deterministic from the HTTP
status and error code. A retryable mutation must retain its original idempotency key.
