CREATE TABLE replicas (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
 ordinal INTEGER NOT NULL CHECK(ordinal >= 0), external_key TEXT NOT NULL,
 lifecycle_state TEXT NOT NULL CHECK(lifecycle_state IN ('pending','provisioning','starting','ready','active','draining','deleting','deleted','failed')),
 provider TEXT NOT NULL, provider_request_id TEXT, provider_resource_id TEXT, endpoint TEXT,
 health TEXT NOT NULL DEFAULT 'unknown', provider_details_json JSONB NOT NULL DEFAULT '{}',
 last_observed_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
 UNIQUE(deployment_id,ordinal), UNIQUE(provider,external_key)
);
CREATE INDEX idx_replicas_deployment_state ON replicas(deployment_id,lifecycle_state,ordinal);
CREATE INDEX idx_replicas_provider_resource ON replicas(provider,provider_resource_id) WHERE provider_resource_id IS NOT NULL;
