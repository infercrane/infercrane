-- Pin the exact private execution contract used by each reservation and keep
-- supplier COGS separate from the customer wallet ledger.
ALTER TABLE model_api_target_bindings
  ADD CONSTRAINT model_api_target_bindings_id_digest_unique UNIQUE(id,contract_digest);

ALTER TABLE model_api_usage_reservations
  ADD COLUMN target_binding_id TEXT,
  ADD COLUMN target_binding_digest TEXT,
  ADD COLUMN supplier_rate_id TEXT,
  ADD COLUMN supplier_rate_version BIGINT CHECK(supplier_rate_version IS NULL OR supplier_rate_version>0),
  ADD COLUMN supplier_rate_digest TEXT CHECK(supplier_rate_digest IS NULL OR supplier_rate_digest ~ '^sha256:[0-9a-f]{64}$'),
  ADD COLUMN cached_input_tokens BIGINT CHECK(cached_input_tokens IS NULL OR cached_input_tokens>=0),
  ADD CONSTRAINT model_api_usage_reservations_target_binding_pair
    CHECK((target_binding_id IS NULL)=(target_binding_digest IS NULL)),
  ADD CONSTRAINT model_api_usage_reservations_target_binding_fk
    FOREIGN KEY(target_binding_id,target_binding_digest)
    REFERENCES model_api_target_bindings(id,contract_digest) ON DELETE RESTRICT,
  ADD CONSTRAINT model_api_usage_reservations_supplier_rate_identity
    CHECK((supplier_rate_id IS NULL AND supplier_rate_version IS NULL AND supplier_rate_digest IS NULL) OR
          (supplier_rate_id IS NOT NULL AND supplier_rate_version IS NOT NULL AND supplier_rate_digest IS NOT NULL));

CREATE TABLE model_api_supplier_cogs (
  reservation_id TEXT PRIMARY KEY REFERENCES model_api_usage_reservations(id) ON DELETE RESTRICT,
  customer_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  supplier_rate_id TEXT NOT NULL,
  supplier_rate_version BIGINT NOT NULL CHECK(supplier_rate_version>0),
  supplier_rate_digest TEXT NOT NULL CHECK(supplier_rate_digest ~ '^sha256:[0-9a-f]{64}$'),
  supplier TEXT NOT NULL,
  supplier_model_id TEXT NOT NULL,
  tuple_key TEXT NOT NULL,
  currency TEXT NOT NULL CHECK(currency='USD'),
  input_tokens BIGINT NOT NULL CHECK(input_tokens>=0),
  cached_input_tokens BIGINT NOT NULL CHECK(cached_input_tokens>=0 AND cached_input_tokens<=input_tokens),
  output_tokens BIGINT NOT NULL CHECK(output_tokens>=0),
  uncached_input_cogs_microusd BIGINT NOT NULL CHECK(uncached_input_cogs_microusd>=0),
  cached_input_cogs_microusd BIGINT NOT NULL CHECK(cached_input_cogs_microusd>=0),
  output_cogs_microusd BIGINT NOT NULL CHECK(output_cogs_microusd>=0),
  supplier_cogs_microusd BIGINT NOT NULL CHECK(supplier_cogs_microusd>=0),
  retail_microusd BIGINT NOT NULL CHECK(retail_microusd>=0),
  gross_profit_microusd BIGINT NOT NULL,
  gross_margin_defined BOOLEAN NOT NULL,
  gross_margin_bps BIGINT,
  reconciliation_digest TEXT NOT NULL UNIQUE CHECK(reconciliation_digest ~ '^sha256:[0-9a-f]{64}$'),
  reserved_at TIMESTAMPTZ NOT NULL,
  settled_at TIMESTAMPTZ NOT NULL CHECK(settled_at>=reserved_at),
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(customer_tenant_id,reservation_id),
  CHECK((gross_margin_defined AND gross_margin_bps IS NOT NULL) OR (NOT gross_margin_defined AND gross_margin_bps IS NULL))
);

CREATE INDEX model_api_supplier_cogs_operator_created_idx
  ON model_api_supplier_cogs(operator_tenant_id,created_at DESC);
CREATE INDEX model_api_supplier_cogs_supplier_created_idx
  ON model_api_supplier_cogs(supplier,created_at DESC);
