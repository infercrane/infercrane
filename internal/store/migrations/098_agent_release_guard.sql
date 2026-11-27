-- Release Guard's decision on a setup an InferCrane agent measured, before
-- any rollout: the setup's runs against the endpoint's own server's runs in
-- the same measured windows. An agent's setup has no deployment revision and
-- no serving plan yet, so the decision cannot be a release_guard_evaluations
-- or an endpoint_release_guard_evaluations row. A candidate that passed names
-- this row as its release_guard_evaluation_id.
CREATE TABLE agent_release_guard_evaluations (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  campaign_id TEXT NOT NULL REFERENCES optimization_campaigns(id) ON DELETE CASCADE,
  candidate_id TEXT NOT NULL REFERENCES optimization_candidate_runs(id) ON DELETE CASCADE,
  active_setup_id TEXT NOT NULL CHECK(active_setup_id ~ '^agent(setup|server)-[a-f0-9]{24}$'),
  candidate_setup_id TEXT NOT NULL CHECK(candidate_setup_id ~ '^agent(setup|server)-[a-f0-9]{24}$'),
  benchmark_id TEXT NOT NULL REFERENCES benchmark_results(id) ON DELETE CASCADE,
  decision TEXT NOT NULL CHECK(decision IN ('PASS','REJECT','INCONCLUSIVE')),
  reason_codes_json JSONB NOT NULL,
  metrics_json JSONB NOT NULL,
  policy_json JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id, candidate_id, benchmark_id)
);
