CREATE TABLE external_target_policies (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE RESTRICT,
  adapter TEXT NOT NULL CHECK(adapter IN ('openai-compatible-external','openrouter')),
  secret_reference_id TEXT NOT NULL REFERENCES secret_references(id) ON DELETE RESTRICT,
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  privacy_acknowledged BOOLEAN NOT NULL DEFAULT FALSE,
  request_limit BIGINT NOT NULL CHECK(request_limit > 0),
  requests_reserved BIGINT NOT NULL DEFAULT 0 CHECK(requests_reserved >= 0),
  cost_limit_microusd BIGINT NOT NULL CHECK(cost_limit_microusd > 0),
  max_request_cost_microusd BIGINT NOT NULL CHECK(max_request_cost_microusd > 0),
  cost_reserved_microusd BIGINT NOT NULL DEFAULT 0 CHECK(cost_reserved_microusd >= 0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,deployment_id),
  CHECK(NOT enabled OR privacy_acknowledged),
  CHECK(max_request_cost_microusd <= cost_limit_microusd),
  CHECK(requests_reserved <= request_limit),
  CHECK(cost_reserved_microusd <= cost_limit_microusd)
);

CREATE INDEX idx_external_policies_target ON external_target_policies(tenant_id,target_id);
