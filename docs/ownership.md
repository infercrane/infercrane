# Ownership

Ownership is expressed by responsibility until public repository teams and handles exist.

| Area | Paths | Required reviewer role |
|---|---|---|
| Data plane and API compatibility | `internal/gateway`, `internal/routes` | Gateway maintainer |
| Persistence and schema | `internal/store`, `internal/store/migrations` | Database maintainer |
| Reconciliation and routers | `internal/reconcile`, `internal/router` | Control-plane maintainer |
| Providers and runtimes | `internal/provision`, `internal/runtime`, `internal/metrics` | Integration maintainer |
| Fleet policy and tenancy | `internal/autoscale`, `internal/capacity`, `internal/authz`, `internal/pricing` | Control-plane maintainer |
| Release and operations | `Dockerfile`, `compose.yaml`, `deploy`, `docs/production.md` | Operations maintainer |
| Engineering governance | `AGENTS.md`, `CLAUDE.md`, `docs`, `tools`, `.github` | Project maintainer |

Before opening the repository to outside contributors, map these roles to public GitHub teams in
`.github/CODEOWNERS`. Do not add placeholder handles that GitHub cannot resolve.
