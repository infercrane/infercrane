ALTER TABLE release_guard_policies
    ADD COLUMN require_quality_evidence BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN minimum_quality_score DOUBLE PRECISION,
    ADD COLUMN max_quality_regression_percent DOUBLE PRECISION,
    ADD CONSTRAINT release_guard_quality_bounds CHECK (
        (minimum_quality_score IS NULL OR minimum_quality_score BETWEEN 0 AND 1) AND
        (max_quality_regression_percent IS NULL OR max_quality_regression_percent BETWEEN 0 AND 100)
    );

CREATE TABLE revision_quality_evidence (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id),
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    revision_id TEXT NOT NULL REFERENCES deployment_revisions(id) ON DELETE CASCADE,
    suite TEXT NOT NULL CHECK(octet_length(suite) BETWEEN 1 AND 256),
    suite_version TEXT NOT NULL CHECK(octet_length(suite_version) BETWEEN 1 AND 256),
    evaluator TEXT NOT NULL CHECK(octet_length(evaluator) BETWEEN 1 AND 256),
    evaluator_version TEXT NOT NULL CHECK(octet_length(evaluator_version) BETWEEN 1 AND 256),
    score DOUBLE PRECISION NOT NULL CHECK(score BETWEEN 0 AND 1),
    passed BOOLEAN NOT NULL,
    sample_count INTEGER NOT NULL CHECK(sample_count BETWEEN 1 AND 10000000),
    artifact_digest TEXT NOT NULL CHECK(artifact_digest ~ '^sha256:[a-f0-9]{64}$'),
    payload_digest TEXT NOT NULL CHECK(payload_digest ~ '^sha256:[a-f0-9]{64}$'),
    signature TEXT NOT NULL,
    public_key TEXT NOT NULL,
    algorithm TEXT NOT NULL CHECK(algorithm='Ed25519-SHA256'),
    key_id TEXT NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id,payload_digest)
);

CREATE INDEX revision_quality_evidence_lookup_idx
    ON revision_quality_evidence(tenant_id,deployment_id,revision_id,evaluated_at DESC,id DESC);
