ALTER TABLE request_records ADD COLUMN session_id_hash TEXT;
ALTER TABLE request_records ADD COLUMN parent_session_id_hash TEXT;
ALTER TABLE request_records ADD COLUMN shared_prefix_hash TEXT;
ALTER TABLE request_records ADD COLUMN tool_pause_ms DOUBLE PRECISION CHECK (tool_pause_ms IS NULL OR tool_pause_ms >= 0);

CREATE TABLE replay_traces (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    deployment_name TEXT NOT NULL,
    revision_id TEXT,
    schema_version TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    request_count INTEGER NOT NULL CHECK (request_count >= 0),
    shape_json JSONB NOT NULL,
    summary_json JSONB NOT NULL,
    shape_digest TEXT NOT NULL CHECK (shape_digest ~ '^[a-f0-9]{64}$'),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX replay_traces_deployment_idx ON replay_traces(tenant_id,deployment_id,created_at DESC);

CREATE TABLE artifact_cache_observations (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_artifact_id TEXT NOT NULL REFERENCES model_artifacts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    location TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('present','prefetching','missing','unknown')),
    source TEXT NOT NULL,
    evidence_json JSONB NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX artifact_cache_latest_idx ON artifact_cache_observations(tenant_id,model_artifact_id,provider,region,observed_at DESC);

CREATE TABLE artifact_prefetches (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    model_artifact_id TEXT NOT NULL REFERENCES model_artifacts(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    location TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('requested','running','succeeded','failed','cancelled')),
    idempotency_key TEXT NOT NULL,
    provider_operation_id TEXT,
    error_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (tenant_id,idempotency_key)
);

CREATE TABLE capacity_operations (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    runtime TEXT NOT NULL,
    compute_mode TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    gpu TEXT NOT NULL DEFAULT '',
    operation TEXT NOT NULL,
    resource_key TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('succeeded','capacity_unavailable','runtime_failed','provider_failed','pending')),
    error_code TEXT,
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ NOT NULL,
    duration_seconds DOUBLE PRECISION NOT NULL CHECK (duration_seconds >= 0),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX capacity_operations_history_idx ON capacity_operations(tenant_id,provider,runtime,compute_mode,region,gpu,completed_at DESC);
