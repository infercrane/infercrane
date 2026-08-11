# v0.8.0-rc.1

Release Guard V2 adds explicit bounded AIPerf validation, exact compatibility and sourced-cost
policy, a durable post-promotion observation monitor, restart-safe automatic rollback, signed
Inference Passports, and passport-gated GitHub release checks.

## Upgrade

Back up PostgreSQL before upgrading. Migration `025_release_evidence.sql` adds bounded V2 policy
columns, signed passport history, and durable release monitors. It is forward-only. Roll the
application forward after the migration; do not run older binaries against the migrated database.

Generate and securely mount an Ed25519 signing key only if passport issuance is required:

```bash
infercrane passport keygen
export INFERCRANE_PASSPORT_SIGNING_KEY_FILE="$HOME/.config/infercrane/passport-signing-key"
```

Existing policies receive conservative bounds and compatibility evidence is required. Automatic
rollback and synthetic-evidence requirements remain disabled until explicitly enabled. Configure
them with `infercrane rollout policy set` after reviewing the temporary cost of retained capacity.

## Rollback

Application rollback across migration 025 is unsupported. Restore the pre-upgrade database backup
before running an older binary. A product rollout rollback is separate and remains available through
the immutable revision lifecycle.

## Qualification status

Local race, PostgreSQL, Docker, SDK/action, dashboard, docs, migration, and deterministic failure
tests are required for the RC tag. Paid provider and real-runtime evidence remains deferred to the
single final v1 qualification pass and must not be inferred from this candidate.
