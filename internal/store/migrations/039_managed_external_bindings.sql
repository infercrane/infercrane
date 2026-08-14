CREATE UNIQUE INDEX backend_bindings_id_tenant_unique_idx
  ON backend_bindings(id,tenant_id);

CREATE TABLE managed_external_binding_budgets (
  binding_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  requests_reserved BIGINT NOT NULL DEFAULT 0 CHECK(requests_reserved >= 0),
  cost_reserved_microusd BIGINT NOT NULL DEFAULT 0 CHECK(cost_reserved_microusd >= 0),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  FOREIGN KEY(binding_id,tenant_id) REFERENCES backend_bindings(id,tenant_id) ON DELETE CASCADE
);

CREATE INDEX managed_external_binding_budgets_tenant_idx
  ON managed_external_binding_budgets(tenant_id,binding_id);
