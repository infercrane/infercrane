-- Migration Proof: a closed API behind an endpoint becomes a measured,
-- priced baseline.
--
-- request_records.cached_input_tokens
--   The part of input_tokens the upstream reported as served from its prompt
--   cache. NULL when the response did not say. Priced at the cached rate.
ALTER TABLE request_records ADD COLUMN cached_input_tokens INTEGER
  CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0);

-- Release Guard and replay capture read one binding's requests.
CREATE INDEX request_records_binding_started_idx ON request_records(binding_id,started_at DESC)
  WHERE binding_id IS NOT NULL;

-- A replay trace is captured from a deployment's requests, or from an
-- endpoint's requests (optionally one binding of it), such as a closed API
-- adopted as an external binding that has no deployment.
ALTER TABLE replay_traces ALTER COLUMN deployment_id DROP NOT NULL;
ALTER TABLE replay_traces ALTER COLUMN deployment_name DROP NOT NULL;
ALTER TABLE replay_traces ADD COLUMN endpoint_id TEXT REFERENCES endpoints(id) ON DELETE CASCADE;
ALTER TABLE replay_traces ADD COLUMN endpoint_name TEXT;
ALTER TABLE replay_traces ADD COLUMN binding_id TEXT REFERENCES backend_bindings(id) ON DELETE CASCADE;
ALTER TABLE replay_traces ADD CONSTRAINT replay_traces_scope_check CHECK (
  (deployment_id IS NOT NULL AND deployment_name IS NOT NULL AND endpoint_id IS NULL AND binding_id IS NULL)
  OR (deployment_id IS NULL AND endpoint_id IS NOT NULL AND endpoint_name IS NOT NULL)
);
CREATE INDEX replay_traces_endpoint_idx ON replay_traces(tenant_id,endpoint_id,created_at DESC)
  WHERE endpoint_id IS NOT NULL;

-- What a closed API binding charges per token. Rows are immutable; the newest
-- row for a binding is its rate card. authority says where the price comes
-- from: the supplier's published list price, or the customer's own contract.
-- A rate card past valid_until prices nothing: the cost becomes unknown.
CREATE TABLE binding_rate_cards (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE CASCADE,
  authority TEXT NOT NULL CHECK (authority IN ('list_price','contract')),
  currency TEXT NOT NULL CHECK (currency = 'USD'),
  input_microusd_per_million BIGINT NOT NULL CHECK (input_microusd_per_million >= 0),
  cached_input_microusd_per_million BIGINT CHECK (cached_input_microusd_per_million IS NULL OR cached_input_microusd_per_million >= 0),
  output_microusd_per_million BIGINT NOT NULL CHECK (output_microusd_per_million >= 0),
  source TEXT NOT NULL DEFAULT '',
  valid_until TIMESTAMPTZ NOT NULL,
  recorded_by TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL,
  CHECK (valid_until > created_at)
);
CREATE INDEX binding_rate_cards_binding_idx ON binding_rate_cards(tenant_id,binding_id,created_at DESC);

-- Signed quality evidence for a closed API behind an external binding: its
-- answers scored on a graded suite, as the baseline a candidate is compared
-- with. source is 'infercrane' for the graded check InferCrane ran and
-- 'customer' for a customer evaluator's signed result. payload_json is the
-- signed payload, kept as text (JSONB would reorder its keys and break the
-- signature) so a verdict can be recomputed and re-verified.
CREATE TABLE binding_quality_evidence (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE CASCADE,
  source TEXT NOT NULL CHECK (source IN ('customer','infercrane')),
  suite TEXT NOT NULL,
  suite_version TEXT NOT NULL,
  evaluator TEXT NOT NULL,
  evaluator_version TEXT NOT NULL,
  score DOUBLE PRECISION NOT NULL CHECK (score >= 0 AND score <= 1),
  sample_count INTEGER NOT NULL CHECK (sample_count > 0),
  pairing_digest TEXT NOT NULL DEFAULT '',
  payload_json TEXT NOT NULL CHECK (length(payload_json) <= 1048576),
  payload_digest TEXT NOT NULL,
  signature TEXT NOT NULL,
  public_key TEXT NOT NULL,
  algorithm TEXT NOT NULL,
  key_id TEXT NOT NULL,
  evaluated_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,payload_digest)
);
CREATE INDEX binding_quality_evidence_binding_idx ON binding_quality_evidence(tenant_id,binding_id,evaluated_at DESC);

-- A signed verdict that a candidate revision answers a graded suite within
-- margin of the closed API it would replace. A migration serving plan cites
-- a PASS verdict in place of a same-model check.
CREATE TABLE migration_quality_verdicts (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  campaign_id TEXT NOT NULL REFERENCES optimization_campaigns(id) ON DELETE CASCADE,
  candidate_run_id TEXT NOT NULL,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  revision_id TEXT NOT NULL,
  verdict TEXT NOT NULL CHECK (verdict IN ('PASS','WATCH','BLOCK','UNKNOWN')),
  code TEXT NOT NULL,
  margin_percent DOUBLE PRECISION NOT NULL CHECK (margin_percent > 0 AND margin_percent <= 50),
  payload_json TEXT NOT NULL CHECK (length(payload_json) <= 1048576),
  payload_digest TEXT NOT NULL,
  signature TEXT NOT NULL,
  public_key TEXT NOT NULL,
  algorithm TEXT NOT NULL,
  key_id TEXT NOT NULL,
  evaluated_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,payload_digest)
);
CREATE INDEX migration_quality_verdicts_campaign_idx ON migration_quality_verdicts(tenant_id,campaign_id,created_at DESC);
CREATE INDEX migration_quality_verdicts_binding_idx ON migration_quality_verdicts(tenant_id,binding_id,revision_id,created_at DESC);
