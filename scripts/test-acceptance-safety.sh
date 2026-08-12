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

# Paid-suite result bookkeeping must be initialized before the first provider
# preflight. This reproduces the nested whole-product invocation without any
# network or paid mutation: Docker fails immediately, the guarded trap runs,
# and a durable FAILED result must still be recorded (never an unbound
# run_dir shell error or an INCOMPLETE report).
mkdir -p "$temporary/failed-preflight-bin"
cat >"$temporary/failed-preflight-bin/docker" <<'EOF'
#!/bin/sh
exit 23
EOF
cat >"$temporary/failed-preflight-bin/curl" <<'EOF'
#!/bin/sh
exit 23
EOF
chmod +x "$temporary/failed-preflight-bin/docker" "$temporary/failed-preflight-bin/curl"
printf '%s\n' fixture-key >"$temporary/runpod-key"
failed_preflight_state="$temporary/failed-preflight"
if PATH="$temporary/failed-preflight-bin:$PATH" \
  RUNPOD_KEY_FILE="$temporary/runpod-key" \
  INFERCRANE_ACCEPTANCE_STATE_DIR="$failed_preflight_state" \
  INFERCRANE_ACCEPTANCE_RUN_ID=failed-preflight \
  "$root/scripts/release-acceptance.sh" qualify --approve-paid-resources \
  >"$temporary/failed-preflight.log" 2>&1; then
  echo "paid qualification unexpectedly survived a failed Docker preflight" >&2
  exit 1
fi
test -f "$failed_preflight_state/failed-preflight/suite-result.json"
jq -e '.command == "qualify" and .outcome == "failed" and .exit_code != 0' \
  "$failed_preflight_state/failed-preflight/suite-result.json" >/dev/null
! grep -q 'unbound variable' "$temporary/failed-preflight.log"
INFERCRANE_ACCEPTANCE_STATE_DIR="$failed_preflight_state" \
  INFERCRANE_ACCEPTANCE_RUN_ID=failed-preflight \
  "$root/scripts/release-acceptance.sh" report >/dev/null
grep -Fq 'Suite outcome: **FAILED**' "$failed_preflight_state/failed-preflight/report.md"

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

# A failed black-box prerequisite must never be converted into a pass by a
# successful cleanup command. Rerunning a fixed identity also invalidates any
# stale marker for the journey before preflight begins.
mkdir -p "$temporary/mock-bin" "$temporary/product-state/product-failure/stages"
printf '%s\n' '#!/bin/sh' 'exit 23' >"$temporary/mock-bin/docker"
chmod +x "$temporary/mock-bin/docker"
printf '%s\n' stale >"$temporary/product-state/product-failure/stages/first-value.passed"
if PATH="$temporary/mock-bin:$PATH" \
  INFERCRANE_PRODUCT_ACCEPTANCE_STATE_DIR="$temporary/product-state" \
  INFERCRANE_PRODUCT_ACCEPTANCE_RUN_ID=product-failure \
  "$root/scripts/product-acceptance.sh" first-value >"$temporary/product-failure.log" 2>&1; then
  echo "product acceptance converted a Docker failure into success" >&2
  exit 1
fi
test ! -e "$temporary/product-state/product-failure/stages/first-value.passed"
grep -q 'docker-preflight (failed' "$temporary/product-failure.log"

# The worker-loss journey requires a running isolated stack. It must not rely
# on a developer's default Compose project or a prior journey leaving services
# behind.
grep -Fq 'COMPOSE_PROJECT_NAME=$project INFERCRANE_DEV_PORT=$port INFERCRANE_SMOKE_URL=http://127.0.0.1:$port' \
  "$root/scripts/product-acceptance.sh"
grep -Fq '"$root/scripts/test-stack.sh" || return' "$root/scripts/product-acceptance.sh"

# Paid protocol probes are release evidence. Never discard their response and
# error output, otherwise a real runtime rejection cannot be diagnosed after
# guarded cleanup removes the provider resource.
grep -Fq 'record elastic-buffered-request ic request' "$root/scripts/release-acceptance.sh"
grep -Fq 'record elastic-streaming-request ic request' "$root/scripts/release-acceptance.sh"
grep -Fq 'record serverless-cold-request ic request' "$root/scripts/release-acceptance.sh"

# Autoscaling qualification must observe the provider transition while a
# sustained workload is still active. This avoids hardware-speed-dependent
# false failures where a short benchmark ends before the control loop samples
# two consecutive queue intervals.
grep -Fq 'INFERCRANE_ACCEPTANCE_AUTOSCALE_OUTPUT_TOKENS:-512' "$root/scripts/release-acceptance.sh"
grep -Fq 'wait_replica_count "$ELASTIC_NAME" 2 "$scale_up_timeout"' "$root/scripts/release-acceptance.sh"
grep -Fq 'wait "$load_pid"' "$root/scripts/release-acceptance.sh"

# Cleanup success and provider inventory absence are separate from the suite
# result. A failed suite report must never look qualified merely because its
# guarded cleanup reached zero resources.
for outcome in passed failed; do
  report_state="$temporary/report-$outcome"
  report_run="$report_state/run-$outcome"
  mkdir -p "$report_run/evidence"
  cat >"$report_run/state.env" <<EOF
RUN_ID='run-$outcome'
CANDIDATE_COMMIT='4543321251822ae08baa80301556eab9ac5c48b4'
ELASTIC_NAME='elastic-$outcome'
SERVERLESS_NAME='serverless-$outcome'
MODEL='model'
GPU='gpu'
EOF
  exit_code=0
  [ "$outcome" = passed ] || exit_code=23
  jq -n --arg outcome "$outcome" --argjson exit_code "$exit_code" \
    '{schema_version:1,command:"elastic-faults",outcome:$outcome,exit_code:$exit_code}' \
    >"$report_run/suite-result.json"
  printf '%s\n' '{"pods":[],"endpoints":[]}' >"$report_run/evidence/provider-direct-after-cleanup.json"
  INFERCRANE_ACCEPTANCE_STATE_DIR="$report_state" INFERCRANE_ACCEPTANCE_RUN_ID="run-$outcome" \
    "$root/scripts/release-acceptance.sh" report >/dev/null
  grep -Fq "Suite outcome: **$(printf '%s' "$outcome" | tr '[:lower:]' '[:upper:]')**" "$report_run/report.md"
  grep -Fq 'Provider inventory confirmation: VERIFIED' "$report_run/report.md"
done

# The whole-product verdict is independently regression-tested: local passes,
# cloud access gaps, human review, and cleanup evidence must remain distinct.
"$root/scripts/test-product-qualification.sh"

echo "acceptance paid-run locks and approval boundaries passed"
