CREATE TABLE environment_promotions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    source_endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    source_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
    destination_endpoint_id TEXT NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    destination_plan_id TEXT NOT NULL REFERENCES serving_plans(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id,idempotency_key),
    UNIQUE(destination_endpoint_id,source_plan_id)
);

CREATE INDEX environment_promotions_destination_idx
    ON environment_promotions(tenant_id,destination_endpoint_id,created_at DESC);
