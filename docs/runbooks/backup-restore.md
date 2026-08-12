---
title: PostgreSQL backup and restore
description: Back up control-plane state, restore into a safe target, and reconcile external resources.
sidebarTitle: Backup and restore
---

# PostgreSQL backup and restore drill

Set a TLS-protected `INFERCRANE_DATABASE_URL`, then create and validate a custom-format backup:

```bash
scripts/backup-postgres.sh infercrane-$(date +%Y%m%d).dump
```

Restore only into a verified empty or disposable target first:

```bash
export INFERCRANE_ALLOW_RESTORE=yes
export INFERCRANE_RESTORE_TARGET_DATABASE=infercrane_restore_drill
scripts/restore-postgres.sh infercrane-20260809.dump
```

Restart InferCrane, check `/readyz`, list deployments, send an inference request, and compare row
counts and recent audit/operation events. The restore refuses a target-name mismatch or a database
with live control-plane heartbeats. Record recovery-point and recovery-time measurements in the
release evidence. Never test destructive restore behavior against the only production copy.

Set an operational backup schedule from the required recovery point objective (RPO); InferCrane does
not silently choose one. Measure restore plus reconciliation against the recovery time objective
(RTO). A database restore is incomplete until owned external resources have been inventoried and
reconciled without duplicate mutation.
