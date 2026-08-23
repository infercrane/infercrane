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

Do **not** restart InferCrane or send a request after the restore yet. Keep every application replica
stopped and the restored database unreachable from the active control plane until the offline state
and authoritative provider inventory reconcile exactly. The restore refuses a target-name mismatch
or a database with live control-plane heartbeats, but that check does not prove external ownership.
Record recovery-point and recovery-time measurements in the release evidence. Never test destructive
restore behavior against the only production copy.

Set an operational backup schedule from the required recovery point objective (RPO); InferCrane does
not silently choose one. Measure restore plus reconciliation against the recovery time objective
(RTO). A database restore is incomplete until owned external resources have been inventoried and
reconciled without duplicate mutation.

## Reconcile restored state with real infrastructure

Keep the restored control plane stopped. Read the restored database only with a DB-operator
credential; this is an offline recovery procedure, not a public CLI workflow:

```bash
psql "$INFERCRANE_DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -c 'SELECT id,name,active_revision_id,candidate_revision_id,desired_state
      FROM deployments ORDER BY tenant_id,name' \
  > restored-deployments.csv

psql "$INFERCRANE_DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -c 'SELECT tenant_id,deployment_id,revision_id,ordinal,external_key,provider,
             provider_request_id,provider_resource_id,lifecycle_state
      FROM replicas ORDER BY tenant_id,deployment_id,revision_id,ordinal' \
  > restored-replicas.csv

psql "$INFERCRANE_DATABASE_URL" -X -v ON_ERROR_STOP=1 --csv \
  -c 'SELECT id,kind,status,idempotency_key,lease_owner,lease_generation
      FROM operations
      WHERE status NOT IN (''succeeded'',''failed'',''cancelled'')
      ORDER BY created_at,id' \
  > restored-nonterminal-operations.csv
```

For every lifecycle-managed deployment, compare its persisted provider identity and ownership tags
with the provider's read-only inventory. A database backup can be older than the provider: an empty
`orphans` response does not prove the provider account is empty. If identity is ambiguous, keep the
restored control plane stopped and resolve ownership manually; do not issue another create or
delete.

Create an evidence directory and move the three CSV files into it before starting a worker. Keep
database URLs and provider credentials out of the bundle:

```bash
evidence="restore-evidence/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$evidence"
mv restored-deployments.csv restored-replicas.csv \
  restored-nonterminal-operations.csv "$evidence/"
```

Then capture authoritative provider inventory using the same scoped identity, account/project,
region, and namespace as the deployment:

<Tabs>
  <Tab title="RunPod">
    ```bash
    runpod_key=$(tr -d '\r\n' < "$RUNPOD_KEY_FILE")
    curl -fsS -H "Authorization: Bearer $runpod_key" \
      https://rest.runpod.io/v1/pods |
      jq -S '[.[] | select((.name // "") | startswith("infercrane-")) |
        {id,name,desiredStatus}] | sort_by(.id)' \
      > "$evidence/provider-pods.json"
    curl -fsS -H "Authorization: Bearer $runpod_key" \
      'https://rest.runpod.io/v1/endpoints?includeWorkers=true' |
      jq -S '[.[] | select((.name // "") | startswith("infercrane-")) |
        {id,name,workersMin,workersMax}] | sort_by(.id)' \
      > "$evidence/provider-endpoints.json"
    unset runpod_key
    ```
  </Tab>
  <Tab title="AWS EC2">
    ```bash
    aws ec2 describe-instances --region "$INFERCRANE_AWS_REGION" \
      --filters Name=tag:infercrane:managed,Values=true \
        Name=instance-state-name,Values=pending,running,stopping,shutting-down \
      --query 'Reservations[].Instances[].{id:InstanceId,state:State.Name,external_key:Tags[?Key==`infercrane:external-key`]|[0].Value}' \
      --output json --no-cli-pager | jq -S 'sort_by(.id)' \
      > "$evidence/provider-inventory.json"
    ```

    Use the same assumed-role session validated for the deployment. An ambient account identity is
    not equivalent evidence.
  </Tab>
  <Tab title="GCP Compute">
    ```bash
    gcloud compute instances list \
      --project "$INFERCRANE_GCP_PROJECT" \
      --filter='labels.infercrane-managed=true' \
      --format=json --quiet |
      jq -S '[.[] | {id,name,status,zone,labels}] | sort_by(.id)' \
      > "$evidence/provider-inventory.json"
    ```
  </Tab>
  <Tab title="Kubernetes or KServe">
    ```bash
    kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" \
      --namespace "$INFERCRANE_KUBERNETES_NAMESPACE" \
      get deployment,service \
      -l app.kubernetes.io/managed-by=infercrane -o json |
      jq -S '[.items[] | {
        kind,uid:.metadata.uid,name:.metadata.name,
        external_key_hash:.metadata.labels["infercrane.dev/external-key-hash"]
      }] | sort_by(.kind,.name)' \
      > "$evidence/provider-inventory.json"

    if kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" \
      api-resources --api-group serving.kserve.io -o name |
      grep -qx inferenceservices; then
      kubectl --context "$INFERCRANE_KUBERNETES_CONTEXT" \
        --namespace "$INFERCRANE_KUBERNETES_NAMESPACE" \
        get inferenceservices.serving.kserve.io \
        -l app.kubernetes.io/managed-by=infercrane -o json |
        jq -S '[.items[] | {uid:.metadata.uid,name:.metadata.name}] | sort_by(.name)' \
        > "$evidence/provider-kserve-inventory.json"
    fi
    ```
  </Tab>
