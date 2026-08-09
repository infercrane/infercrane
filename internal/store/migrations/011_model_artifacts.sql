CREATE TABLE model_artifacts (
 id TEXT PRIMARY KEY,
 tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
 source TEXT NOT NULL,
 repository TEXT NOT NULL,
 requested_revision TEXT NOT NULL,
 immutable_revision TEXT NOT NULL,
 model_identity TEXT NOT NULL,
 approximate_size_bytes BIGINT CHECK(approximate_size_bytes IS NULL OR approximate_size_bytes >= 0),
 cache_state TEXT NOT NULL CHECK(cache_state IN ('unknown','cached','partial','absent')),
 runtime_compatibility_json JSONB NOT NULL DEFAULT '{}',
 resolved_at TIMESTAMPTZ NOT NULL,
 UNIQUE(tenant_id,source,repository,immutable_revision)
);
ALTER TABLE deployment_revisions
 ADD COLUMN model_artifact_id TEXT REFERENCES model_artifacts(id) ON DELETE RESTRICT;
