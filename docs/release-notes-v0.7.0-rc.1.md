---
title: v0.7.0 RC1
description: Deterministic inference decisions, SLO policy, capacity evidence, and governed overflow.
---

# v0.7.0 RC1

v0.7 adds advisory, deterministic infrastructure recommendations from persisted AIPerf evidence and
explicit SLO thresholds. Evaluations preserve an algorithm version, canonical input digest,
benchmark provenance, exact qualification state, rejected candidates, and missing-data disclosure.

Capacity observations are bounded by source and expiry. Queue-based external overflow adds
consecutive breach/recovery observations and cooldown to the existing privacy acknowledgement,
model mapping, hard budgets, and no-replay guarantees.

The API, CLI, SDKs, Terraform SLO resource, dashboard, OpenAPI, docs, and local simulators expose the
same contract. Real provider capacity, pricing, and billing evidence remains deferred.

## Upgrade and rollback

Migration `024_inference_decisions.sql` is forward-only. It adds tenant-scoped decision tables and
overflow columns with conservative defaults, so existing health-only external policies retain their
behavior. Upgrade tests start from the pre-security schema, preserve legacy principal permissions,
then apply every migration through v0.7.

Do not roll the database schema backward. An application rollback to v0.6 leaves the additive v0.7
tables and columns unused; take the normal PostgreSQL backup before upgrading and restore that backup
only when a full data rollback is required.
