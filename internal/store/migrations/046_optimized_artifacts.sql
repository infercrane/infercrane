CREATE TABLE optimized_artifacts (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    idempotency_key TEXT NOT NULL,
    input_digest TEXT NOT NULL CHECK(length(input_digest)=64),
    base_model_artifact_id TEXT NOT NULL REFERENCES model_artifacts(id) ON DELETE RESTRICT,
    kind TEXT NOT NULL CHECK(kind IN ('quantized_checkpoint','speculator_checkpoint','tensorrt_engine')),
    format TEXT NOT NULL,
    tool TEXT NOT NULL,
    tool_version TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    builder_image_digest TEXT NOT NULL,
    calibration_digest TEXT,
    license_spdx TEXT NOT NULL,
    configuration_json JSONB NOT NULL,
    hardware_constraints_json JSONB NOT NULL,
    requires_quality_review BOOLEAN NOT NULL DEFAULT TRUE CHECK(requires_quality_review),
    state TEXT NOT NULL CHECK(state IN ('planned','building','ready','failed','stale')),
    evidence_state TEXT NOT NULL CHECK(evidence_state IN ('unmeasured','measured','qualified','rejected','stale')),
    output_repository TEXT,
    output_immutable_revision TEXT,
    output_digest TEXT,
    build_evidence_json JSONB NOT NULL DEFAULT '{}',
    quality_evidence_id TEXT REFERENCES revision_quality_evidence(id) ON DELETE RESTRICT,
    failure_code TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE(tenant_id,idempotency_key),
    UNIQUE(tenant_id,output_digest),
    CHECK((state='ready' AND output_repository IS NOT NULL AND output_immutable_revision IS NOT NULL AND output_digest IS NOT NULL AND failure_code IS NULL) OR state<>'ready'),
    CHECK((state='failed' AND failure_code IS NOT NULL AND output_repository IS NULL AND output_immutable_revision IS NULL AND output_digest IS NULL) OR state<>'failed')
);

ALTER TABLE optimization_candidate_runs
    ADD COLUMN optimized_artifact_id TEXT REFERENCES optimized_artifacts(id) ON DELETE RESTRICT;

CREATE INDEX optimized_artifacts_tenant_created_idx ON optimized_artifacts(tenant_id,created_at DESC);
