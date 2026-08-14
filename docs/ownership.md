# Ownership

Ownership is expressed by responsibility until public repository teams and handles exist.

| Area | Paths | Required reviewer role |
|---|---|---|
| Data plane and API compatibility | `internal/gateway`, `internal/routes` | Gateway maintainer |
| Persistence and schema | `internal/store`, `internal/store/migrations` | Database maintainer |
| Reconciliation and routers | `internal/reconcile`, `internal/router` | Control-plane maintainer |
| Integration contracts and qualification | `internal/integration`, `internal/support`, `internal/workflows`, `internal/reconcile` | Control-plane maintainer |
| Provider, runtime and metrics adapters | `internal/provision`, `internal/runtime`, `internal/runtimecontract`, `internal/metrics` | Integration maintainer |
| Provider contracts and development fakes | `internal/conformance`, `internal/testtools`, `tools/contract-qualifier`, `scripts/dev-check.sh`, `scripts/test-acceptance-safety.sh` | Integration maintainer |
| Fleet and inference decision policy | `internal/autoscale`, `internal/capacity`, `internal/decision`, `internal/overflow`, `internal/authz`, `internal/pricing` | Control-plane maintainer |
| Governed external capacity and secret resolution | `internal/external`, `internal/secrets`, `internal/store/external_policies.go`, `internal/store/secrets.go` | Security and control-plane maintainers |
| External composition and artifact lineage | `internal/integration`, `internal/trainingartifact`, `internal/store/external_workloads.go`, `cmd/infercrane/sandbox.go`, `cmd/infercrane/training.go` | Security, integration, and control-plane maintainers |
| API contract, SDKs and delivery automation | `internal/apicontract`, `internal/controlclient`, `api`, `sdk`, `integrations/terraform`, `actions/infercrane` | Developer experience and control-plane maintainers |
| Web console API and hosted identity boundary | `internal/authn`, `internal/store/console_identity.go`, `internal/controlapi` | Developer experience and security maintainers |
| Release and operations | `Dockerfile`, `compose.yaml`, `deploy`, `docs/production.md` | Operations maintainer |
| Engineering governance | `AGENTS.md`, `CLAUDE.md`, `docs`, `tools`, `.github` | Project maintainer |

Before opening the repository to outside contributors, map these roles to public GitHub teams in
`.github/CODEOWNERS`. Do not add placeholder handles that GitHub cannot resolve.
