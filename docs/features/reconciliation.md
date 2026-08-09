# Reconciliation and routing

Status: Implemented

## Entry points

- Reconciliation: `internal/reconcile/reconcile.go`
- Router process supervision: `internal/router/router.go`
- vLLM health/model inspection: `internal/runtime/vllm.go`
- Persistent generation operations: `internal/store/operations.go`

## Contract

Reconciliation converts desired deployments and observed worker health into an immutable local
route snapshot. Health probes are concurrent under a fixed bound. Membership and strategy produce
a deterministic hash; changes replace the local upstream router and create an instance-owned
generation.

Transient errors are logged and retried on the next interval rather than terminating the gateway.
No healthy worker removes the route and marks the deployment unhealthy. Partial health produces a
degraded deployment with only healthy members.

## Scaling model

Each gateway replica supervises its own router processes because endpoints use loopback. Instance
IDs must be unique and stable for the life of a replica. Autoscaling worker count is planned, not
implemented.

Related: [ADR 0003](../adr/0003-instance-owned-router-generations.md).
