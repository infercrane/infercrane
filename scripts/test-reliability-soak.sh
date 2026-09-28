#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
count=${INFERCRANE_SOAK_COUNT:-10}
case "$count" in ''|*[!0-9]*) echo "INFERCRANE_SOAK_COUNT must be a positive integer" >&2; exit 2;; esac
[[ "$count" -gt 0 ]] || { echo "INFERCRANE_SOAK_COUNT must be positive" >&2; exit 2; }

packages=(
  ./internal/admission
  ./internal/asyncinference
  ./internal/autoscale
  ./internal/gateway
  ./internal/operations
  ./internal/provision
  ./internal/reconcile
  ./internal/requestquota
  ./internal/routes
  ./internal/workflows
)

echo "==> shuffled race soak ($count repetitions)"
(cd "$root" && go test -race -shuffle=on -count="$count" "${packages[@]}")

if [[ -n "${INFERCRANE_TEST_DATABASE_URL:-}" ]]; then
  echo "==> PostgreSQL fencing/contention soak ($count repetitions)"
  # The full gate already checks every historical migration prefix once. That
  # deterministic O(migrations^2) compatibility matrix and the broad store
  # suite must not be multiplied by the soak count. Repeat the operations whose
  # correctness specifically depends on locking, fencing, leases, or atomic
  # idempotency; every other store test still runs once in the normal gate.
  store_soak_pattern='^(TestAsyncInferenceIsIdempotentFencedCancellableAndExpirable|TestHostedIdentityBootstrapIsConcurrentAndIdempotent|TestManagedFundingIntentGrantsOneConcurrentCreationLease|TestManagedPaymentWebhookIsAtomicAndSessionIdempotent|TestManagedDeploymentReservationIsAtomicIdempotentAndSettledAfterCleanup|TestManagedWalletAuthorizesAndSettlesExactlyOnce|TestConcurrentStartupSerializesMigrations|TestOptimizationCampaignIsBoundedDurableFencedAndCleansRejectedCandidate|TestSubmitCloudDeploymentIsAtomicAndIdempotent|TestRequestQuotaReservationsAreDistributedAndWindowed|TestOperationQueueLeasesAndRecoversExpiredWork|TestEnqueuedOperationIsImmediatelyClaimableByDatabaseClock|TestConcurrentOperationClaimsAreExactlyOnce|TestStaleLeaseCannotCheckpointOrFinish|TestCancellationPersistedBeforeCompletionCannotBeOverwrittenBySuccess|TestDeploymentLifecycleMutationsAreSerialized|TestScaleToQueuesExactlyOneDurableOperation|TestReplicaIntentAndProviderIdentityAreIdempotent)$'
  (cd "$root" && go test -short -race -shuffle=on -count="$count" -run "$store_soak_pattern" ./internal/store)
else
  echo ":: PostgreSQL soak skipped (INFERCRANE_TEST_DATABASE_URL is unset)"
fi

echo "InferCrane reliability soak passed"
