CREATE TABLE model_api_products (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  publisher TEXT NOT NULL,
  description TEXT NOT NULL,
  protocol TEXT NOT NULL CHECK(protocol='openai'),
  tasks_json JSONB NOT NULL CHECK(jsonb_typeof(tasks_json)='array' AND jsonb_array_length(tasks_json)>0),
  capability_contract_json JSONB NOT NULL CHECK(jsonb_typeof(capability_contract_json)='array' AND jsonb_array_length(capability_contract_json)>0),
  input_modalities_json JSONB NOT NULL CHECK(jsonb_typeof(input_modalities_json)='array' AND jsonb_array_length(input_modalities_json)>0),
  output_modalities_json JSONB NOT NULL CHECK(jsonb_typeof(output_modalities_json)='array' AND jsonb_array_length(output_modalities_json)>0),
  context_window_tokens BIGINT CHECK(context_window_tokens IS NULL OR context_window_tokens>0),
  availability TEXT NOT NULL DEFAULT 'catalog_only'
    CHECK(availability IN ('catalog_only','private_preview','available','degraded','unavailable')),
  self_host_eligibility TEXT NOT NULL DEFAULT 'unknown'
    CHECK(self_host_eligibility IN ('unknown','eligible','ineligible')),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CHECK(updated_at>=created_at)
);

-- Retail prices are supplier-neutral customer contracts. Supplier cost and
-- private offer identity live outside this catalog schema.
CREATE TABLE model_api_retail_rate_cards (
  id TEXT PRIMARY KEY,
  product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  version INTEGER NOT NULL CHECK(version>0),
  currency TEXT NOT NULL DEFAULT 'USD' CHECK(currency='USD'),
  input_microusd_per_million BIGINT NOT NULL CHECK(input_microusd_per_million>0),
  cached_input_microusd_per_million BIGINT CHECK(cached_input_microusd_per_million IS NULL OR cached_input_microusd_per_million>=0),
  output_microusd_per_million BIGINT NOT NULL CHECK(output_microusd_per_million>0),
  valid_from TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ NOT NULL,
  published_at TIMESTAMPTZ NOT NULL,
  public_provenance TEXT NOT NULL,
  contract_digest TEXT NOT NULL CHECK(contract_digest LIKE 'sha256:%'),
  UNIQUE(product_id,version),
  UNIQUE(product_id,contract_digest),
  UNIQUE(product_id,id),
  UNIQUE(product_id,id,version),
  CHECK(valid_until>valid_from),
  CHECK(published_at<=valid_from)
);
CREATE INDEX model_api_retail_rate_cards_product_validity_idx
  ON model_api_retail_rate_cards(product_id,valid_from DESC,valid_until DESC);

CREATE FUNCTION infercrane_reject_model_api_rate_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'model API retail rate cards are immutable; publish a new version';
END;
$$;
CREATE TRIGGER model_api_retail_rate_cards_immutable
  BEFORE UPDATE OR DELETE ON model_api_retail_rate_cards
  FOR EACH ROW EXECUTE FUNCTION infercrane_reject_model_api_rate_mutation();

