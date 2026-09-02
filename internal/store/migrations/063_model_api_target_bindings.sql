-- A target binding is the immutable private bridge between one stable public
-- Model API product and one exact execution target. The redundant offer fields
-- below are intentional: the composite foreign key proves that adapter, model,
-- region, product, and operator all came from the pinned offer revision.
ALTER TABLE model_api_supplier_offers
  ADD CONSTRAINT model_api_supplier_offers_target_binding_identity
  UNIQUE(id,version,managed_product_id,operator_tenant_id,adapter,supplier_model_id,region);

CREATE TABLE model_api_target_bindings (
  id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL CHECK(schema_version='model-api-target-binding/v1'),
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  managed_product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  target_kind TEXT NOT NULL CHECK(target_kind IN ('upstream','serverless_gpu','dedicated','byoc')),
  offer_id TEXT NOT NULL,
  offer_version BIGINT NOT NULL CHECK(offer_version>0),
  adapter TEXT NOT NULL,
  supplier_model_id TEXT NOT NULL,
  endpoint_reference TEXT NOT NULL,
  endpoint_config_digest TEXT NOT NULL CHECK(endpoint_config_digest ~ '^sha256:[0-9a-f]{64}$'),
  region TEXT NOT NULL,
  valid_from TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  contract_digest TEXT NOT NULL UNIQUE CHECK(contract_digest ~ '^sha256:[0-9a-f]{64}$'),
  FOREIGN KEY(offer_id,offer_version,managed_product_id,operator_tenant_id,adapter,supplier_model_id,region)
    REFERENCES model_api_supplier_offers(id,version,managed_product_id,operator_tenant_id,adapter,supplier_model_id,region)
    ON DELETE RESTRICT,
  CHECK(valid_until>valid_from),
  CHECK(created_at<=valid_from)
);

CREATE INDEX model_api_target_bindings_operator_product_validity_idx
  ON model_api_target_bindings(operator_tenant_id,managed_product_id,valid_from DESC,valid_until DESC);
CREATE INDEX model_api_target_bindings_offer_idx
  ON model_api_target_bindings(offer_id,offer_version);

CREATE FUNCTION infercrane_reject_model_api_target_binding_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'model API target bindings are immutable; publish a new binding';
END;
$$;

CREATE TRIGGER model_api_target_bindings_immutable
  BEFORE UPDATE OR DELETE ON model_api_target_bindings
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_target_binding_mutation();
