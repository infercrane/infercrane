#!/usr/bin/env bash
# Phase 0b. Reads, per AWS region, what decides where the GPU node can run:
# the on-demand quota for G instances, the zones that offer each GPU type the
# tool allows, and its list price. It reads only; nothing is created.
#
#   tools/eks-agent-run/01-regions.sh                 every enabled region
#   tools/eks-agent-run/01-regions.sh eu-west-1 ...   only these
#
# A quota and an offering do not show capacity: AWS says that only when an
# instance or a capacity reservation is requested (20-create.sh does that).
# Output: a table on stdout and $STATE_DIR/regions.tsv.
set -euo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

# The on-demand "Running On-Demand G and VT instances" quota, in vCPUs.
QUOTA_G=L-DB2E81BA

quota() {
  local region=$1 value
  value=$(aws service-quotas get-service-quota --region "$region" --service-code ec2 --quota-code "$QUOTA_G" \
    --query 'Quota.Value' --output json 2>/dev/null || true)
  if [ -z "$value" ] || [ "$value" = null ]; then
    value=$(aws service-quotas get-aws-default-service-quota --region "$region" --service-code ec2 --quota-code "$QUOTA_G" \
      --query 'Quota.Value' --output json 2>/dev/null || true)
    [ -n "$value" ] && [ "$value" != null ] && value="$value(default)"
  fi
  printf '%s' "${value:-unreadable}"
}

zones_offering() {
  local region=$1 type=$2
  aws ec2 describe-instance-type-offerings --region "$region" --location-type availability-zone \
    --filters "Name=instance-type,Values=$type" --query 'InstanceTypeOfferings[].Location' --output json 2>/dev/null |
    python3 -c 'import json, sys; print(",".join(sorted(json.load(sys.stdin))) or "-")' 2>/dev/null || printf 'unreadable'
}

price() {
  local region=$1 type=$2
  aws pricing get-products --region us-east-1 --service-code AmazonEC2 --output json \
    --filters "Type=TERM_MATCH,Field=instanceType,Value=$type" "Type=TERM_MATCH,Field=regionCode,Value=$region" \
    "Type=TERM_MATCH,Field=operatingSystem,Value=Linux" "Type=TERM_MATCH,Field=tenancy,Value=Shared" \
    "Type=TERM_MATCH,Field=preInstalledSw,Value=NA" "Type=TERM_MATCH,Field=capacitystatus,Value=Used" 2>/dev/null |
    python3 -c '
import json, sys
items = json.load(sys.stdin).get("PriceList", [])
prices = set()
for raw in items:
    product = json.loads(raw)
    for term in product.get("terms", {}).get("OnDemand", {}).values():
        for dimension in term.get("priceDimensions", {}).values():
            prices.add(dimension["pricePerUnit"]["USD"])
print("/".join(sorted(prices)) or "-")' 2>/dev/null || printf 'unreadable'
}

regions_phase() {
  need_tools aws python3
  aws_account
  local regions=("$@") region
  if [ "${#regions[@]}" -eq 0 ]; then
    while IFS= read -r region; do regions+=("$region"); done < <(aws ec2 describe-regions --output json \
      --query 'Regions[].RegionName' | python3 -c 'import json, sys; print("\n".join(sorted(json.load(sys.stdin))))')
  fi
  local out="$STATE_DIR/regions.tsv" type
  printf 'region\tquota_g_vcpus' >"$out"
  for type in $GPU_INSTANCES; do printf '\t%s_zones\t%s_usd_hour' "$type" "$type" >>"$out"; done
  printf '\n' >>"$out"
  for region in "${regions[@]}"; do
    printf '%s\t%s' "$region" "$(quota "$region")" >>"$out"
    for type in $GPU_INSTANCES; do
      printf '\t%s\t%s' "$(zones_offering "$region" "$type")" "$(price "$region" "$type")" >>"$out"
    done
    printf '\n' >>"$out"
  done
  step "G-instance quota (vCPUs), zones offering each GPU type, list price per hour"
  column -t -s $'\t' "$out"
  say ""
  say "A region qualifies when its quota is at least 4 vCPUs per GPU node and a zone offers the type."
  say "Written to $out"
}

run_masked regions_phase "$@"
