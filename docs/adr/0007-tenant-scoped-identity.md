# ADR 0007: Scope identity and resource names by tenant

- Status: Accepted
- Date: 2026-08-09

## Context

A shared bearer secret cannot support teams or hosted operation. Tenant filters added only at API
handlers are insufficient if route keys and database uniqueness remain global.

## Decision

Scoped credentials contain 256 bits of randomness and only their SHA-256 digest is stored.
Credentials belong to a tenant principal with viewer, operator, or admin role and support rotation
and bounded revocation. The bootstrap secret remains an initial global administrator.

Control API authorization resolves a principal before RBAC and tenant filtering. Inference model
listing and routing use tenant-qualified aliases. PostgreSQL target and deployment uniqueness is
tenant-qualified, and apply validates target membership in the same tenant. Audit records carry
the principal and tenant.

## Consequences

The same model alias and target name can safely exist in different tenants. Cross-tenant lookups
return not found rather than revealing existence. Tokens are shown only at creation or rotation.
Gateway replicas refresh hashed credential snapshots every second, keeping authentication off the
inference database path while bounding rotation/revocation propagation.
The bootstrap key must be stored and network-restricted until a dedicated break-glass mechanism is
implemented. Per-tenant request-rate quota enforcement remains a release blocker.

## Verification

PostgreSQL tests cover authentication, rotation, revocation, tenant-qualified resource names, and
visibility. HTTP tests cover viewer denial and model filtering. Docker smoke tests exercise queued
apply through a leased worker and scoped viewer authentication.
