-- Request-path accounting for shared hosted Model APIs. Customer identity,
-- entitlement, and money stay tenant-owned; operator supply references remain
-- private. Immutable retail prices are copied onto each reservation so later
-- publication changes cannot alter settlement.
CREATE TABLE model_api_usage_reservations (
  id TEXT PRIMARY KEY,
  customer_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  entitlement_id TEXT NOT NULL REFERENCES model_api_product_entitlements(id) ON DELETE RESTRICT,
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
  supply_plan_id TEXT NOT NULL REFERENCES model_api_supply_plans(id) ON DELETE RESTRICT,
  candidate_id TEXT NOT NULL,
  offer_id TEXT NOT NULL,
  offer_version BIGINT NOT NULL CHECK(offer_version>0),
  supplier TEXT NOT NULL,
  supplier_model_id TEXT NOT NULL,
  retail_rate_card_id TEXT NOT NULL,
  retail_rate_version INTEGER NOT NULL CHECK(retail_rate_version>0),
  retail_rate_contract_digest TEXT NOT NULL CHECK(retail_rate_contract_digest LIKE 'sha256:%'),
  input_microusd_per_million BIGINT NOT NULL CHECK(input_microusd_per_million>0),
  output_microusd_per_million BIGINT NOT NULL CHECK(output_microusd_per_million>0),
  reserved_microusd BIGINT NOT NULL CHECK(reserved_microusd>0),
  actual_microusd BIGINT CHECK(actual_microusd IS NULL OR actual_microusd>=0),
  input_tokens BIGINT CHECK(input_tokens IS NULL OR input_tokens>=0),
  output_tokens BIGINT CHECK(output_tokens IS NULL OR output_tokens>=0),
  state TEXT NOT NULL CHECK(state IN ('reserved','transmitted','response_started','settled','released','pending_reconciliation')),
  resolution TEXT NOT NULL DEFAULT '',
  transmitted_at TIMESTAMPTZ,
  response_started_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  FOREIGN KEY(customer_tenant_id,product_id)
    REFERENCES model_api_product_entitlements(customer_tenant_id,product_id) ON DELETE RESTRICT,
  FOREIGN KEY(product_id,operator_tenant_id,supply_plan_id)
    REFERENCES model_api_supply_plans(managed_product_id,operator_tenant_id,id) ON DELETE RESTRICT,
  FOREIGN KEY(offer_id,offer_version,product_id)
    REFERENCES model_api_supplier_offers(id,version,managed_product_id) ON DELETE RESTRICT,
  FOREIGN KEY(product_id,retail_rate_card_id,retail_rate_version)
    REFERENCES model_api_retail_rate_cards(product_id,id,version) ON DELETE RESTRICT,
  CHECK(customer_tenant_id<>operator_tenant_id),
  CHECK(updated_at>=created_at),
  CHECK(transmitted_at IS NULL OR transmitted_at>=created_at),
  CHECK(response_started_at IS NULL OR (transmitted_at IS NOT NULL AND response_started_at>=transmitted_at))
);
CREATE INDEX model_api_usage_reservations_customer_state_idx
  ON model_api_usage_reservations(customer_tenant_id,state,created_at DESC);
CREATE INDEX model_api_usage_reservations_reconciliation_idx
  ON model_api_usage_reservations(state,updated_at)
  WHERE state='pending_reconciliation';

CREATE TABLE model_api_usage_ledger (
  id TEXT PRIMARY KEY,
  customer_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  reservation_id TEXT NOT NULL REFERENCES model_api_usage_reservations(id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK(kind='settlement'),
  currency TEXT NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
  amount_microusd BIGINT NOT NULL CHECK(amount_microusd<=0),
  description TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(customer_tenant_id,reservation_id,kind)
);
