# ADR 0003: Router generations are instance-owned

Status: Accepted  
Date: 2026-08-09

## Context

Supervised vLLM Routers listen on loopback. A loopback endpoint persisted without ownership can be
read by another gateway replica, where it points to the wrong network namespace. Generation-based
ports also collide across multiple deployments.

## Decision

Every gateway has a stable unique instance ID. Router generations are keyed by deployment and
owner. Router ports are deterministically partitioned from deployment identity within each
replica. Only the owner publishes the endpoint into its local route directory.

## Consequences

Each replica runs local routers and independently reconciles membership. Capacity planning must
include router processes per replica. A future external-router architecture would supersede this
ADR.
