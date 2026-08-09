# Architecture decision records

ADRs preserve why the system looks the way it does. Accepted ADRs are immutable except for typo
or link corrections. A changed decision gets a new ADR whose status supersedes the old record.

| ADR | Status | Decision |
|---|---|---|
| [0001](0001-control-data-plane.md) | Accepted | Separate the control and request data planes. |
| [0002](0002-postgresql-source-of-truth.md) | Accepted | Use PostgreSQL as the production source of truth. |
| [0003](0003-instance-owned-router-generations.md) | Accepted | Scope loopback routers by gateway instance. |
| [0004](0004-repository-native-engineering-memory.md) | Accepted | Store durable engineering memory in the repository. |
| [0005](0005-durable-operations-and-policy-boundaries.md) | Accepted | Persist mutations and separate fleet policies from provider execution. |
| [0006](0006-leased-operation-execution.md) | Accepted | Lease durable work through PostgreSQL for crash-safe execution. |
| [0007](0007-tenant-scoped-identity.md) | Accepted | Hash scoped credentials and qualify all resource access by tenant. |

Use the next sequential number. Include context, decision, consequences, alternatives, and
verification. Link affected feature documents.
