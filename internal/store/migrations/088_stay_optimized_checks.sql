-- Stay optimized checks run on their own. Each endpoint's check state holds a
-- lease so one control-plane instance checks it at a time, when the next
-- check is due, and what the last check saw (so an engine release, model
-- revision, or price change is acted on once).
ALTER TABLE continual_optimization_states
    ADD COLUMN next_check_at TIMESTAMPTZ,
    ADD COLUMN last_check_at TIMESTAMPTZ,
    ADD COLUMN last_check_error TEXT,
    ADD COLUMN lease_owner TEXT,
    ADD COLUMN lease_expires_at TIMESTAMPTZ,
    ADD COLUMN observed_json JSONB NOT NULL DEFAULT '{}'::jsonb;
CREATE INDEX continual_optimization_states_due_idx
    ON continual_optimization_states(next_check_at);

-- A run a Stay optimized check proposed records why. NULL for a run a person
-- created.
ALTER TABLE optimization_campaigns ADD COLUMN origin_json JSONB;
CREATE INDEX optimization_campaigns_origin_idx
    ON optimization_campaigns(tenant_id,target_deployment_name,created_at DESC)
    WHERE origin_json IS NOT NULL;

-- A custom kernel is pinned to one engine release. When the endpoint's engine
-- changes, a check marks the kernel for re-qualification (it was qualified
-- but not serving) or switches it off (it no longer matches the engine the
-- endpoint runs). Optimization runs that qualify and serve kernels write the
-- rows; Stay optimized only moves them out of qualified or serving.
CREATE TABLE endpoint_custom_kernels (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    operator TEXT NOT NULL CHECK(length(operator) BETWEEN 1 AND 128),
    family TEXT NOT NULL CHECK(length(family) BETWEEN 1 AND 128),
    engine TEXT NOT NULL CHECK(length(engine) BETWEEN 1 AND 64),
    engine_version TEXT NOT NULL CHECK(length(engine_version) BETWEEN 1 AND 64),
    image_digest TEXT NOT NULL CHECK(image_digest ~ '^sha256:[a-f0-9]{64}$'),
    state TEXT NOT NULL CHECK(state IN ('qualified','serving','requalification_needed','off')),
    state_reason TEXT NOT NULL DEFAULT '' CHECK(length(state_reason) <= 512),
    source_campaign_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,deployment_id,operator,engine,engine_version)
);
CREATE INDEX endpoint_custom_kernels_deployment_idx
    ON endpoint_custom_kernels(tenant_id,deployment_id,state);
