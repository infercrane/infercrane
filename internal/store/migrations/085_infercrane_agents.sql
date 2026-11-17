-- InferCrane agents: outbound-only processes beside a customer's model
-- server. An agent's key is a principal of kind agent; the control API lets
-- that kind call only the agent routes. Revoking an agent disables its
-- principal, so the credential snapshot drops it within one refresh.
ALTER TABLE principals
  DROP CONSTRAINT principals_kind_check,
  ADD CONSTRAINT principals_kind_check
    CHECK(kind IN ('service_account','inference_token','cli_session','agent'));

CREATE TABLE agents (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  principal_id TEXT NOT NULL UNIQUE REFERENCES principals(id) ON DELETE CASCADE,
  name TEXT NOT NULL CHECK(name ~ '^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$'),
  state TEXT NOT NULL CHECK(state IN ('active','revoked')),
  created_by TEXT NOT NULL CHECK(char_length(created_by) BETWEEN 1 AND 255),
  agent_version TEXT CHECK(agent_version IS NULL OR char_length(agent_version) BETWEEN 1 AND 256),
  protocol_version TEXT CHECK(protocol_version IS NULL OR char_length(protocol_version) BETWEEN 1 AND 64),
  os TEXT CHECK(os IS NULL OR char_length(os) BETWEEN 1 AND 256),
  arch TEXT CHECK(arch IS NULL OR char_length(arch) BETWEEN 1 AND 256),
  -- Content-free hardware inventory and the local servers the agent last
  -- reported. Bounded by the agent protocol validator.
  hardware_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  servers_json JSONB NOT NULL DEFAULT '[]'::jsonb,
  gateway_server_key TEXT,
  last_seen_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  CHECK((state = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE UNIQUE INDEX agents_tenant_active_name_idx ON agents(tenant_id,name) WHERE state = 'active';
CREATE INDEX agents_tenant_created_idx ON agents(tenant_id,created_at DESC);

-- A target reached only through an agent. The control plane never dials its
-- URL: the address is local to the agent's machine, and health comes from
-- the agent's reports.
ALTER TABLE targets
  ADD COLUMN agent_id TEXT REFERENCES agents(id) ON DELETE RESTRICT,
  ADD COLUMN agent_server_key TEXT CHECK(agent_server_key IS NULL OR agent_server_key ~ '^[a-z0-9][a-z0-9._:-]{0,127}$'),
  ADD COLUMN agent_reported_at TIMESTAMPTZ,
  ADD CONSTRAINT targets_agent_server_pair_check CHECK((agent_id IS NULL) = (agent_server_key IS NULL));

CREATE UNIQUE INDEX targets_agent_server_idx ON targets(agent_id,agent_server_key) WHERE agent_id IS NOT NULL;

-- Engine measurements for a target that no InferCrane deployment owns
-- (an adopted server, observed directly or through an agent). Same shape and
-- validation as operational_measurements, keyed by target instead of a
-- deployment revision.
CREATE TABLE target_measurements (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
  replica_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL,
  value DOUBLE PRECISION NOT NULL,
  unit TEXT NOT NULL,
  evidence_class TEXT NOT NULL CHECK(evidence_class IN ('measured','provider_reported')),
  source TEXT NOT NULL,
  sample_count INTEGER NOT NULL CHECK(sample_count > 0),
  observed_at TIMESTAMPTZ NOT NULL,
  valid_until TIMESTAMPTZ NOT NULL CHECK(valid_until > observed_at),
  created_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,target_id,replica_id,name,source,observed_at)
);

CREATE INDEX target_measurements_window_idx
  ON target_measurements(tenant_id,target_id,name,observed_at DESC);

CREATE INDEX target_measurements_observed_idx
  ON target_measurements(observed_at);

-- An optional bearer credential for a server connected without an agent.
-- Only ciphertext is stored, sealed with the tenant credential key and bound
-- to the adoption row.
ALTER TABLE adopted_workloads
  ADD COLUMN upstream_credential_ciphertext BYTEA,
  ADD COLUMN upstream_credential_nonce BYTEA,
  ADD CONSTRAINT adopted_workloads_upstream_credential_pair_check
    CHECK((upstream_credential_ciphertext IS NULL) = (upstream_credential_nonce IS NULL));
