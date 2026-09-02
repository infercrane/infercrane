-- Private, operator-owned supply is versioned independently from public model
-- products. No supplier credential value belongs in these tables;
-- credential_reference names an entry in the operator's secret manager.
CREATE TABLE model_api_supplier_offers (
  id TEXT NOT NULL,
  version BIGINT NOT NULL CHECK(version>0),
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  managed_product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  supplier TEXT NOT NULL,
  adapter TEXT NOT NULL,
  supplier_model_id TEXT NOT NULL,
  protocol TEXT NOT NULL,
  tuple_key TEXT NOT NULL,
  region TEXT NOT NULL,
  credential_reference TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('active','disabled')),
  capabilities_json JSONB NOT NULL CHECK(jsonb_typeof(capabilities_json)='array'),
  access_state TEXT NOT NULL,
  availability_state TEXT NOT NULL,
  health_state TEXT NOT NULL,
  observed_at TIMESTAMPTZ,
  cost_currency TEXT NOT NULL CHECK(cost_currency='USD'),
  cost_input_microusd_per_mtoken BIGINT CHECK(cost_input_microusd_per_mtoken>=0),
  cost_output_microusd_per_mtoken BIGINT CHECK(cost_output_microusd_per_mtoken>=0),
  cost_cached_input_microusd_per_mtoken BIGINT CHECK(cost_cached_input_microusd_per_mtoken>=0),
  cost_basis_provenance TEXT,
  cost_valid_from TIMESTAMPTZ,
  cost_valid_until TIMESTAMPTZ,
  commercial_state TEXT NOT NULL CHECK(commercial_state IN ('ready','pending','expired')),
  commercial_terms_ref TEXT,
  commercial_valid_until TIMESTAMPTZ,
  hf_repository_id TEXT,
  hf_revision TEXT,
  hf_license TEXT,
  hf_source_url TEXT,
  hf_observed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY(id,version),
  UNIQUE(id,version,managed_product_id),
  CHECK((cost_input_microusd_per_mtoken IS NULL AND cost_output_microusd_per_mtoken IS NULL AND cost_cached_input_microusd_per_mtoken IS NULL AND cost_basis_provenance IS NULL AND cost_valid_from IS NULL AND cost_valid_until IS NULL) OR (cost_input_microusd_per_mtoken IS NOT NULL AND cost_output_microusd_per_mtoken IS NOT NULL AND cost_basis_provenance IS NOT NULL AND cost_valid_from IS NOT NULL AND cost_valid_until>cost_valid_from)),
  CHECK(commercial_state<>'ready' OR (commercial_terms_ref IS NOT NULL AND commercial_valid_until IS NOT NULL)),
  CHECK((hf_repository_id IS NULL AND hf_revision IS NULL AND hf_license IS NULL AND hf_source_url IS NULL AND hf_observed_at IS NULL) OR hf_observed_at IS NOT NULL)
);
CREATE INDEX model_api_supplier_offers_operator_product_idx
  ON model_api_supplier_offers(operator_tenant_id,managed_product_id,state,version DESC);
CREATE INDEX model_api_supplier_offers_current_observation_idx
  ON model_api_supplier_offers(state,observed_at DESC,cost_valid_until);

-- Qualification evidence is exact-tuple, independently expiring evidence. A
-- Hugging Face observation above can aid discovery but never creates one of
-- these records or makes an offer callable.
CREATE TABLE model_api_supply_qualifications (
  id TEXT PRIMARY KEY,
  offer_id TEXT NOT NULL,
  offer_version BIGINT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('qualified','pending','rejected','expired')),
  tuple_key TEXT NOT NULL,
  protocol TEXT NOT NULL,
  region TEXT NOT NULL,
  capabilities_json JSONB NOT NULL CHECK(jsonb_typeof(capabilities_json)='array'),
  scope TEXT NOT NULL,
  evidence_ref TEXT,
  evidence_digest TEXT,
  reason TEXT,
  observed_at TIMESTAMPTZ,
  valid_until TIMESTAMPTZ,
  sample_count BIGINT NOT NULL DEFAULT 0 CHECK(sample_count>=0),
  ttft_p95_ms DOUBLE PRECISION CHECK(ttft_p95_ms>0),
  output_tokens_p5 DOUBLE PRECISION CHECK(output_tokens_p5>0),
  created_at TIMESTAMPTZ NOT NULL,
  FOREIGN KEY(offer_id,offer_version) REFERENCES model_api_supplier_offers(id,version) ON DELETE RESTRICT,
  UNIQUE(id,offer_id,offer_version),
  CHECK((observed_at IS NULL AND valid_until IS NULL) OR valid_until>observed_at),
  CHECK(state<>'qualified' OR (evidence_ref IS NOT NULL AND evidence_digest IS NOT NULL AND observed_at IS NOT NULL AND valid_until IS NOT NULL))
);
CREATE INDEX model_api_supply_qualifications_offer_idx
  ON model_api_supply_qualifications(offer_id,offer_version,state,valid_until DESC);

