CREATE TABLE provider_connections (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  adapter TEXT NOT NULL CHECK(adapter IN ('openrouter','openai-compatible-external')),
  target_id TEXT NOT NULL REFERENCES targets(id) ON DELETE RESTRICT,
  secret_reference_id TEXT NOT NULL REFERENCES secret_references(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,name),
  UNIQUE(tenant_id,target_id,secret_reference_id)
);

CREATE INDEX provider_connections_tenant_idx
  ON provider_connections(tenant_id,name);
