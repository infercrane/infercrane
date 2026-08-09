# Control-plane API v1

Base path: `/api/v1`. All requests require `Authorization: Bearer TOKEN`. Mutations require operator
or admin role; tenant/principal/audit administration requires admin. Cross-tenant resources return
`404` to avoid existence disclosure.

Errors are stable JSON objects:

```json
{"error":{"code":"forbidden","message":"principal is not allowed to perform this action"}}
```

## Resources

| Method and path | Minimum role | Purpose |
|---|---|---|
| `GET /doctor` | viewer | Run control-plane dependency diagnostics; optional `cloud` and `serverless` booleans add provider checks. |
| `POST /deployments/apply` | operator | Queue existing-target convergence; requires `Idempotency-Key`. |
| `POST /deployments` | operator | Atomically create desired cloud deployment and queue convergence; requires `Idempotency-Key`. |
| `GET /deployments` | viewer | List tenant deployments. |
| `GET /deployments/{name}` | viewer | Inspect deployment, targets, replicas, revisions, immutable model artifacts, active durable operation, and persisted request statistics. |
| `GET /deployments/{name}/events` | viewer | List durable deployment events. |
| `GET /deployments/{name}/revisions` | viewer | List immutable revision history. |
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
