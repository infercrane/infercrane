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

## Closed APIs behind an endpoint

An endpoint can route to a closed OpenAI-compatible API (an external binding connected with
`infercrane connect --manage-traffic`). The gateway then:

- sends the API key stored when the server was connected as `Authorization: Bearer`, never the
  caller's InferCrane key. A stored key that cannot be opened makes the route unavailable; the
  gateway never calls such a server without its key.
- asks for token usage on streamed chat and completions requests with
  `stream_options.include_usage`, because closed APIs omit it otherwise. When the caller did not
  ask for usage, the gateway removes the usage-only chunk (empty `choices`, `usage` set) the API
  adds, so the caller's stream is unchanged; other events pass through whole. A caller that set
  `include_usage` itself, either way, is left as it asked.
- records cached input tokens (`usage.prompt_tokens_details.cached_tokens`, or
  `input_tokens_details` on the Responses API) next to input and output tokens. A cached count
  larger than the input is discarded.

These are what a [migration Proof](./migration.mdx) prices and replays.

## Failure behavior

- Unknown aliases return an OpenAI-compatible 404.
- Upstream connection failures return 503; timeouts return 504.
- A buffered request on a `primary-fallback` endpoint whose binding gave no response byte
  (connection failure, `502`, `503` or `504`) moves to the next published binding it has not
  tried, at most twice. `routes.Directory.AcquireFallbackForTenant` picks and pins that binding
  from the in-memory snapshot. Each further binding gets one attempt; only the first spends the
  admission retry budget. Streams, cancelled or timed-out requests, and paid external bindings
  never move. The request record names the binding that answered and sets `fallback_reason`
  (`upstream_connection_failed` or `upstream_<status>`). See
  [endpoints](./endpoints.mdx#when-the-primary-fails-mid-request).
- Weighted endpoints use smooth weighted round-robin (`routes.Directory.selectLocked`), so short
  windows see the configured share.
- Client cancellation and upstream disconnects are recorded distinctly.
- The last published route remains usable during control-plane failure.

## Tests

`internal/gateway/gateway_test.go` and `internal/routes/routes_test.go` cover authentication,
alias rewriting, proxying, ordering, and removal. `internal/gateway/failover_test.go` covers
failover: which failures move, the retry budget, the two-binding bound, paid external bindings,
pinning, and the recorded reason. `TestWeightedEndpointInterleavesLargeWeights` checks that every
window of a weighted endpoint gets its share. `TestServeChatAs*` and
`internal/controlapi/playground_test.go` cover the dashboard entry: tenant isolation, endpoint
restrictions, quota, recording, and that the caller's credential stays out of the runtime request.
`internal/gateway/stream_usage_test.go` covers the usage request on closed-API streams, removing the
added usage chunk across split and CRLF events, and cached input tokens. Add streaming fragmentation, cancellation,
header filtering, and telemetry assertions as the public API expands.

Related: [system architecture](../architecture/system.mdx) and
[system invariants](../architecture/invariants.md).
