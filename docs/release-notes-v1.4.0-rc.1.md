# InferCrane v1.4.0-rc.1 release notes

`v1.4.0-rc.1` adds endpoint overload protection and bounded durable async inference. It is a local
implementation candidate; real provider/runtime qualification remains deferred.

## Product surface

- Database-free inference-path admission for concurrency, queue depth, queue timeout, request size,
  output-token limits and priority classes.
- Bounded buffered retries for managed capacity without replaying streams or paid external calls.
- Durable protocol-native async jobs with idempotency, deadlines, priority, cancellation, lease
  adoption and stale-worker fencing.
- AES-256-GCM request/result persistence with explicit content-storage consent.
- HMAC-signed completion webhooks with bounded attempts and SSRF-hardened delivery.
- CLI, authenticated API, generated OpenAPI, Python and TypeScript surfaces.

## Qualification state

| Evidence | State |
| --- | --- |
| Go unit/race suite | Passed during implementation |
| PostgreSQL migration and async fencing suite | Passed in ephemeral Docker PostgreSQL |
| Mintlify build and link validation | Passed during implementation |
| Real provider/runtime async qualification | Deferred |

No public performance, reliability or provider claim is made by this candidate.