-- This is the operator-private half of a hosted product. The public projection
-- must not expose these workspace or plan references.
CREATE TABLE model_api_operator_publications (
  product_id TEXT PRIMARY KEY REFERENCES model_api_products(id) ON DELETE RESTRICT,
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
  supply_plan_id TEXT NOT NULL,
  qualification_state TEXT NOT NULL DEFAULT 'pending'
    CHECK(qualification_state IN ('pending','qualified','stale')),
  qualification_evidence_id TEXT NOT NULL DEFAULT '',
  qualification_valid_until TIMESTAMPTZ,
  active_retail_rate_card_id TEXT REFERENCES model_api_retail_rate_cards(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(product_id,operator_tenant_id,serving_plan_id),
  FOREIGN KEY(product_id,active_retail_rate_card_id)
    REFERENCES model_api_retail_rate_cards(product_id,id) ON DELETE RESTRICT,
  CHECK(updated_at>=created_at),
  CHECK(
    (qualification_state='qualified' AND qualification_evidence_id<>'' AND qualification_valid_until IS NOT NULL)
    OR
    (qualification_state IN ('pending','stale') AND qualification_evidence_id='' AND qualification_valid_until IS NULL)
  )
);

-- Customer policy and money stay customer-owned while the authorized route is
-- an operator-owned shared plan. This row contains no target, supplier offer,
-- or credential material.
CREATE TABLE model_api_product_entitlements (
  id TEXT PRIMARY KEY,
  customer_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  product_id TEXT NOT NULL REFERENCES model_api_products(id) ON DELETE RESTRICT,
  operator_tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  serving_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
  retail_rate_card_id TEXT NOT NULL REFERENCES model_api_retail_rate_cards(id) ON DELETE RESTRICT,
  retail_rate_version INTEGER NOT NULL CHECK(retail_rate_version>0),
  state TEXT NOT NULL DEFAULT 'pending'
    CHECK(state IN ('pending','active','suspended','revoked')),
  requests_per_minute BIGINT CHECK(requests_per_minute IS NULL OR requests_per_minute>0),
  tokens_per_minute BIGINT CHECK(tokens_per_minute IS NULL OR tokens_per_minute>0),
  monthly_spend_microusd BIGINT CHECK(monthly_spend_microusd IS NULL OR monthly_spend_microusd>0),
  max_request_microusd BIGINT CHECK(max_request_microusd IS NULL OR max_request_microusd>0),
  valid_from TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(customer_tenant_id,product_id),
  FOREIGN KEY(product_id,operator_tenant_id,serving_plan_id)
    REFERENCES model_api_operator_publications(product_id,operator_tenant_id,serving_plan_id) ON DELETE RESTRICT,
  FOREIGN KEY(product_id,retail_rate_card_id,retail_rate_version)
    REFERENCES model_api_retail_rate_cards(product_id,id,version) ON DELETE RESTRICT,
  CHECK(customer_tenant_id<>operator_tenant_id),
  CHECK(valid_until IS NULL OR valid_until>valid_from),
  CHECK(updated_at>=created_at),
  CHECK(max_request_microusd IS NULL OR monthly_spend_microusd IS NULL OR max_request_microusd<=monthly_spend_microusd)
);
CREATE INDEX model_api_product_entitlements_customer_state_idx
  ON model_api_product_entitlements(customer_tenant_id,state,product_id);

INSERT INTO model_api_products(
  id,display_name,publisher,description,protocol,tasks_json,
  capability_contract_json,input_modalities_json,output_modalities_json,
  availability,self_host_eligibility,created_at,updated_at
) VALUES
  ('glm-5.2','GLM-5.2','Z.ai','Planned for coding, reasoning, and bilingual chat workloads.','openai',
    '["coding","reasoning","chat"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW()),
  ('glm-5.3','GLM-5.3','Z.ai','Planned for reasoning and long-context workloads.','openai',
    '["reasoning","long-context","chat"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW()),
  ('glm-5.3-flash','GLM-5.3-Flash','Z.ai','Planned for cost-sensitive and latency-sensitive workloads.','openai',
    '["chat","coding"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW()),
  ('kimi-k3','Kimi-K3','Moonshot AI','Planned for coding and agentic workflows.','openai',
    '["coding","agents","chat"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW()),
  ('kimi-k2.6','Kimi-K2.6','Moonshot AI','Planned for coding, agentic, and long-context workloads.','openai',
    '["coding","agents","long-context","chat"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW()),
  ('deepseek-v4-flash-0731-fast','DeepSeek-V4-Flash-0731-Fast','DeepSeek','Planned for high-throughput workloads after route qualification.','openai',
    '["chat","coding","throughput"]','[{"name":"chat-completions","state":"cataloged"},{"name":"streaming","state":"cataloged"}]','["text"]','["text"]','catalog_only','unknown',NOW(),NOW());
