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

## Failure behavior

- Unknown aliases return an OpenAI-compatible 404.
- Upstream connection failures return 503; timeouts return 504.
- Client cancellation and upstream disconnects are recorded distinctly.
- The last published route remains usable during control-plane failure.

## Tests

`internal/gateway/gateway_test.go` and `internal/routes/routes_test.go` cover authentication,
alias rewriting, proxying, ordering, and removal. Add streaming fragmentation, cancellation,
header filtering, and telemetry assertions as the public API expands.

Related: [system architecture](../architecture/system.mdx) and
[system invariants](../architecture/invariants.md).
