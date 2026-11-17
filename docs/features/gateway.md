# Gateway and request data plane

Status: Implemented

## Entry points

- Composition: `cmd/infercrane/main.go`
- HTTP behavior: `internal/gateway/gateway.go`
- Telemetry: `internal/gateway/telemetry.go`
- Route snapshot: `internal/routes/routes.go`
- Accounting queue: `internal/accounting/recorder.go`

## Contract

The gateway exposes liveness, readiness, metrics, model discovery, and OpenAI-compatible chat
completions. It authenticates with the configured bearer key, resolves an alias from memory,
rewrites the upstream model, and streams through the instance-local router.

PostgreSQL cannot enter the routing decision. Request accounting is best-effort and buffered so a
database slowdown does not add inference latency; queue overflow is logged and counted as a
degraded observability condition.

## Prompts from the dashboard

A dashboard member signs in with a hosted identity, not an API key, so the gateway's own
authentication cannot admit them. `POST /api/v1/playground/chat/completions` closes that gap without
a second inference path:

1. The control API authenticates the caller like any `/api/v1` route and requires the `read`
   permission, the same permission an API key needs for inference. Endpoint-scoped inference tokens
   are refused there, as on every control-plane route.
2. It hands the resolved principal to `Gateway.ServeChatAs`, which runs the same handler as
   `POST /v1/chat/completions`.

From that point nothing differs: the tenant's route snapshot, endpoint restrictions, admission,
request quota, prepaid billing, retries, streaming, `X-Request-ID`, and request recording are the
same code. The caller's credential is not forwarded to the runtime. Authentication for `/v1/*` is
unchanged and still resolves from the in-memory credential snapshot; only the control-plane route
may read PostgreSQL to authenticate, and it does so before routing starts.

Inference errors keep the OpenAI error shape (`error.type`), while a failed authentication uses the
control API's shape (`error.code`).

## Failure behavior

- Unknown aliases return an OpenAI-compatible 404.
- Upstream connection failures return 503; timeouts return 504.
- Client cancellation and upstream disconnects are recorded distinctly.
- The last published route remains usable during control-plane failure.

## Tests

`internal/gateway/gateway_test.go` and `internal/routes/routes_test.go` cover authentication,
alias rewriting, proxying, ordering, and removal. `TestServeChatAs*` and
`internal/controlapi/playground_test.go` cover the dashboard entry: tenant isolation, endpoint
restrictions, quota, recording, and that the caller's credential stays out of the runtime request. Add streaming fragmentation, cancellation,
header filtering, and telemetry assertions as the public API expands.

Related: [system architecture](../architecture/system.mdx) and
[system invariants](../architecture/invariants.md).
