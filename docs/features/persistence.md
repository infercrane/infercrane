# Persistence and migrations

Status: Implemented

## Entry points

- Connection, migration runner and target writes: `internal/store/store.go`
- Control-plane operations and statistics: `internal/store/operations.go`
- Migrations: `internal/store/migrations/*.sql`
- PostgreSQL integration tests: `internal/store/store_test.go`

## Contract

PostgreSQL is the source of truth. Startup serializes migrations using an advisory lock, applies
each new file transactionally, and records it in `schema_migrations`. The pool is bounded and has
connection lifetime/idle limits. High-volume request records use indexed time access and batched
retention.

## Migration policy

- Use sequential zero-padded filenames such as `002_add_example.sql`.
- Never edit an applied migration after merge.
- Prefer expand/migrate/contract changes that tolerate rolling mixed versions.
- Destructive or table-rewriting operations require a dedicated ADR and production rollout plan.
- Schema and feature documentation change in the same pull request.

## Tests

Set `INFERCRANE_TEST_DATABASE_URL` to run real PostgreSQL tests. CI always sets it. Tests may
truncate only the explicitly configured test database and must never infer a production target.

Related: [ADR 0002](../adr/0002-postgresql-source-of-truth.md).
