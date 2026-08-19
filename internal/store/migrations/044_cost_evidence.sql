CREATE TABLE IF NOT EXISTS cost_evidence (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
    source TEXT NOT NULL,
    scope TEXT NOT NULL,
    resource TEXT NOT NULL,
    currency TEXT NOT NULL,
    billing_unit TEXT NOT NULL,
    evidence_class TEXT NOT NULL CHECK (evidence_class IN ('measured', 'provider_reported')),
    amount DOUBLE PRECISION NOT NULL CHECK (amount >= 0 AND amount < 'Infinity'::DOUBLE PRECISION),
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL,
    valid_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    CHECK (window_end > window_start),
    CHECK (valid_until > observed_at),
    UNIQUE (tenant_id, deployment_id, revision_id, source, scope, resource, window_start, window_end)
);

CREATE INDEX IF NOT EXISTS cost_evidence_deployment_window_idx
    ON cost_evidence (tenant_id, deployment_id, window_end DESC, observed_at DESC);
