# PostgreSQL backup and restore drill

Set a TLS-protected `INFERCRANE_DATABASE_URL`, then create and validate a custom-format backup:

```bash
scripts/backup-postgres.sh infercrane-$(date +%Y%m%d).dump
```

Restore only into a verified empty or disposable target first:

```bash
export INFERCRANE_ALLOW_RESTORE=yes
scripts/restore-postgres.sh infercrane-20260809.dump
```

Restart InferCrane, check `/readyz`, list deployments, send an inference request, and compare row
counts and recent audit/operation events. Record recovery-point and recovery-time measurements in
the release evidence. Never test destructive restore behavior against the only production copy.
