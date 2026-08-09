CREATE TABLE tenants (
 id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TIMESTAMPTZ NOT NULL
);
INSERT INTO tenants(id,name,created_at) VALUES('global','global',NOW()) ON CONFLICT DO NOTHING;

ALTER TABLE deployments ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'global' REFERENCES tenants(id);
ALTER TABLE targets ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'global' REFERENCES tenants(id);
CREATE INDEX idx_deployments_tenant_name ON deployments(tenant_id,name);
CREATE INDEX idx_targets_tenant_name ON targets(tenant_id,name);

CREATE TABLE principals (
 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 name TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('viewer','operator','admin')),
 credential_hash TEXT NOT NULL UNIQUE, disabled BOOLEAN NOT NULL DEFAULT FALSE,
 created_at TIMESTAMPTZ NOT NULL, UNIQUE(tenant_id,name)
);
CREATE TABLE tenant_quotas (
 tenant_id TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
 max_deployments INTEGER NOT NULL CHECK(max_deployments >= 0),
 max_replicas INTEGER NOT NULL CHECK(max_replicas >= 0),
 max_requests_per_minute INTEGER NOT NULL CHECK(max_requests_per_minute >= 0),
 updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE operations (
 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id), kind TEXT NOT NULL,
 resource_type TEXT NOT NULL, resource_name TEXT NOT NULL, idempotency_key TEXT,
 status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','cancelled')),
 progress INTEGER NOT NULL DEFAULT 0 CHECK(progress BETWEEN 0 AND 100),
 message TEXT NOT NULL DEFAULT '', request_json JSONB NOT NULL DEFAULT '{}',
 result_json JSONB NOT NULL DEFAULT '{}', error_code TEXT, retryable BOOLEAN NOT NULL DEFAULT FALSE,
 cancel_requested BOOLEAN NOT NULL DEFAULT FALSE, attempt INTEGER NOT NULL DEFAULT 1,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, completed_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX operations_idempotency ON operations(tenant_id,kind,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_operations_resource ON operations(tenant_id,resource_type,resource_name,created_at DESC);

CREATE TABLE audit_events (
 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id), actor TEXT NOT NULL,
 action TEXT NOT NULL, resource_type TEXT NOT NULL, resource_name TEXT NOT NULL,
 outcome TEXT NOT NULL, request_id TEXT, payload_json JSONB NOT NULL DEFAULT '{}',
 created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_audit_tenant_created ON audit_events(tenant_id,created_at DESC);
