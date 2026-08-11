CREATE TABLE context_passports (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id TEXT REFERENCES endpoints(id) ON DELETE CASCADE,
  deployment_id TEXT REFERENCES deployments(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK(status IN ('active','expired')),
  preferred_binding_id TEXT, preferred_target_id TEXT,
  cache_hints_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  last_activity TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
  created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
  CHECK(num_nonnulls(endpoint_id,deployment_id)=1), CHECK(expires_at>created_at)
);
CREATE INDEX context_passports_tenant_expiry ON context_passports(tenant_id,expires_at);

CREATE TABLE burst_guard_policies (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL UNIQUE REFERENCES deployments(id) ON DELETE CASCADE,
  external_policy_id TEXT NOT NULL REFERENCES external_target_policies(id) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL, queue_threshold INTEGER NOT NULL CHECK(queue_threshold>=0),
  breach_intervals INTEGER NOT NULL CHECK(breach_intervals BETWEEN 1 AND 100),
  recovery_intervals INTEGER NOT NULL CHECK(recovery_intervals BETWEEN 1 AND 100),
  cooldown_seconds INTEGER NOT NULL CHECK(cooldown_seconds BETWEEN 1 AND 86400),
  signal_max_age_seconds INTEGER NOT NULL CHECK(signal_max_age_seconds BETWEEN 1 AND 300),
  max_incremental_cost_microusd_hour BIGINT NOT NULL CHECK(max_incremental_cost_microusd_hour>0),
  created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE burst_guard_decisions (
  id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  policy_id TEXT NOT NULL REFERENCES burst_guard_policies(id) ON DELETE CASCADE,
  decision TEXT NOT NULL CHECK(decision IN ('hold','overflow','recover','unknown')),
  reason TEXT NOT NULL, incremental_cost_microusd_hour BIGINT NOT NULL CHECK(incremental_cost_microusd_hour>=0),
  evidence_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL
);
