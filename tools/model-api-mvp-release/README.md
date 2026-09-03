# Model API MVP release manifests

This operator-only tool turns fresh qualification evidence into the nine
immutable manifests required for a canary Model API release. It supports only:

- `glm-5.2`, `glm-5.3`, and `glm-5.3-flash` through direct Z.ai pay-as-you-go
- `qwen3.8-27b-runpod` through the pinned provisional RunPod SGLang package

It does not call a supplier, deploy compute, publish manifests, or expose
credentials. Z.ai launch rates are pinned to the reviewed list-cost profiles.
The resulting zero-margin exception expires with the evidence and no later
than seven days, so an operator must review it weekly. RunPod requires measured
per-token COGS and explicit retail rates; guessed economics fail closed.

Example after qualification:

```sh
go run ./tools/model-api-mvp-release \
  --profile glm-5.3 \
  --qualification ./artifacts/zai-glm-5.3-qualification.json \
  --credential-reference zai-production-key \
  --operator-workspace OPERATOR_WORKSPACE_ID \
  --serving-plan SERVING_PLAN_ID \
  --customer-workspace CANARY_WORKSPACE_ID \
  --commercial-terms-ref contract://zai-mvp-reviewed-2026-09 \
  --output-directory ./artifacts/zai-glm-5.3-release
```

Review the generated files, then publish them in numeric order with:

```sh
infercrane model-api publish product --file 01-product-catalog-only.json
infercrane model-api publish rate --file 02-retail-rate.json
infercrane model-api publish offer --file 03-supplier-offer.json
infercrane model-api publish qualification --file 04-qualification.json
infercrane model-api publish target-binding --file 05-target-binding.json
infercrane model-api publish plan --file 06-supply-plan.json
infercrane model-api publish publication --file 07-publication.json
infercrane model-api publish product --file 08-product-available.json
infercrane model-api publish entitlement --file 09-canary-entitlement.json
```

For Qwen, also pass the exact qualified `--endpoint` plus all four measured
COGS/retail flags. The endpoint-origin digest in the evidence must match.
