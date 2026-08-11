# v1 security review

This review defines the automated security boundary for `v1.0.0-rc.1`. It is not a penetration-test
certificate or a substitute for deployment-specific threat modeling.

| Boundary | Control | Automated evidence |
|---|---|---|
| API and inference authentication | constant-time bootstrap check; hashed scoped credentials; rotation/revocation cache | gateway, authn, control API, and store negative tests |
| Tenant isolation | tenant-qualified store queries and route snapshots | adversarial API/store/gateway tests and race suite |
| Authorization | role ceiling plus explicit scopes; sensitive actions never inherited during migration | authz and control API permission tests |
| Request quota | transactionally reserved UTC-minute leases; in-memory authorization; fail-closed exhaustion | concurrent PostgreSQL reservation, no-I/O pool, and pre-upstream 429 tests |
| Secrets | references or read-only mounts; raw values rejected from APIs and persisted provider details | secret API, provider redaction, URL, benchmark, and Compose tests |
| External inference | explicit privacy acknowledgement, atomic request/cost reservation, no streaming replay | external coordinator, budget, gateway, and overflow tests |
| Provider ownership | deterministic identities, constrained tags/labels, adoption, conflict refusal, exact deletion | provider conformance, fault injection, Kind, and manifest tests |
| Supply chain | pinned direct tools/images, checksum-verified AWS CLI and kubectl, checksums, SPDX SBOMs, provenance workflow | runtime-image build smoke and release artifact verifier |
| Container | non-root runtime; all Linux capabilities dropped; no-new-privileges; no development fakes | production Compose render and Docker stack/image checks |
| Input/transport | bounded JSON/request bodies and headers, strict spec fields, safe upstream URLs, timeouts, streaming-aware shutdown | gateway/control API/spec/config tests |
| Database evolution | advisory lock, transactions, immutable checksums, contiguous ledger, newer-binary rejection | every-prefix, concurrent startup, tamper/gap/newer tests |
| Evidence signing | restricted key files, canonical Ed25519 payload, offline verification, tamper rejection | passport CLI/API/store/package tests |

## Residual risks and operator controls

- The self-hosted bootstrap credential is a break-glass administrator secret. Replace routine use
  with scoped principals and protect the API with TLS and network policy.
- Request-quota policy changes propagate to gateway snapshots within one second. A reduced ceiling
  is fully effective at the next UTC-minute boundary because an issued lease cannot be recalled from
  another process; zero fails closed after refresh. Reserved leases can produce conservative 429
  responses during database failure and never exceed the ceiling under which they were issued.
- Provider CLIs and credential plugins execute as child processes. Pin and review their versions,
  restrict mounted configuration, and repeat SBOM/vulnerability review for the published image.
- The universal image is intentionally larger because it includes optional provider clients and
  benchmark tooling. The release digest, SBOM, and provenance identify the exact artifact; builds
  are not claimed byte-for-byte reproducible across registries or time.
- Metrics are unauthenticated at the application layer. Restrict `/metrics` at the network boundary.
- Real provider isolation, IAM/RBAC, cancellation, deletion, and zero-inventory evidence remains
  `DEFERRED` until the consolidated manual RC workflow passes.

Run `make audit`, `make verify`, `make test-container`, `make test-production-config`, and
`make candidate-artifacts RELEASE_CANDIDATE_TAG=v1.0.0-rc.1` on the clean candidate. Review the
resulting SBOMs and vulnerability output; a green dependency scanner does not prove absence of a
logic vulnerability.
