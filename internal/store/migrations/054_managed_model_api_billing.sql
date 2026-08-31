ALTER TABLE provider_connections DROP CONSTRAINT provider_connections_adapter_check;
ALTER TABLE provider_connections ADD CONSTRAINT provider_connections_adapter_check
  CHECK(adapter IN ('openrouter','openai-compatible-external','modal','runpod-serverless-api','fly-io'));

CREATE TABLE managed_wallets (
  tenant_id TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  currency TEXT NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
  balance_microusd BIGINT NOT NULL DEFAULT 0,
  reserved_microusd BIGINT NOT NULL DEFAULT 0 CHECK(reserved_microusd>=0),
  updated_at TIMESTAMPTZ NOT NULL,
  CHECK(balance_microusd-reserved_microusd>=0)
);

CREATE TABLE managed_usage_reservations (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE RESTRICT,
  supplier TEXT NOT NULL,
  model TEXT NOT NULL,
  reserved_microusd BIGINT NOT NULL CHECK(reserved_microusd>0),
  actual_microusd BIGINT,
  input_tokens BIGINT,
  output_tokens BIGINT,
  state TEXT NOT NULL CHECK(state IN ('reserved','settled','released','pending_reconciliation')),
  resolution TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CHECK(actual_microusd IS NULL OR actual_microusd>=0),
  CHECK(input_tokens IS NULL OR input_tokens>=0),
  CHECK(output_tokens IS NULL OR output_tokens>=0)
);
CREATE INDEX managed_usage_reservations_tenant_created_idx
  ON managed_usage_reservations(tenant_id,created_at DESC);

CREATE TABLE managed_wallet_ledger (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  reservation_id TEXT REFERENCES managed_usage_reservations(id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK(kind IN ('credit','settlement')),
  currency TEXT NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
  amount_microusd BIGINT NOT NULL,
  description TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,reservation_id,kind)
);
CREATE INDEX managed_wallet_ledger_tenant_created_idx
  ON managed_wallet_ledger(tenant_id,created_at DESC);
