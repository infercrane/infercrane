---
title: HTTP control API
description: Authenticated v1 endpoints, roles, idempotency, and stable errors implemented by InferCrane.
---

# HTTP control API

Base path: `/api/v1`. All requests require `Authorization: Bearer TOKEN`, except the public catalog,
the verified billing webhook, and the two CLI sign-in calls `POST /cli/logins` and
`POST /cli/logins/token`, which the CLI makes before it has a key. Mutations require operator
or admin role; tenant/principal/audit administration requires admin. Cross-tenant resources return
`404` to avoid existence disclosure.

InferCrane publishes an executable OpenAPI 3.1 document at `api/openapi.json`. The Mintlify
navigation generates interactive endpoint pages from the same document used to generate the Python
and TypeScript low-level clients. CI compares every contracted method/path pair with the registered
routes in `internal/controlapi/api.go` and rejects stale generated files.

Errors are stable JSON objects:

```json
{"error":{"code":"forbidden","message":"principal is not allowed to perform this action"}}
```

## Resources

| Method and path | Minimum role | Purpose |
|---|---|---|
| `GET /doctor` | viewer | Run control-plane dependency diagnostics; optional `cloud`, `serverless`, and `aws` booleans add read-only provider checks. |
| `GET /whoami` | viewer | Return the authenticated principal, role, and tenant without exposing credential material. |
| `POST /cli/logins` | none | Start a CLI sign-in. Returns a one-time user code, a device code returned only once, the expiry, the poll interval, and the dashboard approval URL when `INFERCRANE_CONSOLE_URL` is set. At most 500 sign-ins wait at once (`429` beyond that). |
| `POST /cli/logins/token` | none (device code) | Poll a sign-in: `pending`, `slow_down`, `denied`, `expired`, `consumed`, or `approved` with the key, returned once. |
| `GET /cli/logins/{code}` | viewer, dashboard principals | Show the device and status of a sign-in before approving it. |
| `POST /cli/logins/{code}/approve` | viewer, dashboard principals | Approve with the caller's own tenant, role, and scopes. CLI keys and endpoint tokens get `403`. |
| `POST /cli/logins/{code}/deny` | viewer, dashboard principals | Deny a sign-in. |
| `GET /cli/sessions` | viewer | List the caller's active CLI keys with device and last use; admins see every member's. |
| `DELETE /cli/sessions/{id}` | owner or admin | Revoke a CLI key. |
| `DELETE /cli/sessions/current` | CLI key | Revoke the calling CLI key (`infercrane logout`); other credentials get `409 not_cli_session`. |
| `POST /agents` | operator | Create an InferCrane agent key for one machine. The key is returned once and can call only the agent API. |
| `GET /agents` | viewer | List agents with version, last sync, connection state, hardware, and reported servers. |
| `GET /agents/{id}` | viewer | Inspect one agent (by ID or active name) and the servers observed through it, with whether their traffic is visible. |
| `DELETE /agents/{id}` | operator | Revoke an agent. Its next sync gets `401` (`agent_revoked`, or `agent_unauthenticated` once the credential snapshot dropped the key) and it stops; its servers are marked unhealthy. |
| `POST /agent/sync` | agent key | Report hardware, servers, health, filtered engine metrics, and content-free request shapes; gzip allowed, 8 MiB at most. Any other key gets `401`. See [InferCrane agent](/architecture/agent). |
| `POST /agent/servers` | agent key | Observe one local server as an observe-only endpoint. |
| `GET /agent/data-plane` | agent key | The agent's data plane: routes of its agent-managed endpoints by server key, SHA-256 digests of the workspace's live inference keys that may call them, their admission policies, the request quota, and `valid_until`. `?generation=` returns `unchanged: true` when nothing changed. |
| `POST /agent/quota-leases` | agent key | Lease at most 1,000 requests of the current minute's workspace quota; `unlimited: true` when the workspace has none. |
| `GET /agent/commands?wait=25s` | agent key | Long-poll for candidate runs queued for this agent (at most four per answer). |
| `POST /agent/commands/{id}` | agent key | Report a command's phase, or its content-free result or failure code. The answer says whether it was cancelled. |
| `POST /agents/{id}/candidate-runs` | operator | Queue a candidate run in the agent's own cluster: start the candidate (digest-pinned image, no shell entry point), measure it against the endpoint's current server with AIPerf, run the answer check, and remove it. At most 16 queued or running per agent. See [Candidate runs](/architecture/agent#candidate-runs). |
| `GET /agents/{id}/candidate-runs` | viewer | List an agent's candidate runs, newest first, with state, phase, and result. |
| `GET /agents/{id}/candidate-runs/{run}` | viewer | Inspect one candidate run. |
| `DELETE /agents/{id}/candidate-runs/{run}` | operator | Cancel a queued or running candidate run; the agent removes the candidate at its next report. |
| `PUT /endpoints/{name}/residency` | operator | Set `any` or `customer-cloud`. A customer-cloud endpoint accepts only bindings to servers an InferCrane agent owns (`409 residency_conflict` while another binding exists), and the hosted gateway refuses it with `403 residency_error`. See [Ownership and residency](/architecture/agent#ownership-and-residency). |
| `POST /playground/chat/completions` | viewer | Send one OpenAI chat completion to an endpoint as the authenticated principal. The body, routing, admission, quota, billing, request recording, `X-Request-ID`, and OpenAI-shaped inference errors are those of `POST /v1/chat/completions`; only authentication differs, so a signed-in dashboard member can test an endpoint without an API key. See [Gateway](/features/gateway#prompts-from-the-dashboard). |
| `GET /optimization/proof-report?campaigns=A,B` | viewer | The Proof Report of 1 to 16 finished runs, signed by content digest once every run has finished. `format=markdown` or `format=html` returns it rendered, as an attachment. See [Proof Report](/features/proof-report). |
| `GET /optimization/campaigns/{id}/report` | viewer | The run's outcome report. A finished run's report carries `signature` when the control plane has a signing key; otherwise `signature` is `null` and `unsigned_reason` says why. See [Read the outcome of a run](/features/optimization#read-the-outcome-of-a-run). |
| `POST /optimization/campaigns/{id}/candidates/{candidate_id}/chat/completions` | viewer | Send one chat completion to the exact revision of a decided candidate. Refused with `409` while the run is still measuring it. See [Prompt a decided setup yourself](/features/optimization#prompt-a-decided-setup-yourself). |
| `GET /integrations` | viewer | Return versioned provider/runtime contracts, registered capabilities, and honest qualification evidence. |
| `GET /compute/providers` | viewer | Return execution readiness per provider. `workspace_owned` is computed for the caller and is true only when it may deploy and optimize there without a compute connection. |
| `POST /deployments/apply` | operator | Queue existing-target convergence; requires `Idempotency-Key`. |
| `POST /deployments` | operator | Atomically create desired cloud deployment and queue convergence; requires `Idempotency-Key`. |
| `GET /deployments` | viewer | List tenant deployments. |
| `GET /deployments/{name}` | viewer | Inspect deployment, targets, replicas, revisions, immutable model artifacts, active durable operation, and persisted request statistics. |
| `GET /deployments/{name}/events` | viewer | List durable deployment events. |
| `POST /deployments/{name}/benchmarks` | operator | Run and persist AIPerf evidence for `active`, `candidate`, or an explicit revision. |
| `GET /deployments/{name}/benchmarks` | viewer | List persisted benchmark history and reproduction metadata. |
| `POST /deployments/{name}/passports` | operator | Assemble, sign, verify, and persist a revision's canonical Inference Passport. |
| `GET /deployments/{name}/passports` | viewer | List signed release evidence and verification state. |
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
| `GET /optimization/accelerator-lab/runs` | viewer | List the organization's Accelerator Lab runs, newest first; optional `campaign_id` (exact) and `limit` (1 to 100, default 20). Returns target, policy, and recorded evidence with digests only; `result_state` is `pending`, `recorded`, or `unreadable`. |
| `GET /targets` | viewer | List registered tenant targets. |
| `POST /targets` | operator | Register an existing HTTP(S) inference target. |
| `GET /orphans` | viewer | List unowned provisioned resources. |
| `GET /audit-events` | admin | List events newest first. `limit` defaults to 100; values above 500 return 500. A full page returns `next` with `before` and `before_id`: pass both to read the next page, including events that share a timestamp. `before` alone (RFC3339) returns events older than that time. |
| `PUT /tenant/quota` | admin | Set deployment, replica, and distributed per-minute request limits; zero blocks that resource. |
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

<Note>
The interactive API explorer targets the local server declared in the specification. Use it only
against an InferCrane control plane you operate, and never paste production credentials into a
shared browser session.
</Note>
