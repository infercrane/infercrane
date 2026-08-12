# Product qualification progress

Updated: 2026-08-11

## Current cycle

Objective: qualify every implemented InferCrane product and operational surface without paid infrastructure,
fix locally reproducible defects, and leave exact real/manual qualification procedures.

### Inspected

- Qualification baseline was clean at `8eea00d`; implementation tag `v2.0.0-rc.1` points to `1886ab8`.
- Complete Cobra command inventory and legacy dispatch.
- Authenticated API contract and server route parity source.
- All top-level internal packages, 35 forward migrations, provider/runtime registries, SDKs, Terraform,
  GitHub workflows, dashboard, Docker/Kind/HA/recovery/release and acceptance scripts.
- Existing v1.1–v2.0 qualification evidence was located but is treated as prior evidence, not as proof that
  this goal's complete feature-by-feature audit is finished.

### Tests run in this cycle

- `go test ./...` — passed before the first database-backed cycle.
- `go vet ./...` — passed.
- `cd integrations/terraform && go test ./...` — passed.
- `make test-container` — its hermetic verifier/race phase passed; the PostgreSQL phase exposed the
  defects below. After correction, the complete clean suite passed and two consecutive invocations
  passed against independently recreated PostgreSQL test containers.
- Targeted packages: `contextpassport`, `routes`, `gateway`, `controlapi`, `store`, and `finops` passed
  outside Docker; database-only assertions run in Compose.
- `./scripts/dev-check.sh full` — passed. Evidence:
  `.infercrane/dev-check/20260811T181856Z-4268`. This includes repository/race/vet, provider contracts,
  acceptance safety, manifests, Kind, Python/TypeScript/Terraform clients, containers, stack smoke,
  failure recovery, control-plane HA, backup/restore, production config, and docs/accessibility.
- `make audit` — Go root/Terraform vulnerability scans and TypeScript high-severity audit passed with
  zero known vulnerabilities.
- `make deadcode release-check` — passed.
- `make snapshot` — four Darwin/Linux amd64/arm64 archives, checksums, and Syft SBOMs built successfully;
  nothing was published.
- `INFERCRANE_ACCEPTANCE_RUN_ID=20260811T183137Z-automated-local make acceptance-local` — passed,
  including a local production-stack restart and cleanup.

### Tests added / bugs fixed

- Fixed a production wiring defect where persisted Context Passports were never supplied to the gateway.
  The server now maintains an atomic background snapshot, creation publishes immediately, expired/stale
  hints are removed, and healthy routing still overrides affinity.
- Added gateway affinity-hit/fallback, directory replacement, API publication, PostgreSQL persistence,
  expiry, and tenant-isolation coverage for Context Passport.
- Fixed FinOps accepting future observations and selecting cost observations outside the report window.
  Removed the fabricated default `USD`: currency is now required evidence.
- Added FinOps future/currency/dedup regression coverage.
- Added PostgreSQL integration coverage for content-free Replay capture, cache observations, delegated
  prefetch idempotency/conflict, capacity aggregation, FinOps persistence, recommendations, and advisory
  Autopilot approval.
- Fixed Autopilot retry conflicts caused by comparing PostgreSQL `JSONB` serialization as raw strings;
  idempotency now compares JSON semantically, including evidence.
- Corrected new integration fixtures so capacity evidence is time-bounded and independent across retries.
- Fixed `capacity --window`: the CLI accidentally sent `window_seconds` as an idempotency header instead
  of a query parameter, causing the server default window to be used.
- Added exact CLI route/body and invalid `--output` coverage for Replay, Capacity Intelligence, FinOps,
  Autopilot, Context Passport, and Burst Guard; all now reject unsupported output formats consistently.
- Fixed repeat qualification contamination: `make test-container` now recreates only its test-profile
  PostgreSQL and runner containers before execution, without touching normal developer services.
- Fixed release-version drift: the binary default, SDK packages/User-Agents, GitHub Action installer,
  production image default, release scripts, and current release documentation now agree on
  `v2.0.0-rc.1`. Client automation now enforces this parity on every run.

### Outstanding

- Automated qualification is complete. The only outstanding work is the exact real-infrastructure and
  human/hosted-system qualification recorded in `final-qualification-report.md`.

### Manual / real infrastructure boundary

No provider credential or paid resource will be used automatically. Real RunPod, AWS, GCP, CoreWeave,
GPU Kubernetes, vLLM/SGLang/custom OCI, provider cancellation/cost/cache/capacity, and real AIPerf evidence
remain candidates for `REAL_INFRA_REQUIRED` until explicitly run with the user.
