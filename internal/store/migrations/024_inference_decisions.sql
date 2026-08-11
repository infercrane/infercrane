CREATE TABLE deployment_slo_policies (
    deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    max_ttft_p95_ms DOUBLE PRECISION,
    max_latency_p95_ms DOUBLE PRECISION,
    max_error_rate DOUBLE PRECISION,
    min_output_tokens_second DOUBLE PRECISION,
    max_hourly_cost DOUBLE PRECISION,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (max_ttft_p95_ms IS NULL OR max_ttft_p95_ms >= 0),
    CHECK (max_latency_p95_ms IS NULL OR max_latency_p95_ms >= 0),
    CHECK (max_error_rate IS NULL OR (max_error_rate >= 0 AND max_error_rate <= 1)),
    CHECK (min_output_tokens_second IS NULL OR min_output_tokens_second >= 0),
    CHECK (max_hourly_cost IS NULL OR max_hourly_cost >= 0),
    CHECK (num_nonnulls(max_ttft_p95_ms,max_latency_p95_ms,max_error_rate,min_output_tokens_second,max_hourly_cost) > 0)
);

CREATE INDEX deployment_slo_policies_tenant_idx ON deployment_slo_policies(tenant_id, deployment_id);

CREATE TABLE inference_recommendations (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('recommended','no_match','unknown')),
    algorithm_version TEXT NOT NULL,
    selected_evidence_id TEXT,
    reason TEXT NOT NULL,
    missing_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    candidates_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    input_snapshot_json JSONB NOT NULL,
    input_digest TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX inference_recommendations_deployment_created_idx
    ON inference_recommendations(tenant_id, deployment_id, created_at DESC, id DESC);

CREATE TABLE capacity_evidence (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    provider TEXT NOT NULL,
    runtime TEXT NOT NULL,
    compute_mode TEXT NOT NULL,
    region TEXT NOT NULL DEFAULT '',
    gpu TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('available','constrained','unavailable','unknown')),
    source TEXT NOT NULL,
    evidence_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    observed_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > observed_at)
);

CREATE INDEX capacity_evidence_lookup_idx
    ON capacity_evidence(tenant_id,provider,runtime,compute_mode,region,gpu,observed_at DESC);

ALTER TABLE external_target_policies
    ADD COLUMN overflow_mode TEXT NOT NULL DEFAULT 'health',
    ADD COLUMN queue_threshold DOUBLE PRECISION,
    ADD COLUMN breach_intervals INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN recovery_intervals INTEGER NOT NULL DEFAULT 2,
    ADD COLUMN cooldown_seconds INTEGER NOT NULL DEFAULT 60,
    ADD COLUMN signal_max_age_seconds INTEGER NOT NULL DEFAULT 30,
    ADD CONSTRAINT external_overflow_mode_check CHECK (overflow_mode IN ('health','health_and_queue')),
    ADD CONSTRAINT external_overflow_bounds_check CHECK (
        breach_intervals BETWEEN 1 AND 100 AND recovery_intervals BETWEEN 1 AND 100 AND
        cooldown_seconds BETWEEN 0 AND 86400 AND signal_max_age_seconds BETWEEN 1 AND 600 AND
        (overflow_mode='health' OR (queue_threshold IS NOT NULL AND queue_threshold > 0))
    );

CREATE TABLE overflow_states (
    deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    external_active BOOLEAN NOT NULL DEFAULT FALSE,
    consecutive_high INTEGER NOT NULL DEFAULT 0,
    consecutive_low INTEGER NOT NULL DEFAULT 0,
    last_changed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (consecutive_high >= 0 AND consecutive_low >= 0)
);

CREATE TABLE overflow_decisions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    route TEXT NOT NULL CHECK (route IN ('primary','external','unavailable')),
    action TEXT NOT NULL CHECK (action IN ('hold','overflow','recover','deny')),
    reason TEXT NOT NULL,
    signal_json JSONB NOT NULL,
    policy_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX overflow_decisions_deployment_created_idx ON overflow_decisions(tenant_id,deployment_id,created_at DESC,id DESC);
