# InferCrane v1.6.0-rc.1

v1.6 makes the control plane locally recoverable and gives operators explicit evidence for replica
membership, mixed-version admission, transport identity, and database recovery.

## Highlights

- Stateless API replicas register live binary/protocol evidence; incompatible overlap fails startup.
- Durable operation leases remain the mutation fence, so stale workers cannot checkpoint or finish reclaimed work.
- Native TLS requires TLS 1.3; optional client-CA configuration enforces mTLS for CLI and SDK clients.
- Backup and restore scripts verify archives, checksums, exact restore targets, live-member absence, and migration state.
- Docker drills prove two-replica API survival and startup against a separately restored PostgreSQL database.

## Qualification

The complete local race, API, CLI, SDK, security, Docker, Kind, HA, restore, docs, and prior-milestone
suite passed before this RC was sealed. No cloud provider or paid infrastructure was contacted.

## Known limitations

- InferCrane does not operate PostgreSQL HA; operators must provide and qualify their database topology.
- Certificate files are read at startup and rotate through a graceful rolling restart.
- Protocol overlap prevents known-incompatible members but does not replace release-note schema compatibility.
- Customer-specific RTO/RPO and real provider failover evidence remain deferred.

