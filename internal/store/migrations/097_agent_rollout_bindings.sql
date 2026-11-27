-- A server a rollout started on an agent's endpoint joins that endpoint as a
-- second agent-managed binding beside the customer's own server. An endpoint
-- has one adopted workload, so the rollout server's link to the endpoint,
-- its binding, and its target is kept here. Only the agent that serves the
-- endpoint can add one; the residency trigger still sees an agent-owned
-- target.
CREATE TABLE agent_rollout_servers (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
  binding_id TEXT NOT NULL REFERENCES backend_bindings(id) ON DELETE CASCADE,
  target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,target_id),
  UNIQUE(binding_id)
);
CREATE INDEX agent_rollout_servers_agent_idx ON agent_rollout_servers(tenant_id,agent_id);
