CREATE TABLE optimization_campaigns (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    input_digest TEXT NOT NULL CHECK(length(input_digest)=64),
    model_identity TEXT NOT NULL,
    objective TEXT NOT NULL,
    source TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('awaiting_approval','approved','running','ranked','guard_passed','rejected','inconclusive','promoted','observed','cancelled','failed','cleaned')),
    proposal_json JSONB NOT NULL,
    max_candidates INTEGER NOT NULL CHECK(max_candidates BETWEEN 1 AND 100),
    approved_max_cost_usd DOUBLE PRECISION CHECK(approved_max_cost_usd IS NULL OR (approved_max_cost_usd > 0 AND approved_max_cost_usd <= 1000000)),
    approval_expires_at TIMESTAMPTZ,
    approved_by TEXT,
    approved_at TIMESTAMPTZ,
    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,idempotency_key),
    CHECK((state='awaiting_approval' AND approved_max_cost_usd IS NULL AND approval_expires_at IS NULL AND approved_by IS NULL AND approved_at IS NULL) OR state<>'awaiting_approval'),
    CHECK((approved_max_cost_usd IS NULL AND approval_expires_at IS NULL AND approved_by IS NULL AND approved_at IS NULL) OR (approved_max_cost_usd IS NOT NULL AND approval_expires_at IS NOT NULL AND approved_by IS NOT NULL AND approved_at IS NOT NULL))
);

CREATE TABLE optimization_candidate_runs (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    campaign_id TEXT NOT NULL REFERENCES optimization_campaigns(id) ON DELETE CASCADE,
    proposal_candidate_id TEXT NOT NULL,
    rank INTEGER NOT NULL CHECK(rank BETWEEN 1 AND 100),
    state TEXT NOT NULL CHECK(state IN ('proposed','provisioning','ready','measuring','validating','ranked','guard_passed','rejected','inconclusive','promoted','observed','failed','cancelled','cleaned')),
    evidence_state TEXT NOT NULL CHECK(evidence_state IN ('unmeasured','modeled','measured','qualified','rejected','stale')),
    deployment_spec_json JSONB NOT NULL,
    predicted_evidence_json JSONB NOT NULL,
    actual_evidence_json JSONB NOT NULL,
    deployment_name TEXT,
    revision_id TEXT,
    benchmark_id TEXT,
    quality_evidence_id TEXT,
    release_guard_evaluation_id TEXT,
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,campaign_id,proposal_candidate_id),
    UNIQUE(tenant_id,campaign_id,rank)
);

CREATE INDEX optimization_campaigns_tenant_created_idx ON optimization_campaigns(tenant_id,created_at DESC);
CREATE INDEX optimization_candidate_runs_campaign_rank_idx ON optimization_candidate_runs(tenant_id,campaign_id,rank);
