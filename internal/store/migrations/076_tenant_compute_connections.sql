CREATE TABLE compute_connections (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    provider TEXT NOT NULL,
    adapter TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('verified','revoked')),
    credential_ciphertext BYTEA NOT NULL,
    credential_nonce BYTEA NOT NULL,
    credential_key_reference TEXT NOT NULL,
    verified_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,name),
    UNIQUE(tenant_id,id)
);

CREATE INDEX compute_connections_tenant_provider_idx
    ON compute_connections(tenant_id,provider,status);

CREATE UNIQUE INDEX deployments_tenant_identity_idx
    ON deployments(tenant_id,id);

CREATE TABLE deployment_compute_connections (
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT PRIMARY KEY,
    compute_connection_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    FOREIGN KEY (tenant_id,deployment_id)
        REFERENCES deployments(tenant_id,id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id,compute_connection_id)
        REFERENCES compute_connections(tenant_id,id) ON DELETE RESTRICT
);

CREATE INDEX deployment_compute_connections_connection_idx
    ON deployment_compute_connections(tenant_id,compute_connection_id);
