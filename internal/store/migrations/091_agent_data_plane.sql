-- The InferCrane agent as the data plane inside a customer's cloud.
--
-- agent-managed: an agent-owned target whose traffic the agent serves with
-- the routes, hashed credential snapshot, and admission and quota leases it
-- pulls from the control plane. The control plane still never dials,
-- probes, or routes to such a target.
ALTER TABLE backend_bindings
  DROP CONSTRAINT backend_bindings_ownership_mode_check,
  ADD CONSTRAINT backend_bindings_ownership_mode_check
    CHECK(ownership_mode IN ('observe-only','traffic-managed','lifecycle-managed','agent-managed'));

ALTER TABLE adopted_workloads
  DROP CONSTRAINT adopted_workloads_ownership_mode_check,
  ADD CONSTRAINT adopted_workloads_ownership_mode_check
    CHECK(ownership_mode IN ('observe-only','traffic-managed','agent-managed'));

-- Residency: customer-cloud means every binding of the endpoint is a server
-- an InferCrane agent owns, so prompts and completions stay where the agent
-- runs. Hosted deployments, external fallbacks, and Model API bindings are
-- refused below, whichever code path tries to add them.
ALTER TABLE endpoints
  ADD COLUMN residency TEXT NOT NULL DEFAULT 'any' CHECK(residency IN ('any','customer-cloud'));

CREATE OR REPLACE FUNCTION infercrane_enforce_endpoint_residency() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  endpoint_residency TEXT;
  target_agent TEXT;
BEGIN
  SELECT residency INTO endpoint_residency FROM endpoints WHERE id = NEW.endpoint_id FOR SHARE;
  IF endpoint_residency IS DISTINCT FROM 'customer-cloud' THEN
    RETURN NEW;
  END IF;
  IF NEW.kind <> 'external' OR NEW.target_id IS NULL THEN
    RAISE EXCEPTION 'endpoint residency customer-cloud allows only servers an InferCrane agent owns'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'endpoint_residency_customer_cloud';
  END IF;
  SELECT agent_id INTO target_agent FROM targets WHERE id = NEW.target_id;
  IF target_agent IS NULL THEN
    RAISE EXCEPTION 'endpoint residency customer-cloud allows only servers an InferCrane agent owns'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'endpoint_residency_customer_cloud';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER backend_bindings_endpoint_residency
  BEFORE INSERT OR UPDATE OF kind, target_id, deployment_id, endpoint_id ON backend_bindings
  FOR EACH ROW EXECUTE FUNCTION infercrane_enforce_endpoint_residency();

-- Work the control plane queues for one agent. The agent fetches it with an
-- outbound long poll and reports content-free results; nothing dials the
-- agent. A command carries a bounded spec, never a shell command, and the
-- agent checks it against its own local allowlists before running it.
CREATE TABLE agent_commands (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('run_candidate')),
  state TEXT NOT NULL CHECK(state IN ('queued','running','succeeded','failed','cancelled','expired')),
  phase TEXT CHECK(phase IS NULL OR phase ~ '^[a-z][a-z0-9_]{0,63}$'),
  spec_json JSONB NOT NULL,
  result_json JSONB,
  failure_code TEXT CHECK(failure_code IS NULL OR failure_code ~ '^[a-z][a-z0-9_]{0,63}$'),
  failure_detail TEXT CHECK(failure_detail IS NULL OR char_length(failure_detail) <= 1024),
  attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 10),
  lease_expires_at TIMESTAMPTZ,
  deadline_at TIMESTAMPTZ NOT NULL,
  created_by TEXT NOT NULL CHECK(char_length(created_by) BETWEEN 1 AND 255),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  started_at TIMESTAMPTZ,
  finished_at TIMESTAMPTZ,
  CHECK((state IN ('succeeded','failed','cancelled','expired')) = (finished_at IS NOT NULL)),
  CHECK(state <> 'running' OR lease_expires_at IS NOT NULL)
);

CREATE INDEX agent_commands_agent_queue_idx ON agent_commands(agent_id,state,created_at);
CREATE INDEX agent_commands_tenant_created_idx ON agent_commands(tenant_id,created_at DESC);
