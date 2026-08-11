CREATE TABLE secret_references (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  resolver TEXT NOT NULL CHECK(resolver IN ('env')),
  reference TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  UNIQUE(tenant_id,name)
);

CREATE INDEX idx_secret_references_tenant ON secret_references(tenant_id,name);
