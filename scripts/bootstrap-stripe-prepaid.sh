#!/bin/sh
set -eu

command -v stripe >/dev/null 2>&1 || {
  echo 'Stripe CLI is required' >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  echo 'jq is required' >&2
  exit 1
}

product_name='InferCrane prepaid credits'
product_id=$(
  stripe products list --limit 100 --color off |
    jq -r --arg name "$product_name" '.data[] | select(.active == true and .name == $name) | .id' |
    head -n 1
)

if [ -z "$product_id" ]; then
  product_id=$(
    stripe products create \
      --name "$product_name" \
      --description 'Prepaid usage credits for InferCrane managed services' \
      -d 'metadata[infercrane_catalog]=prepaid_v1' \
      --confirm \
      --color off |
      jq -r '.id'
  )
fi

prices=$(stripe prices list --product "$product_id" --limit 100 --color off)
price_map='{}'

for dollars in 25 50 100 250 500; do
  cents=$((dollars * 100))
  price_id=$(printf '%s' "$prices" | jq -r --argjson cents "$cents" '.data[] | select(.active == true and .currency == "usd" and .unit_amount == $cents and .type == "one_time") | .id' | head -n 1)
  if [ -z "$price_id" ]; then
    price_id=$(
      stripe prices create \
        --currency usd \
        --product "$product_id" \
        --unit-amount "$cents" \
        --nickname "InferCrane USD $dollars prepaid credit" \
        -d "metadata[infercrane_credit_microusd]=$((dollars * 1000000))" \
        --confirm \
        --color off |
        jq -r '.id'
    )
  fi
  price_map=$(printf '%s' "$price_map" | jq -c --arg key "$dollars" --arg value "$price_id" '. + {($key): $value}')
done

printf 'INFERCRANE_STRIPE_PRICE_IDS_JSON=%s\n' "$price_map"