-- Plans persist both the deterministic compiled result and the exact private
-- inputs used to produce it. The request path consumes a separately published
-- in-memory snapshot; it does not query these tables.
CREATE TABLE model_api_supply_plans (
  id TEXT PRIMARY KEY,
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  managed_product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  protocol TEXT NOT NULL,
  schema_version TEXT NOT NULL,
  digest TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL CHECK(status IN ('ready','insufficient')),
  ranking_basis TEXT NOT NULL,
  request_json JSONB NOT NULL CHECK(jsonb_typeof(request_json)='object'),
  plan_json JSONB NOT NULL CHECK(jsonb_typeof(plan_json)='object'),
  generated_at TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(id,managed_product_id),
  UNIQUE(managed_product_id,operator_tenant_id,id),
  CHECK((status='ready' AND valid_until>generated_at) OR (status='insufficient' AND valid_until IS NULL))
);
CREATE INDEX model_api_supply_plans_operator_product_idx
  ON model_api_supply_plans(operator_tenant_id,managed_product_id,generated_at DESC);

CREATE TABLE model_api_supply_plan_candidates (
  plan_id TEXT NOT NULL REFERENCES model_api_supply_plans(id) ON DELETE CASCADE,
  managed_product_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  offer_id TEXT NOT NULL,
  offer_version BIGINT NOT NULL,
  qualification_id TEXT,
  retail_rate_card_id TEXT NOT NULL,
  retail_rate_version INTEGER NOT NULL CHECK(retail_rate_version>0),
  disposition TEXT NOT NULL CHECK(disposition IN ('primary','fallback','rejected')),
  position INTEGER,
  rejection_reasons_json JSONB NOT NULL DEFAULT '[]'::jsonb CHECK(jsonb_typeof(rejection_reasons_json)='array'),
  materialization_json JSONB NOT NULL CHECK(jsonb_typeof(materialization_json)='object'),
  PRIMARY KEY(plan_id,candidate_id),
  FOREIGN KEY(plan_id,managed_product_id) REFERENCES model_api_supply_plans(id,managed_product_id) ON DELETE CASCADE,
  FOREIGN KEY(offer_id,offer_version,managed_product_id) REFERENCES model_api_supplier_offers(id,version,managed_product_id) ON DELETE RESTRICT,
  FOREIGN KEY(qualification_id,offer_id,offer_version) REFERENCES model_api_supply_qualifications(id,offer_id,offer_version) ON DELETE RESTRICT,
  FOREIGN KEY(managed_product_id,retail_rate_card_id,retail_rate_version)
    REFERENCES model_api_retail_rate_cards(product_id,id,version) ON DELETE RESTRICT,
  CHECK((disposition='primary' AND position=0) OR (disposition='fallback' AND position>0) OR (disposition='rejected' AND position IS NULL)),
  CHECK((disposition='rejected' AND jsonb_array_length(rejection_reasons_json)>0) OR (disposition<>'rejected' AND jsonb_array_length(rejection_reasons_json)=0))
);
CREATE INDEX model_api_supply_plan_candidates_offer_idx
  ON model_api_supply_plan_candidates(offer_id,offer_version,plan_id);

ALTER TABLE model_api_operator_publications
  ADD CONSTRAINT model_api_operator_publications_supply_plan_fk
  FOREIGN KEY(product_id,operator_tenant_id,supply_plan_id)
  REFERENCES model_api_supply_plans(managed_product_id,operator_tenant_id,id)
  ON DELETE RESTRICT;
