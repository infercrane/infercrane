CREATE TABLE finops_reports (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    deployment_name TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('measured','partial','unavailable')),
    known_cost DOUBLE PRECISION CHECK(known_cost IS NULL OR known_cost >= 0),
    estimated_avoidable_cost DOUBLE PRECISION CHECK(estimated_avoidable_cost IS NULL OR estimated_avoidable_cost >= 0),
    summary_json JSONB NOT NULL,
    evidence_json JSONB NOT NULL,
    input_digest TEXT NOT NULL CHECK(length(input_digest)=64),
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX finops_reports_tenant_deployment_created ON finops_reports(tenant_id,deployment_id,created_at DESC);

CREATE TABLE autopilot_plans (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    deployment_name TEXT NOT NULL,
    recommendation_id TEXT NOT NULL REFERENCES inference_recommendations(id) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK(status IN ('advisory','approved')),
    objective TEXT NOT NULL,
    candidate_json JSONB NOT NULL,
    evidence_json JSONB NOT NULL,
    input_digest TEXT NOT NULL CHECK(length(input_digest)=64),
    approved_by TEXT,
    approved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,recommendation_id,objective),
    CHECK((status='advisory' AND approved_by IS NULL AND approved_at IS NULL) OR (status='approved' AND approved_by IS NOT NULL AND approved_at IS NOT NULL))
);
