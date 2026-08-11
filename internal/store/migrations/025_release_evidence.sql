ALTER TABLE release_guard_policies
    ADD COLUMN require_compatibility_evidence BOOLEAN NOT NULL DEFAULT TRUE,
    ADD COLUMN require_synthetic_evidence BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN max_cost_regression_percent DOUBLE PRECISION,
    ADD COLUMN auto_rollback_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN auto_rollback_window_seconds INTEGER NOT NULL DEFAULT 300,
    ADD COLUMN validation_max_requests INTEGER NOT NULL DEFAULT 100,
    ADD COLUMN validation_max_concurrency INTEGER NOT NULL DEFAULT 4,
    ADD CONSTRAINT release_guard_v2_bounds CHECK (
        (max_cost_regression_percent IS NULL OR max_cost_regression_percent >= 0) AND
        auto_rollback_window_seconds BETWEEN 30 AND 3600 AND
        validation_max_requests BETWEEN 1 AND 10000 AND
        validation_max_concurrency BETWEEN 1 AND 128
    );

CREATE TABLE inference_passports (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
    payload_json TEXT NOT NULL CHECK (octet_length(payload_json) <= 4194304),
    payload_digest TEXT NOT NULL,
    signature TEXT NOT NULL,
    public_key TEXT NOT NULL,
    algorithm TEXT NOT NULL CHECK (algorithm='Ed25519-SHA256'),
    key_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, deployment_id, revision_id, payload_digest, key_id)
);

CREATE INDEX inference_passports_deployment_created_idx
    ON inference_passports(tenant_id,deployment_id,created_at DESC,id DESC);

CREATE TABLE release_guard_monitors (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    promoted_revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
    rollback_revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('observing','accepted','rolled_back','failed')),
    deadline TIMESTAMPTZ NOT NULL,
    evaluation_id TEXT REFERENCES release_guard_evaluations(id) ON DELETE SET NULL,
    policy_json JSONB NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (deployment_id,promoted_revision_id)
);

CREATE INDEX release_guard_monitors_pending_idx
    ON release_guard_monitors(tenant_id,status,deadline);
