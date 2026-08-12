#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

# Developer qualification owns its PostgreSQL fixture. A stale database URL
# inherited from an operator shell must not alter the pre-container verifier.
grep -Fq 'step repository env -u INFERCRANE_TEST_DATABASE_URL make -C "$root" verify' \
  "$root/scripts/dev-check.sh"

state="$temporary/state"
mkdir -p "$state/.paid.lock"
printf '%s\n' "$$" >"$state/.paid.lock/pid"
printf '%s\n' "first-run" >"$state/.paid.lock/run_id"

if INFERCRANE_ACCEPTANCE_STATE_DIR="$state" INFERCRANE_ACCEPTANCE_RUN_ID=second-run \
  "$root/scripts/release-acceptance.sh" elastic --approve-paid-resources \
  >"$temporary/concurrent.log" 2>&1; then
  echo "concurrent paid acceptance unexpectedly started" >&2
  exit 1
fi
grep -q 'another paid acceptance run is active' "$temporary/concurrent.log"

empty_state="$temporary/unapproved"
if INFERCRANE_ACCEPTANCE_STATE_DIR="$empty_state" INFERCRANE_ACCEPTANCE_RUN_ID=unapproved \
  "$root/scripts/release-acceptance.sh" elastic >"$temporary/unapproved.log" 2>&1; then
  echo "unapproved paid acceptance unexpectedly started" >&2
  exit 1
fi
grep -q 'refusing paid provider mutation' "$temporary/unapproved.log"
test ! -e "$empty_state/.paid.lock"

if INFERCRANE_V1_ACCEPTANCE_STATE_DIR="$temporary/v1" INFERCRANE_ACCEPTANCE_RUN_ID=unapproved-v1 \
  "$root/scripts/v1-acceptance.sh" qualify >"$temporary/unapproved-v1.log" 2>&1; then
  echo "unapproved v1 qualification unexpectedly started" >&2
  exit 1
fi
grep -q 'qualification requires --approve-paid-resources' "$temporary/unapproved-v1.log"

mkdir -p "$temporary/v1-locked/.paid.lock"
printf '%s\n' "$$" >"$temporary/v1-locked/.paid.lock/pid"
printf '%s\n' existing-run >"$temporary/v1-locked/.paid.lock/run_id"
if INFERCRANE_V1_ACCEPTANCE_STATE_DIR="$temporary/v1-locked" INFERCRANE_ACCEPTANCE_RUN_ID=concurrent-v1 \
  "$root/scripts/v1-acceptance.sh" qualify --approve-paid-resources >"$temporary/concurrent-v1.log" 2>&1; then
  echo "concurrent v1 paid qualification unexpectedly started" >&2
  exit 1
fi
grep -q 'another v1 paid qualification is active' "$temporary/concurrent-v1.log"

if INFERCRANE_V1_ACCEPTANCE_STATE_DIR="$temporary/portable" INFERCRANE_ACCEPTANCE_RUN_ID=unapproved-provider \
  "$root/scripts/portable-provider-acceptance.sh" aws >"$temporary/unapproved-provider.log" 2>&1; then
  echo "unapproved portable provider qualification unexpectedly started" >&2
  exit 1
fi
grep -q 'portable provider acceptance requires --approve-paid-resources' "$temporary/unapproved-provider.log"

mkdir -p "$temporary/v1-report/stale/stages"
for stage in runpod aws kubernetes; do
  printf '%s\n' stale-commit >"$temporary/v1-report/stale/stages/$stage.passed"
done
report=$(INFERCRANE_V1_ACCEPTANCE_STATE_DIR="$temporary/v1-report" INFERCRANE_ACCEPTANCE_RUN_ID=stale \
  "$root/scripts/v1-acceptance.sh" report)
jq -e '.real_infrastructure == "incomplete" and (.passed_stages | length == 0)' "$report" >/dev/null

echo "acceptance paid-run locks and approval boundaries passed"
