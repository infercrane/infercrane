CREATE TABLE managed_spend_reservations (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  resource_type TEXT NOT NULL CHECK(resource_type IN ('deployment','optimization_campaign')),
  resource_name TEXT NOT NULL,
  provider TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('reserved','settled','released','pending_reconciliation')),
  currency TEXT NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
  supplier_hourly_microusd BIGINT NOT NULL CHECK(supplier_hourly_microusd>0),
  retail_hourly_microusd BIGINT NOT NULL CHECK(retail_hourly_microusd>=supplier_hourly_microusd),
  reserved_microusd BIGINT NOT NULL CHECK(reserved_microusd>0),
  actual_microusd BIGINT CHECK(actual_microusd IS NULL OR actual_microusd>=0),
  gross_margin_bps INTEGER NOT NULL CHECK(gross_margin_bps>=0 AND gross_margin_bps<10000),
  runtime_limit_seconds INTEGER NOT NULL CHECK(runtime_limit_seconds>0),
  cleanup_allowance_seconds INTEGER NOT NULL CHECK(cleanup_allowance_seconds>=0),
  pricing_json JSONB NOT NULL,
  resolution TEXT NOT NULL DEFAULT '',
  expires_at TIMESTAMPTZ NOT NULL,
  activated_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,resource_type,resource_name)
);

CREATE INDEX managed_spend_reservations_expiry_idx
  ON managed_spend_reservations(state,expires_at)
  WHERE state='reserved';

ALTER TABLE managed_wallet_ledger
  ADD COLUMN spend_reservation_id TEXT REFERENCES managed_spend_reservations(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX managed_wallet_ledger_spend_reservation_kind_idx
  ON managed_wallet_ledger(tenant_id,spend_reservation_id,kind);
