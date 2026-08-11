CREATE TABLE model_recipes (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    digest TEXT NOT NULL CHECK (digest ~ '^[a-f0-9]{64}$'),
    payload_json JSONB NOT NULL,
    provenance_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id, name, version),
    UNIQUE (tenant_id, digest)
);

CREATE INDEX model_recipes_search_idx ON model_recipes(tenant_id, name, created_at DESC);

CREATE TABLE lab_evaluations (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_identity TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    input_json JSONB NOT NULL,
    results_json JSONB NOT NULL,
    input_digest TEXT NOT NULL CHECK (input_digest ~ '^[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX lab_evaluations_model_idx ON lab_evaluations(tenant_id, model_identity, created_at DESC);
