CREATE TABLE continual_optimization_policies (
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    policy_json JSONB NOT NULL,
    generation BIGINT NOT NULL DEFAULT 1 CHECK(generation > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX continual_optimization_policies_tenant_idx
    ON continual_optimization_policies(tenant_id,updated_at DESC);

CREATE TABLE continual_optimization_states (
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    baseline_json JSONB,
    baseline_digest TEXT CHECK(baseline_digest IS NULL OR baseline_digest ~ '^sha256:[a-f0-9]{64}$'),
    last_experiment_at TIMESTAMPTZ,
    latest_decision_id TEXT,
    generation BIGINT NOT NULL DEFAULT 1 CHECK(generation > 0),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE continual_optimization_decisions (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    input_digest TEXT NOT NULL CHECK(input_digest ~ '^[a-f0-9]{64}$'),
    workload_source TEXT NOT NULL CHECK(workload_source IN ('public_prior','customer_observed')),
    workload_digest TEXT NOT NULL CHECK(workload_digest ~ '^sha256:[a-f0-9]{64}$'),
    action TEXT NOT NULL CHECK(action IN ('wait_for_evidence','screen_public_prior','start_experiment','observe_experiment','observe_canary','rollback_canary','recommend_promotion','retain_current')),
    promotion_eligible BOOLEAN NOT NULL,
    decision_json JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,deployment_id,input_digest)
);
CREATE INDEX continual_optimization_decisions_history_idx
    ON continual_optimization_decisions(tenant_id,deployment_id,created_at DESC);

ALTER TABLE continual_optimization_states
    ADD CONSTRAINT continual_optimization_latest_decision_fk
    FOREIGN KEY(latest_decision_id) REFERENCES continual_optimization_decisions(id) ON DELETE SET NULL;