</Tabs>

Before starting the single restored worker, require every restored replica row to map to exactly one
provider object. Compare `provider_resource_id`, provider, immutable revision, and replica ordinal;
compare `external_key` to the provider ownership tag/label or its provider-specific hash. Also prove
that no provider object with an InferCrane ownership marker is absent from restored state. A missing,
duplicate, empty, permission-denied, truncated, or unparseable result is **not** an empty inventory
and blocks reconciliation. In shared accounts, compare against a timestamped pre-drill baseline;
never require global zero or adopt another tenant's resource.

After the identities agree, start exactly one restored control-plane worker. Only now check
readiness, query the API, watch durable operations/events, and confirm it adopts existing resources
rather than creating replacements:

```bash
curl -fsS "$INFERCRANE_URL/readyz"
infercrane system instances --output json
infercrane deployments --output json
infercrane orphans --output json
infercrane inspect DEPLOYMENT --output json
infercrane inbox --output json
infercrane operation watch OPERATION_ID
infercrane events DEPLOYMENT --output json
```

Wait for adoption and routing evidence to converge, then send one bounded inference request through
the unchanged endpoint and inspect its request ID. Only after that passes may you restore the normal
control-plane replica count. Provider-console or provider-API inventory remains a required external
check because InferCrane cannot prove resources hidden by stale, incomplete, or mis-scoped provider
credentials.

## Recover the active control plane after a failed migration

This recovery changes the database used by the control plane, not the serving resources. Keep the
active runtime and provider capacity intact while all InferCrane application replicas are stopped.

1. Retain the failed/migrated database read-only and export the available post-upgrade evidence using
   the [evidence-preserving rollback procedure](/upgrade#roll-back-without-erasing-evidence).
2. Have the database operator create a separate empty rollback database. Set the TLS-protected
   `INFERCRANE_DATABASE_URL` to that database—not to the failed database—and verify its name before
   restore.
3. Restore the pre-upgrade dump:

   ```bash
   export INFERCRANE_ALLOW_RESTORE=yes
   export INFERCRANE_RESTORE_TARGET_DATABASE=infercrane_rollback
   scripts/restore-postgres.sh /secure/path/infercrane-before-upgrade.dump
   ```

4. Keep every InferCrane replica stopped. Use the offline PostgreSQL exports and provider inventory
   procedure above to compare the restored active revision, candidate, nonterminal operations,
   provider identities, external keys, replica ordinals, and desired replica count with the
   failed-database exports and direct provider inventory. The pre-upgrade backup is authoritative
   through its timestamp; later provider state is an observation to reconcile, not permission to
   overwrite history.
5. Keep reconciliation and provider cleanup stopped while any identity is missing, duplicated, or
   mismatched. Never run `delete`, create a replacement deployment, or clear an orphan merely to make
   the restored database resemble current inventory.
6. Only after every owned resource maps unambiguously to one restored replica intent, start exactly
   one control-plane replica at the pre-upgrade application version. Do not start a mixed old/new
   set. Wait for `/readyz`, then collect API state:

   ```bash
   infercrane system instances --output json
   infercrane deployments --output json
   infercrane rollout inspect DEPLOYMENT --output json
   infercrane inspect DEPLOYMENT --output json
   infercrane orphans --output json
   ```

7. Confirm adoption without a provider create, send one bounded request through the unchanged active
   endpoint, inspect its `X-Request-Id`, and only then restore the normal same-version control-plane
   replica count.

If the active revision cannot be matched or post-backup provider mutations cannot be explained,
leave the data plane in its current safe state and escalate manual recovery. A successful database
restore alone does not prove lifecycle convergence.

Backup/restore does not transfer tenant, organization, endpoint, or provider ownership. For
credential rotation, same-tenant operator handoff, and the unsupported cross-tenant reassignment
boundary, follow [Ownership and credential transfer](/runbooks/ownership-transfer).
