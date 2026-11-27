-- An optimization run on an endpoint an InferCrane agent serves: the agent
-- measures each setup in the customer's own cluster and reports numbers; the
-- control plane judges them with the same code it uses for its own runs and
-- records the evidence in the same tables.
--
-- optimization_campaigns.target_endpoint_name, agent_id
--   An evolve_endpoint run names either the deployment it evolves or, for an
--   agent's endpoint, the endpoint and the agent that measures it.
ALTER TABLE optimization_campaigns
  ADD COLUMN target_endpoint_name TEXT CHECK (target_endpoint_name IS NULL OR char_length(target_endpoint_name) BETWEEN 1 AND 255),
  ADD COLUMN agent_id TEXT REFERENCES agents(id) ON DELETE RESTRICT,
  DROP CONSTRAINT optimization_campaign_target_boundary,
  ADD CONSTRAINT optimization_campaign_target_boundary CHECK(
    (intent='new_endpoint' AND target_deployment_name IS NULL AND target_endpoint_name IS NULL AND agent_id IS NULL)
    OR
    (intent='evolve_endpoint' AND target_deployment_name IS NOT NULL AND target_endpoint_name IS NULL AND agent_id IS NULL)
    OR
    (intent='evolve_endpoint' AND target_deployment_name IS NULL AND target_endpoint_name IS NOT NULL AND agent_id IS NOT NULL)
  );

-- agent_commands.kind measure_window: one setup's measured window.
-- agent_commands.idempotency_key: a step that queues work for an agent and
-- is retried after a lost response finds the command it queued.
ALTER TABLE agent_commands
  DROP CONSTRAINT agent_commands_kind_check,
  ADD CONSTRAINT agent_commands_kind_check CHECK(kind IN ('run_candidate','measure_window')),
  ADD COLUMN idempotency_key TEXT CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 1 AND 128);
CREATE UNIQUE INDEX agent_commands_idempotency_idx ON agent_commands(tenant_id,idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Evidence of a setup an agent measured has no InferCrane deployment or
-- revision. Its identity is the setup itself: agentsetup-<digest> for a
-- setup the agent started, agentserver-<digest> for the customer's own
-- server. A row has exactly one scope.
ALTER TABLE benchmark_results
  ALTER COLUMN deployment_id DROP NOT NULL,
  ALTER COLUMN revision_id DROP NOT NULL,
  ADD COLUMN agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
  ADD COLUMN agent_setup_id TEXT CHECK (agent_setup_id IS NULL OR agent_setup_id ~ '^agent(setup|server)-[a-f0-9]{24}$'),
  ADD CONSTRAINT benchmark_results_scope_check CHECK(
    (deployment_id IS NOT NULL AND revision_id IS NOT NULL AND agent_id IS NULL AND agent_setup_id IS NULL)
    OR
    (deployment_id IS NULL AND revision_id IS NULL AND agent_id IS NOT NULL AND agent_setup_id IS NOT NULL)
  );
CREATE INDEX benchmark_results_agent_setup_idx ON benchmark_results(tenant_id,agent_setup_id,created_at DESC) WHERE agent_setup_id IS NOT NULL;

ALTER TABLE revision_quality_evidence
  ALTER COLUMN deployment_id DROP NOT NULL,
  ALTER COLUMN revision_id DROP NOT NULL,
  ADD COLUMN agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
  ADD COLUMN agent_setup_id TEXT CHECK (agent_setup_id IS NULL OR agent_setup_id ~ '^agent(setup|server)-[a-f0-9]{24}$'),
  ADD CONSTRAINT revision_quality_evidence_scope_check CHECK(
    (deployment_id IS NOT NULL AND revision_id IS NOT NULL AND agent_id IS NULL AND agent_setup_id IS NULL)
    OR
    (deployment_id IS NULL AND revision_id IS NULL AND agent_id IS NOT NULL AND agent_setup_id IS NOT NULL)
  );
CREATE INDEX revision_quality_evidence_agent_setup_idx ON revision_quality_evidence(tenant_id,agent_setup_id,evaluated_at DESC) WHERE agent_setup_id IS NOT NULL;
