#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
commit=$(git -C "$root" rev-parse HEAD)
state="$temporary/$commit"
mkdir -p "$state/gates"

INFERCRANE_PRODUCT_QUALIFICATION_DIR="$temporary" "$root/scripts/qualify-product.sh" report >"$temporary/initial.log"
jq -e '.verdict=="INCOMPLETE" and .summary.NOT_RUN==8 and .summary.REAL_INFRA_REQUIRED==7 and .summary.HUMAN_REQUIRED==1' "$state/report.json" >/dev/null

# Copying evidence from another source revision into the current directory
# must not qualify this commit.
jq -n '{schema_version:1,gate:"developer-environment",status:"PASSED",reason:"stale fixture",commit:"stale-commit",generated_at:"fixture",log:null}' >"$state/gates/developer-environment.json"
INFERCRANE_PRODUCT_QUALIFICATION_DIR="$temporary" "$root/scripts/qualify-product.sh" report >"$temporary/stale.log"
jq -e '([.gates[]|select(.id=="developer-environment")][0].status)=="NOT_RUN"' "$state/report.json" >/dev/null

jq -n --arg commit "$commit" '{schema_version:1,gate:"developer-environment",status:"PASSED",reason:"fixture",commit:$commit,generated_at:"fixture",log:null}' >"$state/gates/developer-environment.json"
jq -n --arg commit "$commit" '{schema_version:1,gate:"product-journeys",status:"FAILED",reason:"fixture failure",commit:$commit,generated_at:"fixture",log:"fixture.log"}' >"$state/gates/product-journeys.json"
INFERCRANE_PRODUCT_QUALIFICATION_DIR="$temporary" "$root/scripts/qualify-product.sh" report >"$temporary/failed.log"
jq -e '.verdict=="FAILED" and ([.gates[]|select(.id=="product-journeys")][0].status)=="FAILED"' "$state/report.json" >/dev/null

for gate in product-journeys adapter-contracts supply-chain; do
  jq -n --arg commit "$commit" --arg gate "$gate" '{schema_version:1,gate:$gate,status:"PASSED",reason:"fixture",commit:$commit,generated_at:"fixture",log:null}' >"$state/gates/$gate.json"
done
INFERCRANE_PRODUCT_QUALIFICATION_DIR="$temporary" "$root/scripts/qualify-product.sh" report >"$temporary/passed.log"
jq -e '.verdict=="LOCAL_QUALIFIED" and .summary.PASSED==4 and .summary.NOT_RUN==4 and .summary.REAL_INFRA_REQUIRED==7' "$state/report.json" >/dev/null

# RunPod evidence is deliberately split so elastic stock failures cannot hide
# successful serverless qualification (or vice versa).
for gate in runpod-serverless-real runpod-serverless-faults-real runpod-elastic-real runpod-elastic-faults-real; do
  jq -e --arg gate "$gate" 'any(.gates[]; .id==$gate and .tier=="real-infrastructure")' "$state/report.json" >/dev/null
done
grep -Fq 'run_gate runpod-serverless-real' "$root/scripts/qualify-product.sh"
grep -Fq 'run_gate runpod-elastic-real' "$root/scripts/qualify-product.sh"

if INFERCRANE_PRODUCT_QUALIFICATION_DIR="$temporary" "$root/scripts/qualify-product.sh" runpod >"$temporary/unapproved.log" 2>&1; then
  echo "paid qualification ran without approval" >&2; exit 1
fi
grep -q -- '--approve-paid-resources' "$temporary/unapproved.log"
echo "product qualification harness passed"
